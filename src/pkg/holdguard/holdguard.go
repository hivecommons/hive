// Package holdguard closes the hold-gate branch-hygiene gap (#5589): a PR
// sitting hold-gated can silently accumulate commits from OTHER authors or
// agents (a worktree cut from a contaminated base, a helpful agent pushing to
// someone else's branch), and nothing re-reviewed that drift before the hold
// lifted and the merge lanes re-opened. The human gate only works if the diff
// the human approved is the diff that merges.
//
// The guard is deterministic hub-side machinery, parallel to (and deliberately
// independent of) the fix-loop escalation ledger in pkg/escalation — it never
// touches escalation.Entry semantics, so the formally verified escalation
// invariants are unaffected. Mechanics:
//
//  1. While a PR is held, the governor snapshots its head SHA plus the commit
//     SHA / author sets into this ledger (first-held snapshot wins — that is
//     the tree the hold gate showed the human).
//  2. When the hold lifts, the snapshot is compared against the current head.
//     Head unchanged ⇒ the author set cannot have changed either (the head SHA
//     pins the entire history) — the entry clears and merge lanes reopen.
//  3. Head moved (new commits, force-push, rebase) ⇒ the PR is kept out of
//     merge eligibility, a one-time evidence comment names the new commits and
//     authors (plain text, never @-mentions), the hold label is re-applied so
//     EVERY merge lane re-gates, and the snapshot re-arms at the drifted head.
//     A human lifting the re-applied hold after reading the evidence IS the
//     fresh approval — that lift compares clean and the PR proceeds.
//  4. Head moved, but a trusted human lifted the gate AFTER the current head
//     arrived (a rebase landed earlier during the hold, then the human
//     reviewed the result and lifted) ⇒ the lift covered the current head,
//     so the entry clears like step 2 instead of re-holding the PR a human
//     just reviewed (#13603). That the head predates the lift is proven only
//     by evidence the client cannot forge: the server-stamped
//     `head_ref_force_pushed` event that produced it (LiftCoversCurrentHead),
//     or the hive's own observation of that exact head on the PR while the
//     gate was still on (LiftCoversObservedHead). Commit dates are
//     client-set and never trusted, and SHA-keyed objects (check suites,
//     statuses) are not used: a SHA pushed elsewhere before the lift carries
//     pre-lift timestamps even if it reaches the branch after. A push the
//     hive did not see while held, a lift by the hive or any bot, or no
//     evidence at all still takes step 3.
//
// Held-gated PRs must also stay mergeable, not just frozen (#10437): while a
// PR is held, the hive may merge its base in with its own identity to clear a
// "behind"/"dirty" mergeable state. That is hygiene, not reviewable content,
// so AdvanceHygiene moves the ledger's baseline to the resulting head instead
// of letting step 2/3 read it as drift on the next lift. When the hive cannot
// resolve a real conflict this way, a one-time comment (ConflictCommentBody)
// tells the human before they open the review, instead of letting them
// discover a stale, unmergeable PR mid-review.
//
// Each merge-from-base is one attempt per (head, base) pair. GitHub answers
// update-branch with 202 and merges later, so an accepted attempt whose new
// head is not visible yet is recorded as requested (MarkUpdateRequested) and
// waited on rather than re-sent; one still unmoved after UpdateConfirmWindow
// stops like a refusal. A refusal the same token would get again (workflows
// permission, a fork the App cannot write to, a conflict) or an accepted
// update that never landed stops attempts for that head and base
// (MarkUpdateStopped) until either moves, and a one-time note
// (UpdateStoppedCommentBody) tells a person to update the branch.
package holdguard

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

// ReHoldLabel is the label the guard re-applies to a drifted PR. It MUST be a
// member of github.HoldLabels so the re-applied label actually re-engages the
// hold machinery (enumeration moves the PR back into Hold.Items and both
// auto-merge sweeps skip it); cmd/hive pins that agreement with a test.
const ReHoldLabel = "hold"

// Retention is how long an entry survives without being observed (held, lifted
// clean, or drifted). Entries for PRs that merged or closed while held have no
// clearing event, so age is the only safe reaper. Fourteen days matches the
// governor's audit PR-attribution window; deliberately NOT a prune-on-absence
// sweep like pkg/escalation uses, because forgetting a snapshot after one
// failed repo enumeration would let the very next tick re-snapshot a
// contaminated head as if a human had approved it.
const Retention = 14 * 24 * time.Hour

// maxTrackedCommits bounds the per-PR snapshot so a pathological branch cannot
// grow the ledger without limit. Matches GitHub's PR commit-list ceiling.
const maxTrackedCommits = 250

// maxListedCommits bounds how many new commits the drift comment enumerates
// line-by-line; the remainder is summarized as a count.
const maxListedCommits = 10

// shortSHALen is the abbreviated commit-SHA length used in the drift comment.
const shortSHALen = 12

// Commit is one commit on a hold-gated PR's branch: enough identity for the
// snapshot sets and the drift comment, nothing more.
type Commit struct {
	SHA string `json:"sha"`
	// Author is the forge login when known, otherwise the git author name.
	Author string `json:"author,omitempty"`
	// Title is the first line of the commit message.
	Title string `json:"title,omitempty"`
}

// Entry is the persisted per-PR snapshot taken while the PR sits hold-gated.
type Entry struct {
	// HeadSHA is the branch head observed when the PR was (last) snapshotted —
	// the tree the hold gate showed the human. An unchanged head at lift time
	// proves the entire history is unchanged.
	HeadSHA string `json:"head_sha"`
	// CommitSHAs are the branch's commit SHAs at snapshot time (bounded by
	// maxTrackedCommits). Empty when the commit list was unavailable; the
	// drift diff then fails toward treating every current commit as new.
	CommitSHAs []string `json:"commit_shas,omitempty"`
	// Authors is the sorted, de-duplicated author set at snapshot time.
	Authors []string `json:"authors,omitempty"`
	// Commented is set once the drift comment posted for the CURRENT drift
	// episode, so a retried label write never re-comments. ReArm resets it —
	// a later, separate drift is a new episode and earns fresh evidence.
	Commented bool `json:"commented,omitempty"`
	// ConflictNoted is set once the "can't keep this mergeable" comment
	// posted for the CURRENT conflict episode (#10437), mirroring Commented's
	// dedup role for drift. AdvanceHygiene resets it the moment a
	// merge-from-base succeeds — the conflict that earned the notice is
	// gone, so a stale "can't merge" comment must not linger unexplained.
	ConflictNoted bool `json:"conflict_noted,omitempty"`
	// WorkflowRefusedHeadSHA is the PR head GitHub refused to update from its
	// base because the base changed .github/workflows/ and the App lacks the
	// workflows permission. Written by builds before UpdateStopped* existed;
	// still read (UpdateStoppedFor) so such a record does not retry, never
	// written now.
	WorkflowRefusedHeadSHA string `json:"workflow_refused_head_sha,omitempty"`
	// UpdateRequestedHeadSHA and UpdateRequestedBaseSHA are the head and base
	// of a merge-from-base GitHub accepted (202) whose new head the guard has
	// not seen yet; UpdateRequestedAt is when. Cleared once the head moves.
	UpdateRequestedHeadSHA string    `json:"update_requested_head_sha,omitempty"`
	UpdateRequestedBaseSHA string    `json:"update_requested_base_sha,omitempty"`
	UpdateRequestedAt      time.Time `json:"update_requested_at,omitempty"`
	// UpdateStoppedHeadSHA and UpdateStoppedBaseSHA are the head and base the
	// guard stopped updating, and UpdateStoppedReason is why (one of the
	// UpdateStop* reasons). No attempt is made again until the head or the
	// base moves.
	UpdateStoppedHeadSHA string `json:"update_stopped_head_sha,omitempty"`
	UpdateStoppedBaseSHA string `json:"update_stopped_base_sha,omitempty"`
	UpdateStoppedReason  string `json:"update_stopped_reason,omitempty"`
	// UpdateStopNotedHeadSHA and UpdateStopNotedReason name the head and
	// reason the person-facing note was last posted for, so a stop retried
	// on a new base with the same answer does not post again.
	UpdateStopNotedHeadSHA string `json:"update_stop_noted_head_sha,omitempty"`
	UpdateStopNotedReason  string `json:"update_stop_noted_reason,omitempty"`
	// ObservedHeldHeadSHA is the latest head the hive itself observed on the
	// PR while the hold gate was still on (#13603). A current head equal to
	// it provably predates the gate lift, so a trusted human's lift reviewed
	// it. Empty on records written before the field existed; those fail
	// closed.
	ObservedHeldHeadSHA string `json:"observed_held_head_sha,omitempty"`
	// HeldAt is when the snapshot was first recorded.
	HeldAt    time.Time `json:"held_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Drift is the verdict comparing a lift-time head against the hold snapshot.
type Drift struct {
	// Moved reports that the branch is not provably the reviewed tree. It is
	// true whenever the heads differ AND whenever either head is unknown —
	// this guard fails closed.
	Moved           bool
	RecordedHeadSHA string
	CurrentHeadSHA  string
	// NewCommits are current commits absent from the snapshot's SHA set. A
	// rebase/force-push renames every SHA, so after one, every commit is
	// (correctly) new: the reviewed tree no longer exists.
	NewCommits []Commit
	// NewAuthors are authors absent from the snapshot's author set.
	NewAuthors []string
}

// Store is the on-PVC hold-snapshot ledger. All methods are safe for
// concurrent use. Persistence conventions mirror pkg/escalation: a JSON map
// keyed "repo#number", written atomically via tmp+rename.
type Store struct {
	mu      sync.Mutex
	path    string
	entries map[string]*Entry
	// clock is the time source; nil means time.Now().UTC(). Overridable via
	// SetClock so retention tests are deterministic.
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

// Snapshot records the hold-time snapshot for a PR, but only if none exists:
// the FIRST held observation is the tree the human gate saw, and later pushes
// while still held must not quietly become the baseline. Returns whether a
// new snapshot was recorded. An empty headSHA records nothing — there is no
// tree to pin.
func (s *Store) Snapshot(repo string, number int, headSHA string, commits []Commit) bool {
	if headSHA == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(repo, number)
	if s.entries[key] != nil {
		return false
	}
	now := s.now()
	e := &Entry{HeadSHA: headSHA, ObservedHeldHeadSHA: headSHA, HeldAt: now, UpdatedAt: now}
	e.CommitSHAs, e.Authors = commitSets(commits)
	s.entries[key] = e
	s.saveLocked()
	return true
}

// Touch refreshes an entry's UpdatedAt so a long-standing hold is never aged
// out by Prune while the PR is still visibly held, and records headSHA as the
// latest head observed while the gate was on (#13603). Call it only while the
// PR is held. An empty headSHA keeps the previous observation. No-op for
// unknown PRs.
func (s *Store) Touch(repo string, number int, headSHA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		if headSHA != "" {
			e.ObservedHeldHeadSHA = headSHA
		}
		e.UpdatedAt = s.now()
		s.saveLocked()
	}
}

// Recorded returns a copy of the PR's snapshot entry, if one exists.
func (s *Store) Recorded(repo string, number int) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[Key(repo, number)]
	if e == nil {
		return Entry{}, false
	}
	out := *e
	out.CommitSHAs = append([]string(nil), e.CommitSHAs...)
	out.Authors = append([]string(nil), e.Authors...)
	return out, true
}

// Clear removes the PR's snapshot — the hold lifted with the reviewed tree
// intact (or the drift episode fully resolved).
func (s *Store) Clear(repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[Key(repo, number)]; !ok {
		return
	}
	delete(s.entries, Key(repo, number))
	s.saveLocked()
}

// MarkCommented records that the drift comment posted for the current drift
// episode, so a retried hold-label write never duplicates the evidence.
func (s *Store) MarkCommented(repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		e.Commented = true
		e.UpdatedAt = s.now()
		s.saveLocked()
	}
}

// ReArm replaces the PR's snapshot with the drifted head — called ONLY after
// the hold label was successfully re-applied, so the human lifting that hold
// is approving exactly this tree. Commented resets: any later drift is a new
// episode and earns its own evidence comment.
func (s *Store) ReArm(repo string, number int, headSHA string, commits []Commit) {
	if headSHA == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(repo, number)
	now := s.now()
	e := s.entries[key]
	if e == nil {
		e = &Entry{HeldAt: now}
		s.entries[key] = e
	}
	e.HeadSHA = headSHA
	e.ObservedHeldHeadSHA = headSHA
	e.CommitSHAs, e.Authors = commitSets(commits)
	e.Commented = false
	e.UpdatedAt = now
	s.saveLocked()
}

// AdvanceHygiene moves a held PR's baseline forward to a head the hive itself
// produced by merging the base into the held branch to keep it mergeable
// (#10437). Unlike ReArm, this is NOT a response to drift: the hive's own
// merge-from-base is hygiene, not reviewable content, so the moved head must
// never register as drift on the PR's next lift check — advancing the
// baseline here is what keeps Diff from flagging it later. No-op for a PR the
// guard never snapshotted (nothing to advance) or an empty headSHA (nothing
// to advance to). ConflictNoted resets: the branch merges cleanly again, so a
// stale "can't keep this mergeable" comment would no longer be true.
func (s *Store) AdvanceHygiene(repo string, number int, headSHA string, commits []Commit) {
	if headSHA == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(repo, number)
	e := s.entries[key]
	if e == nil {
		return
	}
	e.HeadSHA = headSHA
	e.ObservedHeldHeadSHA = headSHA
	e.CommitSHAs, e.Authors = commitSets(commits)
	e.ConflictNoted = false
	e.clearUpdateRequested()
	e.UpdatedAt = s.now()
	s.saveLocked()
}

// MarkConflictNoted records that the "can't keep this mergeable" comment
// posted for the CURRENT conflict episode, so a retried attempt never
// duplicates the notice. AdvanceHygiene clears it once the conflict resolves.
func (s *Store) MarkConflictNoted(repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		e.ConflictNoted = true
		e.UpdatedAt = s.now()
		s.saveLocked()
	}
}

// Reasons the guard stops keeping a held PR mergeable for a head and base.
// The first two match the automerge sweep's reasons for the same refusals.
const (
	UpdateStopWorkflowPermission = "workflow-permission"
	UpdateStopForkNotEditable    = "fork-not-editable"
	UpdateStopConflict           = "conflict"
	UpdateStopUpToDate           = "up-to-date"
	UpdateStopNotLanded          = "update-not-landed"
)

// UpdateConfirmWindow is how long an accepted (202) merge-from-base may take
// to move the head before the guard stops waiting. GitHub normally finishes
// in seconds; past this the update is treated as one that will not land.
const UpdateConfirmWindow = 30 * time.Minute

// UpdateStoppedFor reports whether the guard stopped updating this head on
// this base, and why. A record from a build that only knew the workflows
// refusal (WorkflowRefusedHeadSHA) records no base, so it stops only when the
// caller has no base to compare; with a known base it falls through so a base
// move earns a fresh attempt, which records both SHAs.
func (e Entry) UpdateStoppedFor(headSHA, baseSHA string) (string, bool) {
	if headSHA == "" {
		return "", false
	}
	if e.UpdateStoppedHeadSHA == headSHA && e.UpdateStoppedBaseSHA == baseSHA && e.UpdateStoppedReason != "" {
		return e.UpdateStoppedReason, true
	}
	if e.WorkflowRefusedHeadSHA == headSHA && baseSHA == "" {
		return UpdateStopWorkflowPermission, true
	}
	return "", false
}

// UpdateStopNoted reports whether the person-facing note for reason already
// posted for headSHA.
func (e Entry) UpdateStopNoted(headSHA, reason string) bool {
	return headSHA != "" && e.UpdateStopNotedHeadSHA == headSHA && e.UpdateStopNotedReason == reason
}

func (e *Entry) clearUpdateRequested() {
	e.UpdateRequestedHeadSHA, e.UpdateRequestedBaseSHA = "", ""
	e.UpdateRequestedAt = time.Time{}
}

// MarkUpdateRequested records that GitHub accepted a merge-from-base for
// headSHA on baseSHA whose new head is not visible yet. No-op for an
// untracked PR or an empty head.
func (s *Store) MarkUpdateRequested(repo string, number int, headSHA, baseSHA string) {
	if headSHA == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		e.UpdateRequestedHeadSHA, e.UpdateRequestedBaseSHA = headSHA, baseSHA
		e.UpdateRequestedAt = s.now()
		e.UpdatedAt = s.now()
		s.saveLocked()
	}
}

// UpdatePending reports whether an accepted merge-from-base for headSHA on
// baseSHA is still awaited, and whether UpdateConfirmWindow has passed
// without the head moving.
func (s *Store) UpdatePending(repo string, number int, headSHA, baseSHA string) (pending, expired bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[Key(repo, number)]
	if e == nil || headSHA == "" || e.UpdateRequestedHeadSHA != headSHA || e.UpdateRequestedBaseSHA != baseSHA {
		return false, false
	}
	return true, s.now().Sub(e.UpdateRequestedAt) >= UpdateConfirmWindow
}

// SettleUpdateRequest forgets an awaited merge-from-base once the PR's head
// is no longer the head it was requested for, and reports whether there was
// one. The baseline is left alone: a head that moved between ticks is not
// proven to be the hive's own merge, so the lift check judges it.
func (s *Store) SettleUpdateRequest(repo string, number int, headSHA string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[Key(repo, number)]
	if e == nil || e.UpdateRequestedHeadSHA == "" || headSHA == "" || e.UpdateRequestedHeadSHA == headSHA {
		return false
	}
	e.clearUpdateRequested()
	e.UpdatedAt = s.now()
	s.saveLocked()
	return true
}

// MarkUpdateStopped records that the guard stops updating headSHA on baseSHA
// for reason, until the head or the base moves. No-op for an untracked PR
// or an empty head.
func (s *Store) MarkUpdateStopped(repo string, number int, headSHA, baseSHA, reason string) {
	if headSHA == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		e.UpdateStoppedHeadSHA, e.UpdateStoppedBaseSHA, e.UpdateStoppedReason = headSHA, baseSHA, reason
		e.clearUpdateRequested()
		e.UpdatedAt = s.now()
		s.saveLocked()
	}
}

// MarkUpdateStopNoted records that the person-facing note for reason posted
// for headSHA.
func (s *Store) MarkUpdateStopNoted(repo string, number int, headSHA, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[Key(repo, number)]; e != nil {
		e.UpdateStopNotedHeadSHA, e.UpdateStopNotedReason = headSHA, reason
		e.UpdatedAt = s.now()
		s.saveLocked()
	}
}

// Prune drops entries not observed within retention. See Retention for why
// this is age-based rather than prune-on-absence.
func (s *Store) Prune(retention time.Duration) {
	if retention <= 0 {
		retention = Retention
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-retention)
	changed := false
	for key, e := range s.entries {
		if e.UpdatedAt.Before(cutoff) {
			delete(s.entries, key)
			changed = true
		}
	}
	if changed {
		s.saveLocked()
	}
}

// SetClock overrides the store's time source. Intended for tests so retention
// can be exercised deterministically without sleeping.
func (s *Store) SetClock(fn func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = fn
}

func (s *Store) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now().UTC()
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

// commitSets flattens a commit list into the snapshot's bounded SHA list and
// sorted, de-duplicated author set.
func commitSets(commits []Commit) (shas, authors []string) {
	if len(commits) > maxTrackedCommits {
		commits = commits[len(commits)-maxTrackedCommits:]
	}
	seenAuthor := map[string]bool{}
	for _, c := range commits {
		if c.SHA != "" {
			shas = append(shas, c.SHA)
		}
		if c.Author != "" && !seenAuthor[c.Author] {
			seenAuthor[c.Author] = true
			authors = append(authors, c.Author)
		}
	}
	sort.Strings(authors)
	return shas, authors
}

// Diff compares a lift-time observation against the hold snapshot. It fails
// closed: an unknown recorded or current head reads as Moved. When the
// snapshot carried no commit list (the fetch failed at hold time), every
// current commit is treated as new — more evidence, never less.
func Diff(recorded Entry, currentHeadSHA string, current []Commit) Drift {
	d := Drift{
		RecordedHeadSHA: recorded.HeadSHA,
		CurrentHeadSHA:  currentHeadSHA,
	}
	if recorded.HeadSHA == "" || currentHeadSHA == "" || recorded.HeadSHA != currentHeadSHA {
		d.Moved = true
	}
	if !d.Moved {
		return d
	}
	known := map[string]bool{}
	for _, sha := range recorded.CommitSHAs {
		known[sha] = true
	}
	knownAuthor := map[string]bool{}
	for _, a := range recorded.Authors {
		knownAuthor[a] = true
	}
	seenNewAuthor := map[string]bool{}
	for _, c := range current {
		if c.SHA != "" && !known[c.SHA] {
			d.NewCommits = append(d.NewCommits, c)
			if c.Author != "" && !knownAuthor[c.Author] && !seenNewAuthor[c.Author] {
				seenNewAuthor[c.Author] = true
				d.NewAuthors = append(d.NewAuthors, c.Author)
			}
		}
	}
	sort.Strings(d.NewAuthors)
	return d
}

// PushEvidenceForcePush is the GateLift.PushEvidence kind for the
// `head_ref_force_pushed` timeline event that produced the current head
// (GitHub stamps its created_at server-side) (#13603).
const PushEvidenceForcePush = "head_ref_force_pushed"

// GateLift is the evidence the guard reads about the event that lifted a
// PR's hold gate, relative to the PR's latest head change (#13603).
type GateLift struct {
	// LiftedAt is when the gate label was removed. Zero means unknown.
	LiftedAt time.Time
	// LatestPushAt is when the current head arrived on the branch, taken from
	// the server-stamped force-push event that produced it. Zero means
	// unknown (an ordinary push leaves no such event). Commit committer dates
	// are never acceptable here: git sets them client-side, so a contributor
	// could backdate a commit pushed after the lift and have the stale lift
	// honoured on new code. SHA-keyed timestamps (check suites, statuses)
	// are not either: they date the SHA's first arrival anywhere in the
	// repository, not on this branch.
	LatestPushAt time.Time
	// PushEvidence names the signal LatestPushAt came from
	// (PushEvidenceForcePush); empty when LatestPushAt is zero.
	PushEvidence string
	// ActorIsHive reports that one of the hive's own accounts removed the
	// label; the hive never approves for the human.
	ActorIsHive bool
	// ActorIsBot reports that a bot or GitHub App removed the label.
	ActorIsBot bool
}

// LiftCoversCurrentHead reports whether the gate lift reviewed the PR's
// current head: a trusted human (not a hive account, not a bot) removed the
// gate label AFTER the PR's latest push, so the tree they saw when lifting is
// the tree on the branch now. A rebase that happened earlier during the hold
// is then not drift — the human reviewed its result (#13603).
//
// It fails closed: an unknown lift time or push time, or a lift by the hive
// or any bot, never covers the head. A push at or after the lift is exactly
// the attack the guard exists for and is never covered. Callers must supply
// LatestPushAt from the server-stamped force-push event, never from a commit's
// committer date or a SHA-keyed object.
func LiftCoversCurrentHead(lift GateLift) bool {
	if lift.ActorIsHive || lift.ActorIsBot {
		return false
	}
	if lift.LiftedAt.IsZero() || lift.LatestPushAt.IsZero() {
		return false
	}
	return lift.LiftedAt.After(lift.LatestPushAt)
}

// LiftCoversObservedHead reports whether a trusted human's gate lift covered
// currentHeadSHA because the hive itself saw that exact head on the PR while
// the gate was still on (rec.ObservedHeldHeadSHA) — the proof for an ordinary
// push, which leaves no server-stamped force-push event (#13603). The head
// the lift reviewed is then the head on the branch now; a head the hive never
// observed while held may have arrived after the lift and is not covered.
//
// It fails closed: an unknown lift time, an empty current or observed head,
// or a lift by the hive or any bot never covers the head.
func LiftCoversObservedHead(lift GateLift, rec Entry, currentHeadSHA string) bool {
	if lift.ActorIsHive || lift.ActorIsBot || lift.LiftedAt.IsZero() {
		return false
	}
	if currentHeadSHA == "" || rec.ObservedHeldHeadSHA == "" {
		return false
	}
	return currentHeadSHA == rec.ObservedHeldHeadSHA
}

// shortSHA abbreviates a commit SHA for the drift comment.
func shortSHA(sha string) string {
	if len(sha) > shortSHALen {
		return sha[:shortSHALen]
	}
	return sha
}

// CommentBody renders the one-time drift-evidence comment. Author logins are
// rendered in backticks, NEVER as @-mentions — the point is evidence for
// whoever reviews the PR, not a notification blast at whoever's commits got
// smuggled in.
func CommentBody(d Drift) string {
	var b strings.Builder
	b.WriteString("## 🔒 Hold-gate integrity: branch changed while hold-gated — fresh review required\n\n")
	fmt.Fprintf(&b,
		"This PR's branch moved while it sat hold-gated: the head recorded when the hold was applied was `%s`, but the branch now sits at `%s`. ",
		shortSHA(d.RecordedHeadSHA), shortSHA(d.CurrentHeadSHA))
	b.WriteString("The diff a reviewer saw under the hold is no longer the diff that would merge, so auto-merge is blocked and the `" + ReHoldLabel + "` label has been re-applied.\n\n")

	if len(d.NewCommits) > 0 {
		fmt.Fprintf(&b, "**Commits not present in the hold-time snapshot (%d):**\n\n", len(d.NewCommits))
		listed := d.NewCommits
		if len(listed) > maxListedCommits {
			listed = listed[:maxListedCommits]
		}
		for _, c := range listed {
			line := "- `" + shortSHA(c.SHA) + "`"
			if c.Author != "" {
				line += " by `" + c.Author + "`"
			}
			if c.Title != "" {
				line += " — " + c.Title
			}
			b.WriteString(line + "\n")
		}
		if extra := len(d.NewCommits) - len(listed); extra > 0 {
			fmt.Fprintf(&b, "- …and %d more\n", extra)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("The current commit list could not be compared against the snapshot; treat the entire branch as unreviewed.\n\n")
	}

	if len(d.NewAuthors) > 0 {
		quoted := make([]string, len(d.NewAuthors))
		for i, a := range d.NewAuthors {
			quoted[i] = "`" + a + "`"
		}
		fmt.Fprintf(&b, "**Authors not in the hold-time author set:** %s\n\n", strings.Join(quoted, ", "))
	}

	b.WriteString("A human should review the FULL diff at the current head — a rebase or force-push renames every commit, so everything above needs eyes even if it looks familiar. ")
	b.WriteString("Removing the hold label after that review is the fresh approval: the guard has re-pinned its snapshot to the current head, so a clean lift re-opens the merge lanes.\n")
	return b.String()
}

// ConflictCommentBody renders the one-time notice posted when a held PR falls
// behind its base with a real conflict the hive cannot resolve by merging the
// base in (#10437). Held PRs otherwise stay quietly out of date while waiting
// on a human — this tells the human up front that the review they are about
// to do is of a PR that cannot merge as-is, instead of letting them discover
// a DIRTY PR mid-review.
func ConflictCommentBody() string {
	var b strings.Builder
	b.WriteString("## ⚠️ Hold-gate mergeability: this PR has a real conflict with its base\n\n")
	b.WriteString("This PR is hold-gated and has fallen behind its base branch. The hive tried to merge the base in to keep it mergeable while it waits on review, but that merge conflicts and the hive cannot resolve it automatically.\n\n")
	b.WriteString("It will stay held and will NOT merge on its own even once reviewed — removing `" + ReHoldLabel + "` alone will not be enough. Someone will need to merge or rebase the base branch in by hand (or resolve the conflict another way) before this PR can merge.\n")
	return b.String()
}

// UpdateStoppedCommentBody is the one-time note posted when the guard stops
// keeping a held PR up to date with its base for reason (an UpdateStop*
// reason other than conflict, which has ConflictCommentBody).
func UpdateStoppedCommentBody(reason string) string {
	why := "GitHub did not complete the update."
	action := "use **Update branch** on this PR, or merge the base branch in by hand."
	switch reason {
	case UpdateStopWorkflowPermission:
		why = "GitHub refused the update because the base branch changed files under `.github/workflows/` and the Hive app does not have the workflows permission."
	case UpdateStopForkNotEditable:
		why = "The branch is in a fork the Hive app cannot push to (\"Allow edits by maintainers\" is off, or GitHub refused the push)."
		action = "ask the author to merge the base branch in, or use **Update branch** on this PR if GitHub offers it."
	case UpdateStopNotLanded:
		why = fmt.Sprintf("GitHub accepted the update, but the branch had not moved %d minutes later.", int(UpdateConfirmWindow/time.Minute))
	}
	var b strings.Builder
	b.WriteString("## ⚠️ Hold-gate mergeability: Hive could not update this branch\n\n")
	b.WriteString("This PR is hold-gated and has fallen behind its base branch. Hive keeps held PRs up to date while they wait on review, but it could not update this one. ")
	b.WriteString(why)
	b.WriteString("\n\nHive will not try again until this branch or its base branch changes. To bring it up to date now, ")
	b.WriteString(action)
	b.WriteString("\n")
	return b.String()
}
