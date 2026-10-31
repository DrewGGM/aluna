//go:build integration

package ledger_test

import (
	"errors"
	"flag"
	"os"
	"slices"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// TestLedgerProperties (FF-1): random sequences of accounts, postings, reversals (also of reversals and
// of captures), holds, partial captures, releases (also on an already finished hold) and aggregation
// against PostgreSQL. The model tracks each account's signed balance and the pending amount it expects,
// so each funds rejection must be the one it predicts and each amount the one requested. After each step:
// Σ entries = 0, each account has the model's balance and entries, a sync account's pending amount is
// that of its pending holds, no available balance ends below the limit and no hold changes status except
// through the operation that finished it.
func TestLedgerProperties(t *testing.T) {
	enforced := defaultChecks(t, "200")
	ran := &coverage{}
	rapid.Check(t, func(rt *rapid.T) {
		m := &ledgerModel{t: t, cop: cop(t), ran: ran, posted: map[ids.ID]int64{}, reserved: map[ids.ID]int64{}}
		rt.Repeat(map[string]func(*rapid.T){
			"":          m.check,
			"account":   m.accountStep,
			"post":      m.postStep,
			"reverse":   m.reverseStep,
			"hold":      m.holdStep,
			"capture":   m.captureStep,
			"release":   m.releaseStep,
			"aggregate": m.aggregateStep,
		})
	})
	t.Logf("succeeded %+v", *ran)
	if enforced && !testing.Short() && slices.Contains([]int{ran.posts, ran.reversals, ran.holds, ran.captures, ran.releases, ran.refusals, ran.reterminated}, 0) {
		t.Errorf("some operation never succeeded in %+v: the test does not exercise it", *ran)
	}
}

// coverage counts what actually happened across all sequences: an operation that never succeeds (because
// the ledger always rejects it, for example) would leave half the state machine untested.
type coverage struct {
	posts, reversals, holds, captures, releases, refusals int
	reterminated                                          int // attempts to finish an already finished hold again
}

// defaultChecks runs n sequences unless -rapid.checks or RAPID_CHECKS say how many (CI runs the test
// without the flag) and reports whether n was kept.
func defaultChecks(t *testing.T, n string) bool {
	t.Helper()
	f := flag.Lookup("rapid.checks")
	if f == nil {
		t.Fatal("rapid no longer exposes -rapid.checks")
	}
	explicit := false
	flag.Visit(func(set *flag.Flag) { explicit = explicit || set.Name == f.Name })
	if _, env := os.LookupEnv("RAPID_CHECKS"); explicit || env {
		return false
	}
	previous := f.Value.String()
	if err := f.Value.Set(n); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Value.Set(previous); err != nil {
			t.Error(err)
		}
	})
	return true
}

// ledgerModel is what the sequence knows should be in the database. Holds and reversible transactions
// are kept in creation order: rapid needs to choose among them always in the same order to reproduce a
// failure.
type ledgerModel struct {
	t          *testing.T
	cop        money.Currency
	ran        *coverage
	accounts   []ledger.Account
	posted     map[ids.ID]int64 // expected signed balance
	reserved   map[ids.ID]int64 // expected pending amount on the side through which the account loses balance
	reversible []posting        // posted transactions not yet reversed
	holds      []modelHold
}

// posting is a posted transaction with its signed movements.
type posting struct {
	id   ids.ID
	legs []leg
}

type leg struct {
	account ledger.Account
	units   int64 // debit +, credit −
}

type modelHold struct {
	id              ids.ID
	origin, counter ledger.Account
	amount          int64
	status          ledger.HoldStatus
}

func debitNormal(k ledger.Kind) bool { return ledger.NaturalBalance(k, 1) > 0 }

// room is what the account can lose before reaching its limit.
func (m *ledgerModel) room(a ledger.Account) int64 {
	return ledger.NaturalBalance(a.Kind, m.posted[a.ID]) - m.reserved[a.ID] - a.LowerLimit
}

// refuses reports whether the ledger must reject the movements for funds: a sync account that loses
// natural balance would end below its limit. freed is the pending amount of account freedFrom released in
// the same write (that of the hold being captured).
func (m *ledgerModel) refuses(legs []leg, freedFrom ids.ID, freed int64) bool {
	for _, l := range legs {
		change := ledger.NaturalBalance(l.account.Kind, l.units)
		if l.account.Mode != ledger.BalanceModeSync || change >= 0 {
			continue
		}
		room := m.room(l.account)
		if l.account.ID == freedFrom {
			room += freed
		}
		if room+change < 0 {
			return true
		}
	}
	return false
}

// settled requires err to be the one the model predicts: ErrInsufficientFunds if refused, none
// otherwise. It reports whether the operation succeeded.
func (m *ledgerModel) settled(rt *rapid.T, op string, err error, refused bool) bool {
	if refused {
		if !errors.Is(err, ledger.ErrInsufficientFunds) {
			rt.Fatalf("%s: want ErrInsufficientFunds, got %v", op, err)
		}
		m.ran.refusals++
		return false
	}
	if err != nil {
		rt.Fatalf("%s: unexpected error (the model predicts no rejection): %v", op, err)
	}
	return true
}

func (m *ledgerModel) apply(legs []leg) {
	for _, l := range legs {
		m.posted[l.account.ID] += l.units
	}
}

func (m *ledgerModel) accountStep(rt *rapid.T) {
	in := ledger.CreateAccountInput{
		LegalEntityID: entity, Code: "prop-" + ids.New().String(), Currency: m.cop,
		Kind: rapid.SampledFrom([]ledger.Kind{ledger.KindAsset, ledger.KindLiability, ledger.KindIncome}).Draw(rt, "kind"),
		Mode: rapid.SampledFrom([]ledger.BalanceMode{ledger.BalanceModeSync, ledger.BalanceModeAsync}).Draw(rt, "mode"),
	}
	if in.Mode == ledger.BalanceModeSync {
		in.LowerLimit = rapid.SampledFrom([]int64{0, -200}).Draw(rt, "limit")
	}
	a, err := tryCreateAccount(m.t, in)
	if err != nil {
		rt.Fatal(err)
	}
	m.accounts = append(m.accounts, a)
}

// pair picks two different accounts; the first receives the debit.
func (m *ledgerModel) pair(rt *rapid.T) (debit, credit ledger.Account) {
	if len(m.accounts) < 2 {
		rt.Skip("not enough accounts")
	}
	i := rapid.IntRange(0, len(m.accounts)-1).Draw(rt, "debit")
	j := rapid.IntRange(0, len(m.accounts)-2).Draw(rt, "credit")
	if j >= i {
		j++
	}
	return m.accounts[i], m.accounts[j]
}

// units picks an amount; sometimes the one that leaves the sync account losing balance exactly at its
// limit, or one more, to test the edge from both sides.
func (m *ledgerModel) units(rt *rapid.T, losing ledger.Account) int64 {
	if room := m.room(losing); losing.Mode == ledger.BalanceModeSync && room > 0 && rapid.Bool().Draw(rt, "at limit") {
		return room + rapid.Int64Range(0, 1).Draw(rt, "past")
	}
	return rapid.Int64Range(1, 300).Draw(rt, "amount")
}

func (m *ledgerModel) postStep(rt *rapid.T) {
	debit, credit := m.pair(rt)
	losing := credit
	if !debitNormal(debit.Kind) { // the debit lowers its balance
		losing = debit
	}
	units := m.units(rt, losing)
	legs := []leg{{debit, units}, {credit, -units}}
	refused := m.refuses(legs, ids.ID{}, 0)
	posted, err := inTx(m.t, func(tx postgres.Tx) (ledger.Transaction, error) {
		return move(m.t.Context(), tx, m.cop, debit.ID, credit.ID, units, ids.New().String())
	})
	if m.settled(rt, "post", err, refused) {
		m.apply(legs)
		m.reversible = append(m.reversible, posting{posted.ID, legs})
		m.ran.posts++
	}
}

// reverseStep reverses a posted transaction (a posting, a capture or another reversal): its movements
// with the opposite sign.
func (m *ledgerModel) reverseStep(rt *rapid.T) {
	if len(m.reversible) == 0 {
		rt.Skip("nothing to reverse")
	}
	i := rapid.IntRange(0, len(m.reversible)-1).Draw(rt, "transaction")
	original := m.reversible[i]
	legs := make([]leg, len(original.legs))
	for j, l := range original.legs {
		legs[j] = leg{l.account, -l.units}
	}
	refused := m.refuses(legs, ids.ID{}, 0)
	reversal, err := reverse(m.t, original.id)
	if m.settled(rt, "reversal", err, refused) {
		m.apply(legs)
		m.reversible = append(slices.Delete(m.reversible, i, i+1), posting{reversal.ID, legs})
		m.ran.reversals++
	}
}

func (m *ledgerModel) holdStep(rt *rapid.T) {
	origin, counter := m.pair(rt)
	if origin.Mode != ledger.BalanceModeSync {
		rt.Skip("a hold's source is sync")
	}
	units := m.units(rt, origin)
	outgoing := leg{origin, ledger.NaturalBalance(origin.Kind, -units)} // holding is as if it left now
	refused := m.refuses([]leg{outgoing}, ids.ID{}, 0)
	hold, err := placeHold(m.t, origin.ID, counter.ID, units, time.Hour)
	if m.settled(rt, "hold", err, refused) {
		m.reserved[origin.ID] += units
		m.holds = append(m.holds, modelHold{id: hold.ID(), origin: origin, counter: counter, amount: units, status: ledger.HoldPending})
		m.ran.holds++
	}
}

// someHold picks a hold of the sequence: almost always a pending one and sometimes any, including an
// already finished one.
func (m *ledgerModel) someHold(rt *rapid.T) *modelHold {
	var pending []int
	for i, h := range m.holds {
		if h.status == ledger.HoldPending {
			pending = append(pending, i)
		}
	}
	if len(pending) > 0 && rapid.IntRange(0, 3).Draw(rt, "pending") > 0 {
		return &m.holds[rapid.SampledFrom(pending).Draw(rt, "hold")]
	}
	if len(m.holds) == 0 {
		rt.Skip("no holds")
	}
	return &m.holds[rapid.IntRange(0, len(m.holds)-1).Draw(rt, "hold")]
}

// endedAlready requires that finishing an already finished hold again be ErrHoldNotPending and reports
// whether it was finished.
func (m *ledgerModel) endedAlready(rt *rapid.T, h *modelHold, err error) bool {
	if h.status == ledger.HoldPending {
		return false
	}
	if !errors.Is(err, ledger.ErrHoldNotPending) {
		rt.Fatalf("finishing a %s hold again = %v, want ErrHoldNotPending", h.status, err)
	}
	m.ran.reterminated++
	return true
}

// captureStep posts part of a hold: what is captured leaves the source toward the counterpart and the
// whole pending amount is released.
func (m *ledgerModel) captureStep(rt *rapid.T) {
	h := m.someHold(rt)
	units := rapid.Int64Range(1, h.amount).Draw(rt, "captured")
	out := ledger.NaturalBalance(h.origin.Kind, -units)
	legs := []leg{{h.origin, out}, {h.counter, -out}}
	refused := m.refuses(legs, h.origin.ID, h.amount)
	got, err := captureHold(m.t, h.id, units)
	if m.endedAlready(rt, h, err) || !m.settled(rt, "capture", err, refused) {
		return
	}
	m.apply(legs)
	m.reserved[h.origin.ID] -= h.amount
	h.status = ledger.HoldCaptured
	m.reversible = append(m.reversible, posting{got.tx.ID, legs})
	m.ran.captures++
}

func (m *ledgerModel) releaseStep(rt *rapid.T) {
	h := m.someHold(rt)
	_, err := releaseHold(m.t, h.id)
	if m.endedAlready(rt, h, err) || !m.settled(rt, "release", err, false) {
		return
	}
	m.reserved[h.origin.ID] -= h.amount
	h.status = ledger.HoldReleased
	m.ran.releases++
}

// aggregateStep runs the aggregator until the sequence's async accounts have the model's balance. It does
// not use catchUp: that one waits for every async account in the database.
func (m *ledgerModel) aggregateStep(rt *rapid.T) {
	if !slices.ContainsFunc(m.accounts, func(a ledger.Account) bool { return a.Mode == ledger.BalanceModeAsync }) {
		rt.Skip("no async accounts")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := svc().AggregateAsyncBalances(m.t.Context(), postgres.NewUnitOfWork(testPool), 100); err != nil {
			rt.Fatal(err)
		}
		lagging := 0
		for _, s := range m.states(rt) {
			if s.account.Mode == ledger.BalanceModeAsync && s.posted != m.posted[s.account.ID] {
				lagging++
			}
		}
		if lagging == 0 {
			return
		}
		if time.Now().After(deadline) {
			rt.Fatalf("after 10 s, %d async accounts still lack the model's balance", lagging)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// accountState is an account's materialized balance together with what its entries and holds say.
type accountState struct {
	account                               ledger.Account
	posted, pendingDebits, pendingCredits int64
	entries, held                         int64 // Σ entries and Σ pending holds
}

func (m *ledgerModel) states(rt *rapid.T) []accountState {
	byID := make(map[ids.ID]ledger.Account, len(m.accounts))
	accountIDs := make([]ids.ID, len(m.accounts))
	for i, a := range m.accounts {
		byID[a.ID], accountIDs[i] = a, a.ID
	}
	rows, err := testPool.Query(m.t.Context(), `
		SELECT b.account_id, b.posted, b.pending_debits, b.pending_credits,
		       coalesce((SELECT sum(e.amount) FROM ledger.entries e WHERE e.account_id = b.account_id), 0)::bigint,
		       coalesce((SELECT sum(h.amount) FROM ledger.holds h WHERE h.account_id = b.account_id AND h.status = 'pending'), 0)::bigint
		FROM ledger.balances b WHERE b.account_id = ANY($1)`, accountIDs)
	if err != nil {
		rt.Fatal(err)
	}
	defer rows.Close()
	var states []accountState
	for rows.Next() {
		var id ids.ID
		var s accountState
		if err := rows.Scan(&id, &s.posted, &s.pendingDebits, &s.pendingCredits, &s.entries, &s.held); err != nil {
			rt.Fatal(err)
		}
		s.account = byID[id]
		states = append(states, s)
	}
	if err := rows.Err(); err != nil {
		rt.Fatal(err)
	}
	if len(states) != len(m.accounts) {
		rt.Fatalf("read %d balances for %d accounts", len(states), len(m.accounts))
	}
	return states
}

// check holds the invariants that apply after each step. Every account is in COP: Σ entries per currency
// is a single sum.
func (m *ledgerModel) check(rt *rapid.T) {
	if len(m.accounts) == 0 {
		return
	}
	var total int64
	for _, s := range m.states(rt) {
		total += s.entries
		if want := m.posted[s.account.ID]; s.entries != want {
			rt.Fatalf("account %s: Σ entries %d, the model expects %d", s.account.Code, s.entries, want)
		}
		if s.account.Mode == ledger.BalanceModeSync {
			m.checkSync(rt, s)
		}
	}
	if total != 0 { // postings only go between the sequence's accounts
		rt.Fatalf("Σ entries = %d, want 0", total)
	}
	m.checkHolds(rt)
}

// checkSync: a sync account's balance is the model's, its pending amount on the side through which it
// loses balance is the model's and the sum of its pending holds (and the other side is zero), and its
// available balance does not drop below the limit.
func (m *ledgerModel) checkSync(rt *rapid.T, s accountState) {
	outgoing, incoming := s.pendingDebits, s.pendingCredits
	if debitNormal(s.account.Kind) { // loses balance through the credit side
		outgoing, incoming = s.pendingCredits, s.pendingDebits
	}
	available := ledger.NaturalBalance(s.account.Kind, s.posted) - outgoing
	reserved := m.reserved[s.account.ID]
	if s.posted != s.entries || outgoing != reserved || s.held != reserved || incoming != 0 || available < s.account.LowerLimit {
		rt.Fatalf("account %s (%s, limit %d): posted %d, Σ entries %d, pending %d/%d, Σ holds %d, model %d, available %d",
			s.account.Code, s.account.Kind, s.account.LowerLimit, s.posted, s.entries, outgoing, incoming, s.held, reserved, available)
	}
}

// checkHolds: each hold is in the status the last successful operation gave it; a rejection does not
// change it and a finished one does not change again.
func (m *ledgerModel) checkHolds(rt *rapid.T) {
	if len(m.holds) == 0 {
		return
	}
	holdIDs := make([]ids.ID, len(m.holds))
	for i, h := range m.holds {
		holdIDs[i] = h.id
	}
	rows, err := testPool.Query(m.t.Context(), `SELECT id, status FROM ledger.holds WHERE id = ANY($1)`, holdIDs)
	if err != nil {
		rt.Fatal(err)
	}
	defer rows.Close()
	got := make(map[ids.ID]ledger.HoldStatus, len(m.holds))
	for rows.Next() {
		var id ids.ID
		var status ledger.HoldStatus
		if err := rows.Scan(&id, &status); err != nil {
			rt.Fatal(err)
		}
		got[id] = status
	}
	if err := rows.Err(); err != nil {
		rt.Fatal(err)
	}
	for _, h := range m.holds {
		if got[h.id] != h.status {
			rt.Fatalf("hold %s = %q, want %s", h.id, got[h.id], h.status)
		}
	}
}
