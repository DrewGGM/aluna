package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// CommandAudit es lo que necesita la auditoría de los comandos (FR-40): el servicio que guarda los
// registros y la Unit of Work con que se registra, en una tx aparte, un comando que no confirmó el suyo.
type CommandAudit struct {
	Records *audit.Service
	UoW     *postgres.UnitOfWork
}

// Quién llama antes de autenticarse: un request rechazado sin token queda con este actor y rol; uno
// autenticado, con los de su token (notePrincipal).
const (
	auditActor   = "anónimo"
	auditRole    = "ninguno"
	auditChannel = "api"
)

// Resultados de un comando en su registro, además del código del problem con que se rechazó.
const (
	resultOK     = "ok"
	resultReplay = "replay"
	resultError  = "error" // sin problem escrito: un panic que escapó, o un POST con ruta que no pasó por commands
)

// apartTimeout acota el registro en una tx aparte: corre después de que el request terminó y no debe
// quedarse esperando una base caída.
const apartTimeout = 5 * time.Second

var errNoAuditTrail = errors.New("comando sin rastro de auditoría")

type auditTrailKey struct{}

// auditTrail es el registro de auditoría de un POST en curso. committed dice que el comando confirmó el
// suyo en su propia transacción; rejected es el código del problem que se respondió, si hubo.
type auditTrail struct {
	records   *audit.Service
	base      audit.Record
	committed bool
	rejected  string
}

// auditTrailFrom es el rastro del request; sin él un comando no corre (falla cerrado).
func auditTrailFrom(ctx context.Context) (*auditTrail, error) {
	trail, ok := ctx.Value(auditTrailKey{}).(*auditTrail)
	if !ok {
		return nil, errNoAuditTrail
	}
	return trail, nil
}

// record guarda en tx, la del comando, su registro con el resultado result. Si salió bien, after es el
// resumen de la respuesta y targets los IDs que aparecen en él.
func (t *auditTrail) record(ctx context.Context, tx postgres.Tx, resp storedResponse, result string) error {
	r := t.base
	r.Result = result
	if result == resultOK {
		after, err := summaryOf(resp.body)
		if err != nil {
			return err
		}
		r.After, r.Targets = after, slices.Sorted(maps.Keys(idsIn(after, r.Targets)))
	}
	return t.records.Record(ctx, tx, r)
}

// summaryKeys son los campos de una respuesta que pasan al after: identificadores, montos, monedas,
// estados y fechas. El texto libre del cliente (code, source, external_ref) se queda fuera: puede traer
// datos de personas y el registro dura 10 años.
var summaryKeys = map[string]bool{
	"id": true, "account_id": true, "counter_account_id": true, "amount": true, "captured": true, "currency": true,
	"status": true, "kind": true, "balance_mode": true, "lower_limit": true, "value_date": true, "business_date": true,
	"posted_at": true, "expires_at": true, "entries": true, "hold": true, "transaction": true,
}

// summaryOf es el resumen de una respuesta JSON. Los números quedan como json.Number: un monto en
// unidades menores no pasa por float.
func summaryOf(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("respuesta del comando para auditar: %w", err)
	}
	summary, _ := summarized(v).(map[string]any)
	return summary, nil
}

func summarized(v any) any {
	switch v := v.(type) {
	case map[string]any:
		kept := make(map[string]any, len(v))
		for k, inner := range v {
			if summaryKeys[k] {
				kept[k] = summarized(inner)
			}
		}
		return kept
	case []any:
		out := make([]any, len(v))
		for i, inner := range v {
			out[i] = summarized(inner)
		}
		return out
	default:
		return v
	}
}

// idsIn son, sin repetir, los IDs de known más los del resumen v: los de los recursos y las cuentas que
// tocó el comando.
func idsIn(v any, known []string) map[string]bool {
	found := map[string]bool{}
	for _, id := range known {
		found[id] = true
	}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, inner := range v {
				if id, ok := inner.(string); ok && (k == "id" || strings.HasSuffix(k, "_id")) {
					found[id] = true
				}
				walk(inner)
			}
		case []any:
			for _, inner := range v {
				walk(inner)
			}
		}
	}
	walk(v)
	return found
}

// withAudit le da a cada POST con ruta su rastro de auditoría: quién, desde dónde, con qué correlación y
// qué comando. El comando es el patrón de la ruta y se busca aquí en routes, antes del handler: withRoute
// lo sabe solo después. Una ruta sin limpiar (/v1//transactions) no lleva rastro: el mux la redirige y el
// comando llega en el request siguiente. Si el comando no confirmó su registro (un rechazo antes de su
// tx, un 5xx o un panic), lo registra en una tx aparte con el código del problem que se respondió; el
// defer lo hace aunque un panic escape de withRecovery.
func withAudit(a *CommandAudit, routes *http.ServeMux, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || path.Clean(r.URL.Path) != r.URL.Path {
				next.ServeHTTP(w, r)
				return
			}
			_, pattern := routes.Handler(r)
			if pattern == "" {
				next.ServeHTTP(w, r)
				return
			}
			trail := &auditTrail{records: a.Records, base: audit.Record{
				Actor: auditActor, Role: auditRole, Channel: auditChannel, IP: clientIP(r),
				CorrelationID: requestIDFrom(r.Context()), Command: pattern,
			}}
			defer a.recordApart(r.Context(), trail, log)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), auditTrailKey{}, trail)))
		})
	}
}

// recordApart registra en su propia transacción el comando del rastro si su tx no confirmó el registro.
// Corre aunque el cliente ya se haya ido, con su propio límite.
func (a *CommandAudit) recordApart(ctx context.Context, trail *auditTrail, log *slog.Logger) {
	if trail.committed {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), apartTimeout)
	defer cancel()
	r := trail.base
	r.Result = trail.rejected
	if r.Result == "" {
		r.Result = resultError
	}
	err := a.UoW.Do(ctx, func(tx postgres.Tx) error { return a.Records.Record(ctx, tx, r) })
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "auditar un comando que no confirmó",
			slog.String("command", r.Command), slog.String("request_id", r.CorrelationID), slog.String("error", err.Error()))
	}
}

// notePrincipal anota en el rastro del request, si hay, quién lo hace: el sujeto y los roles del token.
func notePrincipal(ctx context.Context, p access.Principal) {
	if trail, err := auditTrailFrom(ctx); err == nil {
		trail.base.Actor = p.Subject
		if len(p.Roles) > 0 {
			trail.base.Role = strings.Join(p.Roles, ",")
		}
	}
}

// noteTargets anota en el rastro del request, si hay, los IDs que nombra la ruta del comando: su registro
// los lleva aunque se rechace.
func noteTargets(ctx context.Context, targets []string) {
	if trail, err := auditTrailFrom(ctx); err == nil {
		trail.base.Targets = targets
	}
}

// noteRejection anota en el rastro del request, si hay, el código del problem que se le responde.
func noteRejection(ctx context.Context, code string) {
	if trail, err := auditTrailFrom(ctx); err == nil {
		trail.rejected = code
	}
}

// clientIP es la IP de la conexión. No se toma de X-Forwarded-For: la pone cualquiera (Q-85).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
