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

// TestAnAnchorIsWrittenOnce: the anchored hash reads back the same; anchoring it again with the same hash
// does nothing and with a different one is ErrAnchored, without changing the anchored one.
func TestAnAnchorIsWrittenOnce(t *testing.T) {
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, second := sha256.Sum256([]byte("one")), sha256.Sum256([]byte("two"))

	if err := d.Put(t.Context(), 7, first); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(t.Context(), 7, first); err != nil {
		t.Fatalf("repeating the same anchor = %v", err)
	}
	if err := d.Put(t.Context(), 7, second); !errors.Is(err, ErrAnchored) {
		t.Fatalf("rewriting the anchor = %v, want ErrAnchored", err)
	}

	if got, err := d.Get(t.Context(), 7); err != nil || got != first {
		t.Fatalf("anchor = %x (%v), want %x", got, err, first)
	}
}

// TestAMissingAnchorIsNotAnchored: a block with no anchor does not read as a zero hash.
func TestAMissingAnchorIsNotAnchored(t *testing.T) {
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := d.Get(t.Context(), 1); !errors.Is(err, app.ErrNotAnchored) {
		t.Fatalf("missing anchor = %v, want ErrNotAnchored", err)
	}
}

// TestACorruptAnchorIsAnError: an anchor file with more or fewer than 64 hex characters is an error, not a
// panic or a hash.
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
			t.Errorf("anchor %q = %v, want an unreadable anchor error", content, err)
		}
	}
}

// TestTheAnchorDirectoryMustExist: opening an anchor at a path that does not exist is an error, not a new
// empty directory.
func TestTheAnchorDirectoryMustExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")

	if _, err := New(missing); err == nil {
		t.Fatal("an anchor was opened at a path that does not exist")
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("New created %s", missing)
	}
}

// TestALeftoverTemporaryDoesNotBlock: a temporary file left by an interrupted write is not an anchor and
// does not prevent anchoring the block later.
func TestALeftoverTemporaryDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	d, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "anchor-interrupted.tmp"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("block"))

	if err := d.Put(t.Context(), 3, hash); err != nil {
		t.Fatal(err)
	}

	if got, err := d.Get(t.Context(), 3); err != nil || got != hash {
		t.Fatalf("anchor = %x (%v), want %x", got, err, hash)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("the directory has %d entries (%v), want the anchor and the old temporary file", len(entries), err)
	}
}
