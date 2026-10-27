// Package app es el sellado del registro de auditoría (ADR-0012): bloques encadenados por hash y anclados
// fuera de la base, y la verificación de esa cadena.
package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Record es un registro de auditoría tal como se sella. Before y After son el JSON que guardó la base,
// nil si no hay.
type Record struct {
	Seq                      int64
	RecordedAt               time.Time
	Actor, Role, Channel, IP string
	CorrelationID, Command   string
	Targets                  []string
	Result                   string
	Before, After            json.RawMessage
}

// Block es un bloque sellado: su hash encadena el del anterior (PrevHash) con sus registros. El valor
// cero es "no hay bloque": su hash, ceros, es el PrevHash del primero.
type Block struct {
	ID             int64
	PrevHash, Hash [32]byte
	Records        int
}

var (
	// ErrNotAnchored indica un bloque sin hash anclado.
	ErrNotAnchored = errors.New("el bloque no tiene ancla")
	// ErrNoBlocks indica un rango sin bloques sellados ni anclados: no hay nada que verificar.
	ErrNoBlocks = errors.New("no hay bloques sellados en el rango")
)

// AnchorStore guarda fuera de la base el hash de cada bloque (WORM en producción): lo que no se puede
// reescribir desde la base contra lo que se compara la cadena. Get de un bloque sin ancla es
// ErrNotAnchored; Put del mismo hash otra vez no hace nada.
type AnchorStore interface {
	Put(ctx context.Context, blockID int64, hash [32]byte) error
	Get(ctx context.Context, blockID int64) ([32]byte, error)
}

// SealStore es la persistencia del sellado, dentro de una transacción.
type SealStore interface {
	// Unsealed bloquea (FOR UPDATE) hasta limit registros confirmados sin bloque, en orden de seq.
	Unsealed(ctx context.Context, limit int) ([]Record, error)
	// LastBlock es el último bloque, o Block{} si todavía no hay.
	LastBlock(ctx context.Context) (Block, error)
	// InsertBlock guarda un bloque; otro con el mismo prevHash ya sellado es un error (la cadena no se
	// bifurca: el sellador que pierde la carrera se revierte y sigue en la próxima vuelta).
	InsertBlock(ctx context.Context, prevHash, hash [32]byte, records int) (int64, error)
	AssignBlock(ctx context.Context, blockID int64, seqs []int64) error
}

// VerifyStore es la lectura de la cadena para verificarla.
type VerifyStore interface {
	// LastBlock es el último bloque, o Block{} si todavía no hay.
	LastBlock(ctx context.Context) (Block, error)
	// Blocks son los bloques con id entre from y to, en orden.
	Blocks(ctx context.Context, from, to int64) ([]Block, error)
	// BlockBefore es el bloque anterior a id, o Block{} si id es el primero.
	BlockBefore(ctx context.Context, id int64) (Block, error)
	// RecordsOf son los registros del bloque, en orden de seq.
	RecordsOf(ctx context.Context, blockID int64) ([]Record, error)
}

// ErrChainBroken dice en qué bloque deja de cuadrar la cadena: un registro alterado, insertado o borrado,
// un bloque cambiado o borrado, o un ancla que no coincide o que falta.
type ErrChainBroken struct {
	BlockID int64
	Reason  string
}

func (e ErrChainBroken) Error() string {
	return fmt.Sprintf("la cadena de auditoría se rompe en el bloque %d: %s", e.BlockID, e.Reason)
}

// Seal sella en un bloque, dentro de la transacción de store, hasta maxRecords registros sin bloque: hash =
// SHA-256(hash del bloque anterior ‖ registros por seq). Antes ancla el último bloque que ya había (reanclar
// el mismo hash no hace nada): si su ancla falló, se completa aquí, y si vuelve a fallar no se sella nada
// encima, así que solo el último bloque puede quedar sin ancla. Devuelve el bloque nuevo, o Block{} si no
// había registros que sellar; quien llama lo ancla después de confirmar (ver audit.Service.Seal).
func Seal(ctx context.Context, store SealStore, anchor AnchorStore, maxRecords int) (Block, error) {
	records, err := store.Unsealed(ctx, maxRecords)
	if err != nil {
		return Block{}, err
	}
	last, err := store.LastBlock(ctx)
	if err != nil {
		return Block{}, err
	}
	if last.ID != 0 {
		if err := anchor.Put(ctx, last.ID, last.Hash); err != nil {
			return Block{}, err
		}
	}
	if len(records) == 0 {
		return Block{}, nil
	}
	sealed := Block{PrevHash: last.Hash, Records: len(records)}
	if sealed.Hash, err = blockHash(last.Hash, records); err != nil {
		return Block{}, err
	}
	if sealed.ID, err = store.InsertBlock(ctx, sealed.PrevHash, sealed.Hash, sealed.Records); err != nil {
		return Block{}, err
	}
	seqs := make([]int64, len(records))
	for i, r := range records {
		seqs[i] = r.Seq
	}
	return sealed, store.AssignBlock(ctx, sealed.ID, seqs)
}

// Verify recorre los ids de from a to (hasta el último bloque de la base) en orden: cada bloque debe empezar
// en el hash del anterior (también el anterior a from), dar su hash con sus registros y coincidir con su
// ancla, y un id sin bloque no debe tener ancla (un bloque borrado). El primero que no cumple es
// ErrChainBroken; un rango sin nada sellado es ErrNoBlocks. Los bloques borrados después del último de la
// base no se ven (Q-88).
func Verify(ctx context.Context, store VerifyStore, anchor AnchorStore, from, to int64) error {
	last, err := store.LastBlock(ctx)
	if err != nil {
		return err
	}
	to = min(to, last.ID) // con from > to no hay ids que revisar: ErrNoBlocks
	blocks, err := store.Blocks(ctx, from, to)
	if err != nil {
		return err
	}
	prev, err := store.BlockBefore(ctx, from)
	if err != nil {
		return err
	}
	if err := walk(ctx, store, anchor, prev, blocks, from, to); err != nil {
		return err
	}
	if len(blocks) == 0 {
		return ErrNoBlocks
	}
	return nil
}

// walk recorre los ids de from a to en orden: el que tiene bloque (blocks, en orden) se verifica contra el
// hash del anterior, empezando por prev; el que no, no debe tener ancla.
func walk(ctx context.Context, store VerifyStore, anchor AnchorStore, prev Block, blocks []Block, from, to int64) error {
	for id := from; id <= to; id++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(blocks) == 0 || blocks[0].ID != id {
			if err := notAnchored(ctx, anchor, id); err != nil {
				return err
			}
			continue
		}
		if err := verifyBlock(ctx, store, anchor, prev.Hash, blocks[0]); err != nil {
			return err
		}
		prev, blocks = blocks[0], blocks[1:]
	}
	return nil
}

// notAnchored es nil si el id no tiene ancla: un sellado que no confirmó. Con ancla, el bloque se borró.
func notAnchored(ctx context.Context, anchor AnchorStore, id int64) error {
	_, err := anchor.Get(ctx, id)
	switch {
	case err == nil:
		return ErrChainBroken{BlockID: id, Reason: "tiene ancla pero ya no está en la base"}
	case errors.Is(err, ErrNotAnchored):
		return nil
	default:
		return err
	}
}

func verifyBlock(ctx context.Context, store VerifyStore, anchor AnchorStore, prevHash [32]byte, b Block) error {
	if b.PrevHash != prevHash {
		return ErrChainBroken{BlockID: b.ID, Reason: "no empieza en el hash del bloque anterior"}
	}
	records, err := store.RecordsOf(ctx, b.ID)
	if err != nil {
		return err
	}
	hash, err := blockHash(b.PrevHash, records)
	if err != nil {
		return err
	}
	if len(records) != b.Records || hash != b.Hash {
		return ErrChainBroken{BlockID: b.ID, Reason: "sus registros no dan su hash"}
	}
	anchored, err := anchor.Get(ctx, b.ID)
	if errors.Is(err, ErrNotAnchored) {
		return ErrChainBroken{BlockID: b.ID, Reason: "no tiene ancla"}
	}
	if err != nil {
		return err
	}
	if anchored != b.Hash {
		return ErrChainBroken{BlockID: b.ID, Reason: "su hash no es el anclado"}
	}
	return nil
}

// canonicalRecord fija el orden y la forma de cada campo en el hash: cambiar uno cambia el hash de todos
// los bloques sellados con él.
type canonicalRecord struct {
	Seq           int64           `json:"seq"`
	RecordedAt    string          `json:"recorded_at"`
	Actor         string          `json:"actor"`
	Role          string          `json:"role"`
	Channel       string          `json:"channel"`
	IP            string          `json:"ip"`
	CorrelationID string          `json:"correlation_id"`
	Command       string          `json:"command"`
	Targets       []string        `json:"targets"`
	Result        string          `json:"result"`
	Before        json.RawMessage `json:"before"`
	After         json.RawMessage `json:"after"`
}

// blockHash es SHA-256(prev ‖ un JSON por registro terminado en '\n'): JSON compacto, sin escapar <, > ni &
// (así lo puede recalcular cualquier lenguaje), recorded_at en UTC con microsegundos (la precisión de la
// base) y before/after como los devuelve jsonb, compactados.
func blockHash(prev [32]byte, records []Record) ([32]byte, error) {
	var buf bytes.Buffer
	buf.Write(prev[:])
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, r := range records {
		err := enc.Encode(canonicalRecord{
			Seq: r.Seq, RecordedAt: r.RecordedAt.UTC().Format("2006-01-02T15:04:05.000000Z"), Actor: r.Actor, Role: r.Role,
			Channel: r.Channel, IP: r.IP, CorrelationID: r.CorrelationID, Command: r.Command, Targets: r.Targets,
			Result: r.Result, Before: r.Before, After: r.After,
		})
		if err != nil {
			return [32]byte{}, fmt.Errorf("serializar el registro %d: %w", r.Seq, err)
		}
	}
	return sha256.Sum256(buf.Bytes()), nil
}
