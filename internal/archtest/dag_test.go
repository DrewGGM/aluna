// Package archtest verifica con pruebas las reglas de arquitectura: el DAG de módulos y el catálogo
// anti-slop. Solo tiene archivos de prueba: nada en producción lo importa.
package archtest

const modulePath = "github.com/DrewGGM/aluna"

// moduleDeps es el DAG permitido entre bounded contexts: cada módulo lista los módulos que puede
// importar directamente. Una arista nueva se agrega aquí y en el diagrama de la arquitectura en el
// mismo commit.
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

// foundation son paquetes de infraestructura sin lógica de negocio: cualquiera los importa y ellos
// no importan módulos ni adaptadores de entrada.
var foundation = []string{
	"money", "ids", "clock", "postgres", "idempotency", "outbox", "telemetry", "config",
}

// kernel es la parte de foundation que el dominio puede importar (enmienda de ADR-0001).
var kernel = []string{"money", "ids", "clock"}

// entrypoints son adaptadores de entrada que componen módulos; nadie los importa.
var entrypoints = []string{"httpapi", "jobs"}
