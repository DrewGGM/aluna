// Package archtest enforces the architecture rules with tests: the module DAG and the anti-slop catalog.
// It only has test files: nothing in production imports it.
package archtest

const modulePath = "github.com/DrewGGM/aluna"

// moduleDeps is the allowed DAG between bounded contexts: each module lists the modules it may import
// directly. A new edge is added here and to the architecture diagram in the same commit.
var moduleDeps = map[string][]string{
	"access":         nil,
	"accounting":     {"ledger", "calendar"},
	"approvals":      {"access"},
	"audit":          nil,
	"calendar":       nil,
	"compliance":     {"party"},
	"deposits":       {"ledger", "products", "party", "pricing"},
	"ledger":         {"calendar"},
	"party":          nil,
	"payments":       {"ledger", "deposits", "compliance", "pricing"},
	"pricing":        {"products"},
	"products":       {"approvals", "calendar"},
	"reconciliation": {"ledger", "payments"},
	"webhooks":       nil,
}

// foundation lists infrastructure packages without business logic: anyone may import them and they
// import neither modules nor inbound adapters.
var foundation = []string{
	"money", "ids", "clock", "postgres", "idempotency", "outbox", "telemetry", "config",
}

// kernel is the part of foundation the domain may import (ADR-0001 amendment).
var kernel = []string{"money", "ids", "clock"}

// entrypoints are inbound adapters that compose modules; nobody imports them.
var entrypoints = []string{"httpapi", "jobs"}
