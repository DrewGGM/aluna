package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/trace"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/approvals"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/party"
)

// Problem is an RFC 9457 error. Code is stable and meant for machines; Title and Detail are meant for people
// and never carry internal details. TraceID ties the response to the trace and logs of the request.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Code     string `json:"code"`
	Instance string `json:"instance"`
	TraceID  string `json:"trace_id,omitempty"`
}

// WriteProblem answers p as application/problem+json, completed for the requested path.
func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	noteRejection(r.Context(), p.Code)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	json.NewEncoder(w).Encode(p.complete(r.Context(), r.URL.Path)) //nolint:errcheck,gosec // if the client hung up, there is no one to answer; the access log already has the status
}

// complete fills in what is missing: an empty Type is about:blank, an empty Instance is path and TraceID
// comes from the span in ctx, if any.
func (p Problem) complete(ctx context.Context, path string) Problem {
	if p.Type == "" {
		p.Type = "about:blank"
	}
	if p.Instance == "" {
		p.Instance = path
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		p.TraceID = sc.TraceID().String()
	}
	return p
}

// requestError answers a request the generated code could not read: a body over maxBodyBytes (without
// Content-Length, withBodyLimit cuts it) is 413; a missing or empty Idempotency-Key on a command that moves
// money (the generated code gives RequiredHeaderError or InvalidParamFormatError) is 400
// idempotency_key_required; anything else is 400 invalid_request.
func requestError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		WriteProblem(w, r, Problem{Title: "Body too large", Code: "body_too_large", Status: http.StatusRequestEntityTooLarge})
		return
	}
	var missing *gen.RequiredHeaderError
	var empty *gen.InvalidParamFormatError
	if (errors.As(err, &missing) && missing.ParamName == "Idempotency-Key") ||
		(errors.As(err, &empty) && empty.ParamName == "Idempotency-Key") {
		WriteProblem(w, r, Problem{
			Title: "Missing Idempotency-Key", Code: "idempotency_key_required", Status: http.StatusBadRequest,
			Detail: "A command that moves money requires the Idempotency-Key header.",
		})
		return
	}
	WriteProblem(w, r, Problem{
		Title: "Invalid request", Code: "invalid_request", Status: http.StatusBadRequest,
		Detail: "The body or the parameters do not match the contract.",
	})
}

// responseError answers a handler error: one from a known domain with its problem; any other is a 500 that
// is logged with its detail (which never goes out in the response).
func responseError(log *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		if p, ok := problemFor(err); ok {
			WriteProblem(w, r, p)
			return
		}
		log.LogAttrs(r.Context(), slog.LevelError, "handler error",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		WriteProblem(w, r, Problem{Title: "Internal error", Code: "internal_error", Status: http.StatusInternalServerError})
	}
}

var notFound = Problem{Status: http.StatusNotFound, Code: "not_found", Title: "Resource not found"}

func unprocessable(code, title string) Problem {
	return Problem{Status: http.StatusUnprocessableEntity, Code: code, Title: title}
}

func conflict(code, title string) Problem {
	return Problem{Status: http.StatusConflict, Code: code, Title: title}
}

// problems maps domain errors to the API, grouped by module. It is walked in order because an error can
// wrap two sentinels; the first one in the list wins. Detail never carries the internal text of the
// error.
var problems = []struct {
	err error
	p   Problem
}{
	// httpapi
	{errInvalidIdempotencyKey, Problem{
		Status: http.StatusBadRequest, Code: "invalid_request", Title: "Invalid Idempotency-Key",
		Detail: fmt.Sprintf("The key must be UTF-8 text of up to %d characters.", maxIdempotencyKey),
	}},
	{errReversalOfReversal, conflict("reversal_of_reversal", "The transaction is already a reversal")},
	{errInvalidApprovalStatus, Problem{
		Status: http.StatusBadRequest, Code: "invalid_request", Title: "Unknown request status",
		Detail: "status is pending, approved, rejected, failed or expired.",
	}},
	// idempotency
	{idempotency.ErrKeyReused, unprocessable("idempotency_key_reused", "The Idempotency-Key was already used with another payload")},
	{idempotency.ErrInProgress, conflict("request_in_progress", "A request with the same Idempotency-Key is in progress")},
	// money
	{money.ErrUnknownCurrency, unprocessable("unknown_currency", "Unsupported currency")},
	{money.ErrOverflow, unprocessable("amount_overflow", "The amount does not fit in the allowed range")},
	// ledger
	{ledger.ErrDuplicateExternalRef, conflict("duplicate_external_ref", "The external reference was already posted")},
	{ledger.ErrUnbalanced, unprocessable("unbalanced_transaction", "The transaction does not sum to zero in each currency")},
	{ledger.ErrTooFewEntries, unprocessable("too_few_entries", "A transaction needs at least two entries")},
	{ledger.ErrZeroAmount, unprocessable("zero_amount", "An entry cannot have a zero amount")},
	{ledger.ErrInsufficientFunds, unprocessable("insufficient_funds", "Insufficient funds")},
	{ledger.ErrCurrencyMismatch, unprocessable("currency_mismatch", "The amount is not in the currency of its account or its hold")},
	{ledger.ErrInvalidAccount, unprocessable("invalid_account", "Invalid account data")},
	{ledger.ErrInvalidPosting, unprocessable("invalid_posting", "Incomplete posting header")},
	{ledger.ErrAccountNotFound, notFound},
	{ledger.ErrHoldNotFound, notFound},
	{ledger.ErrHoldNotPending, conflict("hold_not_pending", "The hold is no longer pending")},
	{ledger.ErrHoldExpired, conflict("hold_expired", "The hold expired")},
	{ledger.ErrAmountExceedsHold, unprocessable("amount_exceeds_hold", "The amount exceeds the held amount")},
	{ledger.ErrInvalidHold, unprocessable("invalid_hold", "Invalid hold data")},
	{ledger.ErrTransactionNotFound, notFound},
	{ledger.ErrPeriodClosed, conflict("period_closed", "The accounting period of the business date is closed")},
	{ledger.ErrAlreadyReversed, conflict("already_reversed", "The transaction is already reversed")},
	// access
	{access.ErrForbidden, forbidden},
	// party
	{party.ErrInvalidParty, unprocessable("invalid_party", "Invalid party data")},
	{party.ErrAlreadyRegistered, conflict("party_already_registered", "The entity already registered that external_ref")},
	{party.ErrNotFound, notFound},
	// approvals
	{approvals.ErrSelfApproval, Problem{
		Status: http.StatusForbidden, Code: "self_approval_forbidden", Title: "The requester cannot decide their own request",
	}},
	{approvals.ErrNotFound, notFound},
	{approvals.ErrNotPending, conflict("approval_not_pending", "The request was already decided")},
	{approvals.ErrExpired, conflict("approval_expired", "The request expired")},
	{approvals.ErrNoReason, unprocessable("rejection_reason_required", "A rejection needs a reason")},
}

func problemFor(err error) (Problem, bool) {
	for _, m := range problems {
		if errors.Is(err, m.err) {
			return m.p, true
		}
	}
	return Problem{}, false
}
