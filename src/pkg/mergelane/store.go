package mergelane

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultStorePath is where the lane's front records live: the persistent data
// area that already holds the convergence ledger, so a restart reads them back.
const DefaultStorePath = "/data/mergelane/fronts.json"

const (
	storeFileMode      = 0o660
	storeFormatVersion = 1
	storeLockSuffix    = ".lock"
)

// Front stages, as shown in the lane record.
const (
	StageValidating    = "validating"
	StageUpdating      = "updating"
	StageWaitingChecks = "waiting-checks"
	StageMerging       = "merging"
)

// ErrFenced means the front record changed under a caller (another process,
// a restart, a timeout or a release): the caller's validation is void.
var ErrFenced = errors.New("mergelane: front record changed; validation is fenced")

// Front is the durable record of the one pull request at the front of a lane.
type Front struct {
	PR    int    `json:"pr"`
	Path  string `json:"path,omitempty"`
	Epoch uint64 `json:"epoch"`
	Stage string `json:"stage"`
	// EnteredAt is when the PR reached the front; DeadlineAt is the front
	// timeout, restarted on every branch update.
	EnteredAt  time.Time `json:"entered_at"`
	DeadlineAt time.Time `json:"deadline_at"`
	// EvaluatedHead is the head the current validation is evaluating. It is
	// cleared on restart so a validation is always repeated in full.
	EvaluatedHead string `json:"evaluated_head,omitempty"`
	// PinnedHead is the head a branch update was pinned to; until the PR's
	// head moves off it the update has not landed.
	PinnedHead string `json:"pinned_head,omitempty"`
	// LastReason is the last no-merge reason audited for this front, so a
	// round that waits for the same reason is not audited again.
	LastReason string `json:"last_reason,omitempty"`
}

// Waiter is an eligible pull request behind the front.
type Waiter struct {
	PR         int       `json:"pr"`
	Path       string    `json:"path,omitempty"`
	EligibleAt time.Time `json:"eligible_at"`
	LastSeen   time.Time `json:"last_seen"`
}

// Exit is the last departure from the front.
type Exit struct {
	PR     int       `json:"pr"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// Record is one (repo, target branch) lane.
type Record struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Front  *Front `json:"front,omitempty"`
	// Waiting is ordered oldest-eligible first and never holds the front PR.
	Waiting  []Waiter `json:"waiting,omitempty"`
	LastExit *Exit    `json:"last_exit,omitempty"`
	// Epoch increases on every front entry and restart and is never reused;
	// it fences a holder whose front was taken away.
	Epoch uint64 `json:"epoch"`
}

type persistedStore struct {
	Version int      `json:"version"`
	Lanes   []Record `json:"lanes"`
}

// Store is the durable lane record: one JSON file, reloaded under an
// exclusive file lock on every transaction and rewritten atomically, the same
// idiom as the convergence claim ledger. Process memory is never authority.
type Store struct {
	path  string
	mu    sync.Mutex
	lanes map[string]*Record
}

// OpenStore loads the lane record at path, creating an empty one when the file
// does not exist. A file that exists but cannot be parsed is refused and left
// untouched for inspection; the lane then cannot run, which fails closed.
func OpenStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("mergelane: store path is required")
	}
	s := &Store{path: path}
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func laneKey(repo, branch string) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "@" + strings.TrimSpace(branch)
}

func (s *Store) reloadLocked() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.lanes = map[string]*Record{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading lane record %s: %w", s.path, err)
	}
	var p persistedStore
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("lane record %s is unparseable and is left untouched for inspection: %w", s.path, err)
	}
	lanes := make(map[string]*Record, len(p.Lanes))
	for i := range p.Lanes {
		rec := p.Lanes[i]
		key := laneKey(rec.Repo, rec.Branch)
		if _, dup := lanes[key]; dup {
			return fmt.Errorf("lane record %s holds conflicting entries for %s", s.path, key)
		}
		lanes[key] = &rec
	}
	s.lanes = lanes
	return nil
}

func (s *Store) persistLocked() error {
	all := make([]Record, 0, len(s.lanes))
	for _, rec := range s.lanes {
		all = append(all, *rec)
	}
	sort.Slice(all, func(i, j int) bool {
		return laneKey(all[i].Repo, all[i].Branch) < laneKey(all[j].Repo, all[j].Branch)
	})
	data, err := json.MarshalIndent(persistedStore{Version: storeFormatVersion, Lanes: all}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling lane record: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing tmp lane record: %w", err)
	}
	tmpPath := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Chmod(storeFileMode)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmpPath, s.path)
	}
	if werr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing lane record %s: %w", s.path, werr)
	}
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// update runs fn over a freshly reloaded record under the in-process mutex and
// the cross-process file lock, persisting only when fn succeeds.
func (s *Store) update(fn func(lanes map[string]*Record) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o770); err != nil {
		return fmt.Errorf("creating lane record directory: %w", err)
	}
	f, err := os.OpenFile(s.path+storeLockSuffix, os.O_CREATE|os.O_RDWR, storeFileMode)
	if err != nil {
		return fmt.Errorf("opening lane record lock: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking lane record: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	if err := s.reloadLocked(); err != nil {
		return err
	}
	if err := fn(s.lanes); err != nil {
		return err
	}
	return s.persistLocked()
}

// lane returns the record for (repo, branch), creating it on first use.
func lane(lanes map[string]*Record, repo, branch string) *Record {
	key := laneKey(repo, branch)
	rec, ok := lanes[key]
	if !ok {
		rec = &Record{Repo: strings.TrimSpace(repo), Branch: strings.TrimSpace(branch)}
		lanes[key] = rec
	}
	return rec
}

// Snapshot returns a copy of the (repo, branch) lane as last persisted.
func (s *Store) Snapshot(repo, branch string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return Record{}, false, err
	}
	rec, ok := s.lanes[laneKey(repo, branch)]
	if !ok {
		return Record{}, false, nil
	}
	return rec.clone(), true, nil
}

// Lanes returns a copy of every lane recorded for repo, one per target
// branch, ordered by branch. It is read-only, for the lane-state view.
func (s *Store) Lanes(repo string) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	want := strings.ToLower(strings.TrimSpace(repo))
	var out []Record
	for _, rec := range s.lanes {
		if strings.ToLower(strings.TrimSpace(rec.Repo)) == want {
			out = append(out, rec.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Branch < out[j].Branch })
	return out, nil
}

func (r *Record) clone() Record {
	out := *r
	if r.Front != nil {
		f := *r.Front
		out.Front = &f
	}
	out.Waiting = append([]Waiter(nil), r.Waiting...)
	if r.LastExit != nil {
		e := *r.LastExit
		out.LastExit = &e
	}
	return out
}

func (r *Record) removeWaiter(pr int) {
	kept := r.Waiting[:0]
	for _, w := range r.Waiting {
		if w.PR != pr {
			kept = append(kept, w)
		}
	}
	r.Waiting = kept
}

func (r *Record) waiterIndex(pr int) int {
	for i, w := range r.Waiting {
		if w.PR == pr {
			return i
		}
	}
	return -1
}
