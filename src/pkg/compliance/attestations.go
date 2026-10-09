package compliance

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Attestation is an owner's record that they reviewed a framework's controls
// on a date (hivecommons/hive#11081). It records that a person looked; it is
// not a pass/fail verdict and changes no control status.
type Attestation struct {
	Framework  string    `json:"framework"`
	ReviewedOn string    `json:"reviewed_on"`
	By         string    `json:"by"`
	At         time.Time `json:"at"`
	Note       string    `json:"note,omitempty"`
}

// MaxAttestationNoteLen bounds an attestation note, in characters.
const MaxAttestationNoteLen = 2000

// MaxAttestations bounds the attestation store; the oldest are dropped first.
// The audit log keeps its own copy of every attestation.
const MaxAttestations = 5000

// attestationFutureSlack lets reviewed_on be "today" in any time zone.
const attestationFutureSlack = 36 * time.Hour

// NormalizeAttestation trims and validates a, stamping At with now. The
// framework must be a shipped profile id; reviewed_on must be a YYYY-MM-DD
// date that is not in the future; by must name the attesting user.
func NormalizeAttestation(a Attestation, now time.Time) (Attestation, error) {
	a.Framework = strings.ToLower(strings.TrimSpace(a.Framework))
	a.ReviewedOn = strings.TrimSpace(a.ReviewedOn)
	a.By = strings.TrimSpace(a.By)
	a.Note = strings.TrimSpace(a.Note)
	if a.Framework == "" {
		return a, errors.New("framework is required")
	}
	if _, ok := ProfileByID(a.Framework); !ok {
		return a, fmt.Errorf("unknown framework %q", a.Framework)
	}
	day, err := time.Parse("2006-01-02", a.ReviewedOn)
	if err != nil {
		return a, errors.New("reviewed_on must be a YYYY-MM-DD date")
	}
	if day.After(now.Add(attestationFutureSlack)) {
		return a, errors.New("reviewed_on must not be in the future")
	}
	if a.By == "" {
		return a, errors.New("attesting user is unknown")
	}
	if utf8.RuneCountInString(a.Note) > MaxAttestationNoteLen {
		return a, fmt.Errorf("note must be at most %d characters", MaxAttestationNoteLen)
	}
	a.At = now.UTC()
	return a, nil
}

// AttestationStore persists attestations as append-only JSONL mirrored in
// memory. An empty path keeps them in memory only.
type AttestationStore struct {
	mu    sync.Mutex
	path  string
	items []Attestation
}

// NewAttestationStore opens the store at path, loading attestations written
// by a previous process. Malformed lines are skipped. A missing file is an
// empty store; any other read error is returned with the empty store.
func NewAttestationStore(path string) (*AttestationStore, error) {
	s := &AttestationStore{path: path}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, fmt.Errorf("compliance: reading attestations %s: %w", path, err)
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var a Attestation
		if json.Unmarshal(line, &a) != nil || a.At.IsZero() || a.Framework == "" {
			continue
		}
		s.items = append(s.items, a)
	}
	s.capLocked()
	return s, nil
}

func (s *AttestationStore) capLocked() {
	if over := len(s.items) - MaxAttestations; over > 0 {
		s.items = append([]Attestation(nil), s.items[over:]...)
	}
}

// Add records a. The in-memory store is updated even when persisting fails;
// the error is returned for the caller to report.
func (s *AttestationStore) Add(a Attestation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, a)
	s.capLocked()
	if s.path == "" {
		return nil
	}
	line, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("compliance: encoding attestation: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("compliance: creating attestation dir: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("compliance: opening attestations: %w", err)
	}
	_, werr := f.Write(append(line, '\n'))
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("compliance: appending attestation: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("compliance: closing attestations: %w", cerr)
	}
	return nil
}

// List returns attestations newest first, filtered to framework when it is
// non-empty, and to those recorded in [since, until) when either is non-zero.
func (s *AttestationStore) List(framework string, since, until time.Time) []Attestation {
	framework = strings.ToLower(strings.TrimSpace(framework))
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Attestation{}
	for i := len(s.items) - 1; i >= 0; i-- {
		a := s.items[i]
		if framework != "" && a.Framework != framework {
			continue
		}
		if !since.IsZero() && a.At.Before(since) {
			continue
		}
		if !until.IsZero() && !a.At.Before(until) {
			continue
		}
		out = append(out, a)
	}
	return out
}
