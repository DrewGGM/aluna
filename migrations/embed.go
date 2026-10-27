// Package migrations embebe las migraciones SQL para que el binario las aplique con aluna migrate.
// Cada módulo agrega las suyas aquí, numeradas en orden.
package migrations

import "embed"

// FS contiene las migraciones en el formato de goose (-- +goose Up / -- +goose Down).
//
//go:embed *.sql
var FS embed.FS
