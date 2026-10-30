// Package rebasemerge is the "Rebase, then merge as maintainer" follow-through
// (hivecommons/hive: #14822 sat 14h at 085 behind v5 with no lane owning the
// rebase, and "Merge now as maintainer" on a dirty PR would have been a
// GitHub 405). An operator's merge-now on a PR that is behind or conflicting
// records the approval first, arms a follow-through here, and the hive does
// the rest: a fixer rebases the branch (the merge-eligibility router admits
// an armed PR to its conflict route whoever authored it), the dashboard's
// ticker watches the new head, merges it on green with the approval on
// record, or fails the action with the conflicting files and the two ways
// out (adopt into a fresh branch / close as superseded).
//
// The state machine is pure (Advance) so it can be tested without GitHub;
// the store is shared between the dashboard (arms, drives) and cmd/hive
// (routes the rebase).
package rebasemerge

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StatePath is where the shared store persists; /data is the hive's volume.
var StatePath = "/data/rebase-merge-followups.json"

// States of a follow-through, in order.
const (
	// StateApproved: the operator's approval is on record; the rebase kick
	// has not been observed as a new head yet.
	StateApproved = "approved"
	// StateRebasing: a fixer owns the rebase (the PR is in the conflict
	// route); still the head the operator approved.
	StateRebasing = "rebasing"
	// StateCIPending: a new head landed (the rebase); waiting for GitHub to
	// report it clean.
	StateCIPending = "ci-pending"
	StateMerged    = "merged"
	StateFailed    = "failed"
)

// Steps recorded on the follow-through (and mirrored into the operator
// ledger's followed[]).
const (
	StepApprovalRecorded = "approval-recorded"
	StepRebaseKicked     = "rebase-kicked"
	StepRebased          = "rebased"
	StepStillConflicting = "still-conflicting"
	StepCIGreen          = "ci-green"
	StepMerged           = "merged"
	StepFailed           = "failed"
)

// Failure reasons.
const (
	FailConflict     = "rebase-conflict"
	FailCIRed        = "ci-red"
	FailTimeout      = "rebase-timeout"
	FailClosed       = "pr-closed"
	FailNotReachable = "head-not-pushable"
	FailWorkflowPush = "workflow-push-unavailable"
	// FailForeignHead: the head moved after the approval, but not by the
	// hive — a person pushed new content. The approval was bound to the
	// head the operator read, so the follow-through stops instead of
	// merging content nobody approved.
	FailForeignHead = "foreign-head"
)

// Options the failed outcome offers (decision action ids the SPA knows).
const (
	OptionAdoptFresh       = "decide:adopt"
	OptionCloseSuperseded  = "decide:close-superseded"
	OptionReapprove        = "decide:merge-now"
	OptionAdoptFreshLabel  = "Adopt into fresh branch"
	OptionCloseSupersLabel = "Close as superseded"
	OptionReapproveLabel   = "Approve the new head & merge"
)

// Default timing. RebaseDeadline bounds how long a kicked rebase may sit
// without a new head; CIDeadline bounds how long the rebased head may sit
// not-clean before the follow-through gives up.
const (
	DefaultRebaseDeadline = 2 * time.Hour
	DefaultCIDeadline     = 3 * time.Hour
)

// Step is one recorded move of the follow-through.
type Step struct {
	At     time.Time `json:"at"`
	Step   string    `json:"step"`
	Detail string    `json:"detail,omitempty"`
}

// FollowThrough is one armed rebase-then-merge.
type FollowThrough struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// Actor is the operator whose approval is on record; Base the branch
	// the PR is rebased onto.
	Actor string `json:"actor"`
	Base  string `json:"base,omitempty"`
	// ApprovedHead is the head the operator approved; RebasedHead the head
	// the rebase produced (set once observed). HiveAuthored says whether
	// the PR is the hive's own (the self lane rebases) or a contributor's
	// (push-reachable fixer).
	ApprovedHead string `json:"approved_head"`
	RebasedHead  string `json:"rebased_head,omitempty"`
	// ForeignHead is the head a person (not the hive) pushed after the
	// approval, when that is why the follow-through failed.
	ForeignHead  string    `json:"foreign_head,omitempty"`
	HiveAuthored bool      `json:"hive_authored,omitempty"`
	State        string    `json:"state"`
	ArmedAt      time.Time `json:"armed_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Steps        []Step    `json:"steps,omitempty"`
	// FailReason / ConflictFiles / Options describe a failed outcome.
	FailReason    string   `json:"fail_reason,omitempty"`
	ConflictFiles []string `json:"conflict_files,omitempty"`
	Options       []string `json:"options,omitempty"`
	MergeSHA      string   `json:"merge_sha,omitempty"`
}

// Live reports whether the follow-through still wants the hive's work.
func (f FollowThrough) Live() bool {
	return f.State == StateApproved || f.State == StateRebasing || f.State == StateCIPending
}

// Observation is what one poll of GitHub saw.
type Observation struct {
	Now   time.Time
	Head  string
	State string // open / closed
	// HeadByHive is true when Head (if it is not the approved head) was
	// committed by the hive — the fixer's rebase or GitHub's update-branch
	// under the App. A head somebody else pushed after the approval is new
	// content, not a rebase, and the follow-through fails closed on it.
	HeadByHive     bool
	Merged         bool
	MergeableState string // clean / unstable / blocked / dirty / behind / unknown
	// CIRed is true when a required check on Head failed.
	CIRed bool
	// ConflictFiles are the files changed on both sides since the merge
	// base, when the poller worked them out.
	ConflictFiles []string
	// RebaseAttempt / RebaseExhausted mirror the fixer loop guard
	// (Attempts): which rebase-resolve attempt the current dirty head is,
	// and whether the hive has run out. A rebased head that is still dirty
	// goes back to rebasing while attempts remain; once exhausted it fails
	// with the conflict for a person.
	RebaseAttempt   int
	RebaseExhausted bool
}

// Action is what the driver must do after Advance.
type Action string

const (
	ActionNone  Action = ""
	ActionMerge Action = "merge"
	ActionFail  Action = "fail"
)

// Deadlines bounds the follow-through.
type Deadlines struct {
	Rebase time.Duration
	CI     time.Duration
}

func (d Deadlines) orDefault() Deadlines {
	if d.Rebase <= 0 {
		d.Rebase = DefaultRebaseDeadline
	}
	if d.CI <= 0 {
		d.CI = DefaultCIDeadline
	}
	return d
}

func (f *FollowThrough) step(now time.Time, step, detail string) {
	f.Steps = append(f.Steps, Step{At: now, Step: step, Detail: detail})
	f.UpdatedAt = now
}

// rebaseSince is when the current rebase wait started: arming, or the
// last time a rebased head came back still conflicting.
func (f *FollowThrough) rebaseSince() time.Time {
	since := f.ArmedAt
	for _, st := range f.Steps {
		if st.Step == StepStillConflicting && st.At.After(since) {
			since = st.At
		}
	}
	return since
}

func (f *FollowThrough) fail(now time.Time, reason string, files []string) {
	f.State = StateFailed
	f.FailReason = reason
	f.ConflictFiles = slices.Clone(files)
	f.Options = []string{OptionAdoptFresh, OptionCloseSuperseded}
	if reason == FailWorkflowPush {
		// Workflow-path supersession is also fenced by the adoption lane.
		f.Options = nil
	}
	if reason == FailForeignHead {
		f.Options = []string{OptionReapprove, OptionCloseSuperseded}
	}
	f.step(now, StepFailed, reason)
}

// Arm builds a fresh follow-through with the approval recorded as its first
// step: the approval is the durable record, everything after is the hive's.
func Arm(repo string, number int, actor, base, head string, hiveAuthored bool, now time.Time) FollowThrough {
	f := FollowThrough{Repo: repo, Number: number, Actor: actor, Base: base, ApprovedHead: head, HiveAuthored: hiveAuthored, State: StateApproved, ArmedAt: now, UpdatedAt: now}
	f.step(now, StepApprovalRecorded, "approved by @"+actor+" on "+short(head))
	return f
}

// Kicked records that the rebase is in a fixer's hands.
func (f *FollowThrough) Kicked(now time.Time, how string) {
	if f.State != StateApproved {
		return
	}
	f.State = StateRebasing
	f.step(now, StepRebaseKicked, how)
}

// Advance folds one observation into the follow-through and says what the
// driver does next. It is pure: no GitHub calls, no clock reads.
//
//	approved/rebasing + new head            → ci-pending (step rebased)
//	ci-pending + clean                      → merge (step ci-green)
//	any live + dirty past rebase deadline   → fail rebase-conflict (files)
//	any live + CI red on the rebased head   → fail ci-red
//	ci-pending past CI deadline             → fail rebase-timeout
//	closed / merged elsewhere               → merged | failed pr-closed
func Advance(f *FollowThrough, obs Observation, d Deadlines) Action {
	if f == nil || !f.Live() {
		return ActionNone
	}
	d = d.orDefault()
	now := obs.Now
	if obs.Merged {
		f.State = StateMerged
		f.step(now, StepMerged, "merged outside the follow-through")
		return ActionNone
	}
	if obs.State != "" && !strings.EqualFold(obs.State, "open") {
		f.fail(now, FailClosed, nil)
		return ActionFail
	}
	head := strings.TrimSpace(obs.Head)
	if head != "" && head != f.ApprovedHead && head != f.RebasedHead {
		if !f.HiveAuthored && !obs.HeadByHive {
			f.ForeignHead = head
			f.fail(now, FailForeignHead, nil)
			f.Steps[len(f.Steps)-1].Detail = "head " + short(head) + " was pushed by someone other than the hive; the approval was for " + short(f.ApprovedHead)
			return ActionFail
		}
		f.RebasedHead = head
		f.State = StateCIPending
		f.step(now, StepRebased, "new head "+short(head)+" onto "+f.Base)
	}
	ms := strings.ToLower(strings.TrimSpace(obs.MergeableState))
	switch f.State {
	case StateApproved, StateRebasing:
		if ms == "clean" {
			// The approved head became mergeable without a rebase (the
			// label was stale, or base moved back under it).
			f.step(now, StepCIGreen, "GitHub reports "+short(f.ApprovedHead)+" clean")
			return ActionMerge
		}
		if ms == "dirty" && (obs.RebaseExhausted || now.Sub(f.rebaseSince()) >= d.Rebase) {
			f.fail(now, FailConflict, obs.ConflictFiles)
			return ActionFail
		}
		if ms != "dirty" && now.Sub(f.rebaseSince()) >= d.Rebase {
			f.fail(now, FailTimeout, nil)
			return ActionFail
		}
	case StateCIPending:
		if obs.CIRed {
			f.fail(now, FailCIRed, nil)
			return ActionFail
		}
		if ms == "dirty" {
			// The base moved again under the rebased head (or the fixer's
			// resolution did not take). While the loop guard allows it the
			// fixer gets another go; past it the conflict is a person's.
			if obs.RebaseExhausted {
				f.fail(now, FailConflict, obs.ConflictFiles)
				return ActionFail
			}
			f.State = StateRebasing
			f.step(now, StepStillConflicting, "head "+short(f.RebasedHead)+" still conflicting — rebase attempt "+strconv.Itoa(max(obs.RebaseAttempt, 1))+"/"+strconv.Itoa(MaxRebaseAttempts))
			return ActionNone
		}
		if ms == "clean" {
			f.step(now, StepCIGreen, "GitHub reports "+short(f.RebasedHead)+" clean")
			return ActionMerge
		}
		if now.Sub(f.UpdatedAt) >= d.CI {
			f.fail(now, FailTimeout, nil)
			return ActionFail
		}
	}
	return ActionNone
}

// Merged records the merge the driver performed.
func (f *FollowThrough) Merged(now time.Time, sha string) {
	f.State = StateMerged
	f.MergeSHA = sha
	f.step(now, StepMerged, "merged "+short(sha)+" with @"+f.Actor+"'s approval on record")
}

// FailNow fails the follow-through from the driver (a merge call that GitHub
// refused, a head nobody can push).
func (f *FollowThrough) FailNow(now time.Time, reason string, files []string) {
	f.fail(now, reason, files)
}

// FailureText is the one-line toast for a failed follow-through.
func (f FollowThrough) FailureText() string {
	switch f.FailReason {
	case FailConflict:
		msg := fmt.Sprintf("Rebase of #%d onto %s conflicts", f.Number, nonEmpty(f.Base, "its base"))
		if len(f.ConflictFiles) > 0 {
			msg += " in " + strings.Join(f.ConflictFiles, ", ")
		}
		return msg + "."
	case FailCIRed:
		return fmt.Sprintf("CI went red on the rebased head of #%d; nothing merged.", f.Number)
	case FailTimeout:
		return fmt.Sprintf("The rebase of #%d did not land in time; nothing merged.", f.Number)
	case FailClosed:
		return fmt.Sprintf("#%d was closed before the hive could merge it.", f.Number)
	case FailNotReachable:
		return fmt.Sprintf("The hive cannot push to #%d's head (fork without maintainer edits).", f.Number)
	case FailWorkflowPush:
		return fmt.Sprintf("The hive cannot rebase #%d's fork branch with workflow changes (%s). A person with workflow push access must rebase the branch, then approve and merge it again.", f.Number, strings.Join(f.ConflictFiles, ", "))
	case FailForeignHead:
		return fmt.Sprintf("#%d's head moved after your approval, and not by the hive's rebase: someone pushed %s. Your approval was for %s, so nothing merged; read the new head and approve it again.", f.Number, nonEmpty(short(f.ForeignHead), "a new head"), nonEmpty(short(f.ApprovedHead), "the head you saw"))
	}
	return fmt.Sprintf("Rebase-then-merge of #%d failed: %s.", f.Number, f.FailReason)
}

// Store is the persisted set of follow-throughs keyed by repo#number.
type Store struct {
	mu     sync.Mutex
	path   string
	loaded bool
	items  map[string]FollowThrough
}

// NewStore returns a store at path ("" keeps it in memory).
func NewStore(path string) *Store {
	return &Store{path: path, items: map[string]FollowThrough{}}
}

var (
	sharedOnce sync.Once
	shared     *Store
)

// Shared is the process-wide store at StatePath.
func Shared() *Store {
	sharedOnce.Do(func() { shared = NewStore(StatePath) })
	return shared
}

// Key is the store key for repo#number.
func Key(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

func (s *Store) load() {
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
	var items map[string]FollowThrough
	if json.Unmarshal(data, &items) == nil && items != nil {
		s.items = items
	}
}

func (s *Store) persist() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ErrAlreadyArmed is returned by Put for a live follow-through on the key.
var ErrAlreadyArmed = errors.New("rebase-then-merge already armed")

// Put stores f, refusing to replace a live follow-through on the same PR.
func (s *Store) Put(f FollowThrough) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	key := Key(f.Repo, f.Number)
	if prev, ok := s.items[key]; ok && prev.Live() && prev.ArmedAt != f.ArmedAt {
		return ErrAlreadyArmed
	}
	s.items[key] = f
	return s.persist()
}

// Update replaces f unconditionally (the driver advancing it).
func (s *Store) Update(f FollowThrough) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	s.items[Key(f.Repo, f.Number)] = f
	return s.persist()
}

// Get returns the follow-through on repo#number.
func (s *Store) Get(repo string, number int) (FollowThrough, bool) {
	if s == nil {
		return FollowThrough{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	f, ok := s.items[Key(repo, number)]
	return f, ok
}

// Armed reports a live follow-through on repo#number: the hive owns this
// PR's rebase whoever authored it.
func (s *Store) Armed(repo string, number int) bool {
	f, ok := s.Get(repo, number)
	return ok && f.Live()
}

// Live lists every live follow-through, oldest armed first.
func (s *Store) Live() []FollowThrough {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var out []FollowThrough
	for _, f := range s.items {
		if f.Live() {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b FollowThrough) int { return a.ArmedAt.Compare(b.ArmedAt) })
	return out
}

func short(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
