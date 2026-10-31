package app

import "time"

// BalanceAsOf is the bitemporal balance cut (FR-8, ADR-0006): entries with a value date up to ValueDate
// (its calendar day, in its zone) and posted (posted_at) up to KnownAt. A nil cut does not filter that
// axis.
type BalanceAsOf struct {
	ValueDate, KnownAt *time.Time
}
