// Package anchorfs anchors the audit blocks in a directory, for development and testing (ADR-0012): one
// read-only file per block with its hash in hex, never overwritten. In production the anchor is WORM
// storage (Q-6).
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
	// ErrAnchored means the block already has a different anchored hash: the anchor is not rewritten.
	ErrAnchored = errors.New("the block already has a different anchored hash")
	// errNotADir means an anchor path that is not an existing directory.
	errNotADir = errors.New("the audit anchor is not an existing directory")
)

// Dir is the anchor in a directory.
type Dir struct{ path string }

// New opens the anchor at path, which must exist: a misspelled name does not create an empty anchor.
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

// Put anchors hash for blockID. It writes a temporary file, syncs it to disk and links it under the block's
// name: the link fails if it already exists, so an anchor is never rewritten or left half written.
// Repeating it with the same hash does nothing (a retried seal); with a different one it is ErrAnchored.
func (d *Dir) Put(ctx context.Context, blockID int64, hash [32]byte) error {
	tmp, err := d.written(hash)
	if err != nil {
		return fmt.Errorf("anchor block %d: %w", blockID, err)
	}
	defer os.Remove(tmp) //nolint:errcheck // a leftover temporary file is not an anchor: nobody reads it
	final := d.file(blockID)
	err = os.Link(tmp, final)
	if errors.Is(err, fs.ErrExist) {
		return d.checkAnchored(ctx, blockID, hash)
	}
	if err != nil {
		return fmt.Errorf("anchor block %d: %w", blockID, err)
	}
	if err := os.Remove(tmp); err != nil { // before the chmod: on Windows a read-only file cannot be deleted
		return fmt.Errorf("anchor block %d: %w", blockID, err)
	}
	if err := os.Chmod(final, 0o400); err != nil {
		return fmt.Errorf("anchor block %d: %w", blockID, err)
	}
	return nil
}

// written is a temporary file in the directory with the hash in hex, already on disk.
func (d *Dir) written(hash [32]byte) (string, error) {
	f, err := os.CreateTemp(d.path, "anchor-*.tmp")
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

// checkAnchored is nil if blockID already has hash anchored and ErrAnchored if it has a different one.
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

// Get is the hash anchored for blockID; with no file it is app.ErrNotAnchored.
func (d *Dir) Get(_ context.Context, blockID int64) ([32]byte, error) {
	b, err := os.ReadFile(d.file(blockID))
	if errors.Is(err, fs.ErrNotExist) {
		return [32]byte{}, fmt.Errorf("%w: %d", app.ErrNotAnchored, blockID)
	}
	if err != nil {
		return [32]byte{}, fmt.Errorf("read anchor of block %d: %w", blockID, err)
	}
	var hash [32]byte
	text := strings.TrimSpace(string(b))
	if len(text) != hex.EncodedLen(len(hash)) {
		return [32]byte{}, fmt.Errorf("unreadable anchor of block %d: %d characters", blockID, len(text))
	}
	if _, err := hex.Decode(hash[:], []byte(text)); err != nil {
		return [32]byte{}, fmt.Errorf("unreadable anchor of block %d: %w", blockID, err)
	}
	return hash, nil
}
