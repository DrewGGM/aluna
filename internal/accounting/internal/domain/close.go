package domain

import "errors"

var (
	// ErrPeriodNotSquare means a period whose journal does not sum zero in some currency.
	ErrPeriodNotSquare = errors.New("the period's trial balance does not sum zero")
	// ErrOpenBreaks means an entity with reconciliation breaks open: a period does not close over them.
	ErrOpenBreaks = errors.New("there are reconciliation breaks open")
)
