package upstreamwatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// RefStatus is the outcome recorded for one upstream ref.
type RefStatus string

const (
	// StatusFiled means a fork issue was opened for the ref.
	StatusFiled RefStatus = "filed"
	// StatusSkipped means the watch deliberately passed the ref over, for
	// instance because none of the files it touches exist in the fork.
	StatusSkipped RefStatus = "skipped"
	// StatusPorted means the fork issue for the ref was completed.
	StatusPorted RefStatus = "ported"
	// StatusDismissed means the fork issue was closed as "not planned" or
	// carries the upstream/dismissed label. A dismissed ref is never
	// resurfaced.
	StatusDismissed RefStatus = "dismissed"
)

// stateFileVersion is bumped when the on-disk shape changes incompatibly.
const stateFileVersion = 1

// stateDirPerm and stateFilePerm are the modes the state directory and file
// are created with: the file holds no secrets, but only the hive writes it.
const (
	stateDirPerm  os.FileMode = 0o755
	stateFilePerm os.FileMode = 0o644
)

// defaultRecentLimit is how many recent refs Summary returns when the caller
// asks for no particular number.
const defaultRecentLimit = 20

// RefRecord is what the watch remembers about one upstream ref
// ("upstream#<pr>" / "release:<tag>").
type RefRecord struct {
	// Ref is the dedupe key, as produced by RefPR and RefRelease.
	Ref string `json:"ref"`
	// Status is the outcome recorded for the ref.
	Status RefStatus `json:"status"`
	// IssueNumber is the fork issue opened for the ref, zero when none was.
	IssueNumber int `json:"issue_number,omitempty"`
	// FiledAt is when the watch recorded this outcome.
	FiledAt time.Time `json:"filed_at"`
	// Reason explains a skip or a dismissal; empty otherwise.
	Reason string `json:"reason,omitempty"`
}

// RepoState is the durable state for one watched repo.
type RepoState struct {
	// Upstream is the resolved owner/repo this repo follows.
	Upstream string `json:"upstream,omitempty"`
	// Watermark is the upstream timestamp every item at or before which has
	// already been handled. It only ever moves forward, and only past an
	// item that was filed, skipped or ported.
	Watermark time.Time `json:"watermark"`
	// LastRunAt is when the watch last polled this repo.
	LastRunAt time.Time `json:"last_run_at"`
	// Refs is keyed by upstream ref.
	Refs map[string]*RefRecord `json:"refs,omitempty"`
}

// State is the whole watch state, keyed by the configured repo key.
type State map[string]*RepoState

// Outcome is one ref's result, handed to RepoState.Record.
type Outcome struct {
	// Ref is the upstream ref the outcome belongs to.
	Ref string
	// Status is the outcome to record.
	Status RefStatus
	// ItemTime is the upstream timestamp of the item (merged_at /
	// published_at). The watermark advances to it when the status means the
	// item is done with; a zero value leaves the watermark alone.
	ItemTime time.Time
	// IssueNumber is the fork issue opened for the ref, if any.
	IssueNumber int
	// Reason explains a skip or dismissal.
	Reason string
}

// Store persists the upstream watch state. Load returns an empty (non-nil)
// State when nothing has been saved yet.
type Store interface {
	Load() (State, error)
	Save(state State) error
}

// Repo returns the state for key, creating an empty entry when the repo has
// never been seen.
func (s State) Repo(key string) *RepoState {
	if r := s[key]; r != nil {
		return r
	}
	r := &RepoState{}
	s[key] = r
	return r
}

// Record returns the stored record for ref.
func (r *RepoState) Record(ref string) (*RefRecord, bool) {
	rec, ok := r.Refs[ref]
	return rec, ok && rec != nil
}

// Seen reports whether ref has any recorded outcome, which is the dedupe
// guard the poller consults before filing.
func (r *RepoState) Seen(ref string) bool {
	_, ok := r.Record(ref)
	return ok
}

// Dismissed reports whether ref was dismissed and must never be resurfaced.
func (r *RepoState) Dismissed(ref string) bool {
	rec, ok := r.Record(ref)
	return ok && rec.Status == StatusDismissed
}

// IssueFor returns the fork issue number recorded for ref, zero when none is.
func (r *RepoState) IssueFor(ref string) int {
	rec, ok := r.Record(ref)
	if !ok {
		return 0
	}
	return rec.IssueNumber
}

// Put records an outcome at time now and advances the watermark past the item
// when the outcome means the item is done with. A dismissal keeps the issue
// number already recorded for the ref when the caller does not supply one.
func (r *RepoState) Put(o Outcome, now time.Time) {
	if o.Ref == "" {
		return
	}
	if r.Refs == nil {
		r.Refs = make(map[string]*RefRecord, 1)
	}
	issue := o.IssueNumber
	if issue == 0 {
		issue = r.IssueFor(o.Ref)
	}
	r.Refs[o.Ref] = &RefRecord{
		Ref:         o.Ref,
		Status:      o.Status,
		IssueNumber: issue,
		FiledAt:     now,
		Reason:      o.Reason,
	}
	if advancesWatermark(o.Status) {
		r.AdvanceWatermark(o.ItemTime)
	}
}

// Dismiss records ref as dismissed. It never advances the watermark: a
// dismissal says nothing about whether the item itself was handled.
func (r *RepoState) Dismiss(ref, reason string, now time.Time) {
	r.Put(Outcome{Ref: ref, Status: StatusDismissed, Reason: reason}, now)
}

// AdvanceWatermark moves the watermark to at when that is later than the
// current value. Moving it backwards would re-scan already handled items.
func (r *RepoState) AdvanceWatermark(at time.Time) {
	if at.IsZero() || !at.After(r.Watermark) {
		return
	}
	r.Watermark = at
}

// advancesWatermark reports whether an outcome means the item has been dealt
// with, and so the watermark may move past it.
func advancesWatermark(s RefStatus) bool {
	switch s {
	case StatusFiled, StatusSkipped, StatusPorted:
		return true
	default:
		return false
	}
}

// RefSummary is one recent ref as the dashboard divergence view
// (hivecommons/hive#9969) renders it.
type RefSummary struct {
	Ref         string    `json:"ref"`
	Status      RefStatus `json:"status"`
	IssueNumber int       `json:"issue_number,omitempty"`
	FiledAt     time.Time `json:"filed_at"`
}

// RepoSummary is the read shape the dashboard divergence view consumes.
type RepoSummary struct {
	Repo      string    `json:"repo"`
	Upstream  string    `json:"upstream,omitempty"`
	Watermark time.Time `json:"watermark"`
	LastRunAt time.Time `json:"last_run_at"`
	// Surfaced counts refs that produced a fork issue.
	Surfaced int `json:"surfaced"`
	// Ported, Dismissed and Skipped count refs by recorded status.
	Ported    int `json:"ported"`
	Dismissed int `json:"dismissed"`
	Skipped   int `json:"skipped"`
	// Recent lists refs newest-first, capped by the requested limit.
	Recent []RefSummary `json:"recent,omitempty"`
}

// Summary returns the read accessor for one repo. limit caps Recent; zero or
// less means defaultRecentLimit. The second result is false for a repo the
// watch has never recorded.
func (s State) Summary(key string, limit int) (RepoSummary, bool) {
	r := s[key]
	if r == nil {
		return RepoSummary{}, false
	}
	if limit <= 0 {
		limit = defaultRecentLimit
	}
	sum := RepoSummary{
		Repo:      key,
		Upstream:  r.Upstream,
		Watermark: r.Watermark,
		LastRunAt: r.LastRunAt,
	}
	recent := make([]RefSummary, 0, len(r.Refs))
	for _, rec := range r.Refs {
		if rec == nil {
			continue
		}
		if rec.IssueNumber != 0 {
			sum.Surfaced++
		}
		switch rec.Status {
		case StatusPorted:
			sum.Ported++
		case StatusDismissed:
			sum.Dismissed++
		case StatusSkipped:
			sum.Skipped++
		}
		recent = append(recent, RefSummary{Ref: rec.Ref, Status: rec.Status, IssueNumber: rec.IssueNumber, FiledAt: rec.FiledAt})
	}
	sort.Slice(recent, func(i, j int) bool {
		if recent[i].FiledAt.Equal(recent[j].FiledAt) {
			return recent[i].Ref < recent[j].Ref
		}
		return recent[i].FiledAt.After(recent[j].FiledAt)
	})
	if len(recent) > limit {
		recent = recent[:limit]
	}
	sum.Recent = recent
	return sum, true
}

type stateFile struct {
	Version int   `json:"version"`
	Repos   State `json:"repos"`
}

// FileStore is a JSON-file Store. Writes go to a sibling temp file that is
// renamed into place, so a crash mid-write leaves the previous state intact
// and a restart resumes from it instead of re-scanning the upstream from
// zero.
type FileStore struct {
	path string
	mu   sync.Mutex
}

// NewFileStore returns a FileStore at path.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// Path returns the file the store reads and writes.
func (f *FileStore) Path() string { return f.path }

// Load reads the state file. A missing file is an empty state, not an error;
// a corrupt one IS an error, so the watch stops rather than forgetting its
// watermark and refiling every issue it has already filed.
func (f *FileStore) Load() (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var sf stateFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", f.path, err)
	}
	if sf.Version > stateFileVersion {
		return nil, fmt.Errorf("%s has state version %d, this hive understands up to %d", f.path, sf.Version, stateFileVersion)
	}
	out := make(State, len(sf.Repos))
	for key, r := range sf.Repos {
		if r == nil {
			continue
		}
		if r.Refs == nil {
			r.Refs = map[string]*RefRecord{}
		}
		for ref, rec := range r.Refs {
			if rec == nil {
				delete(r.Refs, ref)
			}
		}
		out[key] = r
	}
	return out, nil
}

// Save writes state atomically.
func (f *FileStore) Save(state State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if state == nil {
		state = State{}
	}
	data, err := json.MarshalIndent(stateFile{Version: stateFileVersion, Repos: state}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, stateDirPerm); err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, stateFilePerm); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
