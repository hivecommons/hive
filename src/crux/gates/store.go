package gates

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Journal operations.
const (
	OpOpened   = "opened"
	OpAnswered = "answered"
	OpExpired  = "expired"
)

// JournalEntry is one line of gates.jsonl: who did what to which gate. It
// never carries the subject, context or note, so the journal can be
// shipped or tailed without leaking what the gate was about.
type JournalEntry struct {
	At        time.Time `json:"at"`
	Op        string    `json:"op"`
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	By        string    `json:"by,omitempty"`
	Answer    string    `json:"answer,omitempty"`
	Override  bool      `json:"operator_override,omitempty"`
	ExpiresAt string    `json:"expires_at,omitempty"`
}

// store is the record layout under one gates directory.
type store struct{ dir string }

func (st *store) pendingDir() string  { return filepath.Join(st.dir, PendingDirName) }
func (st *store) resolvedDir() string { return filepath.Join(st.dir, ResolvedDirName) }

func (st *store) pendingPath(id string) string {
	return filepath.Join(st.pendingDir(), id+".json")
}

func (st *store) resolvedPath(id string) string {
	return filepath.Join(st.resolvedDir(), id+".json")
}

func (st *store) init() error {
	for _, d := range []string{st.dir, st.pendingDir(), st.resolvedDir()} {
		if err := os.MkdirAll(d, dirMode); err != nil {
			return fmt.Errorf("gates: %w", err)
		}
	}
	return nil
}

// put writes rec where its status says it belongs: pending while waiting,
// resolved otherwise. Resolving writes the resolved record first and only
// then removes the pending one, so a crash in between leaves both and
// load prefers the resolved copy; it never loses an answer.
func (st *store) put(rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("gates: %w", err)
	}
	path := st.pendingPath(rec.ID)
	if !rec.Waiting() {
		path = st.resolvedPath(rec.ID)
	}
	if err := writeAtomic(path, data); err != nil {
		return err
	}
	if !rec.Waiting() {
		if err := os.Remove(st.pendingPath(rec.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("gates: %w", err)
		}
	}
	return nil
}

// get reads id, resolved first.
func (st *store) get(id string) (Record, error) {
	if !ValidID(id) {
		return Record{}, fmt.Errorf("%w: %q is not a gate id", ErrNotFound, id)
	}
	for _, path := range []string{st.resolvedPath(id), st.pendingPath(id)} {
		rec, err := readRecord(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return rec, err
	}
	return Record{}, fmt.Errorf("%w: gate %s", ErrNotFound, id)
}

// list returns every record on disk, a resolved copy shadowing a stale
// pending one. Order is unspecified; Service sorts.
func (st *store) list() ([]Record, error) {
	byID := map[string]Record{}
	for _, dir := range []string{st.pendingDir(), st.resolvedDir()} {
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("gates: %w", err)
		}
		for _, e := range entries {
			id, ok := strings.CutSuffix(e.Name(), ".json")
			if !ok || e.IsDir() || !ValidID(id) {
				continue
			}
			rec, err := readRecord(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			byID[id] = rec
		}
	}
	out := make([]Record, 0, len(byID))
	for _, rec := range byID {
		out = append(out, rec)
	}
	return out, nil
}

// dropStalePending removes a pending copy left by a crash between the two
// writes of a resolution.
func (st *store) dropStalePending(id string) error {
	if _, err := os.Stat(st.resolvedPath(id)); err != nil {
		return nil
	}
	if err := os.Remove(st.pendingPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("gates: %w", err)
	}
	return nil
}

func (st *store) appendJournal(entry JournalEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("gates: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(st.dir, JournalName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("gates: %w", err)
	}
	_, werr := f.Write(append(data, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("gates: %w", werr)
	}
	return nil
}

func readRecord(path string) (Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, err
		}
		return Record{}, fmt.Errorf("gates: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("gates: %s: corrupt record: %w", filepath.Base(path), err)
	}
	return rec, nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, fileMode); err != nil {
		return fmt.Errorf("gates: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("gates: %w", err)
	}
	return nil
}
