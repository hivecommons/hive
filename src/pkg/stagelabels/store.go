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
	// adoption grace (online 2h / offline 1h) counts from here.
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
	// LastSeen is the last cycle the item was observed; stale records are
	// pruned by Prune so a closed item does not live in the file forever.
	LastSeen time.Time `json:"last_seen"`
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
	if !had || next != prev {
		s.dirty = true
	}
	s.items[key] = next
	return tr
}

// IsRegression reports whether moving from → to is a backward transition
// worth flagging. A lower number is backward, except leaving a repair stage
// (fixing, returned-to-contributor, adopted-by-hive) for review of the new
// head, which is the repair route itself, not a second regression, and
// leaving needs-human: 090 is a parking stage ("the hive cannot act"), not
// progress, so moving out of it means the blocker cleared, whatever the
// destination (hivecommons/hive#12403 carried stage/regressed through
// 090 → 050 → 072 churn that was never a step backwards for the author).
func IsRegression(from, to Stage) bool {
	if !to.Before(from) {
		return false
	}
	switch from {
	case AdoptedByHive:
		// 074 → 072 is an adoption that was declined (superseder closed
		// unmerged): the PR is handed back, not knocked back.
		if to == ReturnedToContributor {
			return false
		}
		return to != ReviewPending && to != Reviewed
	case Fixing, ReturnedToContributor:
		return to != ReviewPending && to != Reviewed
	case NeedsHuman:
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

// Adopted reports whether head is the head the hive adopted (stage 074).
func (r Record) Adopted(head string) bool {
	return head != "" && r.AdoptedHead == head
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
