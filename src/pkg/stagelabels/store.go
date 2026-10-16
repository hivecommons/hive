package stagelabels

import (
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

// DefaultStateDir matches the review swarm's durable data dir (the PVC on a
// hosted spoke) so stage timing survives a pod restart.
const DefaultStateDir = "/data"

// StateFile is the persisted stage-timing file name.
const StateFile = "stage-labels.json"

// StatePath is where the shared Store persists. A var so tests and the
// operator can redirect it.
var StatePath = filepath.Join(DefaultStateDir, StateFile)

// Record is what the hive remembers about one item's stage: when it entered
// the stage (for stalled detection, kept locally so no GitHub timeline GET is
// needed) and any unresolved regression.
type Record struct {
	Stage     Stage     `json:"stage"`
	EnteredAt time.Time `json:"entered_at"`
	// RegressedFrom is the stage the item fell back from; None when the item
	// has not regressed or has since recovered.
	RegressedFrom Stage     `json:"regressed_from,omitempty"`
	RegressedAt   time.Time `json:"regressed_at,omitempty"`
	// RegressionCommented records that the one-line regression comment for
	// the current regression was posted, so it is never posted twice.
	RegressionCommented bool `json:"regression_commented,omitempty"`
	// StallCommented records that the stall note for the current stage
	// visit was posted (stages with StallNote only).
	StallCommented bool `json:"stall_commented,omitempty"`
	// Reason is the lane/CI/verdict reason behind the current stage.
	Reason string `json:"reason,omitempty"`
	// LastSeen is the last cycle the item was observed; stale records are
	// pruned by Prune so a closed item does not live in the file forever.
	LastSeen time.Time `json:"last_seen"`
}

// Regressed reports whether the item carries an unresolved regression.
func (r Record) Regressed() bool { return r.RegressedFrom != None }

// Transition is what Observe reports about one stage update.
type Transition struct {
	Prev, Next Stage
	// Changed is true when the stage differs from the previous record.
	Changed bool
	// Regressed is true when this observation moved the stage backwards
	// (a new regression, which deserves a comment).
	Regressed bool
	// Recovered is true when a prior regression cleared because the stage
	// reached or passed the point it fell from.
	Recovered bool
}

// Store is the persisted per-item stage memory. One Store is shared
// process-wide (Shared) because the automerge sweep builds a fresh engine
// per tick.
type Store struct {
	mu    sync.Mutex
	path  string
	now   func() time.Time
	items map[string]Record
	dirty bool
}

// NewStore returns an empty in-memory store that persists to path ("" =
// never persists; tests use this).
func NewStore(path string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{path: path, now: now, items: map[string]Record{}}
}

var (
	sharedOnce  sync.Once
	sharedStore *Store
)

// Shared returns the process-wide store backed by StatePath, loading it on
// first use. A missing or unreadable file starts empty: the only effect is
// that stalled timers restart from this cycle.
func Shared() *Store {
	sharedOnce.Do(func() {
		sharedStore = NewStore(StatePath, nil)
		_ = sharedStore.Load()
	})
	return sharedStore
}

// Key is the store key for repo#number. repo is owner/repo.
func Key(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + fmt.Sprint(number)
}

// Load replaces the in-memory items with the persisted file. A missing
// file is not an error.
func (s *Store) Load() error {
	if s == nil || s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var items map[string]Record
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("decoding %s: %w", s.path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if items == nil {
		items = map[string]Record{}
	}
	s.items = items
	s.dirty = false
	return nil
}

// Save writes the store if anything changed since the last Save/Load.
func (s *Store) Save() error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// Get returns the record for key.
func (s *Store) Get(key string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	return r, ok
}

// Observe records that key is now in stage for reason and reports the
// transition. A None stage is terminal: the record is dropped and the
// transition carries Prev so the caller can clear labels.
func (s *Store) Observe(key string, stage Stage, reason string) Transition {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	prev, had := s.items[key]
	tr := Transition{Prev: prev.Stage, Next: stage}
	if stage == None {
		if had {
			delete(s.items, key)
			s.dirty = true
			tr.Changed = prev.Stage != None
		}
		return tr
	}
	next := prev
	next.Stage = stage
	next.Reason = reason
	next.LastSeen = now
	if !had || prev.Stage != stage {
		next.EnteredAt = now
		next.StallCommented = false
		tr.Changed = true
	}
	switch {
	case had && prev.Stage != None && IsRegression(prev.Stage, stage):
		// Backwards. A fall while already regressed keeps the ORIGINAL
		// high-water mark so recovery means getting all the way back.
		if !prev.Regressed() || prev.RegressedFrom.Before(prev.Stage) {
			next.RegressedFrom = prev.Stage
		}
		next.RegressedAt = now
		next.RegressionCommented = false
		tr.Regressed = true
	case prev.Regressed() && !stage.Before(prev.RegressedFrom):
		next.RegressedFrom = None
		next.RegressedAt = time.Time{}
		next.RegressionCommented = false
		tr.Recovered = true
	}
	if !had || next != prev {
		s.dirty = true
	}
	s.items[key] = next
	return tr
}

// IsRegression reports whether moving from → to is a backward transition
// worth flagging. A lower number is backward, except leaving a repair stage
// (fixing, returned-to-contributor, adopted-by-hive) for review of the new
// head, which is the repair route itself, not a second regression.
func IsRegression(from, to Stage) bool {
	if !to.Before(from) {
		return false
	}
	switch from {
	case Fixing, ReturnedToContributor, AdoptedByHive:
		return to != ReviewPending && to != Reviewed
	}
	return true
}

// MarkRegressionCommented records that the regression comment was posted.
func (s *Store) MarkRegressionCommented(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.RegressionCommented {
		return
	}
	r.RegressionCommented = true
	s.items[key] = r
	s.dirty = true
}

// MarkStallCommented records that the stall note for this stage visit was posted.
func (s *Store) MarkStallCommented(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.StallCommented {
		return
	}
	r.StallCommented = true
	s.items[key] = r
	s.dirty = true
}

// Forget drops key (closed/merged item).
func (s *Store) Forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[key]; ok {
		delete(s.items, key)
		s.dirty = true
	}
}

// Prune drops records not seen for maxAge (closed items the sweep stopped
// listing). Returns how many were dropped.
func (s *Store) Prune(maxAge time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-maxAge)
	n := 0
	for k, r := range s.items {
		if r.LastSeen.Before(cutoff) {
			delete(s.items, k)
			n++
		}
	}
	if n > 0 {
		s.dirty = true
	}
	return n
}

// Keys lists the stored keys, sorted (for tests and the dashboard).
func (s *Store) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.items))
	for k := range s.items {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Dwell resolves the stalled threshold for stage: the override for its
// slug when set, else the table default. 0 means never stalled.
func Dwell(stage Stage, overrides map[string]time.Duration) time.Duration {
	if d, ok := overrides[stage.Slug()]; ok {
		return d
	}
	return stage.Dwell()
}

// Stalled reports whether r has sat in its stage past the dwell threshold.
func Stalled(r Record, overrides map[string]time.Duration, now time.Time) bool {
	if r.Stage == None || r.EnteredAt.IsZero() {
		return false
	}
	d := Dwell(r.Stage, overrides)
	if d <= 0 {
		return false
	}
	return now.Sub(r.EnteredAt) >= d
}

// RegressionComment is the one-line, idempotent comment posted on a
// backward transition: `stage 100-merge-queue → 070-fixing: <reason>`.
func RegressionComment(from, to Stage, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "state changed"
	}
	return RegressionCommentMarker + "\nstage " + from.String() + " → " + to.String() + ": " + reason
}

// StallComment is the one-line, idempotent note posted when an item stalls
// in a StallNote stage: `stage 080-awaiting-merge-admission stalled after
// 30m: <lane's last skip reason>`.
func StallComment(stage Stage, dwell time.Duration, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "lane has not evaluated this head"
	}
	return StallCommentMarker + "\nstage " + stage.String() + " stalled after " + dwell.String() + ": " + reason
}

// StallCommentMarker tags the stall note.
const StallCommentMarker = "<!-- hive:stage-stalled -->"

// RegressionCommentMarker tags the regression comment so a reader (or a
// later dedupe pass) can find it.
const RegressionCommentMarker = "<!-- hive:stage-regressed -->"
