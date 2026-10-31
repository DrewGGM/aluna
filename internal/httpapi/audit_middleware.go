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
	"slices"
	"strings"
	"time"

	"github.com/DrewGGM/aluna/internal/access"
	"github.com/DrewGGM/aluna/internal/audit"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// CommandAudit is what command auditing needs (FR-40): the service that stores the records and the Unit of
// Work used to record, in a separate tx, a command that did not commit its own.
type CommandAudit struct {
	Records *audit.Service
	UoW     *postgres.UnitOfWork
}

// Who calls before authenticating: a request rejected without a token keeps this actor and role; an
// authenticated one, those of its token (notePrincipal).
const (
	auditActor   = "anonymous"
	auditRole    = "none"
	auditChannel = "api"
)

// Results of a command in its record, besides the code of the problem it was rejected with.
const (
	resultOK     = "ok"
	resultReplay = "replay"
	resultError  = "error" // no problem written: an escaped panic, or a routed POST that skipped commands
)

// apartTimeout bounds the record in a separate tx: it runs after the request ended and must not keep
// waiting on a database that is down.
const apartTimeout = 5 * time.Second

var errNoAuditTrail = errors.New("command without audit trail")

type auditTrailKey struct{}

// auditTrail is the audit record of a POST in progress. committed says the command committed its record in
// its own transaction; rejected is the code of the problem that was answered, if any.
type auditTrail struct {
	records   *audit.Service
	base      audit.Record
	committed bool
	rejected  string
}

// auditTrailFrom is the request's trail; without it a command does not run (fail closed).
func auditTrailFrom(ctx context.Context) (*auditTrail, error) {
	trail, ok := ctx.Value(auditTrailKey{}).(*auditTrail)
	if !ok {
		return nil, errNoAuditTrail
	}
	return trail, nil
}

// record stores in tx, the command's, its record with the result result. If it succeeded, after is the
// summary of the response and targets the IDs that appear in it.
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

// summaryKeys are the response fields that go into after: identifiers, amounts, currencies, statuses and
// dates. The client's free text (code, source, external_ref) is left out: it can carry personal data and the
// record lasts 10 years.
var summaryKeys = map[string]bool{
	"id": true, "account_id": true, "counter_account_id": true, "amount": true, "captured": true, "currency": true,
	"status": true, "kind": true, "balance_mode": true, "lower_limit": true, "value_date": true, "business_date": true,
	"posted_at": true, "expires_at": true, "entries": true, "hold": true, "transaction": true,
}

// summaryOf is the summary of a JSON response. Numbers stay as json.Number: an amount in minor units never
// goes through float.
func summaryOf(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("command response to audit: %w", err)
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

// idsIn are, without repeats, the IDs in known plus those in the summary v: the resources and accounts the
// command touched.
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

// withAudit gives every routed POST its audit trail: who, from where, with which correlation and which
// command. The command is the route pattern and it is looked up here in routes, before the handler: withRoute
// only knows it afterwards. A path that the mux redirects to its clean path (/v1//transactions,
// redirectsToCleanPath) carries no trail: the command arrives in the next request. If the command did not
// commit its record (a rejection before its tx, a 5xx or a panic), it records it in a separate tx with the
// code of the problem that was answered; the defer does it even if a panic escapes withRecovery.
func withAudit(a *CommandAudit, routes *http.ServeMux, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || redirectsToCleanPath(r) {
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

// recordApart records the trail's command in its own transaction if its tx did not commit the record. It
// runs even if the client already left, with its own limit.
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
		log.LogAttrs(ctx, slog.LevelError, "audit a command that did not commit",
			slog.String("command", r.Command), slog.String("request_id", r.CorrelationID), slog.String("error", err.Error()))
	}
}

// notePrincipal notes in the request's trail, if any, who makes it: the subject and roles of the token.
func notePrincipal(ctx context.Context, p access.Principal) {
	if trail, err := auditTrailFrom(ctx); err == nil {
		trail.base.Actor = p.Subject
		if len(p.Roles) > 0 {
			trail.base.Role = strings.Join(p.Roles, ",")
		}
	}
}

// noteTargets notes in the request's trail, if any, the IDs that the command's path names: its record
// carries them even if it is rejected.
func noteTargets(ctx context.Context, targets []string) {
	if trail, err := auditTrailFrom(ctx); err == nil {
		trail.base.Targets = targets
	}
}

// noteRejection notes in the request's trail, if any, the code of the problem it is answered with.
func noteRejection(ctx context.Context, code string) {
	if trail, err := auditTrailFrom(ctx); err == nil {
		trail.rejected = code
	}
}

// clientIP is the IP of the connection. It is not taken from X-Forwarded-For: anyone can set it (Q-85).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
