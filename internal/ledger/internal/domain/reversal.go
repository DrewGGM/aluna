package domain

import "fmt"

// ReversalRuleVersion is the version of the ReversalEntries rule: it changes if the rule changes.
const ReversalRuleVersion = "reversal.v1"

// ReversalEntries are the entries that undo entries (FR-3): the same accounts, in the same order, with
// the opposite sign. What was posted is not touched; it is corrected by posting this.
func ReversalEntries(entries []EntryInput) ([]EntryInput, error) {
	reversed := make([]EntryInput, len(entries))
	for i, e := range entries {
		amount, err := e.Amount.Neg()
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		reversed[i] = EntryInput{AccountID: e.AccountID, Amount: amount}
	}
	return reversed, nil
}
