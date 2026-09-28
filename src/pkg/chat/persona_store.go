package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/persona"
)

// personaStoreFileVersion is the on-disk schema version of FilePersonaStore.
const personaStoreFileVersion = 1

// personaStoreFileMode keeps persona notes (free text a user typed) readable
// by the hive process only.
const personaStoreFileMode os.FileMode = 0o600

// personaStoreFile is the JSON document FilePersonaStore persists. Personas
// are grouped by transport because chat author IDs are transport-native (a
// Slack user ID, a Discord snowflake, a Matrix MXID) and nothing maps one
// transport's ID to another's, so two transports could otherwise collide on
// the same opaque ID.
type personaStoreFile struct {
	Version  int                                  `json:"version"`
	Personas map[string]map[string]persona.Record `json:"personas"`
}

// FilePersonaStore is the durable persona store shared by every chat transport
// of one hive (hivecommons/hive#9175). It keeps the records in memory and
// rewrites the whole file atomically on every change, so a persona, its
// learning signals, pending suggestions, and adjustment history survive a
// restart. Use ForTransport to get the PersonaStore a transport's chat
// service reads and writes.
type FilePersonaStore struct {
	path string

	mu       sync.Mutex
	personas map[string]map[string]persona.Record
}

// OpenFilePersonaStore loads the store at path. A missing file is an empty
// store; an unreadable or malformed file is an error and is left untouched,
// so a bad file is never silently replaced by an empty one.
func OpenFilePersonaStore(path string) (*FilePersonaStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("persona store path is empty")
	}
	s := &FilePersonaStore{path: path, personas: map[string]map[string]persona.Record{}}
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed hive state path chosen by the binary, not user input.
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading persona store %s: %w", path, err)
	}
	var doc personaStoreFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing persona store %s: %w", path, err)
	}
	if doc.Version != personaStoreFileVersion {
		return nil, fmt.Errorf("persona store %s has unsupported version %d", path, doc.Version)
	}
	for transport, records := range doc.Personas {
		for author, record := range records {
			s.setLocked(transport, author, record.Normalize())
		}
	}
	return s, nil
}

// ForTransport returns the PersonaStore view for one chat transport. The name
// should be the transport's Backend.Name().
func (s *FilePersonaStore) ForTransport(transport string) PersonaStore {
	return transportPersonaStore{store: s, transport: transport}
}

func (s *FilePersonaStore) get(transport, author string) (persona.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.personas[transport][author]
	if !ok {
		return persona.Record{}, false
	}
	return record.Normalize(), true
}

func (s *FilePersonaStore) put(transport, author string, record persona.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.personas[transport][author]
	s.setLocked(transport, author, record.Normalize())
	if err := s.persistLocked(); err != nil {
		if existed {
			s.personas[transport][author] = previous
		} else {
			delete(s.personas[transport], author)
			if len(s.personas[transport]) == 0 {
				delete(s.personas, transport)
			}
		}
		return err
	}
	return nil
}

func (s *FilePersonaStore) setLocked(transport, author string, record persona.Record) {
	records := s.personas[transport]
	if records == nil {
		records = map[string]persona.Record{}
		s.personas[transport] = records
	}
	records[author] = record
}

func (s *FilePersonaStore) persistLocked() error {
	data, err := json.MarshalIndent(personaStoreFile{Version: personaStoreFileVersion, Personas: s.personas}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding persona store: %w", err)
	}
	return writePersonaStoreAtomic(s.path, data)
}

func writePersonaStoreAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating persona store directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating persona store temp file: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing persona store: %w", err)
	}
	if err := tmp.Chmod(personaStoreFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod persona store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing persona store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing persona store: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replacing persona store: %w", err)
	}
	committed = true
	return nil
}

// transportPersonaStore is one transport's view of a FilePersonaStore.
type transportPersonaStore struct {
	store     *FilePersonaStore
	transport string
}

func (t transportPersonaStore) GetPersona(_ context.Context, author string) (persona.Record, bool, error) {
	record, ok := t.store.get(t.transport, author)
	return record, ok, nil
}

func (t transportPersonaStore) PutPersona(_ context.Context, author string, record persona.Record) error {
	return t.store.put(t.transport, author, record)
}
