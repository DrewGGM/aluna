//go:build integration

package deposits_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/outbox"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/pricing"
	"github.com/DrewGGM/aluna/internal/products"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, &testPool)) }

// recorder keeps the emitted events instead of publishing them.
type recorder struct{ events []outbox.Event }

func (r *recorder) Emit(_ context.Context, _ postgres.Tx, e outbox.Event) error {
	r.events = append(r.events, e)
	return nil
}

const author = "product-designer"

// world is a legal entity on 2026-03-02 with the services of the modules deposits uses.
type world struct {
	entity   ids.ID
	events   *recorder
	ledger   *ledger.Service
	parties  *party.Service
	products *products.Service
	pricing  *pricing.Service
	deposits *deposits.Service
}

func newWorld(t *testing.T) world {
	t.Helper()
	return newWorldOn(t, "2026-03-02")
}

// screenedClear screens every party clear: these tests are not about screening.
type screenedClear struct{}

func (screenedClear) ScreenParty(context.Context, postgres.Tx, party.Party) (bool, error) {
	return true, nil
}

// newWorldOn is a world whose business date is day.
func newWorldOn(t *testing.T, day string) world {
	t.Helper()
	w := world{entity: ids.New(), events: &recorder{}}
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entity', $2, 'COP')`, w.entity, day)
		return err
	})
	w.ledger, w.parties, w.products = ledger.New(calendar.New(), w.events), party.New(w.events, screenedClear{}), products.New(w.events)
	w.pricing = pricing.New(w.products, calendar.New(), pricing.SingleAccount)
	w.deposits = deposits.New(w.ledger, w.parties, w.products, calendar.New(), w.pricing, w.events)
	return w
}

func inTx(t *testing.T, fn func(tx postgres.Tx) error) {
	t.Helper()
	if err := postgres.NewUnitOfWork(testPool).Do(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

func try(t *testing.T, fn func(tx postgres.Tx) error) error {
	t.Helper()
	return postgres.NewUnitOfWork(testPool).Do(t.Context(), fn)
}

// party registers a party of the world's entity with KYC kyc (pending, or approved through a change).
func (w world) party(t *testing.T, kyc party.KYCStatus) ids.ID {
	t.Helper()
	var p party.Party
	inTx(t, func(tx postgres.Tx) error {
		var err error
		p, err = w.parties.Register(t.Context(), tx, party.RegisterInput{
			LegalEntityID: w.entity, Kind: party.KindNatural, ExternalRef: "onboarding-" + ids.New().String(), RiskLevel: party.RiskLow,
		})
		if err != nil || kyc == party.KYCPending {
			return err
		}
		_, err = w.parties.ChangeKYC(t.Context(), tx, w.entity, p.ID, 1, kyc)
		return err
	})
	return p.ID
}

// product creates a product of the world's entity with the definition of a products testdata file and,
// if publish, publishes it.
func (w world) product(t *testing.T, file string, publish bool) (products.Product, products.Version) {
	t.Helper()
	return w.productOf(t, fixture(t, file), publish)
}

// fixture is the definition of a products testdata file.
func fixture(t *testing.T, file string) []byte {
	t.Helper()
	definition, err := os.ReadFile(filepath.Join("..", "products", "internal", "domain", "testdata", file)) //nolint:gosec // products' test fixtures
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

// productOf is product with definition.
func (w world) productOf(t *testing.T, definition []byte, publish bool) (products.Product, products.Version) {
	t.Helper()
	var p products.Product
	var v products.Version
	inTx(t, func(tx postgres.Tx) error {
		var err error
		if p, err = w.products.CreateProduct(t.Context(), tx, w.entity, "p"+strings.ReplaceAll(ids.New().String(), "-", ""), "Product"); err != nil {
			return err
		}
		if v, err = w.products.CreateVersion(t.Context(), tx, w.entity, p.ID, author, definition); err != nil || !publish {
			return err
		}
		if _, err = w.products.RequestPublication(t.Context(), tx, w.entity, v.ID, author); err != nil {
			return err
		}
		v, err = w.products.Publish(t.Context(), tx, w.entity, v.ID)
		return err
	})
	return p, v
}

func (w world) open(t *testing.T, partyID, productID ids.ID) (deposits.Account, error) {
	t.Helper()
	var a deposits.Account
	err := try(t, func(tx postgres.Tx) error {
		var err error
		a, err = w.deposits.Open(t.Context(), tx, deposits.OpenInput{LegalEntityID: w.entity, PartyID: partyID, ProductID: productID})
		return err
	})
	return a, err
}

func (w world) change(t *testing.T, id ids.ID, to deposits.Status) error {
	t.Helper()
	return try(t, func(tx postgres.Tx) error {
		return w.deposits.ChangeStatus(t.Context(), tx, w.entity, id, to, "operator-a")
	})
}

// TestAnAccountOpens (FR-19, FR-22, US-5): with KYC approved, the account opens active, anchored to the
// product's version in force, on the entity's business date, held in a liability ledger account of the
// product's currency that cannot go below 0, and its opening is an event.
func TestAnAccountOpens(t *testing.T) {
	w := newWorld(t)
	product, version := w.product(t, "valid-savings.json", true)

	a, err := w.open(t, w.party(t, party.KYCApproved), product.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := w.deposits.Account(t.Context(), testPool, w.entity, a.ID)
	if err != nil {
		t.Fatal(err)
	}

	if stored != a || a.Status != deposits.Active || a.ProductVersionID != version.ID ||
		a.OpenedOn != (calendar.Date{Year: 2026, Month: time.March, Day: 2}) || a.LastCustomerActivityOn != a.OpenedOn {
		t.Fatalf("account = %+v, stored %+v; want it active, anchored to %s, opened on 2026-03-02", a, stored, version.ID)
	}
	w.expectLedgerAccount(t, a.LedgerAccountID)
	w.expectOpened(t, a)
}

// expectOpened checks that the last event is the opening of account a, with its payload.
func (w world) expectOpened(t *testing.T, a deposits.Account) {
	t.Helper()
	if len(w.events.events) == 0 {
		t.Fatal("no events, want the account's opening")
	}
	last := w.events.events[len(w.events.events)-1]
	data, err := json.Marshal(last.Data)
	want := `{"account_id":"` + a.ID.String() + `","party_id":"` + a.PartyID.String() + `","product_version_id":"` +
		a.ProductVersionID.String() + `","currency":"COP","status":"` + string(a.Status) + `"}`
	if err != nil || last.Type != "aluna.deposits.account.opened.v1" || last.Subject != "deposit-accounts/"+a.ID.String() ||
		last.LegalEntityID != w.entity || last.SchemaVersion != 1 || string(data) != want {
		t.Fatalf("last event = %+v with data %s, want the opening %s", last, data, want)
	}
}

// expectLedgerAccount checks that ledger account id is a sync COP liability that cannot go below 0.
func (w world) expectLedgerAccount(t *testing.T, id ids.ID) {
	t.Helper()
	held, err := w.ledger.Account(t.Context(), testPool, w.entity, id)
	if err != nil {
		t.Fatal(err)
	}
	if held.Kind != ledger.KindLiability || held.LowerLimit != 0 || held.Currency.Code() != "COP" || held.Mode != ledger.BalanceModeSync {
		t.Fatalf("ledger account = %+v, want a sync COP liability with lower limit 0", held)
	}
}

// TestTheKYCMustSatisfyTheProduct (FR-17): a party whose KYC the product does not allow cannot open it, and
// nothing is created; with a product that allows a pending KYC (a simplified deposit), the account opens
// pending, until the KYC is approved.
func TestTheKYCMustSatisfyTheProduct(t *testing.T) {
	w := newWorld(t)
	pending := w.party(t, party.KYCPending)
	savings, _ := w.product(t, "valid-savings.json", true)
	simplified, _ := w.product(t, "valid-simplified-deposit.json", true)

	_, rejected := w.open(t, pending, savings.ID)
	opened, err := w.open(t, pending, simplified.ID)

	if !errors.Is(rejected, deposits.ErrKYCNotSatisfied) {
		t.Fatalf("pending KYC on a product that needs it approved = %v, want ErrKYCNotSatisfied", rejected)
	}
	if err != nil || opened.Status != deposits.Pending {
		t.Fatalf("pending KYC on a simplified deposit = %+v (%v), want it pending", opened, err)
	}
	w.expectOpened(t, opened)
	if n := count(t, `SELECT count(*) FROM deposits.accounts WHERE legal_entity_id = $1`, w.entity); n != 1 {
		t.Fatalf("accounts of the entity = %d, want only the simplified one", n)
	}
}

// TestAnAccountNeedsAProductInForce: a product without a published version, another entity's product and an
// unknown party do not open an account.
func TestAnAccountNeedsAProductInForce(t *testing.T) {
	w := newWorld(t)
	approved := w.party(t, party.KYCApproved)
	draft, _ := w.product(t, "valid-savings.json", false)
	other := newWorld(t)
	foreign, _ := other.product(t, "valid-savings.json", true)

	_, unpublished := w.open(t, approved, draft.ID)
	_, notOurs := w.open(t, approved, foreign.ID)
	savings, _ := w.product(t, "valid-savings.json", true)
	_, unknown := w.open(t, ids.New(), savings.ID)

	if !errors.Is(unpublished, products.ErrNoPublishedVersion) || !errors.Is(notOurs, products.ErrProductNotFound) || !errors.Is(unknown, party.ErrNotFound) {
		t.Fatalf("unpublished = %v, another entity's product = %v, unknown party = %v", unpublished, notOurs, unknown)
	}
}

// pendingAccount opens a simplified deposit for a party with KYC pending, and returns it and its party.
func (w world) pendingAccount(t *testing.T) (deposits.Account, ids.ID) {
	t.Helper()
	simplified, _ := w.product(t, "valid-simplified-deposit.json", true)
	partyID := w.party(t, party.KYCPending)
	a, err := w.open(t, partyID, simplified.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a, partyID
}

// TestAPendingAccountWaitsForItsKYC (FR-22): a pending account is not activated while its party's KYC is
// pending, does not go where the diagram does not take it, and another entity does not change it.
func TestAPendingAccountWaitsForItsKYC(t *testing.T) {
	w := newWorld(t)
	a, _ := w.pendingAccount(t)

	frozen, active := w.change(t, a.ID, deposits.Frozen), w.change(t, a.ID, deposits.Active)
	other := try(t, func(tx postgres.Tx) error {
		return w.deposits.ChangeStatus(t.Context(), tx, ids.New(), a.ID, deposits.Closed, "operator-a")
	})

	if !errors.Is(frozen, deposits.ErrInvalidTransition) || !errors.Is(active, deposits.ErrKYCNotSatisfied) || !errors.Is(other, deposits.ErrAccountNotFound) {
		t.Fatalf("pending → frozen = %v, activate with the KYC pending = %v, another entity closes it = %v", frozen, active, other)
	}
}

// TestTheLifeCycleFollowsTheDiagram (FR-22): once its party's KYC is approved, a pending account goes
// active → frozen → active → dormant → active → closed, each change recorded with its actor, and a closed
// account does not reopen.
func TestTheLifeCycleFollowsTheDiagram(t *testing.T) {
	w := newWorld(t)
	a, partyID := w.pendingAccount(t)
	inTx(t, func(tx postgres.Tx) error {
		_, err := w.parties.ChangeKYC(t.Context(), tx, w.entity, partyID, 1, party.KYCApproved)
		return err
	})

	for _, to := range []deposits.Status{deposits.Active, deposits.Frozen, deposits.Active, deposits.Dormant, deposits.Active, deposits.Closed} {
		if err := w.change(t, a.ID, to); err != nil {
			t.Fatalf("→ %s: %v", to, err)
		}
	}
	reopen := w.change(t, a.ID, deposits.Active)

	got, err := w.deposits.Account(t.Context(), testPool, w.entity, a.ID)
	if err != nil || got.Status != deposits.Closed || !errors.Is(reopen, deposits.ErrInvalidTransition) {
		t.Fatalf("account = %s (%v), reopen = %v; want it closed for good", got.Status, err, reopen)
	}
	if n := count(t, `SELECT count(*) FROM deposits.status_changes WHERE account_id = $1 AND actor = 'operator-a'`, a.ID); n != 6 {
		t.Fatalf("recorded changes = %d, want 6", n)
	}
}

// TestAnAccountClosesEmpty: an account with a balance does not close, and one with a pending hold does not
// either: the hold is resolved first.
func TestAnAccountClosesEmpty(t *testing.T) {
	w := newWorld(t)
	savings, _ := w.product(t, "valid-savings.json", true)
	funded, err := w.open(t, w.party(t, party.KYCApproved), savings.ID)
	if err != nil {
		t.Fatal(err)
	}
	held, err := w.open(t, w.party(t, party.KYCApproved), savings.ID)
	if err != nil {
		t.Fatal(err)
	}
	cash, cop := w.cash(t), copOf(t)
	today := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	deposit := func(tx postgres.Tx, to ids.ID, units int64) error {
		_, err := w.ledger.Post(t.Context(), tx, ledger.PostInput{
			LegalEntityID: w.entity, Code: "deposit", ValueDate: today, BusinessDate: today, Source: "test", ExternalRef: ids.New().String(),
			Actor: "tester", Channel: "test", RuleVersion: "v1",
			Entries: []ledger.EntryInput{{AccountID: cash, Amount: money.New(units, cop)}, {AccountID: to, Amount: money.New(-units, cop)}},
		})
		return err
	}
	inTx(t, func(tx postgres.Tx) error {
		if err := deposit(tx, funded.LedgerAccountID, 10000); err != nil {
			return err
		}
		if err := deposit(tx, held.LedgerAccountID, 500); err != nil {
			return err
		}
		// the account holds what it has: its funds wait for a debit to the cash account
		_, err := w.ledger.PlaceHold(t.Context(), tx, ledger.PlaceHoldInput{
			LegalEntityID: w.entity, AccountID: held.LedgerAccountID, CounterAccountID: cash, Amount: money.New(500, cop), ExpiresAt: time.Now().Add(time.Hour),
		})
		return err
	})

	withBalance, withHold := w.change(t, funded.ID, deposits.Closed), w.change(t, held.ID, deposits.Closed)

	if !errors.Is(withBalance, deposits.ErrNotEmpty) || !errors.Is(withHold, deposits.ErrPendingHolds) {
		t.Fatalf("close with a balance = %v, with a pending hold = %v; want ErrNotEmpty and ErrPendingHolds", withBalance, withHold)
	}
}

func copOf(t *testing.T) money.Currency {
	t.Helper()
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	return cop
}

// cash is an asset account of the world's entity that may go negative, the other side of the test postings.
func (w world) cash(t *testing.T) ids.ID {
	t.Helper()
	var a ledger.Account
	inTx(t, func(tx postgres.Tx) error {
		var err error
		a, err = w.ledger.CreateAccount(t.Context(), tx, ledger.CreateAccountInput{
			LegalEntityID: w.entity, Code: "cash/" + ids.New().String(), Kind: ledger.KindAsset, Currency: copOf(t),
			Mode: ledger.BalanceModeSync, LowerLimit: -1000000,
		})
		return err
	})
	return a.ID
}

// TestTheDatabaseGuardsTheAccounts: even as the tables' owner, an account is not deleted or truncated, its
// identity does not change, a closed one does not change and its history is not touched.
func TestTheDatabaseGuardsTheAccounts(t *testing.T) {
	w := newWorld(t)
	savings, _ := w.product(t, "valid-savings.json", true)
	open, err := w.open(t, w.party(t, party.KYCApproved), savings.ID)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := w.open(t, w.party(t, party.KYCApproved), savings.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.change(t, closed.ID, deposits.Closed); err != nil {
		t.Fatal(err)
	}
	expectProhibited(t, []prohibited{
		{"delete", `DELETE FROM deposits.accounts WHERE id = '` + open.ID.String() + `'`, "never deleted"},
		{"change the party", `UPDATE deposits.accounts SET party_id = gen_random_uuid() WHERE id = '` + open.ID.String() + `'`, "only the status"},
		{"change the product version", `UPDATE deposits.accounts SET product_version_id = gen_random_uuid() WHERE id = '` + open.ID.String() + `'`, "only the status"},
		{"change the ledger account", `UPDATE deposits.accounts SET ledger_account_id = gen_random_uuid() WHERE id = '` + open.ID.String() + `'`, "only the status"},
		{"change the currency", `UPDATE deposits.accounts SET currency = 'USD' WHERE id = '` + open.ID.String() + `'`, "only the status"},
		{"change the opening date", `UPDATE deposits.accounts SET opened_on = '2020-01-01' WHERE id = '` + open.ID.String() + `'`, "only the status"},
		{"truncate the accounts", `TRUNCATE deposits.accounts CASCADE`, "never updated, deleted or truncated"},
		{"reopen", `UPDATE deposits.accounts SET status = 'active' WHERE id = '` + closed.ID.String() + `'`, "is closed"},
		{"erase the history", `DELETE FROM deposits.status_changes WHERE account_id = '` + closed.ID.String() + `'`, "never updated, deleted or truncated"},
		{"rewrite the history", `UPDATE deposits.status_changes SET actor = 'other' WHERE account_id = '` + closed.ID.String() + `'`, "never updated, deleted or truncated"},
	})
}

// prohibited is a statement the database must reject, even from the tables' owner, with its message.
type prohibited struct{ name, sql, message string }

// expectProhibited runs each statement as the tables' owner, under a savepoint, and checks that it fails with
// prohibited_sql_statement_attempted and its message.
func expectProhibited(t *testing.T, cases []prohibited) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var err error
			pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
				sp, beginErr := tx.Begin(t.Context())
				if beginErr != nil {
					return beginErr
				}
				_, err = sp.Exec(t.Context(), c.sql)
				return sp.Rollback(t.Context())
			})
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "2F003" || !strings.Contains(pgErr.Message, c.message) {
				t.Fatalf("got %v, want prohibited_sql_statement_attempted: %s", err, c.message)
			}
		})
	}
}

func count(t *testing.T, query string, arg any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(t.Context(), query, arg).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
