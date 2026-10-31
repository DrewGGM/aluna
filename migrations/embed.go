// Package migrations embeds the SQL migrations so the binary applies them with aluna migrate. Every module
// adds its own here, numbered in order.
package migrations

import "embed"

// FS holds the migrations in goose format (-- +goose Up / -- +goose Down).
//
//go:embed *.sql
var FS embed.FS
