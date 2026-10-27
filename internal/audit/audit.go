// Package audit es el registro de auditoría de comandos (FR-40, ADR-0012): quién pidió qué, desde dónde y
// qué resultó, guardado en la misma transacción que el efecto del comando.
package audit

import (
	"context"
	"errors"
	"fmt"

	"github.com/DrewGGM/aluna/internal/audit/internal/adapters/anchorfs"
	pg "github.com/DrewGGM/aluna/internal/audit/internal/adapters/postgres"
	"github.com/DrewGGM/aluna/internal/audit/internal/app"
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

// AnchorStore guarda fuera de la base el hash de cada bloque sellado (ADR-0012): WORM en producción, un
// directorio en desarrollo (anchorfs).
type AnchorStore = app.AnchorStore

// ErrChainBroken dice en qué bloque deja de cuadrar la cadena de auditoría.
type ErrChainBroken = app.ErrChainBroken

// ErrNoBlocks indica un rango de Verify sin bloques sellados: no hay nada que verificar.
var ErrNoBlocks = app.ErrNoBlocks

var errNoAnchor = errors.New("el servicio de auditoría no tiene ancla: solo registra")

// Service guarda los registros de auditoría y, con un ancla, los sella y verifica.
type Service struct {
	anchor AnchorStore
}

// New crea el servicio que solo registra (la api): Seal y Verify fallan sin ancla.
func New() *Service { return &Service{} }

// NewSealing crea el servicio que además sella y verifica contra anchor.
func NewSealing(anchor AnchorStore) *Service { return &Service{anchor: anchor} }

// WithFileAnchor es NewSealing con el ancla en el directorio dir, que debe existir; en producción el
// ancla es WORM (Q-6).
func WithFileAnchor(dir string) (*Service, error) {
	anchor, err := anchorfs.New(dir)
	if err != nil {
		return nil, err
	}
	return NewSealing(anchor), nil
}

// Seal sella en un bloque, en su propia transacción, hasta maxRecords registros confirmados sin bloque, y
// ancla su hash después del commit (ADR-0012): un ancla siempre es de un bloque confirmado. Si anclar
// falla, el bloque queda sin ancla y el próximo Seal la completa antes de sellar otro. Devuelve el id del
// bloque, o 0 si no había nada que sellar.
func (s *Service) Seal(ctx context.Context, uow *postgres.UnitOfWork, maxRecords int) (int64, error) {
	if s.anchor == nil {
		return 0, errNoAnchor
	}
	var sealed app.Block
	err := uow.Do(ctx, func(tx postgres.Tx) error {
		var err error
		sealed, err = app.Seal(ctx, pg.NewStore(tx), s.anchor, maxRecords)
		return err
	})
	if err != nil || sealed.ID == 0 {
		return 0, err
	}
	if err := s.anchor.Put(ctx, sealed.ID, sealed.Hash); err != nil {
		return 0, err
	}
	return sealed.ID, nil
}

// Verify recalcula la cadena de los bloques from a to y la compara con sus anclas: un registro alterado,
// insertado o borrado, o un bloque cambiado o borrado dentro del rango, es ErrChainBroken con el primer
// bloque que no cuadra; un rango sin bloques es ErrNoBlocks. q puede ser el pool o una Tx.
func (s *Service) Verify(ctx context.Context, q postgres.Querier, from, to int64) error {
	if s.anchor == nil {
		return errNoAnchor
	}
	return app.Verify(ctx, pg.NewStore(q), s.anchor, from, to)
}

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
