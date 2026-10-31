package rebasemerge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxRebaseAttempts is the loop guard on the autonomous rebase-resolve
// task: a PR gets this many fixer rebases per conflict episode. Each
// attempt is a new head the fixer pushed that GitHub still reports dirty;
// past the cap the hive stops re-kicking and the row asks for a person
// (hivecommons/hive#14822, #15103).
const MaxRebaseAttempts = 2

// AttemptsPath is where the per-PR attempt ledger lives in the pod.
var AttemptsPath = "/data/rebase-attempts.json"

// Attempt is one PR's conflict episode: the dirty heads seen, oldest
// first. The first dirty head is attempt 1 (the fixer's first task); a
// pushed head that is still dirty is the next attempt, and so is an
// attempt the fixer never pushed for within the per-attempt deadline
// (Expire; hivecommons/hive#16023). Past MaxRebaseAttempts the episode is
// exhausted. A PR that stops conflicting ends its episode (Clear) so a
// later conflict starts fresh.
type Attempt struct {
	Repo   string   `json:"repo"`
	Number int      `json:"number"`
	Heads  []string `json:"heads"`
	// Stalled counts attempts that timed out with no push from the fixer;
	// each one is a failed attempt.
	Stalled int `json:"stalled,omitempty"`
	// DispatchedAttempt is the attempt the router last handed to Fixer,
	// at DispatchedAt; the router announces an attempt once.
	DispatchedAttempt int       `json:"dispatched_attempt,omitempty"`
	DispatchedAt      time.Time `json:"dispatched_at,omitempty"`
	Fixer             string    `json:"fixer,omitempty"`
	// RequestedBy is the operator whose approval (or merge-now) asked for
	// the rebase from the dashboard, so the fixer router lists the PR even
	// when neither the hive nor a trusted author opened it.
	RequestedBy string    `json:"requested_by,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Count is the current attempt number (1-based): dirty heads seen plus
// attempts that timed out waiting for the fixer.
func (a Attempt) Count() int { return len(a.Heads) + a.Stalled }

// Exhausted: more attempts than the cap allows rebases.
func (a Attempt) Exhausted() bool { return a.Count() > MaxRebaseAttempts }

// Dispatched reports whether the current attempt was handed to a fixer.
func (a Attempt) Dispatched() bool {
	return a.DispatchedAttempt > 0 && a.DispatchedAttempt == a.Count() && !a.DispatchedAt.IsZero()
}

// Waiting is how long the current attempt has waited for the fixer's
// push since it was dispatched; 0 when it was not dispatched.
func (a Attempt) Waiting(now time.Time) time.Duration {
	if !a.Dispatched() || now.Before(a.DispatchedAt) {
		return 0
	}
	return now.Sub(a.DispatchedAt)
}

// Head is the newest dirty head seen.
func (a Attempt) Head() string {
	if len(a.Heads) == 0 {
		return ""
	}
	return a.Heads[len(a.Heads)-1]
}

// Attempts is the persisted attempt ledger.
type Attempts struct {
	mu     sync.Mutex
	path   string
	loaded bool
	items  map[string]Attempt
}

// NewAttempts returns a ledger at path ("" keeps it in memory).
func NewAttempts(path string) *Attempts {
	return &Attempts{path: path, items: map[string]Attempt{}}
}

var (
	sharedAttemptsOnce sync.Once
	sharedAttempts     *Attempts
)

// SharedAttempts is the process-wide ledger at AttemptsPath.
func SharedAttempts() *Attempts {
	sharedAttemptsOnce.Do(func() { sharedAttempts = NewAttempts(AttemptsPath) })
	return sharedAttempts
}

func (s *Attempts) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var items map[string]Attempt
	if json.Unmarshal(data, &items) == nil && items != nil {
		s.items = items
	}
}

func (s *Attempts) persist() {
	if s.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// Observe records that head is conflicting on repo#number now and returns
// the episode. The same head observed again is the same attempt; a new
// head is the next one.
func (s *Attempts) Observe(repo string, number int, head string, now time.Time) Attempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	key := Key(repo, number)
	a, ok := s.items[key]
	head = strings.TrimSpace(head)
	if !ok {
		a = Attempt{Repo: repo, Number: number, StartedAt: now}
	}
	if head != "" && (len(a.Heads) == 0 || a.Heads[len(a.Heads)-1] != head) {
		a.Heads = append(a.Heads, head)
		a.UpdatedAt = now
		s.items[key] = a
		s.persist()
	} else if !ok {
		a.UpdatedAt = now
		s.items[key] = a
		s.persist()
	}
	return a
}

// Request opens (or joins) the episode for repo#number on behalf of an
// operator: the dashboard's approve / merge-now on a conflicting PR asks
// the fixer to rebase it on the next router pass.
func (s *Attempts) Request(repo string, number int, head, actor string, now time.Time) Attempt {
	a := s.Observe(repo, number, head, now)
	actor = strings.TrimSpace(actor)
	if actor == "" || a.RequestedBy == actor {
		return a
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(repo, number)
	a = s.items[key]
	a.RequestedBy = actor
	a.UpdatedAt = now
	s.items[key] = a
	s.persist()
	return a
}

// Dispatch records that the current attempt of repo#number's open episode
// was routed to fixer. first is true only the first time an attempt is
// dispatched, so the router announces each attempt once and later sweeps
// keep the original dispatch time.
func (s *Attempts) Dispatch(repo string, number int, fixer string, now time.Time) (a Attempt, first bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	key := Key(repo, number)
	a, ok := s.items[key]
	if !ok || a.Exhausted() || a.Dispatched() {
		return a, false
	}
	a.DispatchedAttempt = a.Count()
	a.DispatchedAt = now
	a.Fixer = strings.TrimSpace(fixer)
	a.UpdatedAt = now
	s.items[key] = a
	s.persist()
	return a, true
}

// Expire counts the dispatched attempt of repo#number as failed once it
// has waited timeout with no push from the fixer (a push is a new head,
// which Observe already counts as the next attempt). advanced is true when
// the episode moved to its next attempt; a timeout <= 0 never expires.
func (s *Attempts) Expire(repo string, number int, timeout time.Duration, now time.Time) (a Attempt, advanced bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	key := Key(repo, number)
	a, ok := s.items[key]
	if !ok || timeout <= 0 || a.Exhausted() || !a.Dispatched() || now.Sub(a.DispatchedAt) < timeout {
		return a, false
	}
	a.Stalled++
	a.UpdatedAt = now
	s.items[key] = a
	s.persist()
	return a, true
}

// Get returns the episode for repo#number, if one is open.
func (s *Attempts) Get(repo string, number int) (Attempt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	a, ok := s.items[Key(repo, number)]
	return a, ok
}

// Clear ends the episode: the PR is no longer conflicting (rebased,
// merged or closed).
func (s *Attempts) Clear(repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	key := Key(repo, number)
	if _, ok := s.items[key]; !ok {
		return
	}
	delete(s.items, key)
	s.persist()
}

// AttemptText is the row pill: "Rebasing · attempt 1/2", or
// "Rebase failed · 2/2 — needs a person" once exhausted.
func AttemptText(a Attempt) string {
	if a.Exhausted() {
		return "Rebase failed · " + strconv.Itoa(MaxRebaseAttempts) + "/" + strconv.Itoa(MaxRebaseAttempts) + " — needs a person"
	}
	n := a.Count()
	if n < 1 {
		n = 1
	}
	return "Rebasing · attempt " + strconv.Itoa(n) + "/" + strconv.Itoa(MaxRebaseAttempts)
}

// AttemptTextAt is AttemptText with the wait on a dispatched attempt:
// "Rebasing · attempt 1/2 · waiting for scanner 12m".
func AttemptTextAt(a Attempt, now time.Time) string {
	text := AttemptText(a)
	if a.Exhausted() || !a.Dispatched() {
		return text
	}
	fixer := a.Fixer
	if fixer == "" {
		fixer = "the fixer"
	}
	return text + " · waiting for " + fixer + " " + waitingText(a.Waiting(now))
}

func waitingText(d time.Duration) string {
	m := int(d / time.Minute)
	if m < 1 {
		return "<1m"
	}
	return strconv.Itoa(m) + "m"
}
