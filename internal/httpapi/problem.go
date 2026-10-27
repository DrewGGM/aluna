package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/DrewGGM/aluna/internal/ledger"
	"github.com/DrewGGM/aluna/internal/money"
)

// Problem es un error RFC 9457. Code es estable y para máquinas; Title y Detail son para personas y
// nunca llevan detalles internos.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Code     string `json:"code"`
	Instance string `json:"instance"`
}

// WriteProblem responde p como application/problem+json. Un Type vacío es about:blank y un Instance
// vacío es la ruta pedida.
func WriteProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if p.Type == "" {
		p.Type = "about:blank"
	}
	if p.Instance == "" {
		p.Instance = r.URL.Path
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	json.NewEncoder(w).Encode(p) //nolint:errcheck,gosec // si el cliente cortó, no hay a quién responder; el log de acceso ya tiene el status
}

// requestError responde a un request que el código generado no pudo leer: un cuerpo que superó
// maxBodyBytes (sin Content-Length lo corta withBodyLimit) es 413; el resto, 400.
func requestError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		WriteProblem(w, r, Problem{Title: "Cuerpo demasiado grande", Code: "body_too_large", Status: http.StatusRequestEntityTooLarge})
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

// problems traduce los errores de dominio a la API, agrupados por módulo. Se recorre en orden porque un
// error puede envolver dos sentinelas; gana el primero de la lista. Detail nunca lleva el texto
// interno del error.
var problems = []struct {
	err error
	p   Problem
}{
	// money
	{money.ErrUnknownCurrency, unprocessable("unknown_currency", "Moneda no soportada")},
	{money.ErrOverflow, unprocessable("amount_overflow", "El importe no cabe en el rango permitido")},
	// ledger
	{ledger.ErrDuplicateExternalRef, Problem{Status: http.StatusConflict, Code: "duplicate_external_ref", Title: "La referencia externa ya se asentó"}},
	{ledger.ErrUnbalanced, unprocessable("unbalanced_transaction", "La transacción no suma cero en cada moneda")},
	{ledger.ErrTooFewEntries, unprocessable("too_few_entries", "Una transacción necesita al menos dos entries")},
	{ledger.ErrZeroAmount, unprocessable("zero_amount", "Un entry no puede tener importe cero")},
	{ledger.ErrInsufficientFunds, unprocessable("insufficient_funds", "Fondos insuficientes")},
	{ledger.ErrCurrencyMismatch, unprocessable("currency_mismatch", "El entry no está en la moneda de su cuenta")},
	{ledger.ErrInvalidAccount, unprocessable("invalid_account", "Datos de cuenta inválidos")},
	{ledger.ErrInvalidPosting, unprocessable("invalid_posting", "Cabecera del posting incompleta")},
	{ledger.ErrAccountNotFound, notFound},
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
