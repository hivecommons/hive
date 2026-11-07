// Package runstate is the saved-run-results store of the Crux work services
// (S7 in hivecommons/crux#114): a stable, caller-visible ID assigned at
// accept time, an explicit status set, one JSON record per item under
// WorkDir()/runs/<id>/record.json and an append-only runs/index.jsonl
// history, mirroring the sandbox-runs conventions in sandbox_run.go.
//
// The store never guesses. A record that was accepted or running when the
// serving process stopped is reconciled to StatusUnfinished on the next
// start (Reconcile); the caller decides whether to re-run. An idempotency
// key supplied by the caller makes a repeated create return the existing
// record instead of a second one.
package runstate

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Statuses. The set is closed: a driver maps these onto its own meaning;
// crux only ever reports what happened to the step.
const (
	StatusAccepted    = "accepted"
	StatusRunning     = "running"
	StatusWaiting     = "waiting"
	StatusCompleted   = "completed"
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
	StatusUnfinished  = "unfinished"
	StatusUnsupported = "unsupported"
)

const (
	// DirName is the per-concern directory under the work dir.
	DirName = "runs"
	// RecordName is the per-item record file inside the item's directory.
	RecordName = "record.json"
	// IndexName is the append-only status history, one IndexEntry per line.
	IndexName = "index.jsonl"
	// KindRun is a contained run (S2); S6 steps add their own kind later.
	KindRun = "run"

	idRandomBytes = 6
	maxIDLen      = 64
	dirMode       = 0o770
	fileMode      = 0o660
)

// ErrNotFound is returned by Get and Update for an unknown ID.
var ErrNotFound = errors.New("runstate: record not found")

// Record is the saved state of one item. Detail carries the owning service's
// payload (for S2 the command result) and is opaque to this package.
type Record struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	Status         string          `json:"status"`
	Owner          string          `json:"owner,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Detail         json.RawMessage `json:"detail,omitempty"`
}

// IndexEntry is one line of the append-only history: every status the item
// passed through, in order.
type IndexEntry struct {
	At     time.Time `json:"at"`
	ID     string    `json:"id"`
	Kind   string    `json:"kind"`
	Status string    `json:"status"`
	Reason string    `json:"reason,omitempty"`
}

// Terminal reports whether status is final: nothing moves out of it.
func Terminal(status string) bool {
	switch status {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusUnfinished, StatusUnsupported:
		return true
	}
	return false
}

// Valid reports whether status is one of the closed set.
func Valid(status string) bool {
	switch status {
	case StatusAccepted, StatusRunning, StatusWaiting:
		return true
	}
	return Terminal(status)
}

// Dir is the runs directory under workDir.
func Dir(workDir string) string { return filepath.Join(workDir, DirName) }

// DeriveIdempotencyKey scopes a caller-supplied key to its owner so two
// callers using the same key cannot read each other's records. The result
// is what Record.IdempotencyKey stores; an empty key derives to "".
func DeriveIdempotencyKey(owner, key string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	h := sha256.New()
	for _, p := range []string{owner, key} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Store is the on-disk record store for one work dir. It is safe for
// concurrent use within one process; a work dir is served by one process.
type Store struct {
	dir string

	mu   sync.Mutex
	keys map[string]string // derived idempotency key -> id
	// Now and NewID are overridable for tests.
	Now   func() time.Time
	NewID func() string
}

// Open loads the store at Dir(workDir), creating the directory and
// rebuilding the idempotency index from the records already on disk.
func Open(workDir string) (*Store, error) {
	s := &Store{dir: Dir(workDir), keys: map[string]string{}, Now: time.Now, NewID: newID}
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return nil, fmt.Errorf("runstate: %w", err)
	}
	recs, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if r.IdempotencyKey != "" {
			s.keys[r.IdempotencyKey] = r.ID
		}
	}
	return s, nil
}

// Path is the directory of one item: its record and the owning service's
// artifacts live there.
func (s *Store) Path(id string) string { return filepath.Join(s.dir, id) }

// Create saves rec as a new item in StatusAccepted and returns it with its
// ID and timestamps filled in. When rec.IdempotencyKey names an existing
// item, that item's current record is returned with created=false and
// nothing is written.
func (s *Store) Create(rec Record) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.IdempotencyKey != "" {
		if id, ok := s.keys[rec.IdempotencyKey]; ok {
			existing, err := s.read(id)
			return existing, false, err
		}
	}
	if rec.Kind == "" {
		rec.Kind = KindRun
	}
	rec.ID = s.NewID()
	rec.Status = StatusAccepted
	rec.CreatedAt = s.Now().UTC()
	rec.StartedAt, rec.FinishedAt = nil, nil
	if err := os.MkdirAll(s.Path(rec.ID), dirMode); err != nil {
		return Record{}, false, fmt.Errorf("runstate: %w", err)
	}
	if err := s.write(rec); err != nil {
		return Record{}, false, err
	}
	if rec.IdempotencyKey != "" {
		s.keys[rec.IdempotencyKey] = rec.ID
	}
	return rec, true, nil
}

// Get returns the saved record whatever the process state.
func (s *Store) Get(id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(id)
}

// Update applies fn to the saved record and writes the result. fn may change
// Status (validated against the closed set), Reason and Detail; a terminal
// record is never moved to another status. Timestamps are maintained here:
// StartedAt on the move to running, FinishedAt on the move to a terminal
// status.
func (s *Store) Update(id string, fn func(*Record) error) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.read(id)
	if err != nil {
		return Record{}, err
	}
	before := rec.Status
	if err := fn(&rec); err != nil {
		return Record{}, err
	}
	rec.ID = id
	if !Valid(rec.Status) {
		return Record{}, fmt.Errorf("runstate: invalid status %q", rec.Status)
	}
	if Terminal(before) && rec.Status != before {
		return Record{}, fmt.Errorf("runstate: %s is already %s", id, before)
	}
	if rec.Status != before {
		now := s.Now().UTC()
		if rec.Status == StatusRunning && rec.StartedAt == nil {
			rec.StartedAt = &now
		}
		if Terminal(rec.Status) && rec.FinishedAt == nil {
			rec.FinishedAt = &now
		}
	}
	if err := s.write(rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// List returns every saved record, oldest first.
func (s *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("runstate: %w", err)
	}
	var out []Record
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, err := s.read(e.Name())
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // a directory with artifacts but no record yet
			}
			return nil, err
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Reconcile moves every accepted or running record to StatusUnfinished with
// reason and returns the records it changed. It runs at `crux serve` start:
// the process that owned those items is gone, so their exit is not knowable
// and they are never restarted silently.
func (s *Store) Reconcile(reason string) ([]Record, error) {
	recs, err := s.List()
	if err != nil {
		return nil, err
	}
	var changed []Record
	for _, r := range recs {
		if r.Status != StatusAccepted && r.Status != StatusRunning {
			continue
		}
		rec, err := s.Update(r.ID, func(rec *Record) error {
			rec.Status = StatusUnfinished
			rec.Reason = reason
			return nil
		})
		if err != nil {
			return changed, err
		}
		changed = append(changed, rec)
	}
	return changed, nil
}

func (s *Store) read(id string) (Record, error) {
	if !validID(id) {
		return Record{}, ErrNotFound
	}
	data, err := os.ReadFile(filepath.Join(s.Path(id), RecordName))
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("runstate: %w", err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("runstate: %s: %w", id, err)
	}
	return rec, nil
}

// write replaces the record atomically and appends the status to the index.
func (s *Store) write(rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("runstate: %w", err)
	}
	path := filepath.Join(s.Path(rec.ID), RecordName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, fileMode); err != nil {
		return fmt.Errorf("runstate: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("runstate: %w", err)
	}
	line, err := json.Marshal(IndexEntry{At: s.Now().UTC(), ID: rec.ID, Kind: rec.Kind, Status: rec.Status, Reason: rec.Reason})
	if err != nil {
		return fmt.Errorf("runstate: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, IndexName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("runstate: %w", err)
	}
	_, werr := f.Write(append(line, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("runstate: %w", werr)
	}
	return nil
}

// validID keeps a caller-supplied ID from escaping the runs directory.
func validID(id string) bool {
	if id == "" || len(id) > maxIDLen {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func newID() string {
	var b [idRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("runstate: crypto/rand: %v", err))
	}
	return "run_" + hex.EncodeToString(b[:])
}
