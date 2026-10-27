package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"time"
)

const (
	headerRequestID = "X-Request-Id"
	maxRequestIDLen = 128
	maxBodyBytes    = 1 << 20
	requestIDChars  = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_."
)

// middleware envuelve h con la cadena común, del más externo al más interno. Un middleware nuevo se
// agrega en esta lista con la forma func(http.Handler) http.Handler.
func middleware(log *slog.Logger, h http.Handler) http.Handler {
	chain := []func(http.Handler) http.Handler{
		withRequestID,
		withAccessLog(log),
		withRecovery(log),
		withBodyLimit,
	}
	for _, mw := range slices.Backward(chain) {
		h = mw(h)
	}
	return h
}

type requestIDKey struct{}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// withRequestID conserva el X-Request-Id del cliente solo si es seguro de escribir en un log; si no,
// genera uno.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(headerRequestID)
		if !validRequestID(id) {
			id = rand.Text()
		}
		w.Header().Set(headerRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func validRequestID(id string) bool {
	return id != "" && len(id) <= maxRequestIDLen && strings.Trim(id, requestIDChars) == ""
}

// statusRecorder recuerda el status de la respuesta; 0 significa que todavía no se escribió nada.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Flush mantiene http.Flusher y marca la respuesta como enviada: tras un flush ya no cabe un 500.
func (s *statusRecorder) Flush() {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	http.NewResponseController(s.ResponseWriter).Flush() //nolint:errcheck,gosec // Flusher no tiene cómo devolver el error; el siguiente Write lo verá
}

// code es el status que vio el cliente: un handler que no escribe nada responde 200.
func (s *statusRecorder) code() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

// withAccessLog escribe una línea por request, también si el request termina en un panic que aborta
// la conexión. Registra la ruta sin el query string, que puede traer datos personales.
func withAccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w}
			defer logRequest(log, r, rec, time.Now())
			next.ServeHTTP(rec, r)
		})
	}
}

func logRequest(log *slog.Logger, r *http.Request, rec *statusRecorder, start time.Time) {
	log.LogAttrs(r.Context(), slog.LevelInfo, "request",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", rec.code()),
		slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		slog.String("request_id", requestIDFrom(r.Context())),
	)
}

// withRecovery convierte un panic en un 500 problem+json. El stack va al log, nunca a la respuesta.
func withRecovery(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w}
			defer recoverPanic(log, rec, r)
			next.ServeHTTP(rec, r)
		})
	}
}

// recoverPanic se difiere directamente para que recover() atrape el panic del handler.
func recoverPanic(log *slog.Logger, rec *statusRecorder, r *http.Request) {
	v := recover()
	if v == nil {
		return
	}
	if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(v) //nolint:forbidigo // net/http usa ErrAbortHandler para cortar la conexión a propósito
	}
	log.LogAttrs(r.Context(), slog.LevelError, "panic",
		slog.String("value", fmt.Sprint(v)),
		slog.String("stack", string(debug.Stack())),
		slog.String("request_id", requestIDFrom(r.Context())),
	)
	if rec.status != 0 {
		// Las cabeceras ya salieron: se corta la conexión para que el cliente no tome como completa
		// una respuesta truncada.
		panic(http.ErrAbortHandler) //nolint:forbidigo // así net/http aborta la respuesta en curso
	}
	// Las cabeceras que dejó el handler (Set-Cookie, Content-Length, Location) no aplican al 500.
	header := rec.Header()
	requestID := header.Get(headerRequestID)
	clear(header)
	header.Set(headerRequestID, requestID)
	WriteProblem(rec, r, Problem{Title: "Error interno", Code: "internal_error", Status: http.StatusInternalServerError})
}

// withBodyLimit rechaza con 413 un cuerpo declarado de más de maxBodyBytes. Un cuerpo sin
// Content-Length se corta en ese mismo límite y el handler recibe *http.MaxBytesError: convertirlo en
// 413 es tarea del handler (el código generado lo hará en T-012).
func withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBodyBytes {
			WriteProblem(w, r, Problem{
				Title: "Cuerpo demasiado grande", Code: "body_too_large", Status: http.StatusRequestEntityTooLarge,
				Detail: fmt.Sprintf("El cuerpo supera %d bytes.", maxBodyBytes),
			})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}
