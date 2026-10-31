package domain

// AccountInfo names the shape and holds untyped dictionaries.
type AccountInfo struct {
	Attributes map[string]any
	Legacy     map[string]interface{}
}

// Describe takes an open type.
func Describe(v interface{}) string { return "" }

// Payload is an alias of an open type.
type Payload = any

// Apply takes a function with an open type: it is reported only once.
func Apply(fn func(any)) {}
