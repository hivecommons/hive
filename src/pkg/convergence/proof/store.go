package proof

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
)

// Sentinel errors. Callers match with errors.Is; the wrapped detail names the
// offending key and instants for logs.
var (
	// ErrEvidenceRegression: a Put carried an observation OLDER than the
	// receipt already stored under the same key. Arrival order can never
	// regress current truth — the late evidence is refused, and the caller's
	// next re-observation of current state decides, not delivery order.
	ErrEvidenceRegression = errors.New("older evidence cannot replace a newer receipt")
	// ErrMalformedRecord: the record fails validation and can never be
	// recorded (a malformed receipt read back from disk likewise refuses Open).
	ErrMalformedRecord = errors.New("proof record is malformed")
)

// storeFileMode is 0660 (group-writable) for the same reason the outcome
// ledger and beads.Store.persist use it: the file lives under the shared
// /data root and more than one node-group member may legitimately rewrite it.
const storeFileMode = 0o660

// storeFormatVersion is the persisted schema version, written so a future
// widening is a deliberate, reviewable migration rather than an accident.
const storeFormatVersion = 1

// Store is the durable proof-receipt owner: an in-memory index over one JSON
// file, reloaded on boot, rewritten atomically on every accepted write. No
// process memory is authoritative — restart reconstruction IS the file. The
// ONLY authorized writer is the proof observer normalizing current
// authoritative producer evidence; nothing else holds a Put seam.
type Store struct {
	path    string
	mu      sync.Mutex
	records map[string]*Record
}

// persistedStore is the on-disk shape, named separately so the file format is
// a deliberate artifact (the persistedLedger/persistedGenerations convention).
type persistedStore struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}

// Open loads the proof store at path, creating an empty one if the file does
// not exist. A file that exists but cannot be parsed or validated is a
// REFUSAL: Open fails, the bytes are left exactly as found for operator
// inspection, and no handle exists through which a write could replace them —
// corrupt durable state must be visible and can never become satisfaction or
// silently reset to an empty truth (the accepted #4249/#4251 failure
// behavior, following the hub generations quarantine rationale).
func Open(path string) (*Store, error) {
	s := &Store{path: path, records: make(map[string]*Record)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading proof store %s: %w", path, err)
	}
	var persisted persistedStore
	if err := json.Unmarshal(data, &persisted); err != nil {
		return nil, fmt.Errorf("proof store %s is unparseable and is left untouched for inspection: %w", path, err)
	}
	for i := range persisted.Records {
		rec := persisted.Records[i]
		if err := rec.Validate(); err != nil {
			return nil, fmt.Errorf("proof store %s holds a malformed receipt: %w", path, err)
		}
		key := rec.Key()
		if _, dup := s.records[key]; dup {
			return nil, fmt.Errorf("proof store %s holds conflicting receipts for %s", path, key)
		}
		stored := rec.clone()
		s.records[key] = &stored
	}
	return s, nil
}

// Get returns a detached copy of the receipt stored under key. The copy is
// the read seam: verifiers hold immutable values and can never race a writer.
func (s *Store) Get(key string) (Record, bool) {
	if key == "" {
		return Record{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[key]
	if !ok {
		return Record{}, false
	}
	return rec.clone(), true
}

// List returns detached copies of every receipt, sorted by key for
// deterministic projections and tests.
func (s *Store) List() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, rec.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// Put records one normalized observation under its fingerprint key. The
// rules, in order:
//
//  1. A malformed record is refused (never stored, never satisfaction).
//  2. Duplicate identical evidence deduplicates: the stored receipt is
//     returned unchanged and nothing is persisted or re-emitted.
//  3. Evidence observed BEFORE the stored receipt's instant is refused
//     (ErrEvidenceRegression): out-of-order delivery cannot regress current
//     truth. Conflicting results therefore never resolve by arrival order —
//     only a genuinely newer re-observation of current state supersedes.
//  4. Otherwise the record supersedes the stored receipt atomically
//     (marshal → *.tmp → rename), install-after-persist, under the store
//     mutex — which is the explicit serialization rule for racing writers:
//     one deterministic winner per key, no interleaved bytes, no subject
//     collisions.
func (s *Store) Put(rec Record) (Record, error) {
	if err := rec.Validate(); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrMalformedRecord, err)
	}
	key := rec.Key()
	s.mu.Lock()
	defer s.mu.Unlock()
	if stored, ok := s.records[key]; ok {
		if stored.equivalent(rec) {
			return stored.clone(), nil
		}
		if rec.ObservedAt.Before(stored.ObservedAt) {
			return Record{}, fmt.Errorf("%s: stored %s vs offered %s: %w",
				key, stored.ObservedAt.Format("2006-01-02T15:04:05Z07:00"),
				rec.ObservedAt.Format("2006-01-02T15:04:05Z07:00"), ErrEvidenceRegression)
		}
	}
	work := rec.clone()
	prior, hadPrior := s.records[key]
	s.records[key] = &work
	if err := s.persistLocked(); err != nil {
		if hadPrior {
			s.records[key] = prior
		} else {
			delete(s.records, key)
		}
		return Record{}, err
	}
	return work.clone(), nil
}

// persistLocked writes the whole store: marshal → *.tmp → rename, exactly the
// beads.Store.persist / outcome ledger idiom. Callers hold s.mu, which is
// what keeps two writers from interleaving bytes in the same tmp path.
func (s *Store) persistLocked() error {
	all := make([]Record, 0, len(s.records))
	for _, rec := range s.records {
		all = append(all, rec.clone())
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Key() < all[j].Key() })

	data, err := json.MarshalIndent(persistedStore{Version: storeFormatVersion, Records: all}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling proof store: %w", err)
	}
	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, storeFileMode); err != nil {
		return fmt.Errorf("writing tmp proof store: %w", err)
	}
	return os.Rename(tmpPath, s.path)
}
