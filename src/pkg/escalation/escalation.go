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

// ReEngageCooldown is the minimum spacing between two re-engagements of the
// SAME red head SHA.
//
// Without it, MaxReEngagements is not a budget of six ATTEMPTS — it is a budget
// of six governor TICKS. StaleRed gates on now-FirstRedAt, and FirstRedAt does
// not advance while the head SHA is unchanged, so once a red PR crosses
// RedPRStaleAfter it reads as stale on every subsequent tick, forever. The
// reaper then burned all six re-engagements back-to-back at tick cadence
// (~2 min) and the PR was escalated to needs-human ~13 minutes after it went
// stale — before the owning agent's next kick could even be built, let alone
// answered. Observed on kubestellar/console#23459 and #23475: both reached
// re_engagements=6 with exactly ONE entry in RedSHAs, i.e. they were parked for
// a human having never received a single repair attempt, which is precisely the
// outcome the re-engagement path exists to prevent.
//
// Spacing re-engagements by RedPRStaleAfter makes each one cost at least as
// much wall-clock as the staleness signal that justified it, so the six
// attempts span >=1h and a normally-cadenced agent gets real kicks in between.
const ReEngageCooldown = RedPRStaleAfter

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
	// LabelApplied records that the needs-human label was confirmed on the
	// forge (either we added it successfully or we observed it present). It
	// is what lets a LATER absence of the label be read as "a human removed
	// it — return the PR to the automated lane" rather than "our AddLabels
	// call failed last pass — retry it". Without this distinction a label
	// failure would un-park and re-escalate (re-comment) the PR every pass.
	LabelApplied bool `json:"label_applied,omitempty"`
	// LabelAppliedAt is when LabelApplied was set. An absence of the label
	// observed within LabelUnparkGrace of it is ignored rather than read as
	// a human un-park: the enumeration that feeds a pass can lag the label
	// write by a tick, and a stale listing must not bounce the PR out of and
	// back into escalation.
	LabelAppliedAt time.Time `json:"label_applied_at,omitempty"`
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
	// LastReEngagedAt is when the most recent re-engagement was granted for
	// CurRedSHA. ReEngageCooldown is enforced against it so the budget is
	// spent at the pace an agent can actually answer, not at governor-tick
	// pace. Reset alongside ReEngagements whenever CurRedSHA changes.
	LastReEngagedAt time.Time `json:"last_re_engaged_at,omitempty"`

	// Machinery is the MachineryVersion under which this entry's attempts
	// were burned. Older-generation entries are granted amnesty (see
	// MachineryVersion).
	Machinery int `json:"machinery,omitempty"`

	// ReviewerPassedSHA / ReviewerPassedAt record the reviewer-lane pass that
	// Sweep reconciled into this entry: the head SHA the reviewer left on the
	// branch, and when the hub first observed that verdict. They deliberately
	// SURVIVE the ledger reset that reconciliation performs, so a later
	// re-escalation can hand the PR to a human with the reviewer's context
	// attached (#5617 item 3) rather than the bare label set a human used to
	// get. They are cleared only with the entry itself: a PR that goes green
	// has converged, and a future regression starts a fresh story.
	ReviewerPassedSHA string    `json:"reviewer_passed_sha,omitempty"`
	ReviewerPassedAt  time.Time `json:"reviewer_passed_at,omitempty"`
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
	// Pending is set when this pass could NOT conclude CI state for the PR:
	// checks still running, no check runs yet, or the check-run fetch itself
	// failed (EnrichCIStatus maps an API error to "pending" too). A pending
	// observation is not evidence that the loop converged, so Sweep leaves
	// the entry exactly as it was — it neither counts an attempt nor forgets
	// the history. Before this flag existed every non-red pass was read as
	// green and wiped the entry, INCLUDING its Escalated marker, so one
	// transient API error was enough to make the hub re-post the "Fix loop
	// escalated" comment on the next red pass (tuna-os/corral#268 collected
	// fifteen identical escalation comments in 32 hours that way).
	Pending bool
	// Labeled reports that the forge already shows the needs-human label on
	// the PR. The label is the durable, human-visible record of escalation;
	// the ledger is a cache of it. Sweep treats a labeled PR as escalated no
	// matter what the ledger says (never re-comment), and treats a ledger
	// entry whose confirmed label has since disappeared as un-parked by a
	// human (fresh budget, back to the automated lane).
	Labeled bool
	Excerpt string
	// Labels are the PR's current forge labels. Sweep reads them to reconcile
	// reviewer-lane verdicts (which are expressed as label edits, possibly made
	// entirely outside the hub's view) back into the ledger — see the
	// reviewer-pass reset in Sweep.
	Labels []string
}

// Result reports the ledger's verdict for one observed PR.
type Result struct {
	Attempts    int
	Escalated   bool // escalation actions already fired (now or previously)
	NewlyEscala bool // this pass crossed the threshold — fire actions now
	// Exhausted is set when the escalation was (or is being) triggered by the
	// re-engagement budget running out on an UNCHANGED red head SHA rather than
	// by the distinct-SHA threshold. The evidence comment words the two cases
	// differently: "N distinct fix attempts" is misleading when N is 1.
	Exhausted bool
	// NeedsLabel is set for a PR the ledger already escalated whose needs-human
	// label is not confirmed on the forge and was not observed this pass — the
	// AddLabels call failed at escalation time. The caller should retry ONLY
	// the label (never the comment) and then call MarkLabelApplied.
	NeedsLabel bool
}

// PruneAfter is how long a ledger entry survives after its PR stops
// appearing in the enumerated open set before it is dropped. Merged and
// closed PRs age out; a PR that merely fell out of ONE pass (a per-repo
// listing error, pagination hiccup) keeps its history — and, crucially, its
// Escalated marker — instead of being re-counted from zero and re-escalated
// with a fresh comment when it reappears.
const PruneAfter = 24 * time.Hour

// LabelUnparkGrace is how long after the needs-human label was confirmed a
// pass must be before the label's ABSENCE is trusted as a deliberate removal
// by a human (see Entry.LabelAppliedAt).
const LabelUnparkGrace = 5 * time.Minute

// Sweep folds a full enumeration pass into the ledger: increments attempt
// counts for red PRs with unseen head SHAs, clears entries for PRs that went
// green, prunes PRs no longer present, and reports which PRs crossed the
// escalation threshold on this pass. Pending observations (checks still
// running) are no-ops: they neither count nor clear (#5617, gap G2). The
// caller performs the side effects (comment, label, notify) and then calls
// MarkEscalated for each.
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
		e := s.entries[key]

		// The forge label is authoritative in both directions.
		//
		// Label present: the PR IS escalated, whatever the ledger says. This
		// is what makes escalation idempotent across ledger loss (a wiped or
		// unwritable /data, a pruned entry, an amnesty pass): the comment can
		// never be posted twice onto a PR that already wears the label.
		if o.Labeled {
			if e == nil {
				e = &Entry{Machinery: MachineryVersion}
				s.entries[key] = e
			}
			e.Escalated = true
			if !e.LabelApplied {
				e.LabelApplied = true
				e.LabelAppliedAt = s.now()
			}
			e.Machinery = MachineryVersion
			if o.Red && o.HeadSHA != "" && !containsSHA(e.RedSHAs, o.HeadSHA) {
				e.RedSHAs = appendSHA(e.RedSHAs, o.HeadSHA)
			}
			if o.Excerpt != "" {
				e.LastExcerpt = o.Excerpt
			}
			e.UpdatedAt = s.now()
			results[key] = Result{Attempts: len(e.RedSHAs), Escalated: true}
			continue
		}
		// Label absent but the ledger says we escalated AND confirmed the
		// label: a human took the label off to return the PR to the automated
		// lane ("Remove the needs-human label after addressing the root
		// cause"). Honour that — fresh budget, fresh distinct-SHA count —
		// instead of leaving the ledger's Escalated flag to keep the PR
		// hands-off forever, which is what happened before.
		if e != nil && e.Escalated && e.LabelApplied && s.now().Sub(e.LabelAppliedAt) >= LabelUnparkGrace {
			e.Escalated = false
			e.LabelApplied = false
			e.LabelAppliedAt = time.Time{}
			e.RedSHAs = nil
			e.ReEngagements = 0
			e.LastReEngagedAt = time.Time{}
			e.UpdatedAt = s.now()
		}

		if o.Pending {
			// Inconclusive pass: keep the entry exactly as it is. It is
			// neither a converged loop (which would clear history) nor a new
			// failed attempt. Report the current state so callers keep
			// treating an already-escalated PR as hands-off.
			if e != nil {
				e.UpdatedAt = s.now()
				results[key] = Result{
					Attempts:   len(e.RedSHAs),
					Escalated:  e.Escalated,
					Exhausted:  e.ReEngagements >= MaxReEngagements,
					NeedsLabel: e.Escalated && !e.LabelApplied,
				}
			}
			continue
		}
		if !o.Red {
			// Green: the loop converged — forget the history entirely so a
			// future regression on the same PR starts a fresh count.
			delete(s.entries, key)
			continue
		}
		if e == nil {
			e = &Entry{Machinery: MachineryVersion}
			s.entries[key] = e
		}
		// Machinery amnesty (see MachineryVersion): attempts and escalations
		// burned under an older fix-dispatch generation are wiped once, and
		// the distinct-SHA ledger restarts, so the CURRENT machinery gets its
		// own budget before a human is paged again. Without the RedSHAs reset
		// the very next sweep would re-escalate on the old ledger. (A PR that
		// still wears the label was handled above and stays parked: the
		// forge, not the ledger, is the record of escalation.)
		if e.Machinery < MachineryVersion {
			e.Machinery = MachineryVersion
			e.ReEngagements = 0
			e.LastReEngagedAt = time.Time{}
			e.Escalated = false
			e.LabelApplied = false
			e.RedSHAs = nil
		}
		// Reviewer-verdict reconciliation (#5511, gap G1): the reviewer lane's
		// REPAIR/DE-ESCALATE verdict is expressed as label edits only —
		// needs-human removed, reviewer-passed added — usually via a direct
		// `gh pr edit` the hub never observes. Without syncing that verdict
		// into the ledger the entry stays Escalated forever, and if the
		// reviewer's pushed fix goes red again the PR is orphaned: the fix
		// lane and the reaper skip escalated rows, reviewer-passed excludes it
		// from the reviewer lane, and NewlyEscala requires !Escalated so
		// needs-human can never re-fire (proven by the Spin model in
		// src/formal/escalation, property P5). Reset the entry the way
		// machinery amnesty does — un-escalate, restart the distinct-SHA
		// ledger, fresh re-engagement budget, Machinery untouched — so the PR
		// re-enters the normal lifecycle. A later re-escalation then fires
		// normally, and the reviewer-passed label routes it to a human rather
		// than back to the reviewer.
		if e.Escalated && containsLabel(o.Labels, ReviewerPassedLabel) && !containsLabel(o.Labels, NeedsHumanLabel) {
			e.Escalated = false
			e.RedSHAs = nil
			e.ReEngagements = 0
			// Remember WHAT the reviewer left on the branch and WHEN, so a
			// later re-escalation can hand the human the reviewer's context
			// instead of a bare label (#5617 item 3). Keyed on the SHA so a
			// repeat reconciliation of the same verdict cannot walk the
			// timestamp forward and misdate the hand-off.
			if e.ReviewerPassedSHA != o.HeadSHA {
				e.ReviewerPassedSHA = o.HeadSHA
				e.ReviewerPassedAt = s.now()
			}
		}
		if o.HeadSHA != "" && !containsSHA(e.RedSHAs, o.HeadSHA) {
			e.RedSHAs = appendSHA(e.RedSHAs, o.HeadSHA)
		}
		// Maintain the zero-API staleness clock: a change of red head SHA means
		// the branch moved (a fix was pushed, still red) — reset the first-seen
		// time and the per-SHA re-engagement counter so a freshly-pushed red SHA
		// is treated as "just started", not stale.
		if o.HeadSHA != "" && o.HeadSHA != e.CurRedSHA {
			e.CurRedSHA = o.HeadSHA
			e.FirstRedAt = s.now()
			e.ReEngagements = 0
			e.LastReEngagedAt = time.Time{}
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
			Exhausted:   exhausted && len(e.RedSHAs) < threshold,
			NeedsLabel:  e.Escalated && !e.LabelApplied,
		}
	}
	// Prune PRs that left the open set (merged or closed) — but only once
	// they have been gone for PruneAfter, so one pass that failed to list a
	// repo does not erase the ledger for every PR in it.
	now := s.now()
	for key, e := range s.entries {
		if !seen[key] && now.Sub(e.UpdatedAt) >= PruneAfter {
			delete(s.entries, key)
		}
	}
	s.saveLocked()
	return results
}

// appendSHA appends sha to the distinct red-SHA history, bounded to
// maxTrackedSHAs (oldest dropped first).
func appendSHA(shas []string, sha string) []string {
	shas = append(shas, sha)
	if len(shas) > maxTrackedSHAs {
		shas = shas[len(shas)-maxTrackedSHAs:]
	}
	return shas
}

// MarkEscalated records that escalation side effects fired for the PR, so they
// are never repeated.
func (s *Store) MarkEscalated(repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		e.Escalated = true
		e.UpdatedAt = s.now()
	}
	s.saveLocked()
}

// MarkLabelApplied records that the needs-human label is confirmed present
// on the forge for the PR. Call it after a successful AddLabels. From then on
// a pass that observes the label ABSENT is read as a deliberate human
// un-park (see Sweep), not as a failed label call to retry.
func (s *Store) MarkLabelApplied(repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		e.LabelApplied = true
		e.LabelAppliedAt = s.now()
		e.UpdatedAt = s.now()
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

// ReviewerPass reports the reviewer-lane pass recorded for a PR: the head SHA
// the reviewer left on the branch, and when Sweep reconciled that verdict into
// the ledger. ok is false when no reviewer has ever passed on this PR — which
// is what routes the escalation sweep between the plain first-escalation
// comment and the structured hand-off note.
func (s *Store) ReviewerPass(repo string, number int) (sha string, at time.Time, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[Key(repo, number)]
	if e == nil || e.ReviewerPassedAt.IsZero() {
		return "", time.Time{}, false
	}
	return e.ReviewerPassedSHA, e.ReviewerPassedAt, true
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
		if o.Pending {
			// Inconclusive pass (checks running, or the check fetch failed):
			// neither red nor green, so the staleness clock keeps whatever
			// it had. Clearing it here would let a single API error make a
			// stuck PR look freshly red again on the next pass.
			continue
		}
		if !o.Red {
			// Green now: drop the staleness/re-engagement record so a future
			// regression starts a fresh clock. (Attempt history is managed by
			// Sweep, not here.)
			if e := s.entries[key]; e != nil {
				e.CurRedSHA = ""
				e.FirstRedAt = time.Time{}
				e.ReEngagements = 0
				e.LastReEngagedAt = time.Time{}
			}
			continue
		}
		if o.HeadSHA == "" {
			continue
		}
		e := s.entries[key]
		if e == nil {
			e = &Entry{Machinery: MachineryVersion}
			s.entries[key] = e
		}
		if o.HeadSHA != e.CurRedSHA {
			e.CurRedSHA = o.HeadSHA
			e.FirstRedAt = now
			e.ReEngagements = 0
			e.LastReEngagedAt = time.Time{}
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
		e = &Entry{Machinery: MachineryVersion}
		s.entries[key] = e
	}
	// If the tracked SHA differs from the observed head, sync to the observed
	// head and reset the counter — the branch moved.
	if headSHA != "" && headSHA != e.CurRedSHA {
		e.CurRedSHA = headSHA
		e.FirstRedAt = s.now()
		e.ReEngagements = 0
		e.LastReEngagedAt = time.Time{}
	}
	// Machinery amnesty: attempts burned under an older fix-dispatch
	// generation don't count against the current one. Grant one fresh set
	// and pull the PR back out of the escalated (needs-human) state so the
	// current machinery gets its own chance before a human is paged again.
	if e.Machinery < MachineryVersion {
		e.Machinery = MachineryVersion
		e.ReEngagements = 0
		e.LastReEngagedAt = time.Time{}
		e.Escalated = false
		e.LabelApplied = false
		e.LabelAppliedAt = time.Time{}
		e.RedSHAs = nil
	}
	// An escalated (needs-human) entry is out of the automated lane entirely:
	// re-engaging it would burn budget on a PR the fix loop is standing down
	// from and let a caller log "re-engaged fix loop" for a human-parked PR
	// (#5511, gap G3 — the merge-request watcher's terminal path reaches here
	// without consulting the escalated set, unlike the governor reaper). The
	// amnesty above may have just un-escalated an older-generation entry, in
	// which case re-engagement proceeds on its fresh budget as intended.
	if e.Escalated {
		return false
	}
	if e.ReEngagements >= MaxReEngagements {
		return false
	}
	// Budget is spent at agent pace, not tick pace: a stale red SHA re-reads as
	// stale every tick, so without this the whole budget evaporates in minutes.
	if !e.LastReEngagedAt.IsZero() && s.now().Sub(e.LastReEngagedAt) < ReEngageCooldown {
		return false
	}
	e.ReEngagements++
	e.LastReEngagedAt = s.now()
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

func containsLabel(labels []string, label string) bool {
	for _, l := range labels {
		if l == label {
			return true
		}
	}
	return false
}

// ReviewerHandoff is the record of a completed reviewer-lane pass, carried
// into the escalation comment when a PR the reviewer already adjudicated goes
// red again. SHA is the head commit the reviewer left on the branch; At is
// when Sweep reconciled that verdict into the ledger.
type ReviewerHandoff struct {
	SHA string
	At  time.Time
}

// CommentBody renders the escalation comment posted on the PR. It leads with
// the raw CI evidence — the whole point is that a human (or the next agent
// pass) sees the actual error, not just "CI failed".
//
// exhausted selects the wording for the second trigger: the re-engagement
// budget ran out on a head SHA that never moved (no fix was ever pushed), as
// opposed to the distinct-SHA threshold ("N fix attempts, still red").
func CommentBody(attempts int, failingChecks []string, excerpt string, exhausted bool) string {
	return commentBody(attempts, failingChecks, excerpt, exhausted, nil)
}

// HandoffCommentBody renders the escalation comment for a PR that has ALREADY
// had a reviewer-lane pass (#5617 item 3). The lane is a one-pass ladder: a PR
// that re-escalates after `reviewer-passed` is excluded from the reviewer work
// list forever and belongs to a true human. Until now that human's only
// context was the label set — nothing distinguished a first escalation from a
// second one, nor said that a mechanical repair had already been tried and had
// not held. This body says both, and points at the reviewer's own audited
// record rather than restating it (the hub never saw the reviewer's reasoning;
// claiming to summarise it would be invention).
func HandoffCommentBody(attempts int, failingChecks []string, excerpt string, exhausted bool, h ReviewerHandoff) string {
	return commentBody(attempts, failingChecks, excerpt, exhausted, &h)
}

func commentBody(attempts int, failingChecks []string, excerpt string, exhausted bool, h *ReviewerHandoff) string {
	var b strings.Builder
	b.WriteString("## 🛑 Fix loop escalated — human attention needed\n\n")
	if exhausted {
		fmt.Fprintf(&b, "This PR has stayed red on the same commit through **%d automated fix re-dispatches** with no new commit pushed (%d distinct red head%s seen). ", MaxReEngagements, attempts, plural(attempts))
	} else {
		fmt.Fprintf(&b, "This PR has failed CI on **%d distinct fix attempts** (new commits, still red). ", attempts)
	}
	b.WriteString("The hive has stopped dispatching further automated fixes for it.\n\n")
	if h != nil {
		b.WriteString("### A reviewer already adjudicated this PR — this is the second failure\n\n")
		b.WriteString("The reviewer lane repaired or de-escalated this PR once and returned it to the\n")
		b.WriteString("automated lane; that is what the `" + ReviewerPassedLabel + "` label records.\n\n")
		if h.SHA != "" {
			fmt.Fprintf(&b, "- **Head commit the reviewer left on the branch:** `%s`\n", h.SHA)
		}
		if !h.At.IsZero() {
			fmt.Fprintf(&b, "- **Returned to the automated lane:** %s\n", h.At.UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "- **Since then:** %d distinct fix attempts, all still red. That count is\n", attempts)
		b.WriteString("  measured FROM the reviewer's pass — the ledger restarts at the verdict.\n\n")
		b.WriteString("What the reviewer tried, and why it believed the repair sufficient, is recorded on\n")
		b.WriteString("this PR: look for its `Reviewer adjudication:` comment (posted through the hive\n")
		b.WriteString("relay and attributed as `agent_pr_reviewed`) and the matching advisory bead. Read\n")
		b.WriteString("that first — the repair it describes has now been tried and has not held, so\n")
		b.WriteString("repeating it is the one approach already known to fail here.\n\n")
		b.WriteString("**No further automated pass is coming.** One reviewer pass per PR is the whole\n")
		b.WriteString("ladder: the reviewer work list excludes `" + ReviewerPassedLabel + "` rows permanently, so from\n")
		b.WriteString("here this PR is a human's or it is nobody's.\n\n")
	}
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

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// NeedsHumanLabel is the label applied to escalated PRs. Kick builders exclude
// items carrying it from fix dispatch.
const NeedsHumanLabel = "needs-human"

// ReviewerPassedLabel marks a PR the reviewer lane (#5480) has repaired or
// de-escalated once. Canonical here because Sweep's reviewer-verdict
// reconciliation reads it against the ledger; the scheduler's reviewer lane
// (pkg/scheduler) re-exports it for the work-list builder and kick contract.
const ReviewerPassedLabel = "reviewer-passed"

// HasNeedsHumanLabel reports whether labels (as enumerated from the forge)
// contains NeedsHumanLabel. Case-insensitive: GitHub label matching is.
func HasNeedsHumanLabel(labels []string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, NeedsHumanLabel) {
			return true
		}
	}
	return false
}
