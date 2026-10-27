package domain

// AccountInfo nombra la forma y guarda diccionarios sin tipo.
type AccountInfo struct {
	Attributes map[string]any
	Legacy     map[string]interface{}
}

// Describe recibe un tipo abierto.
func Describe(v interface{}) string { return "" }

// Payload es un alias de un tipo abierto.
type Payload = any

// Apply recibe una función con un tipo abierto: se reporta una sola vez.
func Apply(fn func(any)) {}
