package github

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// MergeAttemptRetention is how long a finished merge attempt (merged or
// refused) stays in the registry so a tracker poll that lands after the
// merge call still sees that one was made, and a failed one still carries
// its reason.
const MergeAttemptRetention = 10 * time.Minute

// MergeAttempt is one PR merge the hive is attempting right now, or just
// attempted (hivecommons/hive#15245). Every merge path — the automerge
// sweep, the dashboard's "merge as maintainer", the relay, the forge
// adapter — begins one the moment the GitHub merge call is issued and ends
// it with the outcome, so the Activity Monitor's merge gate can show
// "Merging" even when the PR goes from pre-merge stage to merged within a
// single tracker poll.
type MergeAttempt struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// Path names the caller (PRAuditPath*).
	Path      string    `json:"path,omitempty"`
	StartedAt time.Time `json:"started_at"`
	// EndedAt is zero while the merge call is in flight.
	EndedAt time.Time `json:"ended_at,omitempty"`
	Merged  bool      `json:"merged,omitempty"`
	// Error is the refusal, in words, when the attempt ended unmerged.
	Error string `json:"error,omitempty"`
	// Head is the head SHA the merge call targeted ("" when the caller did
	// not know it). A refusal belongs to that head: once the PR's head
	// moves (merge-from-base, new push) the tracker drops the "Merge
	// failed" word and keeps the refusal as history
	// (hivecommons/hive#15245 follow-up).
	Head string `json:"head,omitempty"`
}

// InFlight reports whether the merge call has not returned yet.
func (a MergeAttempt) InFlight() bool { return !a.StartedAt.IsZero() && a.EndedAt.IsZero() }

// Failed reports whether the attempt ended without a merge.
func (a MergeAttempt) Failed() bool { return !a.EndedAt.IsZero() && !a.Merged }

// MergeAttempts is the registry of merge attempts, keyed by repo#number.
// One lives process-wide (BeginMergeAttempt / EndMergeAttempt /
// MergeAttemptFor) so the dashboard, which never holds the clients that
// merge, can read it.
type MergeAttempts struct {
	mu       sync.Mutex
	items    map[string]MergeAttempt
	now      func() time.Time
	onChange func()
}

// NewMergeAttempts returns an empty registry reading the clock from now
// (time.Now when nil).
func NewMergeAttempts(now func() time.Time) *MergeAttempts {
	if now == nil {
		now = time.Now
	}
	return &MergeAttempts{items: map[string]MergeAttempt{}, now: now}
}

func mergeAttemptKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// SetOnChange registers fn to run (outside the lock) after every Begin and
// End; the dashboard uses it to invalidate its tracker cache so the next
// poll sees the attempt.
func (m *MergeAttempts) SetOnChange(fn func()) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

// Begin records that a merge of repo#number is being attempted now by
// path, replacing any earlier attempt on the PR.
func (m *MergeAttempts) Begin(repo string, number int, path string) {
	m.BeginOnHead(repo, number, path, "")
}

// BeginOnHead is Begin for a merge call aimed at head (the PR head SHA
// the caller verified or asked GitHub to expect).
func (m *MergeAttempts) BeginOnHead(repo string, number int, path, head string) {
	if m == nil || number <= 0 || strings.TrimSpace(repo) == "" {
		return
	}
	m.mu.Lock()
	now := m.now()
	m.purgeLocked(now)
	m.items[mergeAttemptKey(repo, number)] = MergeAttempt{Repo: strings.TrimSpace(repo), Number: number, Path: path, StartedAt: now, Head: strings.TrimSpace(head)}
	fn := m.onChange
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// End records the outcome of the attempt begun on repo#number. err is the
// refusal when merged is false; an End with no Begin records nothing.
func (m *MergeAttempts) End(repo string, number int, merged bool, err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	key := mergeAttemptKey(repo, number)
	a, ok := m.items[key]
	if !ok || !a.EndedAt.IsZero() {
		m.mu.Unlock()
		return
	}
	a.EndedAt, a.Merged = m.now(), merged
	if !merged {
		if err != nil {
			a.Error = err.Error()
		} else {
			a.Error = "GitHub did not merge the pull request"
		}
	}
	m.items[key] = a
	fn := m.onChange
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Get returns the attempt on repo#number still inside the retention
// window, if any.
func (m *MergeAttempts) Get(repo string, number int) (MergeAttempt, bool) {
	if m == nil {
		return MergeAttempt{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeLocked(m.now())
	a, ok := m.items[mergeAttemptKey(repo, number)]
	return a, ok
}

// Snapshot returns every attempt still inside the retention window.
func (m *MergeAttempts) Snapshot() []MergeAttempt {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeLocked(m.now())
	out := make([]MergeAttempt, 0, len(m.items))
	for _, a := range m.items {
		out = append(out, a)
	}
	return out
}

// purgeLocked drops finished attempts older than the retention window.
// In-flight attempts are kept: a merge call that never returned is still
// what the hive is doing.
func (m *MergeAttempts) purgeLocked(now time.Time) {
	for k, a := range m.items {
		if !a.EndedAt.IsZero() && now.Sub(a.EndedAt) > MergeAttemptRetention {
			delete(m.items, k)
		}
	}
}

var defaultMergeAttempts = NewMergeAttempts(nil)

// DefaultMergeAttempts is the process-wide registry.
func DefaultMergeAttempts() *MergeAttempts { return defaultMergeAttempts }

// BeginMergeAttempt / EndMergeAttempt / MergeAttemptFor act on the
// process-wide registry.
func BeginMergeAttempt(repo string, number int, path string) {
	defaultMergeAttempts.Begin(repo, number, path)
}

// BeginMergeAttemptOnHead is BeginMergeAttempt with the head SHA the merge
// call targets.
func BeginMergeAttemptOnHead(repo string, number int, path, head string) {
	defaultMergeAttempts.BeginOnHead(repo, number, path, head)
}

func EndMergeAttempt(repo string, number int, merged bool, err error) {
	defaultMergeAttempts.End(repo, number, merged, err)
}

func MergeAttemptFor(repo string, number int) (MergeAttempt, bool) {
	return defaultMergeAttempts.Get(repo, number)
}

// MergeAttemptsSnapshot is every attempt in the process-wide registry.
func MergeAttemptsSnapshot() []MergeAttempt { return defaultMergeAttempts.Snapshot() }
