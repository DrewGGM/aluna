package app

import (
	"time"

	"github.com/DrewGGM/aluna/internal/money"
)

// Balance is an account's materialized balance, signed (debit +, credit −), in the account's currency. On
// an async account the aggregator maintains it (T-023).
type Balance struct {
	Posted, PendingDebits, PendingCredits money.Amount
	Version                               int64
}

// BalanceAsOf is the bitemporal balance cut (FR-8, ADR-0006): entries with a value date up to ValueDate
// (its calendar day, in its zone) and posted (posted_at) up to KnownAt. A nil cut does not filter that
// axis.
type BalanceAsOf struct {
	ValueDate, KnownAt *time.Time
}
