package httpapi

import (
	"encoding/json"
	"net/http"
)

// Problem es un error RFC 9457. Code es estable y para máquinas; Title y Detail son para personas y
// nunca llevan detalles internos.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Code     string `json:"code,omitempty"`
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
