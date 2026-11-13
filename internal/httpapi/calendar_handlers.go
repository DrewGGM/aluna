package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/DrewGGM/aluna/internal/calendar"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/postgres"
)

var rolloverBusinessDate = operation{method: http.MethodPost, path: "/v1/business-date/rollover", success: http.StatusOK}

// errNoBusinessDateToClose is a rollover without business_date: it would decode as 0001-01-01 and answer that the
// business date is not the one expected.
var errNoBusinessDateToClose = errors.New("missing business date to close")

// CalendarHandlers serve the operations of the calendar (ADR-0006): the entity's business date and its rollover.
type CalendarHandlers struct {
	calendar *calendar.Service
	commands commands
	reader   postgres.Querier
}

// NewCalendarHandlers creates the calendar handlers; keys stores the responses of the rollover by Idempotency-Key, and
// reader is what the business date is read from.
func NewCalendarHandlers(c *calendar.Service, uow *postgres.UnitOfWork, keys *idempotency.Store, reader postgres.Querier) *CalendarHandlers {
	return &CalendarHandlers{calendar: c, commands: commands{uow: uow, keys: keys}, reader: reader}
}

// GetBusinessDate reads the business date of the token's entity.
func (h *CalendarHandlers) GetBusinessDate(ctx context.Context, _ gen.GetBusinessDateRequestObject) (gen.GetBusinessDateResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	day, err := h.calendar.BusinessDate(ctx, h.reader, p.LegalEntityID)
	if err != nil {
		return nil, err
	}
	return gen.GetBusinessDate200JSONResponse{LegalEntityId: [16]byte(p.LegalEntityID), BusinessDate: dateJSON(day)}, nil
}

// RolloverBusinessDate closes the business date of the token's entity that the body names and advances it to the next
// business day, once per Idempotency-Key.
func (h *CalendarHandlers) RolloverBusinessDate(ctx context.Context, req gen.RolloverBusinessDateRequestObject) (gen.RolloverBusinessDateResponseObject, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body.BusinessDate.IsZero() { // before the key: a request without it is not stored
		return nil, errNoBusinessDateToClose
	}
	resp, err := h.commands.idempotent(ctx, p, rolloverBusinessDate, req.Params.IdempotencyKey, req.Body, func(tx postgres.Tx) (any, error) {
		closed := calendarDate(req.Body.BusinessDate.Time)
		next, err := h.calendar.Rollover(ctx, tx, p.LegalEntityID, closed)
		if err != nil {
			return nil, err
		}
		return gen.BusinessDateRollover{ClosedBusinessDate: dateJSON(closed), BusinessDate: dateJSON(next)}, nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}
