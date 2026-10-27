//go:build integration

package audit_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/audit/internal/adapters/anchorfs"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
)

// sealer es un servicio de auditoría que sella con el ancla en un directorio temporal.
func sealer(t *testing.T) *audit.Service {
	t.Helper()
	return audit.NewSealing(dirAnchor(t))
}

func dirAnchor(t *testing.T) *anchorfs.Dir {
	t.Helper()
	anchor, err := anchorfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return anchor
}

func seal(t *testing.T, s *audit.Service, maxRecords int) (int64, error) {
	t.Helper()
	return s.Seal(t.Context(), postgres.NewUnitOfWork(testPool), maxRecords)
}

// sealAll sella todo lo que hay sin bloque, en bloques de a lo más maxRecords, y devuelve los bloques.
func sealAll(t *testing.T, s *audit.Service, maxRecords int) []int64 {
	t.Helper()
	var blocks []int64
	for {
		id, err := seal(t, s, maxRecords)
		if err != nil {
			t.Fatal(err)
		}
		if id == 0 {
			return blocks
		}
		blocks = append(blocks, id)
	}
}

// sealedRecords guarda n registros y los sella con s; devuelve sus correlaciones y los bloques que selló.
// La base es de todo el paquete: los bloques pueden llevar también registros de otras pruebas, y cada
// prueba verifica solo los suyos, anclados en su directorio. Una prueba que altera un bloque no estorba a
// las siguientes: Seal encadena desde el hash guardado en la base.
func sealedRecords(t *testing.T, s *audit.Service, n int) ([]string, []int64) {
	t.Helper()
	correlations := make([]string, n)
	for i := range correlations {
		r := record()
		if err := save(t, r); err != nil {
			t.Fatal(err)
		}
		correlations[i] = r.CorrelationID
	}
	blocks := sealAll(t, s, 2)
	if len(blocks) == 0 {
		t.Fatal("no se selló ningún bloque")
	}
	return correlations, blocks
}

func blockOf(t *testing.T, correlation string) int64 {
	t.Helper()
	var block *int64
	if err := testPool.QueryRow(t.Context(), `SELECT block_id FROM audit.records WHERE correlation_id = $1`, correlation).Scan(&block); err != nil {
		t.Fatal(err)
	}
	if block == nil {
		t.Fatalf("el registro %s no quedó sellado", correlation)
	}
	return *block
}

// tamper corre stmts como dueño con los triggers de las tablas de auditoría apagados: lo que haría quien
// administra la base para alterar la evidencia.
func tamper(t *testing.T, stmts ...string) {
	t.Helper()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		all := append([]string{"ALTER TABLE audit.records DISABLE TRIGGER USER", "ALTER TABLE audit.blocks DISABLE TRIGGER USER"}, stmts...)
		all = append(all, "ALTER TABLE audit.records ENABLE TRIGGER USER", "ALTER TABLE audit.blocks ENABLE TRIGGER USER")
		for _, stmt := range all {
			if _, err := tx.Exec(t.Context(), stmt); err != nil {
				return err
			}
		}
		return nil
	})
}

func expectBrokenAt(t *testing.T, s *audit.Service, from, to, block int64) {
	t.Helper()
	err := s.Verify(t.Context(), testPool, from, to)
	var broken audit.ErrChainBroken
	if !errors.As(err, &broken) || broken.BlockID != block {
		t.Fatalf("Verify(%d, %d) = %v, se esperaba la cadena rota en el bloque %d", from, to, err, block)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestSealedBlocksVerify (ADR-0012): sellar encadena los registros en bloques de a lo más maxRecords, cada
// registro queda en un bloque y la cadena verifica contra sus anclas.
func TestSealedBlocksVerify(t *testing.T) {
	s := sealer(t)
	correlations, blocks := sealedRecords(t, s, 3)

	for _, c := range correlations {
		blockOf(t, c)
	}
	if len(blocks) < 2 {
		t.Fatalf("3 registros en bloques de 2 dieron %d bloques", len(blocks))
	}
	if err := s.Verify(t.Context(), testPool, blocks[0], blocks[len(blocks)-1]); err != nil {
		t.Fatalf("Verify de una cadena sin tocar = %v", err)
	}
	if id, err := seal(t, s, 2); err != nil || id != 0 {
		t.Fatalf("sellar sin registros pendientes = %d, %v; se esperaba 0", id, err)
	}
}

// TestTamperingBreaksTheChain (AB-7): alterar o borrar un registro sellado, aun como dueño y sin triggers,
// o cambiar el hash de su bloque o del anterior (que no está en el rango verificado), hace fallar Verify
// en ese bloque.
func TestTamperingBreaksTheChain(t *testing.T) {
	cases := map[string]func(t *testing.T, correlation string, block int64){
		"registro alterado": func(t *testing.T, c string, _ int64) {
			tamper(t, `UPDATE audit.records SET result = 'ok-falso' WHERE correlation_id = '`+c+`'`)
		},
		"registro borrado": func(t *testing.T, c string, _ int64) {
			tamper(t, `DELETE FROM audit.records WHERE correlation_id = '`+c+`'`)
		},
		"bloque con otro hash": func(t *testing.T, _ string, b int64) {
			tamper(t, `UPDATE audit.blocks SET hash = sha256(hash) WHERE id = `+itoa(b))
		},
		"bloque anterior con otro hash": func(t *testing.T, _ string, b int64) {
			tamper(t, `UPDATE audit.blocks SET hash = sha256(hash) WHERE id = (SELECT max(id) FROM audit.blocks WHERE id < `+itoa(b)+`)`)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := sealer(t)
			sealedRecords(t, s, 1) // un bloque anterior al del registro de la prueba
			correlations, _ := sealedRecords(t, s, 1)
			block := blockOf(t, correlations[0])
			if err := s.Verify(t.Context(), testPool, block, block); err != nil {
				t.Fatalf("antes de alterar: %v", err)
			}

			change(t, correlations[0], block)

			expectBrokenAt(t, s, block, block, block)
		})
	}
}

// TestTruncatingAndResealingIsDetected: quien borra el último bloque, cambia sus registros y los deja
// sellar de nuevo no engaña a Verify: el bloque borrado tiene ancla y ya no está en la base.
func TestTruncatingAndResealingIsDetected(t *testing.T) {
	s := sealer(t)
	correlations, _ := sealedRecords(t, s, 1)
	block := blockOf(t, correlations[0])

	tamper(t,
		`UPDATE audit.records SET block_id = NULL, result = 'ok-falso' WHERE block_id = `+itoa(block),
		`DELETE FROM audit.blocks WHERE id = `+itoa(block))
	resealed := sealAll(t, s, 100)

	expectBrokenAt(t, s, block, resealed[len(resealed)-1], block)
}

// TestABlockWithoutItsAnchorBreaksTheChain: verificar contra un ancla que no tiene el bloque, o que tiene
// otro hash, rompe la cadena en ese bloque.
func TestABlockWithoutItsAnchorBreaksTheChain(t *testing.T) {
	correlations, _ := sealedRecords(t, sealer(t), 1)
	block := blockOf(t, correlations[0])
	wrong := dirAnchor(t)
	if err := wrong.Put(t.Context(), block, [32]byte{1}); err != nil {
		t.Fatal(err)
	}

	expectBrokenAt(t, audit.NewSealing(dirAnchor(t)), block, block, block)
	expectBrokenAt(t, audit.NewSealing(wrong), block, block, block)
}

// TestAnEmptyRangeIsNotIntact: un rango sin bloques ni anclas no se da por verificado.
func TestAnEmptyRangeIsNotIntact(t *testing.T) {
	far := lastBlock(t) + 1000

	if err := sealer(t).Verify(t.Context(), testPool, far, far+10); !errors.Is(err, audit.ErrNoBlocks) {
		t.Fatalf("Verify de un rango vacío = %v, se esperaba ErrNoBlocks", err)
	}
}

// flakyAnchor es un ancla cuyos primeros failures Put de bloques posteriores a after fallan, como un
// almacenamiento caído un rato.
type flakyAnchor struct {
	*anchorfs.Dir
	after    int64
	failures int
}

var errAnchorDown = errors.New("el ancla no responde")

func (a *flakyAnchor) Put(ctx context.Context, id int64, hash [32]byte) error {
	if id > a.after && a.failures > 0 {
		a.failures--
		return errAnchorDown
	}
	return a.Dir.Put(ctx, id, hash)
}

// TestAFailedAnchorIsRetried: si anclar falla después de confirmar el bloque, el bloque queda sellado sin
// ancla; mientras el ancla siga caída no se sella nada encima, y cuando vuelve se completa y la cadena
// verifica entera.
func TestAFailedAnchorIsRetried(t *testing.T) {
	sealAll(t, sealer(t), 100) // lo pendiente de otras pruebas no es parte de esta
	s := audit.NewSealing(&flakyAnchor{Dir: dirAnchor(t), after: lastBlock(t), failures: 2})
	first, second := record(), record()
	if err := save(t, first); err != nil {
		t.Fatal(err)
	}

	if _, err := seal(t, s, 100); !errors.Is(err, errAnchorDown) {
		t.Fatalf("primer sellado = %v, se esperaba la falla del ancla", err)
	}
	block := blockOf(t, first.CorrelationID) // confirmado, sin ancla
	if err := save(t, second); err != nil {
		t.Fatal(err)
	}
	if _, err := seal(t, s, 100); !errors.Is(err, errAnchorDown) {
		t.Fatalf("sellado con el ancla aún caída = %v, se esperaba la falla", err)
	}
	if next := lastBlock(t); next != block {
		t.Fatalf("se selló el bloque %d encima del %d, que no tiene ancla", next, block)
	}
	if _, err := seal(t, s, 100); err != nil {
		t.Fatal(err)
	}

	if err := s.Verify(t.Context(), testPool, block, blockOf(t, second.CorrelationID)); err != nil {
		t.Fatalf("Verify tras volver el ancla = %v", err)
	}
}

// TestTheFirstBrokenBlockIsReported: con un bloque alterado y otro borrado después, Verify nombra el
// primero.
func TestTheFirstBrokenBlockIsReported(t *testing.T) {
	s := sealer(t)
	altered, _ := sealedRecords(t, s, 1)
	deleted, _ := sealedRecords(t, s, 1)
	first, second := blockOf(t, altered[0]), blockOf(t, deleted[0])
	tamper(t,
		`UPDATE audit.records SET result = 'ok-falso' WHERE correlation_id = '`+altered[0]+`'`,
		`UPDATE audit.records SET block_id = NULL WHERE block_id = `+itoa(second),
		`DELETE FROM audit.blocks WHERE id = `+itoa(second))

	expectBrokenAt(t, s, first, second, first)
}

// TestTheAppRoleCannotRewriteTheChain: aluna_app solo puede poner block_id, y solo con un bloque creado en
// su misma transacción; un registro sellado no cambia de bloque, ni siquiera como dueño; dos bloques no
// pueden colgar del mismo anterior.
func TestTheAppRoleCannotRewriteTheChain(t *testing.T) {
	correlations, _ := sealedRecords(t, sealer(t), 1)
	c, block := correlations[0], blockOf(t, correlations[0])
	pending := record()
	if err := save(t, pending); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		stmt, state string
		owner       bool
	}{
		"cambiar el resultado":            {`UPDATE audit.records SET result = 'otro' WHERE correlation_id = '` + c + `'`, "42501", false},
		"quitarle el bloque":              {`UPDATE audit.records SET block_id = NULL WHERE correlation_id = '` + c + `'`, "2F003", false},
		"meter un pendiente en uno viejo": {`UPDATE audit.records SET block_id = ` + itoa(block) + ` WHERE correlation_id = '` + pending.CorrelationID + `'`, "2F003", false},
		"reescribir el bloque como dueño": {`UPDATE audit.records SET block_id = block_id WHERE correlation_id = '` + c + `'`, "2F003", true},
		"bifurcar la cadena":              {`INSERT INTO audit.blocks (prev_hash, hash, records) SELECT prev_hash, hash, 1 FROM audit.blocks WHERE id = ` + itoa(block), "23505", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var state string
			if tc.owner {
				state = ownerSQLState(t, tc.stmt)
			} else {
				_, err := testPool.Exec(t.Context(), tc.stmt)
				state = sqlState(err)
			}
			if state != tc.state {
				t.Fatalf("%s = SQLSTATE %q, se esperaba %q", tc.stmt, state, tc.state)
			}
		})
	}
}

// TestConcurrentSealersDoNotForkTheChain: varios selladores a la vez dejan una sola cadena: cada bloque
// empieza en el hash del anterior (el que pierde la carrera falla por la unicidad de prev_hash).
func TestConcurrentSealersDoNotForkTheChain(t *testing.T) {
	s := sealer(t)
	for range 20 {
		if err := save(t, record()); err != nil {
			t.Fatal(err)
		}
	}
	first := lastBlock(t) + 1
	var wg sync.WaitGroup
	sealed := make([]int64, 4)
	for i := range sealed {
		wg.Go(func() {
			id, err := seal(t, s, 3)
			if err != nil && sqlState(err) != "23505" { // perder la carrera por prev_hash es lo único esperado
				t.Errorf("sellador %d: %v", i, err)
			}
			sealed[i] = id
		})
	}
	wg.Wait()
	if slices.Max(sealed) == 0 {
		t.Fatal("ningún sellador concurrente selló un bloque")
	}
	sealAll(t, s, 3)

	if err := s.Verify(t.Context(), testPool, first, lastBlock(t)); err != nil {
		t.Fatalf("Verify tras sellar en paralelo = %v", err)
	}
}

func lastBlock(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := testPool.QueryRow(t.Context(), `SELECT coalesce(max(id), 0) FROM audit.blocks`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

var errUndo = errors.New("se revierte a propósito")

// intoFreshBlockAsOwner crea como dueño un bloque en una transacción, trata de pasar a él el registro de la
// correlación con la asignación SQL extraSet, devuelve el SQLSTATE y revierte todo: la cadena no cambia.
func intoFreshBlockAsOwner(t *testing.T, correlation, extraSet string) string {
	t.Helper()
	var state string
	err := pgx.BeginFunc(t.Context(), testPool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), "SET LOCAL ROLE NONE"); err != nil {
			return err
		}
		var block int64
		if err := tx.QueryRow(t.Context(), `INSERT INTO audit.blocks (prev_hash, hash, records)
			VALUES (sha256(convert_to(gen_random_uuid()::text, 'UTF8')), sha256('x'::bytea), 1) RETURNING id`).Scan(&block); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `UPDATE audit.records SET block_id = $1`+extraSet+` WHERE correlation_id = $2`, block, correlation)
		state = sqlState(err)
		return errUndo
	})
	if !errors.Is(err, errUndo) && state == "" {
		t.Fatal(err)
	}
	return state
}

// TestSealingInItsOwnTransactionSetsOnlyBlockID: en la transacción que crea un bloque, un registro
// pendiente puede pasar a él; uno ya sellado no, y nadie, ni el dueño, cambia otro campo al sellar.
func TestSealingInItsOwnTransactionSetsOnlyBlockID(t *testing.T) {
	correlations, _ := sealedRecords(t, sealer(t), 1)
	pending := record()
	if err := save(t, pending); err != nil {
		t.Fatal(err)
	}

	if state := intoFreshBlockAsOwner(t, pending.CorrelationID, ""); state != "" {
		t.Fatalf("sellar un pendiente en un bloque nuevo = SQLSTATE %q, se esperaba que pasara", state)
	}
	if state := intoFreshBlockAsOwner(t, pending.CorrelationID, ", result = 'otro'"); state != "2F003" {
		t.Errorf("sellar y cambiar el resultado = SQLSTATE %q, se esperaba el trigger (2F003)", state)
	}
	if state := intoFreshBlockAsOwner(t, correlations[0], ""); state != "2F003" {
		t.Errorf("pasar un sellado a otro bloque = SQLSTATE %q, se esperaba el trigger (2F003)", state)
	}
}

// TestAWideRangeStopsAtTheLastBlock: verificar "todo" con un to enorme (hasta el máximo de int64) revisa
// hasta el último bloque de la base y termina, sin una búsqueda de ancla por cada id del rango.
func TestAWideRangeStopsAtTheLastBlock(t *testing.T) {
	s := sealer(t)
	correlations, _ := sealedRecords(t, s, 1)
	block := blockOf(t, correlations[0])

	if err := s.Verify(t.Context(), testPool, block, math.MaxInt64); err != nil {
		t.Fatalf("Verify hasta el máximo = %v", err)
	}
}
