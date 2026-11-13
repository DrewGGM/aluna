//go:build restore

// Package restore rehearses the point-in-time recovery of an entity's cluster (FF-13, docs/runbook.md): a base
// backup and the archived WAL bring the database back to the instant before a mistake, and the restored database
// passes `aluna audit verify` and the reconciliation controls. It drives the Docker CLI, as the runbook does.
package restore

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/reconciliation"
)

// image is the PostgreSQL of deploy/compose.yaml.
const image = "postgres:16"

// TestPointInTimeRestore: a cluster archives its WAL; after a base backup and more work, someone with the owner's
// role tampers a sealed audit record and a balance, which the audit chain and the controls catch. The base backup
// plus the WAL up to the instant before the mistake bring back every posting and block up to that instant and none
// after it, and the restored database passes the audit chain and the controls. It then seals again into a new
// anchor generation, as the runbook says, and the chain still verifies from its first block.
func TestPointInTimeRestore(t *testing.T) {
	bin, anchors := buildAluna(t), t.TempDir()
	drill := "aluna-restore-" + strings.ToLower(rand.Text()[:10])
	wal, base := volume(t, drill+"-wal"), volume(t, drill+"-base")
	docker(t, "run", "--rm", "-v", wal+":/wal", "-v", base+":/base", image, "chown", "postgres:postgres", "/wal", "/base")

	primary := start(t, drill+"-primary",
		dockerOptions{"-e", "POSTGRES_USER=aluna", "-e", "POSTGRES_PASSWORD=aluna", "-v", wal + ":/wal", "-v", base + ":/base"},
		serverSettings{"wal_level=replica", "archive_mode=on", "archive_command=test ! -f /wal/%f && cp %p /wal/%f"})
	aluna(t, bin, primary, anchors, "migrate", "up")
	e := newEntity(t, primary, anchors)
	e.work(t, 3)
	docker(t, "exec", "-u", "postgres", primary.name, "pg_basebackup", "-U", "aluna", "-D", "/base/data", "-X", "stream", "-c", "fast")
	e.work(t, 3) // only the WAL brings these back
	target, kept := e.dbNow(t), e.done
	e.work(t, 2) // after the target: lost on purpose
	e.mistake(t)
	err := run(t.Context(), bin, primary, anchors, "audit", "verify", "--from", "1", "--to", strconv.Itoa(e.done.blocks))
	if err == nil || !strings.Contains(err.Error(), "breaks at block 1:") {
		t.Fatalf("audit verify on the tampered primary = %v, want the chain broken at block 1", err)
	}
	if breaks := e.controls(t, primary); !slices.Contains(breaks, "balances_match_entries") {
		t.Fatalf("the controls found %v on the tampered primary, want balances_match_entries", breaks)
	}
	archiveAll(t, primary)
	docker(t, "stop", primary.name)

	docker(t, "run", "--rm", "-u", "postgres", "-v", base+":/base", image, "touch", "/base/data/recovery.signal")
	restored := start(t, drill+"-restored", dockerOptions{"-e", "PGDATA=/base/data", "-v", wal + ":/wal", "-v", base + ":/base"},
		serverSettings{"restore_command=cp /wal/%f %p", "recovery_target_time=" + target, "recovery_target_action=promote"})
	waitPromoted(t, restored)
	e.expectRestored(t, restored, kept)
	aluna(t, bin, restored, anchors, "audit", "verify", "--from", "1", "--to", strconv.Itoa(kept.blocks))
	if breaks := e.controls(t, restored); len(breaks) != 0 {
		t.Fatalf("breaks open on the restored database: %v", breaks)
	}

	next := anchorGeneration(t, anchors, kept.blocks)
	resumed := e.resume(t, restored, kept, next)
	resumed.work(t, 1)
	aluna(t, bin, restored, next, "audit", "verify", "--from", "1", "--to", strconv.Itoa(resumed.done.blocks))
}

// anchorGeneration is a new anchor directory with the anchors of blocks 1 to last copied from anchors. The old one
// keeps the anchors of the lost blocks, and an anchor is never removed: if the restored sequence jumps ahead, as it
// usually does, verify reads them as anchored blocks that were deleted; if not, sealing reuses an anchored id and stops.
func anchorGeneration(t *testing.T, anchors string, last int) string {
	t.Helper()
	next := t.TempDir()
	for id := 1; id <= last; id++ {
		name := fmt.Sprintf("%020d.sha256", id)
		hash, err := os.ReadFile(filepath.Join(anchors, name)) //nolint:gosec // the test's own directory
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(next, name), hash, 0o400); err != nil { //nolint:gosec // the test's own directory
			t.Fatal(err)
		}
	}
	return next
}

// cluster is a PostgreSQL container of the drill and the owner's DSN to it from the host.
type cluster struct{ name, dsn string }

// app is the cluster's DSN with the application's role, as the processes connect.
func (c cluster) app() string { return c.dsn + "&options=-c%20role%3Daluna_app" }

// open opens a pool to dsn that the test closes at the end.
func open(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// docker runs the Docker CLI and returns its trimmed output; a failure stops the test.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := dockerCmd(t.Context(), args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, errOut.String())
	}
	return strings.TrimSpace(out.String())
}

// dockerCmd is the Docker CLI with args, in one place for its gosec exemption.
func dockerCmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "docker", args...) //nolint:gosec // the arguments are the drill's own
}

// volume creates a Docker volume that the test removes at the end.
func volume(t *testing.T, name string) string {
	t.Helper()
	docker(t, "volume", "create", name)
	t.Cleanup(func() { cleanup("volume", "rm", "-f", name) })
	return name
}

// cleanup runs a Docker command after the test, whose context is already done.
func cleanup(args ...string) {
	_ = dockerCmd(context.Background(), args...).Run() // a leftover shows in docker ps
}

type (
	// dockerOptions are the options of docker run: environment and volumes.
	dockerOptions []string
	// serverSettings are the PostgreSQL settings the server starts with, each as name=value.
	serverSettings []string
)

// start runs image as container name with options and settings, with its port on the host's loopback, and waits for it
// to accept TCP connections: the image's initialization serves only the socket.
func start(t *testing.T, name string, options dockerOptions, settings serverSettings) cluster {
	t.Helper()
	args := append(append([]string{"run", "-d", "--name", name, "-p", "127.0.0.1::5432"}, options...), image)
	for _, s := range settings {
		args = append(args, "-c", s)
	}
	t.Cleanup(func() { cleanup("rm", "-f", "-v", name) }) // before the run: a container created but not started leaks too
	docker(t, args...)
	deadline := time.Now().Add(2 * time.Minute)
	for dockerCmd(t.Context(), "exec", name, "pg_isready", "-h", "127.0.0.1", "-U", "aluna", "-d", "aluna").Run() != nil {
		if time.Now().After(deadline) {
			t.Fatalf("%s is not ready:\n%s", name, logs(name))
		}
		time.Sleep(500 * time.Millisecond)
	}
	addr := docker(t, "port", name, "5432/tcp")
	return cluster{name: name, dsn: "postgres://aluna:aluna@" + addr + "/aluna?sslmode=disable"}
}

// logs is the tail of the container's log, to explain a failure.
func logs(name string) string {
	out, _ := dockerCmd(context.Background(), "logs", "--tail", "40", name).CombinedOutput() // diagnostics only
	return string(out)
}

// buildAluna compiles the binary under test.
func buildAluna(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "aluna")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "./cmd/aluna") //nolint:gosec // bin is a path in the test's TempDir
	cmd.Dir = filepath.Join("..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// run runs aluna args against c: migrate as the owner, the rest as the application.
func run(ctx context.Context, bin string, c cluster, anchors string, args ...string) error {
	dsn := c.app()
	if args[0] == "migrate" {
		dsn = c.dsn
	}
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // the binary this test built
	cmd.Env = append(os.Environ(), "ALUNA_DATABASE_URL="+dsn, "ALUNA_AUDIT_ANCHOR_DIR="+anchors)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Join(err, errors.New(string(out)))
	}
	return nil
}

// aluna is run that stops the test on a failure.
func aluna(t *testing.T, bin string, c cluster, anchors string, args ...string) {
	t.Helper()
	if err := run(t.Context(), bin, c, anchors, args...); err != nil {
		t.Fatalf("aluna %s: %v", strings.Join(args, " "), err)
	}
}

// archiveAll switches to a new WAL segment and waits until the archiver has copied the one it closed, so the
// archive holds every commit up to now.
func archiveAll(t *testing.T, c cluster) {
	t.Helper()
	pool := open(t, c.dsn)
	var segment string
	if err := pool.QueryRow(t.Context(), `SELECT pg_walfile_name(pg_switch_wal())`).Scan(&segment); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		var archived bool
		if err := pool.QueryRow(t.Context(), `SELECT coalesce(last_archived_wal >= $1, false) FROM pg_stat_archiver`, segment).Scan(&archived); err != nil {
			t.Fatal(err)
		}
		if archived {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("WAL segment %s was not archived:\n%s", segment, logs(c.name))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitPromoted waits until c finishes replaying the WAL up to its target and accepts writes.
func waitPromoted(t *testing.T, c cluster) {
	t.Helper()
	pool := open(t, c.dsn)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var recovering bool
		err := pool.QueryRow(t.Context(), `SELECT pg_is_in_recovery()`).Scan(&recovering)
		if err == nil && !recovering {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not promoted (%v):\n%s", c.name, err, logs(c.name))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// progress is how far the work went: postings made and the last audit block sealed.
type progress struct{ postings, blocks int }

// entity is the drill's legal entity on the primary: two ledger accounts and the work done so far.
type entity struct {
	id             ids.ID
	cop            money.Currency
	cash, deposits ids.ID
	owner, app     *pgxpool.Pool
	ledger         *ledger.Service
	records        *audit.Service
	done           progress
	businessDate   time.Time
}

// newEntity creates the entity and its accounts on c, sealing its audit blocks into anchors.
func newEntity(t *testing.T, c cluster, anchors string) *entity {
	t.Helper()
	e := &entity{id: ids.New(), owner: open(t, c.dsn), app: open(t, c.app()), businessDate: time.Date(2026, time.March, 2, 0, 0, 0, 0, time.UTC)}
	if _, err := e.owner.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
		VALUES ($1, 'Restore drill', $2, 'COP')`, e.id, e.businessDate); err != nil {
		t.Fatal(err)
	}
	var err error
	if e.cop, err = money.CurrencyOf("COP"); err != nil {
		t.Fatal(err)
	}
	if e.records, err = audit.WithFileAnchor(anchors); err != nil {
		t.Fatal(err)
	}
	events, err := outbox.New(e.app)
	if err != nil {
		t.Fatal(err)
	}
	e.ledger = ledger.New(calendar.New(events, time.Now), events)
	e.inTx(t, func(tx postgres.Tx) error {
		cash, err := e.account(t, tx, "1105", ledger.KindAsset)
		if err != nil {
			return err
		}
		e.cash = cash
		e.deposits, err = e.account(t, tx, "2105", ledger.KindLiability)
		return err
	})
	return e
}

func (e *entity) account(t *testing.T, tx postgres.Tx, code string, kind ledger.Kind) (ids.ID, error) {
	a, err := e.ledger.CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
		LegalEntityID: e.id, Code: code, Kind: kind, Currency: e.cop, Mode: ledger.BalanceModeSync, LowerLimit: -1_000_000_000_000,
	})
	return a.ID, err
}

func (e *entity) inTx(t *testing.T, fn func(tx postgres.Tx) error) {
	t.Helper()
	if err := postgres.NewUnitOfWork(e.app).Do(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

// work posts n deposits, each audited in its own transaction as the api does, and seals them into a block.
func (e *entity) work(t *testing.T, n int) {
	t.Helper()
	for range n {
		e.inTx(t, func(tx postgres.Tx) error {
			posted, err := e.ledger.Post(t.Context(), tx, ledger.PostInput{
				LegalEntityID: e.id, Code: "cash.deposit", ValueDate: e.businessDate, BusinessDate: e.businessDate, Source: "drill",
				ExternalRef: ids.New().String(), Actor: "drill", Channel: "api", RuleVersion: "v1",
				Entries: []ledger.EntryInput{{AccountID: e.cash, Amount: money.New(50_000_00, e.cop)}, {AccountID: e.deposits, Amount: money.New(-50_000_00, e.cop)}},
			})
			if err != nil {
				return err
			}
			return e.records.Record(t.Context(), tx, audit.Record{
				Actor: "drill", Role: "operator", Channel: "api", IP: "127.0.0.1", CorrelationID: ids.New().String(),
				Command: "POST /v1/transactions", Targets: []string{posted.ID.String()}, Result: "201",
			})
		})
	}
	e.done.postings += n
	block, err := e.records.Seal(t.Context(), postgres.NewUnitOfWork(e.app), 1000)
	if err != nil || block == 0 {
		t.Fatalf("seal: block %d, %v", block, err)
	}
	e.done.blocks = int(block)
}

// dbNow is the database's current instant, as recovery_target_time takes it.
func (e *entity) dbNow(t *testing.T) string {
	t.Helper()
	var now string
	if err := e.owner.QueryRow(t.Context(), `SELECT clock_timestamp()::text`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// mistake is what PITR exists for: someone with the owner's role turns the triggers off and changes a sealed audit
// record and a balance.
func (e *entity) mistake(t *testing.T) {
	t.Helper()
	conn, err := e.owner.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`SET session_replication_role = replica`, nil},
		{`UPDATE audit.records SET result = '500' WHERE seq = 1`, nil},
		{`UPDATE ledger.balances SET posted = posted + 1 WHERE account_id = $1`, []any{e.cash}},
		{`RESET session_replication_role`, nil},
	} {
		if _, err := conn.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.sql, err)
		}
	}
}

// resume is the entity on the restored cluster c, where it is as it was at the target, sealing into anchors.
func (e *entity) resume(t *testing.T, c cluster, at progress, anchors string) *entity {
	t.Helper()
	r := *e
	r.owner, r.app, r.done = open(t, c.dsn), open(t, c.app()), at
	var err error
	if r.records, err = audit.WithFileAnchor(anchors); err != nil {
		t.Fatal(err)
	}
	events, err := outbox.New(r.app)
	if err != nil {
		t.Fatal(err)
	}
	r.ledger = ledger.New(calendar.New(events, time.Now), events)
	return &r
}

// controls runs the reconciliation controls on c and returns the controls of the entity's open breaks.
func (e *entity) controls(t *testing.T, c cluster) []string {
	t.Helper()
	pool := open(t, c.app())
	events, err := outbox.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	recon := reconciliation.New(events)
	if err := recon.RunControls(t.Context(), postgres.NewUnitOfWork(pool)); err != nil {
		t.Fatal(err)
	}
	page, err := recon.Breaks(t.Context(), pool, e.id, reconciliation.BreakOpen, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	controls := make([]string, 0, len(page.Breaks))
	for _, b := range page.Breaks {
		controls = append(controls, string(b.Control))
	}
	return controls
}

// expectRestored checks that c has the postings and blocks up to the target, and nothing after it.
func (e *entity) expectRestored(t *testing.T, c cluster, kept progress) {
	t.Helper()
	var posted, blocks int
	if err := open(t, c.dsn).QueryRow(t.Context(), `SELECT (SELECT count(*) FROM ledger.transactions WHERE legal_entity_id = $1),
		(SELECT count(*) FROM audit.blocks)`, e.id).Scan(&posted, &blocks); err != nil {
		t.Fatal(err)
	}
	if got := (progress{postings: posted, blocks: blocks}); got != kept {
		t.Fatalf("restored %+v, want %+v (the primary reached %+v)", got, kept, e.done)
	}
}
