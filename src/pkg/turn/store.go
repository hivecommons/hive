package turn

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const envelopeFileMode = 0o660

// FileStore durably stores one JSON envelope per session ID.
type FileStore struct {
	Dir string
}

// Persist writes env using the same scrub-on-persist serialization as ToJSON.
func (s FileStore) Persist(ctx context.Context, env SessionEnvelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.pathFor(env.SessionID)
	if err != nil {
		return err
	}
	data, err := env.ToPrettyJSON()
	if err != nil {
		return fmt.Errorf("marshaling turn envelope: %w", err)
	}
	return writeAtomic(path, data)
}

// Load reads a persisted envelope by session ID.
func (s FileStore) Load(ctx context.Context, sessionID string) (SessionEnvelope, error) {
	if err := ctx.Err(); err != nil {
		return SessionEnvelope{}, err
	}
	path, err := s.pathFor(sessionID)
	if err != nil {
		return SessionEnvelope{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionEnvelope{}, fmt.Errorf("reading turn envelope %s: %w", path, err)
	}
	env, err := ParseEnvelope(data)
	if err != nil {
		return SessionEnvelope{}, fmt.Errorf("parsing turn envelope %s: %w", path, err)
	}
	return env, nil
}

func (s FileStore) pathFor(sessionID string) (string, error) {
	if strings.TrimSpace(s.Dir) == "" {
		return "", fmt.Errorf("turn: FileStore.Dir is required")
	}
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("turn: session_id is required")
	}
	return filepath.Join(s.Dir, safeEnvelopeName(sessionID)+".json"), nil
}

func safeEnvelopeName(sessionID string) string {
	var b strings.Builder
	for _, r := range sessionID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o770); err != nil {
		return fmt.Errorf("creating turn envelope directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating tmp turn envelope: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing tmp turn envelope: %w", err)
	}
	if err := tmp.Chmod(envelopeFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod tmp turn envelope: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing tmp turn envelope: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing tmp turn envelope: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming tmp turn envelope: %w", err)
	}
	cleanup = false
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
