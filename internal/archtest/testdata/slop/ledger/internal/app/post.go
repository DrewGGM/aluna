package app

// Poster takes an open type.
type Poster interface {
	Post(payload any) error
}

// First is generic: any as a constraint is not an open type.
func First[T any](items []T) T { return items[0] }

// LedgerManager names the shape; the untyped cache is not AS-5 outside the domain.
type LedgerManager struct{}

var cache map[string]any
