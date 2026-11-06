//go:build integration

package jobs_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/jobs"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/postgres/pgtest"
	"github.com/DrewGGM/aluna/internal/products"
)

// savingsAccount opens an active savings account, for a party with KYC approved, of an entity whose business
// date is 2026-03-02.
func savingsAccount(t *testing.T, deps jobs.Deps) deposits.Account {
	t.Helper()
	entity := ids.New()
	pgtest.AsOwner(t, testPool, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO calendar.legal_entities (id, name, business_date, functional_currency)
			VALUES ($1, 'Entity', '2026-03-02', 'COP')`, entity)
		return err
	})
	cop, err := money.CurrencyOf("COP")
	if err != nil {
		t.Fatal(err)
	}
	var a deposits.Account
	inTx(t, func(tx postgres.Tx) error {
		ctx := t.Context()
		for code, kind := range map[string]ledger.Kind{"A.CASH.VAULT": ledger.KindAsset, "E.INTEREST.SAVINGS": ledger.KindExpense} {
			if _, err := deps.Ledger.CreateAccount(ctx, tx, ledger.CreateAccountInput{
				LegalEntityID: entity, Code: code, Kind: kind, Currency: cop, Mode: ledger.BalanceModeSync,
			}); err != nil {
				return err
			}
		}
		customer, err := approvedParty(t, tx, deps, entity)
		if err != nil {
			return err
		}
		product, err := publishedSavings(t, tx, deps, entity)
		if err != nil {
			return err
		}
		a, err = deps.Deposits.Open(ctx, tx, deposits.OpenInput{LegalEntityID: entity, PartyID: customer, ProductID: product})
		return err
	})
	return a
}

// approvedParty registers a party of the entity with KYC approved.
func approvedParty(t *testing.T, tx postgres.Tx, deps jobs.Deps, entity ids.ID) (ids.ID, error) {
	t.Helper()
	parties := party.New(deps.Outbox, screenedClear{})
	p, err := parties.Register(t.Context(), tx, party.RegisterInput{
		LegalEntityID: entity, Kind: party.KindNatural, ExternalRef: "onboarding-" + ids.New().String(), RiskLevel: party.RiskLow,
	})
	if err != nil {
		return ids.ID{}, err
	}
	_, err = parties.ChangeKYC(t.Context(), tx, entity, p.ID, 1, party.KYCApproved)
	return p.ID, err
}

// publishedSavings publishes a savings product of the entity, with the definition of products' test fixture.
func publishedSavings(t *testing.T, tx postgres.Tx, deps jobs.Deps, entity ids.ID) (ids.ID, error) {
	t.Helper()
	definition, err := os.ReadFile(filepath.Join("..", "products", "internal", "domain", "testdata", "valid-savings.json"))
	if err != nil {
		return ids.ID{}, err
	}
	ctx, catalog := t.Context(), products.New(deps.Outbox)
	product, err := catalog.CreateProduct(ctx, tx, entity, "p"+strings.ReplaceAll(ids.New().String(), "-", ""), "Product")
	if err != nil {
		return ids.ID{}, err
	}
	version, err := catalog.CreateVersion(ctx, tx, entity, product.ID, "author", definition)
	if err != nil {
		return ids.ID{}, err
	}
	if _, err := catalog.RequestPublication(ctx, tx, entity, version.ID, "author"); err != nil {
		return ids.ID{}, err
	}
	_, err = catalog.Publish(ctx, tx, entity, version.ID)
	return product.ID, err
}

// TestACashDepositReachesThePartysTotal (FR-43): the worker publishes the transaction of a deposit at the teller
// to the consumer it subscribed, which finds the party in deposits and adds to its month's total.
func TestACashDepositReachesThePartysTotal(t *testing.T) {
	deps := newDeps(t)
	a := savingsAccount(t, deps)
	inTx(t, func(tx postgres.Tx) error {
		_, err := deps.Deposits.Deposit(t.Context(), tx, deposits.CashInput{
			LegalEntityID: a.LegalEntityID, AccountID: a.ID, Amount: money.New(100_00, a.Currency), ExternalRef: ids.New().String(),
			Actor: "teller-a", Channel: compliance.ChannelCash,
		})
		return err
	})

	finished, timeout := startWorker(t, deps), time.After(10*time.Second)
	for {
		var units int64
		err := testPool.QueryRow(t.Context(), `SELECT total_units FROM compliance.cash_totals
			WHERE legal_entity_id = $1 AND party_id = $2 AND month = '2026-03-01' AND direction = 'deposit' AND currency = 'COP'`,
			a.LegalEntityID, a.PartyID).Scan(&units)
		if err == nil {
			if units != 100_00 {
				t.Fatalf("total = %d, want 100_00", units)
			}
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case ev := <-finished:
			if ev.Kind == river.EventKindJobFailed {
				t.Fatalf("job %s failed: %v", ev.Job.Kind, ev.Job.Errors)
			}
		case <-timeout:
			t.Fatal("no total within 10 s")
		}
	}
}
