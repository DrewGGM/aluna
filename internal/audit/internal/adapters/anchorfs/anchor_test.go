package anchorfs

import (
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DrewGGM/aluna/internal/audit/internal/app"
)

// TestAnAnchorIsWrittenOnce: el hash anclado se lee igual; anclarlo otra vez con el mismo hash no hace
// nada y con otro es ErrAnchored, sin cambiar el anclado.
func TestAnAnchorIsWrittenOnce(t *testing.T) {
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, second := sha256.Sum256([]byte("uno")), sha256.Sum256([]byte("dos"))

	if err := d.Put(t.Context(), 7, first); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(t.Context(), 7, first); err != nil {
		t.Fatalf("repetir el mismo ancla = %v", err)
	}
	if err := d.Put(t.Context(), 7, second); !errors.Is(err, ErrAnchored) {
		t.Fatalf("reescribir el ancla = %v, se esperaba ErrAnchored", err)
	}

	if got, err := d.Get(t.Context(), 7); err != nil || got != first {
		t.Fatalf("ancla = %x (%v), se esperaba %x", got, err, first)
	}
}

// TestAMissingAnchorIsNotAnchored: un bloque sin ancla no se lee como hash cero.
func TestAMissingAnchorIsNotAnchored(t *testing.T) {
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := d.Get(t.Context(), 1); !errors.Is(err, app.ErrNotAnchored) {
		t.Fatalf("ancla que no existe = %v, se esperaba ErrNotAnchored", err)
	}
}

// TestACorruptAnchorIsAnError: un archivo de ancla con más o menos de 64 caracteres hex es un error, no un
// panic ni un hash.
func TestACorruptAnchorIsAnError(t *testing.T) {
	dir := t.TempDir()
	d, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for id, content := range map[int64]string{1: strings.Repeat("ab", 33), 2: "abcd", 3: strings.Repeat("zz", 32)} {
		if err := os.WriteFile(d.file(id), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := d.Get(t.Context(), id); err == nil || errors.Is(err, app.ErrNotAnchored) {
			t.Errorf("ancla %q = %v, se esperaba un error de ancla ilegible", content, err)
		}
	}
}

// TestTheAnchorDirectoryMustExist: abrir un ancla en una ruta que no existe es un error, no un directorio
// nuevo y vacío.
func TestTheAnchorDirectoryMustExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-existe")

	if _, err := New(missing); err == nil {
		t.Fatal("se abrió un ancla en una ruta que no existe")
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("New creó %s", missing)
	}
}

// TestALeftoverTemporaryDoesNotBlock: un temporal que dejó una escritura cortada no es un ancla ni impide
// anclar el bloque después.
func TestALeftoverTemporaryDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	d, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ancla-cortada.tmp"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("bloque"))

	if err := d.Put(t.Context(), 3, hash); err != nil {
		t.Fatal(err)
	}

	if got, err := d.Get(t.Context(), 3); err != nil || got != hash {
		t.Fatalf("ancla = %x (%v), se esperaba %x", got, err, hash)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("el directorio tiene %d entradas (%v), se esperaban el ancla y el temporal viejo", len(entries), err)
	}
}
