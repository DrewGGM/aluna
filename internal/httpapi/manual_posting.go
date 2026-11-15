package httpapi

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/ids"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// manualInput is the posting that body asks for on behalf of p, through the API: with the business date it must
// bring, entries of valid amounts and a source no module reserves.
func manualInput(body gen.PostTransactionJSONRequestBody, p access.Principal) (ledger.PostInput, error) {
	if err := requireBusinessDate(body.BusinessDate); err != nil {
		return ledger.PostInput{}, err
	}
	if moduleSource(body.Source) {
		return ledger.PostInput{}, fmt.Errorf("%w: the %s source belongs to its module", ledger.ErrInvalidPosting, body.Source)
	}
	entries := make([]ledger.EntryInput, len(body.Entries))
	for i, e := range body.Entries {
		amount, err := amountOf(e.Amount)
		if err != nil {
			return ledger.PostInput{}, fmt.Errorf("entry %d: %w", i, err)
		}
		entries[i] = ledger.EntryInput{AccountID: ids.ID(e.AccountId), Amount: amount}
	}
	return ledger.PostInput{
		LegalEntityID: p.LegalEntityID, Code: body.Code, ValueDate: body.ValueDate.Time, BusinessDate: body.BusinessDate.Time,
		Source: body.Source, ExternalRef: body.ExternalRef, Actor: p.Subject, Channel: apiChannel, RuleVersion: manualRuleVersion,
		Entries: entries,
	}, nil
}

// postManual posts in within tx after locking the deposit accounts it touches, which must be active.
func postManual(ctx context.Context, l *ledger.Service, d *deposits.Service, tx postgres.Tx, in ledger.PostInput) (ledger.Transaction, error) {
	accounts := make([]ids.ID, len(in.Entries))
	for i, e := range in.Entries {
		accounts[i] = e.AccountID
	}
	if err := d.LockActiveHeldIn(ctx, tx, in.LegalEntityID, accounts); err != nil {
		return ledger.Transaction{}, err
	}
	return l.Post(ctx, tx, in)
}

// checkManual posts in under a savepoint and undoes it: a posting the ledger would refuse is refused when it is
// requested, not when it is approved, which checks it again. The undo runs even if the request was canceled; if it
// fails, its error comes back and not the check's, since the posting may still be in tx.
func checkManual(ctx context.Context, l *ledger.Service, d *deposits.Service, tx postgres.Tx, in ledger.PostInput) error {
	if _, err := tx.Exec(ctx, "SAVEPOINT manual_posting_check"); err != nil {
		return fmt.Errorf("manual posting check: %w", err)
	}
	_, postErr := postManual(ctx, l, d, tx, in)
	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkUndoTimeout)
	defer cancel()
	if _, err := tx.Exec(undoCtx, "ROLLBACK TO SAVEPOINT manual_posting_check; RELEASE SAVEPOINT manual_posting_check"); err != nil {
		return fmt.Errorf("undo the manual posting check that answered %v: %w", postErr, err) //nolint:errorlint // postErr must not match: the posting may not be undone
	}
	return postErr
}

// checkUndoTimeout bounds undoing checkManual's posting, which runs even if the request was canceled.
const checkUndoTimeout = 5 * time.Second

// ManualPostingThresholds is, by currency code, the total of a manual posting's debits, in minor units, from which
// it goes through maker-checker (ADR-0019); a currency it does not name never does. It is the process's, the same
// for every entity it serves.
type ManualPostingThresholds map[string]int64

// reached reports whether entries debit, in some currency, at least its threshold.
func (t ManualPostingThresholds) reached(entries []ledger.EntryInput) bool {
	debits := map[string]int64{}
	for _, e := range entries {
		if units, code := e.Amount.Units(), e.Amount.Currency().Code(); units > 0 {
			// saturates: debits that wrapped around would fall under the threshold and skip the approval
			debits[code] = min(debits[code], math.MaxInt64-units) + units
		}
	}
	for code, total := range debits {
		if threshold, named := t[code]; named && total >= threshold {
			return true
		}
	}
	return false
}

// postOnApproval is the Executor of manualPostingKind: it posts the requested body in the entity of the approver,
// who becomes its actor, as PostTransaction would have; a rejection of the ledger or of a deposit account leaves the
// request failed.
func postOnApproval(l *ledger.Service, d *deposits.Service) approvals.Executor {
	return onApproval("manual posting", func(ctx context.Context, tx postgres.Tx, body gen.PostTransactionJSONRequestBody, checker access.Principal) (gen.Transaction, error) {
		in, err := manualInput(body, checker)
		if err != nil {
			return gen.Transaction{}, err
		}
		posted, err := postManual(ctx, l, d, tx, in)
		if err != nil {
			return gen.Transaction{}, err
		}
		return toTransactionJSON(posted), nil
	})
}
