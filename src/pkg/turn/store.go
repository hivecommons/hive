package turn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// FileStore persists one envelope with an atomic temp-file + rename commit.
// The containing directory must already exist; callers own its lifecycle and
// access-control policy.
type FileStore struct {
	Path string
}

func (s FileStore) Persist(ctx context.Context, env SessionEnvelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Path == "" {
		return fmt.Errorf("turn: persistence path is required")
	}
	data, err := env.ToJSON()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	tmp, err := os.CreateTemp(dir, ".turn-envelope-*.tmp")
	if err != nil {
		return fmt.Errorf("turn: create temporary envelope: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("turn: protect temporary envelope: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("turn: write temporary envelope: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("turn: sync temporary envelope: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("turn: close temporary envelope: %w", err)
	}
	if err := os.Rename(tmpPath, s.Path); err != nil {
		return fmt.Errorf("turn: commit envelope: %w", err)
	}
	keep = true
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("turn: open envelope directory: %w", err)
	}
	defer func() { _ = directory.Close() }() // read-only fd opened only to fsync the dir entry; nothing to lose on close error
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("turn: sync envelope directory: %w", err)
	}
	return nil
}

func (s FileStore) Load() (SessionEnvelope, error) {
	if s.Path == "" {
		return SessionEnvelope{}, fmt.Errorf("turn: persistence path is required")
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return SessionEnvelope{}, fmt.Errorf("turn: read envelope: %w", err)
	}
	return ParseEnvelope(data)
}
