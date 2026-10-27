// Package audit es el registro de auditoría de comandos (FR-40, ADR-0012): quién pidió qué, desde dónde y
// qué resultó, guardado en la misma transacción que el efecto del comando.
package audit

import (
	"context"
	"errors"
	"fmt"

	pg "github.com/DrewGGM/aluna/internal/audit/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/postgres"
)

// ErrPersonalData indica un Before o un After con una clave de datos personales: el registro guarda un
// resumen del cambio, nunca datos de las personas (Constitución VII).
var ErrPersonalData = errors.New("el registro de auditoría no guarda datos personales")

// personalKeys son las claves que marcan datos personales en un Before o un After, en cualquier nivel.
// Se comparan tal cual: los resúmenes vienen del JSON de la API, en snake_case.
var personalKeys = map[string]bool{"name": true, "document_number": true, "email": true, "phone": true}

// Record es un registro de auditoría. Command es la operación (la ruta de la API), Targets los IDs que
// tocó, Result cómo terminó y Before/After el resumen del estado.
type Record struct {
	Actor, Role, Channel, IP string
	CorrelationID            string
	Command                  string
	Targets                  []string
	Result                   string
	Before, After            map[string]any
}

// Service guarda los registros de auditoría.
type Service struct{}

// New crea el servicio de auditoría.
func New() *Service { return &Service{} }

// Record guarda r dentro de tx, la transacción del comando: si el comando se revierte, su registro también.
// Un Before o un After con datos personales es ErrPersonalData y no se guarda.
func (s *Service) Record(ctx context.Context, tx postgres.Tx, r Record) error {
	if err := rejectPersonalData("before", r.Before); err != nil {
		return err
	}
	if err := rejectPersonalData("after", r.After); err != nil {
		return err
	}
	return pg.Insert(ctx, tx, pg.Row{
		Actor: r.Actor, Role: r.Role, Channel: r.Channel, IP: r.IP, CorrelationID: r.CorrelationID, Command: r.Command,
		Targets: r.Targets, Result: r.Result, Before: r.Before, After: r.After,
	})
}

// rejectPersonalData es ErrPersonalData si v (lo que sale de decodificar JSON: mapas, listas y valores)
// tiene en algún nivel una clave de datos personales; path dice dónde, para el error.
func rejectPersonalData(path string, v any) error {
	switch v := v.(type) {
	case map[string]any:
		for k, inner := range v {
			if personalKeys[k] {
				return fmt.Errorf("%w: %s.%s", ErrPersonalData, path, k)
			}
			if err := rejectPersonalData(path+"."+k, inner); err != nil {
				return err
			}
		}
	case []any:
		for i, inner := range v {
			if err := rejectPersonalData(fmt.Sprintf("%s[%d]", path, i), inner); err != nil {
				return err
			}
		}
	}
	return nil
}
