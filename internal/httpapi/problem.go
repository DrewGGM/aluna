package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/trace"

	"github.com/DrewGGM/aluna/internal/httpapi/gen"
	"github.com/DrewGGM/aluna/internal/idempotency"
	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
)

// Problem es un error RFC 9457. Code es estable y para máquinas; Title y Detail son para personas y
// nunca llevan detalles internos. TraceID une la respuesta con la traza y los logs del request.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Code     string `json:"code"`
	Instance string `json:"instance"`
	TraceID  string `json:"trace_id,omitempty"`
}

// WriteProblem responde p como application/problem+json, completado para la ruta pedida.
func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	json.NewEncoder(w).Encode(p.complete(r.Context(), r.URL.Path)) //nolint:errcheck,gosec // si el cliente cortó, no hay a quién responder; el log de acceso ya tiene el status
}

// complete llena lo que falta: un Type vacío es about:blank, un Instance vacío es path y TraceID sale
// del span de ctx, si hay.
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

// requestError responde a un request que el código generado no pudo leer: un cuerpo que superó
// maxBodyBytes (sin Content-Length lo corta withBodyLimit) es 413; una Idempotency-Key ausente o vacía
// en un comando que mueve dinero (el código generado da RequiredHeaderError o InvalidParamFormatError),
// 400 idempotency_key_required; el resto, 400 invalid_request.
func requestError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		WriteProblem(w, r, Problem{Title: "Cuerpo demasiado grande", Code: "body_too_large", Status: http.StatusRequestEntityTooLarge})
		return
	}
	var missing *gen.RequiredHeaderError
	var empty *gen.InvalidParamFormatError
	if (errors.As(err, &missing) && missing.ParamName == "Idempotency-Key") ||
		(errors.As(err, &empty) && empty.ParamName == "Idempotency-Key") {
		WriteProblem(w, r, Problem{
			Title: "Falta Idempotency-Key", Code: "idempotency_key_required", Status: http.StatusBadRequest,
			Detail: "Un comando que mueve dinero exige la cabecera Idempotency-Key.",
		})
		return
	}
	WriteProblem(w, r, Problem{
		Title: "Request inválido", Code: "invalid_request", Status: http.StatusBadRequest,
		Detail: "El cuerpo o los parámetros no cumplen el contrato.",
	})
}

// responseError responde al error de un handler: el de un dominio conocido con su problem; cualquier
// otro es un 500 que se registra con su detalle (que nunca sale en la respuesta).
func responseError(log *slog.Logger) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		if p, ok := problemFor(err); ok {
			WriteProblem(w, r, p)
			return
		}
		log.LogAttrs(r.Context(), slog.LevelError, "error del handler",
			slog.String("error", err.Error()), slog.String("request_id", requestIDFrom(r.Context())))
		WriteProblem(w, r, Problem{Title: "Error interno", Code: "internal_error", Status: http.StatusInternalServerError})
	}
}

var notFound = Problem{Status: http.StatusNotFound, Code: "not_found", Title: "Recurso no encontrado"}

func unprocessable(code, title string) Problem {
	return Problem{Status: http.StatusUnprocessableEntity, Code: code, Title: title}
}

func conflict(code, title string) Problem {
	return Problem{Status: http.StatusConflict, Code: code, Title: title}
}

// problems traduce los errores de dominio a la API, agrupados por módulo. Se recorre en orden porque un
// error puede envolver dos sentinelas; gana el primero de la lista. Detail nunca lleva el texto
// interno del error.
var problems = []struct {
	err error
	p   Problem
}{
	// httpapi
	{errInvalidIdempotencyKey, Problem{
		Status: http.StatusBadRequest, Code: "invalid_request", Title: "Idempotency-Key inválida",
		Detail: fmt.Sprintf("La clave debe ser texto UTF-8 de hasta %d caracteres.", maxIdempotencyKey),
	}},
	// idempotency
	{idempotency.ErrKeyReused, unprocessable("idempotency_key_reused", "La Idempotency-Key ya se usó con otro payload")},
	{idempotency.ErrInProgress, conflict("request_in_progress", "Hay una solicitud con la misma Idempotency-Key en curso")},
	// money
	{money.ErrUnknownCurrency, unprocessable("unknown_currency", "Moneda no soportada")},
	{money.ErrOverflow, unprocessable("amount_overflow", "El importe no cabe en el rango permitido")},
	// ledger
	{ledger.ErrDuplicateExternalRef, conflict("duplicate_external_ref", "La referencia externa ya se asentó")},
	{ledger.ErrUnbalanced, unprocessable("unbalanced_transaction", "La transacción no suma cero en cada moneda")},
	{ledger.ErrTooFewEntries, unprocessable("too_few_entries", "Una transacción necesita al menos dos entries")},
	{ledger.ErrZeroAmount, unprocessable("zero_amount", "Un entry no puede tener importe cero")},
	{ledger.ErrInsufficientFunds, unprocessable("insufficient_funds", "Fondos insuficientes")},
	{ledger.ErrCurrencyMismatch, unprocessable("currency_mismatch", "El importe no está en la moneda de su cuenta o de su hold")},
	{ledger.ErrInvalidAccount, unprocessable("invalid_account", "Datos de cuenta inválidos")},
	{ledger.ErrInvalidPosting, unprocessable("invalid_posting", "Cabecera del posting incompleta")},
	{ledger.ErrAccountNotFound, notFound},
	{ledger.ErrHoldNotFound, notFound},
	{ledger.ErrHoldNotPending, conflict("hold_not_pending", "El hold ya no está pendiente")},
	{ledger.ErrHoldExpired, conflict("hold_expired", "El hold venció")},
	{ledger.ErrAmountExceedsHold, unprocessable("amount_exceeds_hold", "El importe supera lo retenido")},
	{ledger.ErrInvalidHold, unprocessable("invalid_hold", "Datos del hold inválidos")},
	{ledger.ErrTransactionNotFound, notFound},
}

func problemFor(err error) (Problem, bool) {
	for _, m := range problems {
		if errors.Is(err, m.err) {
			return m.p, true
		}
	}
	return Problem{}, false
}
