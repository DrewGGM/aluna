package reconciliation

import (
	"context"

	"github.com/DrewGGM/aluna/internal/postgres"
	"github.com/DrewGGM/aluna/internal/reconciliation/internal/app"
)

// Types of a statement.
type (
	// Statement is what a rail reports of one day.
	Statement = app.Statement
	// StatementLine is a movement the rail reports.
	StatementLine = app.StatementLine
	// Direction is which way a line went.
	Direction = app.Direction
	// ImportResult is what an import found.
	ImportResult = app.ImportResult
)

// Directions of a statement's line.
const (
	Incoming = app.Incoming
	Outgoing = app.Outgoing
)

// Errors of a statement.
var (
	ErrInvalidStatement   = app.ErrInvalidStatement
	ErrDuplicateStatement = app.ErrDuplicateStatement
)

// ImportStatement matches statement in against the payment orders of its rail, within tx, and opens a break for
// each exception (FR-34). See app.Import.
func (s *Service) ImportStatement(ctx context.Context, tx postgres.Tx, in Statement) (ImportResult, error) {
	return app.Import(ctx, s.within(tx), in)
}
