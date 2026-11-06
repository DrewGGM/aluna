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
	"github.com/DrewGGM/aluna/internal/compliance"
	"github.com/DrewGGM/aluna/internal/deposits"
	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
	"github.com/DrewGGM/aluna/internal/party"
	"github.com/DrewGGM/aluna/internal/payments"
	"github.com/DrewGGM/aluna/internal/pricing"
	"github.com/DrewGGM/aluna/internal/products"
	"github.com/DrewGGM/aluna/internal/reconciliation"
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

// invalidRequest is a 400 invalid_request whose detail says what a valid request carries.
func invalidRequest(title, detail string) Problem {
	return Problem{Status: http.StatusBadRequest, Code: "invalid_request", Title: title, Detail: detail}
}

// problems maps domain errors to the API, grouped by module. It is walked in order because an error can
// wrap two sentinels; the first one in the list wins. Detail never carries the internal text of the
// error.
var problems = []struct {
	err error
	p   Problem
}{
	// httpapi
	{errInvalidIdempotencyKey, invalidRequest("Invalid Idempotency-Key",
		fmt.Sprintf("The key must be UTF-8 text of up to %d characters.", maxIdempotencyKey))},
	{errReversalOfReversal, conflict("reversal_of_reversal", "The transaction is already a reversal")},
	{errInvalidResolution, invalidRequest("Unknown resolution", "resolution is cleared or confirmed.")},
	{errInvalidApprovalStatus, invalidRequest("Unknown request status", "status is pending, approved, rejected, failed or expired.")},
	// idempotency
	{idempotency.ErrKeyReused, unprocessable("idempotency_key_reused", "The Idempotency-Key was already used with another payload")},
	{idempotency.ErrInProgress, conflict("request_in_progress", "A request with the same Idempotency-Key is in progress")},
	// money
	{money.ErrUnknownCurrency, unprocessable("unknown_currency", "Unsupported currency")},
	{money.ErrOverflow, unprocessable("amount_overflow", "The amount does not fit in the allowed range")},
	// ledger
	{ledger.ErrInvalidPage, invalidRequest("Invalid page",
		fmt.Sprintf("limit is 1 to %d, and cursor is the next_cursor of a previous page.", ledger.MaxPageSize))},
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
	{party.ErrInvalidKYCTransition, unprocessable("invalid_kyc_transition", "The KYC status cannot change that way")},
	{party.ErrStaleKYCChange, conflict("kyc_change_stale", "The party changed after the KYC change was requested")},
	{party.ErrNotPendingScreening, conflict("not_pending_screening", "The party is not pending screening")},
	// products
	{products.ErrInvalidDefinition, unprocessable("invalid_product_definition", "The definition does not follow deposit-product.v1.json")},
	{products.ErrInvalidProduct, unprocessable("invalid_product", "Invalid product code or name")},
	{products.ErrProductExists, conflict("product_already_exists", "The entity already has a product with that code")},
	{products.ErrProductNotFound, notFound},
	{products.ErrVersionNotFound, notFound},
	{products.ErrNotPublishable, conflict("version_not_publishable", "The version is already published or retired")},
	{products.ErrNotTheAuthor, Problem{Status: http.StatusForbidden, Code: "not_version_author", Title: "Only the author of the version asks for its publication"}},
	{products.ErrNotPending, conflict("version_not_pending", "The version is not pending publication")},
	{products.ErrSuperseded, conflict("version_superseded", "A later version of the product is in force")},
	{products.ErrInvalidMarketParameter, unprocessable("invalid_market_parameter", "Invalid market parameter")},
	{products.ErrMarketParameterOverlap, conflict("market_parameter_overlap", "Another value of the parameter holds in that period")},
	{products.ErrNoMarketParameter, Problem{Status: http.StatusNotFound, Code: "no_market_parameter", Title: "No value of the parameter holds on that day"}},
	{products.ErrNoPublishedVersion, conflict("no_published_version", "The product has no published version")},
	// deposits; deposits.ErrProductAccountMissing, ErrMinimumWageMissing, ErrMinimumWageTooLow and
	// ErrSMMLVOutsideCOP have no problem on purpose: a misconfigured product or market parameter is a 500, which
	// is not stored with the Idempotency-Key. A missing account or wage can be fixed and the request retried; a
	// version with limits in SMMLV outside COP cannot change, so its accounts need another version
	{deposits.ErrAccountNotFound, notFound},
	{deposits.ErrAccountNotActive, conflict("account_not_active", "Only an active account moves money")},
	{deposits.ErrKYCNotSatisfied, unprocessable("kyc_not_satisfied", "The party's KYC status does not allow the product")},
	{deposits.ErrInvalidMovement, unprocessable("invalid_movement", "The amount must be positive and the accounts different")},
	{deposits.ErrLimitExceeded, unprocessable("limit_exceeded", "The movement exceeds a limit of the product")},
	// payments; payments.ErrRailNotConfigured has no problem on purpose: an entity without the rail's clearing
	// account is a 500, which is not stored with the Idempotency-Key, and the request can be retried once the
	// account exists
	{payments.ErrDuplicatePayment, conflict("duplicate_payment", "A payment with that reference was already made")},
	{payments.ErrInvalidOrder, unprocessable("invalid_payment_order", "The payment is in another currency than its account, through an unknown rail, without a reference, or its beneficiary is not one account of up to 64 characters or one key of up to 128")},
	{payments.ErrUnverifiedNotification, Problem{
		Status: http.StatusUnauthorized, Code: "invalid_signature", Title: "The rail did not sign the notification in the last 5 minutes",
	}},
	{payments.ErrInvalidNotification, invalidRequest("Invalid notification",
		"A notification of the rail carries an outcome, accepted or rejected with its reason, or a transfer with ISO 20022 identifiers, the code of the bank that sent it, a positive amount, its settlement date and its creditor by account or by key; references and reasons are text of up to 255 characters.")},
	{payments.ErrOrderNotFound, notFound},
	{payments.ErrOutcomeConflict, conflict("outcome_conflict", "The payment order already took another outcome")},
	// reconciliation
	{reconciliation.ErrInvalidStatement, unprocessable("invalid_statement", "A statement is of a known rail, with a reference of up to 35 characters, a date and up to 5000 lines, each with an end-to-end id of up to 35, a positive amount, a direction and that date")},
	{reconciliation.ErrDuplicateStatement, conflict("duplicate_statement", "That statement of the rail was already imported")},
	{reconciliation.ErrBreakNotFound, notFound},
	{reconciliation.ErrBreakResolved, conflict("break_already_resolved", "The break is already resolved")},
	{reconciliation.ErrInvalidResolution, unprocessable("invalid_resolution", "A resolution has a cause of 1 to 1000 characters")},
	{reconciliation.ErrInvalidPage, invalidRequest("Invalid page", "A page has 1 to 200 breaks, of status open or resolved, and its cursor is one a previous page gave.")},
	// compliance
	{compliance.ErrCaseNotFound, notFound},
	{compliance.ErrCaseResolved, conflict("case_already_resolved", "The screening case is already resolved")},
	// pricing
	{pricing.ErrInvalidQuote, unprocessable("invalid_quote", "The operation is not priced, or the amount is not positive or not in the account's currency")},
	{pricing.ErrQuoteUnavailable, conflict("quote_unavailable", "The quote does not exist for this operation, was already used or expired, or its GMF exemption was used meanwhile")},
	{pricing.ErrGMFNotEligible, unprocessable("gmf_not_eligible", "The account cannot be exempt from GMF")},
	{pricing.ErrGMFExemptionTaken, conflict("gmf_exemption_taken", "The GMF exemption is already on an account of the party")},
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
			return withDetail(m.p, err), true
		}
	}
	return Problem{}, false
}

// withDetail adds to p what the client needs to know from err and that its code alone does not say: the limit
// a movement exceeds (FR-24).
func withDetail(p Problem, err error) Problem {
	var limit *deposits.LimitError
	if errors.As(err, &limit) {
		p.Detail = fmt.Sprintf("The movement exceeds the product's %s limit of %d minor units.", limit.Name, limit.Units)
	}
	return p
}
