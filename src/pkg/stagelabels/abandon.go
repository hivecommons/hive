package stagelabels

// The stale contributor PR idle clock (governor.labels.stage.abandon_after).
//
// A contributor PR the hive cannot move ends at 071-needs-author (the
// author must act) or at an adoption-stalled 085 (the hive adopted it but
// has nothing it can push). Before this the park was posted once and then
// nothing ever happened again. The idle clock measures how long such a park
// has been quiet; past the threshold the record carries an abandoned flag
// (Record.AbandonedAt) that the dashboard reads as "abandoned Nd: <the
// original blocker>" and routes to the "close or take over" review group.
// The stage and the Reason stay as they are: every consumer of the
// adoption-stalled: prefix keeps working, and no new stage is invented.

import (
	"strconv"
	"strings"
	"time"
)

// AbandonRecheckEvery is how long a failed confirming read waits before the
// next sweep tries again; a successful one never repeats for the same idle
// stretch (it either sets the flag or moves the clock).
const AbandonRecheckEvery = time.Hour

// AbandonCandidate reports whether a PR at stage with reason is in one of
// the parks the idle clock applies to: 071 needs-author, or 085 with an
// adoption-stalled: reason. Every other 085 is a live decision a person
// owes, not a PR that went quiet on the hive.
func AbandonCandidate(stage Stage, reason string) bool {
	switch stage {
	case NeedsAuthor:
		return true
	case NeedsDecision:
		return strings.HasPrefix(strings.TrimSpace(reason), LaneReasonAdoptionStalledPrefix)
	}
	return false
}

// sameParkRun reports whether a History hop at s continues the park run of
// a record now at cur: 071 continues 071; for 085, an 085↔074 flap (the
// adoption retried and stalled again) is one run, not a new one.
func sameParkRun(cur, s Stage) bool {
	switch cur {
	case NeedsAuthor:
		return s == NeedsAuthor
	case NeedsDecision:
		return s == NeedsDecision || s == AdoptedByHive
	}
	return false
}

// ParkRunSince is when the record's current continuous park run started:
// the earliest entry to its current stage in the trailing run of History
// hops that sameParkRun keeps together. EnteredAt alone resets on every
// 085↔074 flap, so it would restart the clock each time the hive retried.
// Zero when the record is not an abandon candidate. History is capped at
// HistoryLimit, so a run longer than that reads from the oldest hop kept,
// which only makes the clock later, never earlier.
func (r Record) ParkRunSince() time.Time {
	if !AbandonCandidate(r.Stage, r.Reason) {
		return time.Time{}
	}
	since := r.EnteredAt
	for i := len(r.History) - 1; i >= 0; i-- {
		h := r.History[i]
		if !sameParkRun(r.Stage, h.Stage) {
			break
		}
		if h.Stage == r.Stage && !h.At.IsZero() && (since.IsZero() || h.At.Before(since)) {
			since = h.At
		}
	}
	return since
}

// IdleSince is when the PR's idle clock started: the later of the last
// outside activity (LastActivityAt: a head push, a comment or review by
// anyone but the hive, a verdict, CI or non-hive label change) and the
// start of the current park run. The hive's own comments and its stage
// label churn never move LastActivityAt, so they never restart it. Zero
// when the record is not an abandon candidate.
func (r Record) IdleSince() time.Time {
	since := r.ParkRunSince()
	if since.IsZero() {
		return since
	}
	if r.LastActivityAt.After(since) {
		return r.LastActivityAt
	}
	return since
}

// IdleFor is how long the PR has been idle at now (0 when unknown or not
// a candidate).
func (r Record) IdleFor(now time.Time) time.Duration {
	since := r.IdleSince()
	if since.IsZero() || !now.After(since) {
		return 0
	}
	return now.Sub(since)
}

// Abandoned reports whether the record carries the abandoned flag for its
// current park. A flag left behind on a record that moved on (the stage
// changed and no sweep cleared it yet) does not count.
func (r Record) Abandoned() bool {
	return !r.AbandonedAt.IsZero() && AbandonCandidate(r.Stage, r.Reason)
}

// AbandonBlocker is the blocker a park reason stands for: the reason
// without its adoption-stalled: prefix ("check-failing:unit"), or the 071
// reason as it is.
func AbandonBlocker(reason string) string {
	reason = strings.TrimSpace(reason)
	if rest, ok := strings.CutPrefix(reason, LaneReasonAdoptionStalledPrefix); ok && strings.TrimSpace(rest) != "" {
		return strings.TrimSpace(rest)
	}
	return reason
}

// AbandonedReasonText renders the abandoned reason a person reads:
// "abandoned 9d: check-failing:unit". idle under a day reads in hours.
func AbandonedReasonText(idle time.Duration, blocker string) string {
	var age string
	switch {
	case idle >= 24*time.Hour:
		age = strconv.Itoa(int(idle/(24*time.Hour))) + "d"
	case idle >= time.Hour:
		age = strconv.Itoa(int(idle/time.Hour)) + "h"
	default:
		age = "<1h"
	}
	blocker = strings.TrimSpace(blocker)
	if blocker == "" {
		return "abandoned " + age
	}
	return "abandoned " + age + ": " + blocker
}

// AbandonedReason is the abandoned reason for the record at now, or "" when
// it is not abandoned.
func (r Record) AbandonedReason(now time.Time) string {
	if !r.Abandoned() {
		return ""
	}
	since := r.AbandonedIdleSince
	if since.IsZero() {
		since = r.IdleSince()
	}
	var idle time.Duration
	if !since.IsZero() && now.After(since) {
		idle = now.Sub(since)
	}
	blocker := r.AbandonedBlocker
	if blocker == "" {
		blocker = AbandonBlocker(r.Reason)
	}
	return AbandonedReasonText(idle, blocker)
}

// AbandonAction is what the stage pass does about a record's idle clock.
type AbandonAction int

const (
	// AbandonKeep: nothing to do (not idle long enough, already flagged,
	// or the setting is off with no flag on record).
	AbandonKeep AbandonAction = iota
	// AbandonClear: drop the flag (activity since it was set, the PR left
	// the park, or the setting was turned off).
	AbandonClear
	// AbandonConfirm: the PR crossed the threshold but this cycle did not
	// fetch its comment and review counts; read them once before flagging.
	AbandonConfirm
	// AbandonFlag: set the flag.
	AbandonFlag
)

// AbandonStep decides the idle clock's action for r at now. after is
// governor.labels.stage.abandon_after (<= 0 is off); countsKnown says the
// lane fetched the PR's comment and review counts this cycle, so the
// activity on record is current. Contributor-only: the caller never asks
// for a hive-authored PR.
func (r Record) AbandonStep(after time.Duration, countsKnown bool, now time.Time) AbandonAction {
	flagged := !r.AbandonedAt.IsZero()
	switch {
	case after <= 0, !AbandonCandidate(r.Stage, r.Reason):
		if flagged {
			return AbandonClear
		}
		return AbandonKeep
	case flagged:
		if r.LastActivityAt.After(r.AbandonedAt) {
			return AbandonClear
		}
		return AbandonKeep
	case r.IdleFor(now) < after:
		return AbandonKeep
	case countsKnown:
		return AbandonFlag
	case !r.AbandonCheckedAt.IsZero() && r.AbandonCheckedAt.After(r.IdleSince()) && now.Sub(r.AbandonCheckedAt) < AbandonRecheckEvery:
		return AbandonKeep
	}
	return AbandonConfirm
}

// AbandonCloseDue reports whether an abandoned PR whose warning went out
// at AbandonNoticeAt may be closed at now: the grace has passed and no
// outside activity is on record since the notice. The caller still checks
// holds, keep-open decisions and a fresh read of comments and reviews.
func (r Record) AbandonCloseDue(grace time.Duration, now time.Time) bool {
	if !r.Abandoned() || r.AbandonNoticeAt.IsZero() || !r.AbandonClosedAt.IsZero() {
		return false
	}
	if r.LastActivityAt.After(r.AbandonNoticeAt) {
		return false
	}
	return now.Sub(r.AbandonNoticeAt) >= grace
}

// MarkAbandoned sets the abandoned flag on key for its current park,
// keeping the blocker and when the idle clock started. A record already
// flagged, or not in a park the clock applies to, is left alone.
func (s *Store) MarkAbandoned(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || !r.AbandonedAt.IsZero() || !AbandonCandidate(r.Stage, r.Reason) {
		return false
	}
	r.AbandonedAt = now
	r.AbandonedBlocker = AbandonBlocker(r.Reason)
	r.AbandonedIdleSince = r.IdleSince()
	s.items[key] = r
	s.dirty = true
	return true
}

// ClearAbandoned drops the abandoned flag and the auto-close state that
// hangs off it; the idle clock restarts from the activity that cleared it.
func (s *Store) ClearAbandoned(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || (r.AbandonedAt.IsZero() && r.AbandonNoticeAt.IsZero() && r.AbandonClosedAt.IsZero()) {
		return false
	}
	r.AbandonedAt, r.AbandonedBlocker, r.AbandonedIdleSince = time.Time{}, "", time.Time{}
	r.AbandonNoticeAt, r.AbandonClosedAt = time.Time{}, time.Time{}
	s.items[key] = r
	s.dirty = true
	return true
}

// NoteAbandonCheck records the outcome of the one confirming read: it ran
// at now, and activityAt (zero when the read found none) is the latest
// comment or review by someone other than the hive since the idle clock
// started. Activity moves LastActivityAt, which restarts the clock exactly
// as if the lane had seen it.
func (s *Store) NoteAbandonCheck(key string, activityAt, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.AbandonCheckedAt = now
	if !activityAt.IsZero() {
		if activityAt.After(now) {
			activityAt = now
		}
		if activityAt.After(r.LastActivityAt) {
			r.LastActivityAt = activityAt
		}
	}
	s.items[key] = r
	s.dirty = true
}

// MarkAbandonNoticed records that the auto-close warning was posted at now.
func (s *Store) MarkAbandonNoticed(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || !r.AbandonNoticeAt.IsZero() {
		return
	}
	r.AbandonNoticeAt = now
	s.items[key] = r
	s.dirty = true
}

// MarkAbandonClosed records that the hive closed the abandoned PR at now,
// so a retried pass never posts a second close comment.
func (s *Store) MarkAbandonClosed(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || !r.AbandonClosedAt.IsZero() {
		return
	}
	r.AbandonClosedAt = now
	s.items[key] = r
	s.dirty = true
}
