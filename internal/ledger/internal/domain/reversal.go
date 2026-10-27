package domain

import "fmt"

// ReversalRuleVersion es la versión de la regla de ReversalEntries: cambia si cambia la regla.
const ReversalRuleVersion = "reversal.v1"

// ReversalEntries son los entries que deshacen entries (FR-3): las mismas cuentas, en el mismo orden,
// con el signo contrario. Lo asentado no se toca; se corrige asentando esto.
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
