package stagelabels

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
// the stage and when it last saw activity (for stalled detection, kept
// locally so no GitHub timeline GET is needed), its recent transitions and
// any unresolved regression (narrated in a comment, no longer a label).
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
	// RegressionCommentHead is the head SHA the last regression comment was
	// posted for. Unlike RegressionCommented it survives a recover→regress
	// flap, so a gate flapping on one head posts at most one regression
	// comment for it (hivecommons/hive#12557). Empty for items without a head.
	RegressionCommentHead string `json:"regression_comment_head,omitempty"`
	// StallCommented records that the stall note for the current stage
	// visit was posted (stages with StallNote only).
	StallCommented bool `json:"stall_commented,omitempty"`
	// Reason is the lane/CI/verdict reason behind the current stage.
	Reason string `json:"reason,omitempty"`
	// Author is the item's author login, lower-cased, as the lane saw it
	// (hivecommons/hive#12502). It lets the dashboard answer "which of this
	// contributor's PRs are returned to them" from the store alone, with no
	// GitHub call. Empty for records written before the field existed.
	Author string `json:"author,omitempty"`
	// NudgeHead, NudgeCount and LastNudgeAt track the stage-072 contributor
	// nudge: the head the nudges were for, how many were posted for it
	// (cap 2) and when the last one went out.
	NudgeHead   string    `json:"nudge_head,omitempty"`
	NudgeCount  int       `json:"nudge_count,omitempty"`
	LastNudgeAt time.Time `json:"last_nudge_at,omitempty"`
	// FirstNudgeAt is when the first nudge for NudgeHead went out: the
	// adoption grace (online 2h / offline 15m) counts from here.
	FirstNudgeAt time.Time `json:"first_nudge_at,omitempty"`
	// NudgeOnline records whether the author's relay was online at the first
	// nudge for NudgeHead (selects the online vs offline grace).
	NudgeOnline bool `json:"nudge_online,omitempty"`
	// AdoptedHead / AdoptedBy / AdoptedAt record the stage-074 adoption: the
	// contributor head the hive adopted, the superseding App PR's number
	// (0 while the attempt is recorded but no PR exists) and when. One
	// attempt per head.
	AdoptedHead string    `json:"adopted_head,omitempty"`
	AdoptedBy   int       `json:"adopted_by,omitempty"`
	AdoptedAt   time.Time `json:"adopted_at,omitempty"`
	// AdoptCheckedAt is the last time the superseding PR's state was polled.
	AdoptCheckedAt time.Time `json:"adopt_checked_at,omitempty"`
	// DCOHead, DCOTotal and DCOUnsigned cache the commit check for one head
	// (hivecommons/hive#12542): how many non-merge commits the PR carried
	// and how many lacked a Signed-off-by trailer. One commits GET per head,
	// never per cycle.
	DCOHead     string `json:"dco_head,omitempty"`
	DCOTotal    int    `json:"dco_total,omitempty"`
	DCOUnsigned int    `json:"dco_unsigned,omitempty"`
	// EntryCommented records that the stage's entry comment (085 "needs a
	// decision", 090 "ready for a human merge") was posted for the current
	// stage visit; reset on every stage change so each transition explains
	// itself exactly once (hivecommons/hive#12528).
	EntryCommented bool `json:"entry_commented,omitempty"`
	// History is the item's recent stage transitions, oldest first, capped
	// at HistoryLimit: each entry is the stage entered, when, and the reason
	// at entry. The dashboard's Hive activity monitor renders it as the
	// item's tracker trail; nothing in the reconciler reads it back.
	History []Hop `json:"history,omitempty"`
	// AdoptMode is how the hive adopted the PR: AdoptModeSupersede (an App
	// PR from hive/adopt-<n> carries the head) or AdoptModeInPlace (the
	// hive's fix lane pushes to the PR's own branch, maintainer_can_modify).
	// Empty on records written before in-place adoption existed, which were
	// all supersessions.
	AdoptMode string `json:"adopt_mode,omitempty"`
	// AdoptReason is why the hive adopted ("contributor-offline",
	// "contributor-unregistered", "grace-elapsed").
	AdoptReason string `json:"adopt_reason,omitempty"`
	// YieldedHead is the contributor head the hive yielded an in-place
	// adoption on (the author pushed again); the yield note is posted once
	// per head.
	YieldedHead string `json:"yielded_head,omitempty"`
	// HivePushedHeads are heads the hive recorded pushing to this adopted PR.
	// They are trusted local evidence for in-place adoption attribution when
	// the later GitHub commit object cannot link the commit to the App.
	HivePushedHeads []string `json:"hive_pushed_heads,omitempty"`
	// LastSeen is the last cycle the item was observed; stale records are
	// pruned by Prune so a closed item does not live in the file forever.
	LastSeen time.Time `json:"last_seen"`
	// LastActivityAt is when somebody other than the hive last touched the
	// item as far as the reconciler can tell (hivecommons/hive#12534): a new
	// head, a comment, a review/verdict, a CI state change or a non-hive
	// label change. Stalled measures quiet time from here, never from the
	// PR's updated_at (which the hive's own writes bump).
	LastActivityAt time.Time `json:"last_activity_at,omitempty"`
	// ActivityHead, ActivityComments, ActivityReviews and ActivityDigest are
	// the last observed activity inputs; a change in any of them is
	// activity. ActivityComments counts the hive's own comments too, so the
	// reconciler adds the ones it posts (CountOwnComments) and they never
	// read as someone else acting. ActivityReviews counts review
	// submissions by people (not the hive app), with or without inline
	// comments: a body-only Approve / Request-changes is activity too.
	ActivityHead     string `json:"activity_head,omitempty"`
	ActivityComments int    `json:"activity_comments,omitempty"`
	ActivityReviews  int    `json:"activity_reviews,omitempty"`
	ActivityDigest   string `json:"activity_digest,omitempty"`
	// WaitingOnHive records that, at the last observation, the item was
	// waiting on the hive (required checks running, reviewer verdict
	// pending for the head): it cannot stall, whatever the quiet time.
	WaitingOnHive bool `json:"waiting_on_hive,omitempty"`
	// HiveHeldWarned is set once the hive-side alert for an item held by
	// the hive past its dwell has been logged; activity resets it.
	HiveHeldWarned bool `json:"hive_held_warned,omitempty"`
}

// Activity is what the reconciler observed this cycle that a person (or
// CI) could have changed; see Store.NoteActivity.
type Activity struct {
	// Head is the current head SHA ("" for an issue).
	Head string
	// Comments is the issue + review comment count, or -1 when unknown
	// (the lane did not fetch the PR this cycle).
	Comments int
	// OwnComments is how many comments the hive posted on the item after
	// Comments was fetched this cycle. They are added to the stored count
	// but never compared, so the next fetch (which includes them) does not
	// read them as a person acting.
	OwnComments int
	// Reviews is the number of review submissions by people (the hive
	// app's own reviews excluded), or -1 when unknown (the lane did not
	// list reviews this cycle). It catches a body-only Approve or
	// Request-changes, which Comments (inline comments only) misses.
	Reviews int
	// Digest summarises the verdict, CI state and non-hive labels; any
	// change is activity.
	Digest string
	// At, when non-zero, is the real timestamp of the activity when the
	// caller has one; zero means "now" (the cycle that first observed it,
	// which is what the PR reconciler passes: the list payload carries no
	// commit or comment timestamps).
	At time.Time
	// WaitingOnHive says the item is waiting on the hive (checks running,
	// verdict pending): it must not stall.
	WaitingOnHive bool
}

// QuietSince is when the item's current quiet period began: the later of
// entering the stage and the last activity.
func (r Record) QuietSince() time.Time {
	if r.LastActivityAt.After(r.EnteredAt) {
		return r.LastActivityAt
	}
	return r.EnteredAt
}

// QuietFor is how long the item has been quiet at now (0 if unknown).
func (r Record) QuietFor(now time.Time) time.Duration {
	since := r.QuietSince()
	if since.IsZero() || !now.After(since) {
		return 0
	}
	return now.Sub(since)
}

// Hop is one stage entry in a Record's History: the stage entered, where
// it came from (None on the first observation), when and why. It is also
// the record of backward moves now that the stage/regressed label is
// retired.
type Hop struct {
	From   Stage     `json:"from,omitempty"`
	Stage  Stage     `json:"stage"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
	// Regressed marks a backward hop (IsRegression); it replaces the
	// retired stage/regressed label as the record of the move.
	Regressed bool `json:"regressed,omitempty"`
}

// HistoryLimit bounds Record.History so the store file stays small for an
// item that flaps between stages.
const HistoryLimit = 8

// appendHop appends h to history, dropping the oldest entries past
// HistoryLimit. The result never aliases history's backing array when it
// grows, so a Record copy held by a caller is not mutated.
func appendHop(history []Hop, h Hop) []Hop {
	out := make([]Hop, 0, len(history)+1)
	out = append(out, history...)
	out = append(out, h)
	if len(out) > HistoryLimit {
		out = out[len(out)-HistoryLimit:]
	}
	return out
}

// DCOFor returns the cached commit check for head: total non-merge commits,
// how many are unsigned, and whether the cache is for this head at all.
func (r Record) DCOFor(head string) (total, unsigned int, ok bool) {
	if head == "" || r.DCOHead != head {
		return 0, 0, false
	}
	return r.DCOTotal, r.DCOUnsigned, true
}

// DCO is the cached verdict for head (DCOUnknown when not cached).
func (r Record) DCO(head string) DCOState {
	total, unsigned, ok := r.DCOFor(head)
	switch {
	case !ok || total == 0:
		return DCOUnknown
	case unsigned > 0:
		return DCONo
	}
	return DCOYes
}

// Regressed reports whether the item carries an unresolved regression.
func (r Record) Regressed() bool { return r.RegressedFrom != None }

// RegressionCommentDue reports whether the regression comment for the
// current regression still needs posting on head: not when it was already
// posted for this regression, nor when one was already posted for the same
// non-empty head SHA in an earlier recover→regress cycle.
func (r Record) RegressionCommentDue(head string) bool {
	if r.RegressionCommented {
		return false
	}
	head = strings.TrimSpace(head)
	return head == "" || !strings.EqualFold(r.RegressionCommentHead, head)
}

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
		next.EntryCommented = false
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
	case prev.Regressed() && (!stage.Before(prev.RegressedFrom) || !IsRegression(prev.RegressedFrom, stage)):
		// Back at (or past) the high-water mark, or the mark was a stage a
		// fall from is no longer counted (records written before the rule
		// changed heal here).
		next.RegressedFrom = None
		next.RegressedAt = time.Time{}
		next.RegressionCommented = false
		tr.Recovered = true
	}
	if tr.Changed {
		next.History = appendHop(prev.History, Hop{From: prev.Stage, Stage: stage, At: now, Reason: reason, Regressed: tr.Regressed})
	}
	if !had || tr.Changed || !recordEqual(next, prev) {
		s.dirty = true
	}
	s.items[key] = next
	return tr
}

// recordEqual compares two records field by field; History (a slice) makes
// Record incomparable with ==, and it only changes when the stage does.
func recordEqual(a, b Record) bool {
	a.History, b.History = nil, nil
	return reflect.DeepEqual(a, b)
}

// NoteActivity folds this cycle's activity inputs into key's record and
// reports whether anything a person could have done changed since the last
// observation. The first observation of a record seeds LastActivityAt
// (with a.At when known, else now) so the quiet clock starts at upgrade
// rather than reading a long-idle item as stalled on the first pass.
func (s *Store) NoteActivity(key string, a Activity) (changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return false
	}
	now := s.now()
	at := a.At
	if at.IsZero() || at.After(now) {
		at = now
	}
	switch {
	case r.LastActivityAt.IsZero():
		changed = true
	case a.Head != "" && a.Head != r.ActivityHead:
		changed = true
	case a.Comments >= 0 && a.Comments > r.ActivityComments:
		changed = true
	case a.Reviews >= 0 && a.Reviews > r.ActivityReviews:
		changed = true
	case a.Digest != r.ActivityDigest:
		changed = true
	}
	if changed && at.After(r.LastActivityAt) {
		r.LastActivityAt = at
	}
	if changed {
		r.HiveHeldWarned = false
	}
	if a.Head != "" {
		r.ActivityHead = a.Head
	}
	if a.Comments >= 0 {
		r.ActivityComments = a.Comments + a.OwnComments
	}
	if a.Reviews >= 0 {
		r.ActivityReviews = a.Reviews
	}
	r.ActivityDigest = a.Digest
	r.WaitingOnHive = a.WaitingOnHive
	s.items[key] = r
	s.dirty = true
	return changed
}

// CountOwnComments records that the hive itself posted n comments on key,
// so the next observed comment count does not read as outside activity.
// The credit sits on top of the last count NoteActivity stored, so it must
// be applied after this cycle's NoteActivity (or in a cycle that observes
// no count at all): a NoteActivity that follows it overwrites the credit
// with the pre-comment count and the next fetch reads the hive's own
// comment as a person acting.
func (s *Store) CountOwnComments(key string, n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.ActivityComments += n
	s.items[key] = r
	s.dirty = true
}

// IsRegression reports whether moving from → to is a backward transition
// worth flagging. A lower number is backward, except leaving a repair stage
// (fixing, needs-author, returned-to-contributor, adopted-by-hive) for review of the new
// head, which is the repair route itself, not a second regression, and
// leaving the two human stages: 085 and 090 are parking stages ("the hive
// cannot act"), not progress, so moving out of them means the picture
// changed, whatever the destination — 090 → 072 is the hive learning the
// head is red, not the author losing ground (hivecommons/hive#12403 carried
// stage/regressed through 090 → 050 → 072 churn that was never a step
// backwards for the author).
func IsRegression(from, to Stage) bool {
	if !to.Before(from) {
		return false
	}
	switch from {
	case AdoptedByHive:
		// 074 → 072 is an adoption that was declined (superseder closed
		// unmerged): the PR is handed back, not knocked back.
		if to == ReturnedToContributor || to == NeedsAuthor {
			return false
		}
		return to != ReviewPending && to != Reviewed
	case Fixing, NeedsAuthor, ReturnedToContributor:
		return to != ReviewPending && to != Reviewed
	case ReadyForHumanMerge, NeedsDecision:
		return false
	}
	return true
}

// MarkRegressionCommented records that the regression comment was posted.
func (s *Store) MarkRegressionCommented(key string) {
	s.MarkRegressionCommentedForHead(key, "")
}

// MarkRegressionCommentedForHead records that the regression comment was
// posted for head, so later regressions on the same head stay silent (see
// RegressionCommentDue). An empty head records the comment only for the
// current regression.
func (s *Store) MarkRegressionCommentedForHead(key, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	head = strings.TrimSpace(head)
	if r.RegressionCommented && (head == "" || r.RegressionCommentHead == head) {
		return
	}
	r.RegressionCommented = true
	if head != "" {
		r.RegressionCommentHead = head
	}
	s.items[key] = r
	s.dirty = true
}

// MarkEntryCommented records that the entry comment for the current stage
// visit was posted.
func (s *Store) MarkEntryCommented(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.EntryCommented {
		return
	}
	r.EntryCommented = true
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

// MarkNudged records that nudge number n for head was posted at now. A new
// head restarts the count and the adoption grace; online says whether the
// author's relay was reachable (first nudge decides the grace kind).
func (s *Store) MarkNudged(key, head string, n int, online bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	if r.NudgeHead != head {
		r.NudgeCount = 0
		r.FirstNudgeAt = now
		r.NudgeOnline = online
	}
	if r.FirstNudgeAt.IsZero() {
		r.FirstNudgeAt = now
		r.NudgeOnline = online
	}
	r.NudgeHead = head
	if n > r.NudgeCount {
		r.NudgeCount = n
	} else {
		r.NudgeCount++
	}
	r.LastNudgeAt = now
	s.items[key] = r
	s.dirty = true
}

// Adoption modes (Record.AdoptMode).
const (
	AdoptModeSupersede = "supersede"
	AdoptModeInPlace   = "in-place"
)

// NudgeStart is when the adoption grace for NudgeHead starts counting:
// FirstNudgeAt, or LastNudgeAt for records written before FirstNudgeAt
// existed (a single nudge recorded only its time). Zero when never nudged.
func (r Record) NudgeStart() time.Time {
	if !r.FirstNudgeAt.IsZero() {
		return r.FirstNudgeAt
	}
	if r.NudgeCount > 0 {
		return r.LastNudgeAt
	}
	return time.Time{}
}

// Adopted reports whether head is the head the hive adopted (stage 074).
func (r Record) Adopted(head string) bool {
	return head != "" && r.AdoptedHead == head
}

// AdoptedInPlace reports whether the hive adopted this PR in place (its fix
// lane owns the PR's own branch) and the adoption is still live. The head is
// not compared: the hive's own pushes move it (see Store.MarkAdoptedHead).
func (r Record) AdoptedInPlace() bool {
	return r.AdoptedHead != "" && r.AdoptMode == AdoptModeInPlace
}

// HivePushedHead reports whether the hive recorded that it pushed head.
func (r Record) HivePushedHead(head string) bool {
	head = strings.TrimSpace(head)
	if head == "" {
		return false
	}
	for _, h := range r.HivePushedHeads {
		if h == head {
			return true
		}
	}
	return false
}

// HasHivePushEvidence reports whether any item in repo has recorded a
// hive-pushed head. It lets the adoption reconciler keep old/first-deploy
// in-place adoptions fail-open until the new attribution evidence exists.
func (s *Store) HasHivePushEvidence(repo string) bool {
	if s == nil {
		return false
	}
	prefix := Key(repo, 0)
	prefix = strings.TrimSuffix(prefix, "#0") + "#"
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, r := range s.items {
		if strings.HasPrefix(key, prefix) && len(r.HivePushedHeads) > 0 {
			return true
		}
	}
	return false
}

// AdoptionLive reports whether the hive currently owns this PR (stage 074):
// an in-place adoption that was not yielded, or a superseding adoption of
// exactly this head. Adoption is an ownership state, not a CI state — it
// holds across CI going green or a verdict arriving, until the adoption is
// released (ClearAdoption / MarkYielded) or the PR reaches the merge gate
// (Stage.YieldsToAdoption; hivecommons/hive#12736).
func (r Record) AdoptionLive(head string) bool {
	return r.AdoptedInPlace() || r.Adopted(head)
}

// MarkAdoptedInPlace records an in-place adoption of head for reason.
func (s *Store) MarkAdoptedInPlace(key, head, reason string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.AdoptedHead, r.AdoptedBy, r.AdoptedAt, r.AdoptCheckedAt = head, 0, now, now
	r.AdoptMode, r.AdoptReason = AdoptModeInPlace, reason
	s.items[key] = r
	s.dirty = true
}

// MarkAdoptedHead moves a live adoption to a new head the hive itself
// pushed, so the record keeps reading as adopted.
func (s *Store) MarkAdoptedHead(key, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.AdoptedHead == "" || head == "" {
		return
	}
	r.AdoptedHead = head
	s.items[key] = r
	s.dirty = true
}

// MarkHivePushedHead records a head that the hive itself pushed to an
// in-place-adopted PR. It is a no-op unless the record is a live in-place
// adoption, so stray push-site calls cannot create ownership.
func (s *Store) MarkHivePushedHead(key, head string) {
	head = strings.TrimSpace(head)
	if head == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || !r.AdoptedInPlace() {
		return
	}
	for _, h := range r.HivePushedHeads {
		if h == head {
			return
		}
	}
	r.HivePushedHeads = append(r.HivePushedHeads, head)
	s.items[key] = r
	s.dirty = true
}

// MarkYielded forgets an in-place adoption because the contributor pushed
// head themselves, remembering the head so the yield note posts once.
func (s *Store) MarkYielded(key, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.AdoptedHead, r.AdoptedBy, r.AdoptedAt, r.AdoptCheckedAt = "", 0, time.Time{}, time.Time{}
	r.AdoptMode, r.AdoptReason = "", ""
	r.YieldedHead = head
	s.items[key] = r
	s.dirty = true
}

// MarkAdopted records the stage-074 adoption of head by superseding PR `by`
// (0 = attempted, nothing opened).
func (s *Store) MarkAdopted(key, head string, by int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.AdoptedHead, r.AdoptedBy, r.AdoptedAt, r.AdoptCheckedAt = head, by, now, now
	r.AdoptMode = AdoptModeSupersede
	s.items[key] = r
	s.dirty = true
}

// MarkAdoptChecked records that the superseding PR was polled at now.
func (s *Store) MarkAdoptChecked(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.AdoptCheckedAt = now
	s.items[key] = r
	s.dirty = true
}

// ClearAdoption forgets the adoption (the superseding PR closed unmerged).
func (s *Store) ClearAdoption(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.AdoptedHead, r.AdoptedBy, r.AdoptedAt, r.AdoptCheckedAt = "", 0, time.Time{}, time.Time{}
	r.AdoptMode, r.AdoptReason = "", ""
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

// NoteAuthor records login as the author of key (hivecommons/hive#12502).
// A no-op for an unknown key or an empty login; the login is lower-cased
// so lookups by GitHub username are case-insensitive like GitHub itself.
func (s *Store) NoteAuthor(key, login string) {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.Author == login {
		return
	}
	r.Author = login
	s.items[key] = r
	s.dirty = true
}

// NoteDCO caches the commit check for head on an existing record.
func (s *Store) NoteDCO(key, head string, total, unsigned int) {
	if head == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || (r.DCOHead == head && r.DCOTotal == total && r.DCOUnsigned == unsigned) {
		return
	}
	r.DCOHead, r.DCOTotal, r.DCOUnsigned = head, total, unsigned
	s.items[key] = r
	s.dirty = true
}

// AuthorItem is one stored record with its key, as ByAuthor returns it.
type AuthorItem struct {
	Key    string
	Record Record
}

// ByAuthor returns every record whose Author is login (case-insensitive),
// optionally restricted to stage (None = any stage), sorted by key so the
// dashboard view is stable across polls.
func (s *Store) ByAuthor(login string, stage Stage) []AuthorItem {
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AuthorItem
	for k, r := range s.items {
		if r.Author != login || (stage != None && r.Stage != stage) {
			continue
		}
		out = append(out, AuthorItem{Key: k, Record: r})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
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

// Stalled reports whether nobody has worked r for its stage's dwell: the
// quiet time (since the later of entering the stage and the last observed
// activity) reached the threshold and the item is not waiting on the hive.
// requireQuiet=false (governor.labels.stage.stalled.require_quiet) measures
// from stage entry alone, the pre-#12534 rule; waiting on the hive still
// never stalls.
func Stalled(r Record, overrides map[string]time.Duration, now time.Time) bool {
	return StalledWith(r, overrides, now, true)
}

// StalledWith is Stalled with the require_quiet policy explicit.
func StalledWith(r Record, overrides map[string]time.Duration, now time.Time, requireQuiet bool) bool {
	if r.Stage == None || r.EnteredAt.IsZero() || r.WaitingOnHive {
		return false
	}
	d := Dwell(r.Stage, overrides)
	if d <= 0 {
		return false
	}
	since := r.EnteredAt
	if requireQuiet {
		since = r.QuietSince()
	}
	return now.Sub(since) >= d
}

// HiveHeld reports whether the item has been quiet past its stage's dwell
// while waiting on the hive. The author is not the holder, so this is not
// a stage/stalled case: it is the hive's own lane (reviewer, CI) that
// needs attention, and the engine surfaces it as a log warning and a
// dashboard count rather than a label on the PR (hivecommons/hive#12534).
func HiveHeld(r Record, overrides map[string]time.Duration, now time.Time) bool {
	if r.Stage == None || r.EnteredAt.IsZero() || !r.WaitingOnHive {
		return false
	}
	d := Dwell(r.Stage, overrides)
	return d > 0 && r.QuietFor(now) >= d
}

// MarkHiveHeldWarned records that the hive-held alert was logged for key.
func (s *Store) MarkHiveHeldWarned(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || r.HiveHeldWarned {
		return
	}
	r.HiveHeldWarned = true
	s.items[key] = r
	s.dirty = true
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
// in a StallNote stage: `stage 080-awaiting-merge-admission: no activity
// for 30m0s: <lane's last skip reason>`.
func StallComment(stage Stage, dwell time.Duration, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "lane has not evaluated this head"
	}
	return StallCommentMarker + "\nstage " + stage.String() + ": no activity for " + dwell.String() + ": " + reason
}

// StallCommentMarker tags the stall note.
const StallCommentMarker = "<!-- hive:stage-stalled -->"

// RegressionCommentMarker tags the regression comment so a reader (or a
// later dedupe pass) can find it.
const RegressionCommentMarker = "<!-- hive:stage-regressed -->"
