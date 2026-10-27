// Package anchorfs es el ancla de los bloques de auditoría en un directorio, para desarrollo y pruebas
// (ADR-0012): un archivo de solo lectura por bloque con su hash en hexadecimal, que no se sobrescribe. En
// producción el ancla es un almacenamiento WORM (Q-6).
package anchorfs

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/DrewGGM/aluna/internal/audit/internal/app"
)

var (
	// ErrAnchored indica que el bloque ya tiene otro hash anclado: el ancla no se reescribe.
	ErrAnchored = errors.New("el bloque ya tiene otro hash anclado")
	// errNotADir indica una ruta de ancla que no es un directorio existente.
	errNotADir = errors.New("el ancla de auditoría no es un directorio existente")
)

// Dir es el ancla en un directorio.
type Dir struct{ path string }

// New abre el ancla en path, que debe existir: un nombre mal escrito no crea un ancla vacía.
func New(path string) (*Dir, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errNotADir, path, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s", errNotADir, path)
	}
	return &Dir{path: path}, nil
}

func (d *Dir) file(blockID int64) string {
	return filepath.Join(d.path, fmt.Sprintf("%020d.sha256", blockID))
}

// Put ancla hash para blockID. Escribe un temporal, lo baja a disco y lo enlaza con el nombre del bloque:
// el enlace falla si ya existe, así que un ancla nunca se reescribe ni queda a medio escribir. Repetirlo
// con el mismo hash no hace nada (un sellado reintentado); con otro es ErrAnchored.
func (d *Dir) Put(ctx context.Context, blockID int64, hash [32]byte) error {
	tmp, err := d.written(hash)
	if err != nil {
		return fmt.Errorf("anclar el bloque %d: %w", blockID, err)
	}
	defer os.Remove(tmp) //nolint:errcheck // un temporal que quede no es un ancla: nadie lo lee
	final := d.file(blockID)
	err = os.Link(tmp, final)
	if errors.Is(err, fs.ErrExist) {
		return d.checkAnchored(ctx, blockID, hash)
	}
	if err != nil {
		return fmt.Errorf("anclar el bloque %d: %w", blockID, err)
	}
	if err := os.Remove(tmp); err != nil { // antes del chmod: en Windows un archivo de solo lectura no se borra
		return fmt.Errorf("anclar el bloque %d: %w", blockID, err)
	}
	if err := os.Chmod(final, 0o400); err != nil {
		return fmt.Errorf("anclar el bloque %d: %w", blockID, err)
	}
	return nil
}

// written es un temporal en el directorio con el hash en hex, ya en disco.
func (d *Dir) written(hash [32]byte) (string, error) {
	f, err := os.CreateTemp(d.path, "ancla-*.tmp")
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(hex.EncodeToString(hash[:]))
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", errors.Join(err, os.Remove(f.Name()))
	}
	return f.Name(), nil
}

// checkAnchored es nil si blockID ya tiene anclado hash y ErrAnchored si tiene otro.
func (d *Dir) checkAnchored(ctx context.Context, blockID int64, hash [32]byte) error {
	anchored, err := d.Get(ctx, blockID)
	if err != nil {
		return err
	}
	if anchored != hash {
		return fmt.Errorf("%w: %d", ErrAnchored, blockID)
	}
	return nil
}

// Get es el hash anclado para blockID; sin archivo es app.ErrNotAnchored.
func (d *Dir) Get(_ context.Context, blockID int64) ([32]byte, error) {
	b, err := os.ReadFile(d.file(blockID))
	if errors.Is(err, fs.ErrNotExist) {
		return [32]byte{}, fmt.Errorf("%w: %d", app.ErrNotAnchored, blockID)
	}
	if err != nil {
		return [32]byte{}, fmt.Errorf("leer el ancla del bloque %d: %w", blockID, err)
	}
	var hash [32]byte
	text := strings.TrimSpace(string(b))
	if len(text) != hex.EncodedLen(len(hash)) {
		return [32]byte{}, fmt.Errorf("ancla del bloque %d ilegible: %d caracteres", blockID, len(text))
	}
	if _, err := hex.Decode(hash[:], []byte(text)); err != nil {
		return [32]byte{}, fmt.Errorf("ancla del bloque %d ilegible: %w", blockID, err)
	}
	return hash, nil
}
