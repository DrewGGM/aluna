package app

// Poster recibe un tipo abierto.
type Poster interface {
	Post(payload any) error
}

// First es genérico: any como restricción no es un tipo abierto.
func First[T any](items []T) T { return items[0] }

// LedgerManager nombra la forma; la caché sin tipo no es AS-5 fuera del dominio.
type LedgerManager struct{}

var cache map[string]any
