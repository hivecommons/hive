// Package escalation implements the fix-loop circuit breaker for agent-authored
// PRs: when the same PR keeps failing CI across distinct fix attempts (new head
// SHAs, still red), the hub stops re-dispatching fix work and escalates to a
// human with the raw CI failure evidence attached.
//
// The counting, evidence-gathering, and stop-order are all deterministic code —
// no agent judgment is involved. This exists because of the 2026-07-31→08-04
// incident on kubestellar/console: a truncated test-file split kept main red for
// four days while the scanner iterated blind fix PRs, never seeing the one-line
// "ReferenceError: seedMission is not defined" buried in the shard logs.
package escalation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultThreshold is the number of distinct red head SHAs (fix attempts that
// still failed CI) after which a PR is escalated instead of re-dispatched.
const DefaultThreshold = 3

// maxTrackedSHAs bounds the per-PR attempt history so the ledger cannot grow
// without limit on a pathological PR.
const maxTrackedSHAs = 20

// RedPRStaleAfter is how long a PR's CURRENT red head SHA must have gone
// unchanged before the re-engagement machinery treats it as "stuck" (rather
// than "still churning"). It is a zero-API staleness signal: we record when
// each red head SHA was first seen and compare to now — no GitHub commit-date
// fetch is needed. Ten minutes is long enough that a fix agent actively pushing
// new commits (each a new SHA, each resetting the clock) is never mistaken for
// a stalled PR, but short enough that a genuinely abandoned red PR is re-engaged
// within a couple of governor ticks. This keys ONLY off GitHub check state +
// commit staleness — it is entirely language/linter-agnostic.
const RedPRStaleAfter = 10 * time.Minute

// MaxReEngagements bounds how many times the re-engagement machinery (merge
// watcher #2 / governor reaper #4) may re-dispatch a fix for the SAME red head
// SHA of a PR before it stops and leaves the PR to the distinct-SHA escalation
// path. Without this cap a permanently-red PR whose head never moves would be
// re-dispatched every tick forever. Distinct from DefaultThreshold, which
// counts distinct red SHAs (real fix attempts); this counts re-nudges of an
// unchanged red SHA.
const MaxReEngagements = 6

// MachineryVersion identifies the GENERATION of the fix-dispatch machinery.
// Bump it when the kick/repair pipeline changes materially enough that
// attempts burned under the previous generation are no longer predictive of
// the next attempt's success. Entries whose recorded generation is older get
// ONE fresh set of re-engagements (and are un-escalated) on their next
// TryReEngage — without this, a PR escalated under machinery that could not
// possibly have fixed it (pre-#4828 kicks carried no CI evidence, no branch
// name, no push-to-branch instruction, and most "attempts" never produced a
// single commit) stays human-parked forever even after the machinery is
// repaired. Observed on kubestellar/console 2026-09-01: nine split PRs
// escalated under generation-1 no-op attempts, permanently outside the loop.
//
// Generation 2: evidence-rich FIX-BEFORE-NEW kicks (#4828) + per-agent
// attribution + AGENTS.md repair contracts.
const MachineryVersion = 2

// Entry is the persisted per-PR attempt record.
type Entry struct {
	// RedSHAs are the distinct head SHAs observed with failing CI, oldest
	// first. Length == number of failed fix attempts.
	RedSHAs []string `json:"red_shas"`
	// Escalated is set once the escalation actions (comment + label) have
	// fired, so they never repeat for the same PR.
	Escalated bool `json:"escalated"`
	// LastExcerpt is the most recent CI failure excerpt, kept so the
	// escalation comment can include evidence even if the final enumeration
	// pass failed to fetch annotations.
	LastExcerpt string    `json:"last_excerpt,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`

	// CurRedSHA is the head SHA of the CURRENT red observation. FirstRedAt is
	// when that SHA was first seen red. Together they are the zero-API
	// staleness signal used by the re-engagement paths (#2/#3/#4): a red SHA
	// unchanged for longer than RedPRStaleAfter is "stale/stuck". A new red
	// SHA (an agent pushed a fix that is still red) resets both, so an actively
	// worked PR never reads as stale.
	CurRedSHA  string    `json:"cur_red_sha,omitempty"`
	FirstRedAt time.Time `json:"first_red_at,omitempty"`
	// ReEngagements counts how many times a fix has been re-dispatched for the
	// CURRENT red SHA (CurRedSHA). Reset to 0 whenever CurRedSHA changes or the
	// PR goes green. The re-engagement cap (MaxReEngagements) reads this so a
	// permanently-red, never-moving PR is not nudged forever.
	ReEngagements int `json:"re_engagements,omitempty"`
	// Machinery is the MachineryVersion under which this entry's attempts
	// were burned. Older-generation entries are granted amnesty (see
	// MachineryVersion).
	Machinery int `json:"machinery,omitempty"`
}

// Store is the on-PVC attempt ledger. All methods are safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	path    string
	entries map[string]*Entry // key: "repo#number"
	// clock is the time source; nil means time.Now().UTC(). Overridable via
	// SetClock so staleness/re-engagement tests are deterministic.
	clock func() time.Time
}

// Load reads the ledger at path, returning an empty usable store on any error
// (a missing or corrupt ledger must never block enumeration).
func Load(path string) *Store {
	s := &Store{path: path, entries: map[string]*Entry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var m map[string]*Entry
	if json.Unmarshal(data, &m) == nil && m != nil {
		s.entries = m
	}
	return s
}

// Key builds the ledger key for a PR.
func Key(repo string, number int) string {
	return fmt.Sprintf("%s#%d", repo, number)
}

// Observation is one PR's CI state from the current enumeration pass.
type Observation struct {
	Repo    string
	Number  int
	HeadSHA string
	Red     bool // CI status is failure
	Excerpt string
}

// Result reports the ledger's verdict for one observed PR.
type Result struct {
	Attempts    int
	Escalated   bool // escalation actions already fired (now or previously)
	NewlyEscala bool // this pass crossed the threshold — fire actions now
}

// Sweep folds a full enumeration pass into the ledger: increments attempt
// counts for red PRs with unseen head SHAs, clears entries for PRs that went
// green, prunes PRs no longer present, and reports which PRs crossed the
// escalation threshold on this pass. The caller performs the side effects
// (comment, label, notify) and then calls MarkEscalated for each.
func (s *Store) Sweep(obs []Observation, threshold int) map[string]Result {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]bool, len(obs))
	results := make(map[string]Result, len(obs))
	for _, o := range obs {
		key := Key(o.Repo, o.Number)
		seen[key] = true
		if !o.Red {
			// Green: the loop converged — forget the history entirely so a
			// future regression on the same PR starts a fresh count.
			delete(s.entries, key)
			continue
		}
		e := s.entries[key]
		if e == nil {
			e = &Entry{Machinery: MachineryVersion}
			s.entries[key] = e
		}
		// Machinery amnesty (see MachineryVersion): attempts and escalations
		// burned under an older fix-dispatch generation are wiped once, and
		// the distinct-SHA ledger restarts, so the CURRENT machinery gets its
		// own budget before a human is paged again. Without the RedSHAs reset
		// the very next sweep would re-escalate on the old ledger.
		if e.Machinery < MachineryVersion {
			e.Machinery = MachineryVersion
			e.ReEngagements = 0
			e.Escalated = false
			e.RedSHAs = nil
		}
		if o.HeadSHA != "" && !containsSHA(e.RedSHAs, o.HeadSHA) {
			e.RedSHAs = append(e.RedSHAs, o.HeadSHA)
			if len(e.RedSHAs) > maxTrackedSHAs {
				e.RedSHAs = e.RedSHAs[len(e.RedSHAs)-maxTrackedSHAs:]
			}
		}
		// Maintain the zero-API staleness clock: a change of red head SHA means
		// the branch moved (a fix was pushed, still red) — reset the first-seen
		// time and the per-SHA re-engagement counter so a freshly-pushed red SHA
		// is treated as "just started", not stale.
		if o.HeadSHA != "" && o.HeadSHA != e.CurRedSHA {
			e.CurRedSHA = o.HeadSHA
			e.FirstRedAt = s.now()
			e.ReEngagements = 0
		}
		if o.Excerpt != "" {
			e.LastExcerpt = o.Excerpt
		}
		e.UpdatedAt = s.now()
		// A PR whose CURRENT red SHA has exhausted its re-engagement budget has
		// had every automated nudge it is going to get. If no new SHA ever
		// appears — fix attempts are not even being PUSHED, e.g. the agents lost
		// their write credentials (kubestellar/console, 2026-08-22: eight red
		// PRs re-engaged every cycle for 15h with zero pushes) — the distinct-SHA
		// count can never advance, so without this clause the PR sits in limbo
		// forever: nudged, never fixed, never handed to a human. Budget
		// exhaustion on an unchanged red SHA is therefore escalation-worthy in
		// its own right.
		exhausted := e.ReEngagements >= MaxReEngagements
		results[key] = Result{
			Attempts:    len(e.RedSHAs),
			Escalated:   e.Escalated,
			NewlyEscala: !e.Escalated && (len(e.RedSHAs) >= threshold || exhausted),
		}
	}
	// Prune PRs that left the open set (merged or closed).
	for key := range s.entries {
		if !seen[key] {
			delete(s.entries, key)
		}
	}
	s.saveLocked()
	return results
}

// MarkEscalated records that escalation side effects fired for the PR, so they
// are never repeated.
func (s *Store) MarkEscalated(repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		e.Escalated = true
		e.UpdatedAt = time.Now().UTC()
	}
	s.saveLocked()
}

// Excerpt returns the stored failure excerpt for a PR ("" if none).
func (s *Store) Excerpt(repo string, number int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		return e.LastExcerpt
	}
	return ""
}

// Attempts returns the recorded failed-attempt count for a PR.
func (s *Store) Attempts(repo string, number int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		return len(e.RedSHAs)
	}
	return 0
}

// nowFn is the store's clock, overridable for tests via SetClock.
func (s *Store) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now().UTC()
}

// SetClock overrides the store's time source. Intended for tests so staleness
// can be exercised deterministically without sleeping.
func (s *Store) SetClock(fn func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = fn
}

// ObserveRed folds a set of CI observations into the staleness clock WITHOUT
// touching the distinct-SHA attempt count that drives human escalation. It
// records the first-seen time of each PR's current red head SHA (resetting it
// when the SHA changes) and clears the record for PRs that went green. This is
// called early in the eval cycle so the claim-suppression guard (#3), the merge
// watcher (#2), and the reaper (#4) all read a consistent, current staleness
// signal within the same tick. Idempotent: re-observing the same red SHA leaves
// FirstRedAt unchanged.
func (s *Store) ObserveRed(obs []Observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, o := range obs {
		key := Key(o.Repo, o.Number)
		if !o.Red {
			// Green now: drop the staleness/re-engagement record so a future
			// regression starts a fresh clock. (Attempt history is managed by
			// Sweep, not here.)
			if e := s.entries[key]; e != nil {
				e.CurRedSHA = ""
				e.FirstRedAt = time.Time{}
				e.ReEngagements = 0
			}
			continue
		}
		if o.HeadSHA == "" {
			continue
		}
		e := s.entries[key]
		if e == nil {
			e = &Entry{}
			s.entries[key] = e
		}
		if o.HeadSHA != e.CurRedSHA {
			e.CurRedSHA = o.HeadSHA
			e.FirstRedAt = now
			e.ReEngagements = 0
		}
		if o.Excerpt != "" {
			e.LastExcerpt = o.Excerpt
		}
		e.UpdatedAt = now
	}
	s.saveLocked()
}

// StaleRed reports whether the PR's CURRENT red head SHA (headSHA) has been
// unchanged and red for at least RedPRStaleAfter. It is the single source of
// truth for "this red PR is stuck, not churning". A PR whose headSHA does not
// match the tracked CurRedSHA (the branch moved and we have not observed it yet)
// is treated as NOT stale — fail safe toward leaving healthy/fresh PRs alone.
// Keys only off check state + commit staleness; nothing language-specific.
func (s *Store) StaleRed(repo string, number int, headSHA string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[Key(repo, number)]
	if e == nil || e.CurRedSHA == "" || headSHA == "" || e.CurRedSHA != headSHA {
		return false
	}
	if e.FirstRedAt.IsZero() {
		return false
	}
	return s.now().Sub(e.FirstRedAt) >= RedPRStaleAfter
}

// TryReEngage atomically decides whether the re-engagement paths (#2/#4) may
// dispatch another fix for the PR's current red head SHA, and if so records the
// attempt. It returns true at most MaxReEngagements times per red SHA: the cap
// is what stops a permanently-red, never-moving PR from being nudged every tick
// forever. A changed head SHA resets the counter (via ObserveRed/Sweep), so a
// PR that is actively being fixed is never blocked by the cap. Returns false
// once the cap is reached (the distinct-SHA escalation path then owns the PR).
func (s *Store) TryReEngage(repo string, number int, headSHA string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(repo, number)
	e := s.entries[key]
	if e == nil {
		e = &Entry{}
		s.entries[key] = e
	}
	// If the tracked SHA differs from the observed head, sync to the observed
	// head and reset the counter — the branch moved.
	if headSHA != "" && headSHA != e.CurRedSHA {
		e.CurRedSHA = headSHA
		e.FirstRedAt = s.now()
		e.ReEngagements = 0
	}
	// Machinery amnesty: attempts burned under an older fix-dispatch
	// generation don't count against the current one. Grant one fresh set
	// and pull the PR back out of the escalated (needs-human) state so the
	// current machinery gets its own chance before a human is paged again.
	if e.Machinery < MachineryVersion {
		e.Machinery = MachineryVersion
		e.ReEngagements = 0
		e.Escalated = false
		e.RedSHAs = nil
	}
	if e.ReEngagements >= MaxReEngagements {
		return false
	}
	e.ReEngagements++
	e.UpdatedAt = s.now()
	s.saveLocked()
	return true
}

// ReEngagements returns how many re-engagements have fired for the PR's current
// red SHA. Exposed for tests and observability.
func (s *Store) ReEngagements(repo string, number int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		return e.ReEngagements
	}
	return 0
}

func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

func containsSHA(shas []string, sha string) bool {
	for _, s := range shas {
		if s == sha {
			return true
		}
	}
	return false
}

// CommentBody renders the escalation comment posted on the PR. It leads with
// the raw CI evidence — the whole point is that a human (or the next agent
// pass) sees the actual error, not just "CI failed".
func CommentBody(attempts int, failingChecks []string, excerpt string) string {
	var b strings.Builder
	b.WriteString("## 🛑 Fix loop escalated — human attention needed\n\n")
	fmt.Fprintf(&b, "This PR has failed CI on **%d distinct fix attempts** (new commits, still red). ", attempts)
	b.WriteString("The hive has stopped dispatching further automated fixes for it.\n\n")
	if len(failingChecks) > 0 {
		sorted := append([]string(nil), failingChecks...)
		sort.Strings(sorted)
		fmt.Fprintf(&b, "**Failing checks:** %s\n\n", strings.Join(sorted, ", "))
	}
	if excerpt != "" {
		b.WriteString("**Raw failure evidence (from check-run annotations):**\n\n```\n")
		b.WriteString(excerpt)
		b.WriteString("\n```\n\n")
	}
	b.WriteString("Remove the `needs-human` label after addressing the root cause to return the PR to the automated fix lane.\n")
	return b.String()
}

// NeedsHumanLabel is the label applied to escalated PRs. Kick builders exclude
// items carrying it from fix dispatch.
const NeedsHumanLabel = "needs-human"
