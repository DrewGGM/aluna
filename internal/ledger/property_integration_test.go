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

// TestLedgerProperties (FF-1): secuencias al azar de cuentas, postings, reversas (también de reversas y
// de captures), holds, capture parcial, release (también sobre un hold ya terminado) y agregación contra
// PostgreSQL. El modelo lleva por cuenta el saldo con signo y el pendiente que espera, así que cada
// rechazo por fondos tiene que ser el que predice y cada importe el que se pidió. Tras cada paso: Σ
// entries = 0, cada cuenta tiene el saldo y los entries del modelo, el pendiente de una sync es el de
// sus holds pending, ningún disponible queda bajo el límite y ningún hold cambia de estado salvo por la
// operación que lo terminó.
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
	t.Logf("pasaron %+v", *ran)
	if enforced && !testing.Short() && slices.Contains([]int{ran.posts, ran.reversals, ran.holds, ran.captures, ran.releases, ran.refusals, ran.reterminated}, 0) {
		t.Errorf("alguna operación nunca pasó en %+v: la prueba no la ejercita", *ran)
	}
}

// coverage cuenta lo que de verdad pasó en todas las secuencias: una operación que nunca pasa (porque el
// ledger la rechaza siempre, por ejemplo) dejaría la mitad de la máquina de estados sin probar.
type coverage struct {
	posts, reversals, holds, captures, releases, refusals int
	reterminated                                          int // intentos de terminar otra vez un hold terminado
}

// defaultChecks corre n secuencias salvo que -rapid.checks o RAPID_CHECKS digan cuántas (el CI corre la
// prueba sin la flag) y dice si quedó n.
func defaultChecks(t *testing.T, n string) bool {
	t.Helper()
	f := flag.Lookup("rapid.checks")
	if f == nil {
		t.Fatal("rapid ya no expone -rapid.checks")
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

// ledgerModel es lo que la secuencia sabe que debería haber en la base. Los holds y las transacciones
// reversibles van en orden de creación: rapid necesita elegir entre ellos siempre en el mismo orden para
// reproducir un fallo.
type ledgerModel struct {
	t          *testing.T
	cop        money.Currency
	ran        *coverage
	accounts   []ledger.Account
	posted     map[ids.ID]int64 // saldo con signo esperado
	reserved   map[ids.ID]int64 // pendiente esperado del lado por el que la cuenta pierde saldo
	reversible []posting        // transacciones asentadas que aún no se revierten
	holds      []modelHold
}

// posting es una transacción asentada con sus movimientos con signo.
type posting struct {
	id   ids.ID
	legs []leg
}

type leg struct {
	account ledger.Account
	units   int64 // débito +, crédito −
}

type modelHold struct {
	id              ids.ID
	origin, counter ledger.Account
	amount          int64
	status          ledger.HoldStatus
}

func debitNatured(k ledger.Kind) bool { return ledger.NaturalBalance(k, 1) > 0 }

// room es lo que la cuenta puede perder antes de llegar a su límite.
func (m *ledgerModel) room(a ledger.Account) int64 {
	return ledger.NaturalBalance(a.Kind, m.posted[a.ID]) - m.reserved[a.ID] - a.LowerLimit
}

// refuses dice si el ledger tiene que rechazar los movimientos por fondos: una cuenta sync que pierde
// saldo natural quedaría bajo su límite. freed es el pendiente de la cuenta freedFrom que se libera en la
// misma escritura (el del hold que se captura).
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

// settled exige que err sea el que predice el modelo: ErrInsufficientFunds si refused, ninguno si no. Dice
// si la operación pasó.
func (m *ledgerModel) settled(rt *rapid.T, op string, err error, refused bool) bool {
	if refused {
		if !errors.Is(err, ledger.ErrInsufficientFunds) {
			rt.Fatalf("%s: se esperaba ErrInsufficientFunds, fue %v", op, err)
		}
		m.ran.refusals++
		return false
	}
	if err != nil {
		rt.Fatalf("%s: error inesperado (el modelo no predice un rechazo): %v", op, err)
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
		Kind: rapid.SampledFrom([]ledger.Kind{ledger.KindAsset, ledger.KindLiability, ledger.KindIncome}).Draw(rt, "clase"),
		Mode: rapid.SampledFrom([]ledger.BalanceMode{ledger.BalanceModeSync, ledger.BalanceModeAsync}).Draw(rt, "modo"),
	}
	if in.Mode == ledger.BalanceModeSync {
		in.LowerLimit = rapid.SampledFrom([]int64{0, -200}).Draw(rt, "límite")
	}
	a, err := tryCreateAccount(m.t, in)
	if err != nil {
		rt.Fatal(err)
	}
	m.accounts = append(m.accounts, a)
}

// pair elige dos cuentas distintas; la primera recibe el débito.
func (m *ledgerModel) pair(rt *rapid.T) (debit, credit ledger.Account) {
	if len(m.accounts) < 2 {
		rt.Skip("faltan cuentas")
	}
	i := rapid.IntRange(0, len(m.accounts)-1).Draw(rt, "débito")
	j := rapid.IntRange(0, len(m.accounts)-2).Draw(rt, "crédito")
	if j >= i {
		j++
	}
	return m.accounts[i], m.accounts[j]
}

// units elige un importe; a veces el que deja justo en su límite a la cuenta sync que pierde saldo, o uno
// más, para probar el borde por los dos lados.
func (m *ledgerModel) units(rt *rapid.T, losing ledger.Account) int64 {
	if room := m.room(losing); losing.Mode == ledger.BalanceModeSync && room > 0 && rapid.Bool().Draw(rt, "al límite") {
		return room + rapid.Int64Range(0, 1).Draw(rt, "pasado")
	}
	return rapid.Int64Range(1, 300).Draw(rt, "importe")
}

func (m *ledgerModel) postStep(rt *rapid.T) {
	debit, credit := m.pair(rt)
	losing := credit
	if !debitNatured(debit.Kind) { // el débito le baja el saldo
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

// reverseStep revierte una transacción asentada (un posting, un capture u otra reversa): sus movimientos
// con el signo contrario.
func (m *ledgerModel) reverseStep(rt *rapid.T) {
	if len(m.reversible) == 0 {
		rt.Skip("nada que revertir")
	}
	i := rapid.IntRange(0, len(m.reversible)-1).Draw(rt, "transacción")
	original := m.reversible[i]
	legs := make([]leg, len(original.legs))
	for j, l := range original.legs {
		legs[j] = leg{l.account, -l.units}
	}
	refused := m.refuses(legs, ids.ID{}, 0)
	reversal, err := reverse(m.t, original.id)
	if m.settled(rt, "reversa", err, refused) {
		m.apply(legs)
		m.reversible = append(slices.Delete(m.reversible, i, i+1), posting{reversal.ID, legs})
		m.ran.reversals++
	}
}

func (m *ledgerModel) holdStep(rt *rapid.T) {
	origin, counter := m.pair(rt)
	if origin.Mode != ledger.BalanceModeSync {
		rt.Skip("el origen de un hold es sync")
	}
	units := m.units(rt, origin)
	outgoing := leg{origin, ledger.NaturalBalance(origin.Kind, -units)} // retener es como si saliera ya
	refused := m.refuses([]leg{outgoing}, ids.ID{}, 0)
	hold, err := placeHold(m.t, origin.ID, counter.ID, units, time.Hour)
	if m.settled(rt, "hold", err, refused) {
		m.reserved[origin.ID] += units
		m.holds = append(m.holds, modelHold{id: hold.ID(), origin: origin, counter: counter, amount: units, status: ledger.HoldPending})
		m.ran.holds++
	}
}

// someHold elige un hold de la secuencia: casi siempre uno pending y a veces cualquiera, también uno ya
// terminado.
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
		rt.Skip("no hay holds")
	}
	return &m.holds[rapid.IntRange(0, len(m.holds)-1).Draw(rt, "hold")]
}

// endedAlready exige que terminar otra vez un hold terminado sea ErrHoldNotPending y dice si lo estaba.
func (m *ledgerModel) endedAlready(rt *rapid.T, h *modelHold, err error) bool {
	if h.status == ledger.HoldPending {
		return false
	}
	if !errors.Is(err, ledger.ErrHoldNotPending) {
		rt.Fatalf("terminar otra vez un hold %s = %v, se esperaba ErrHoldNotPending", h.status, err)
	}
	m.ran.reterminated++
	return true
}

// captureStep asienta parte de un hold: lo capturado sale del origen hacia la contrapartida y el
// pendiente completo se libera.
func (m *ledgerModel) captureStep(rt *rapid.T) {
	h := m.someHold(rt)
	units := rapid.Int64Range(1, h.amount).Draw(rt, "capturado")
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

// aggregateStep corre el agregador hasta que las cuentas async de la secuencia tienen el saldo del
// modelo. No usa catchUp: ese espera a todas las cuentas async de la base.
func (m *ledgerModel) aggregateStep(rt *rapid.T) {
	if !slices.ContainsFunc(m.accounts, func(a ledger.Account) bool { return a.Mode == ledger.BalanceModeAsync }) {
		rt.Skip("no hay cuentas async")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := svc.AggregateAsyncBalances(m.t.Context(), postgres.NewUnitOfWork(testPool), 100); err != nil {
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
			rt.Fatalf("tras 10 s quedan %d cuentas async sin el saldo del modelo", lagging)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// accountState es el saldo materializado de una cuenta junto a lo que dicen sus entries y sus holds.
type accountState struct {
	account                               ledger.Account
	posted, pendingDebits, pendingCredits int64
	entries, held                         int64 // Σ entries y Σ holds pending
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
		rt.Fatalf("leí %d saldos de %d cuentas", len(states), len(m.accounts))
	}
	return states
}

// check son los invariantes que valen tras cada paso. Todas las cuentas son en COP: Σ entries por moneda
// es una sola suma.
func (m *ledgerModel) check(rt *rapid.T) {
	if len(m.accounts) == 0 {
		return
	}
	var total int64
	for _, s := range m.states(rt) {
		total += s.entries
		if want := m.posted[s.account.ID]; s.entries != want {
			rt.Fatalf("cuenta %s: Σ entries %d, el modelo espera %d", s.account.Code, s.entries, want)
		}
		if s.account.Mode == ledger.BalanceModeSync {
			m.checkSync(rt, s)
		}
	}
	if total != 0 { // solo se asienta entre cuentas de la secuencia
		rt.Fatalf("Σ entries = %d, se esperaba 0", total)
	}
	m.checkHolds(rt)
}

// checkSync: el saldo de una cuenta sync es el del modelo, su pendiente del lado por el que pierde saldo
// es el del modelo y la suma de sus holds pending (y el otro lado está en cero), y su disponible no baja
// del límite.
func (m *ledgerModel) checkSync(rt *rapid.T, s accountState) {
	outgoing, incoming := s.pendingDebits, s.pendingCredits
	if debitNatured(s.account.Kind) { // pierde saldo por el crédito
		outgoing, incoming = s.pendingCredits, s.pendingDebits
	}
	available := ledger.NaturalBalance(s.account.Kind, s.posted) - outgoing
	reserved := m.reserved[s.account.ID]
	if s.posted != s.entries || outgoing != reserved || s.held != reserved || incoming != 0 || available < s.account.LowerLimit {
		rt.Fatalf("cuenta %s (%s, límite %d): posted %d, Σ entries %d, pendiente %d/%d, Σ holds %d, modelo %d, disponible %d",
			s.account.Code, s.account.Kind, s.account.LowerLimit, s.posted, s.entries, outgoing, incoming, s.held, reserved, available)
	}
}

// checkHolds: cada hold está en el estado que le dio la última operación que pasó; un rechazo no lo
// cambia y uno terminado no vuelve a cambiar.
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
			rt.Fatalf("hold %s = %q, se esperaba %s", h.id, got[h.id], h.status)
		}
	}
}
