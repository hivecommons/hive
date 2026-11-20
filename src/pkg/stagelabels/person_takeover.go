package stagelabels

// The opt-in timer for a person's sent-back PR
// (governor.labels.stage.adoption.person_after, hivecommons/hive#19395).
//
// The hive never takes over a person's PR on its own (hivecommons/hive#15202):
// an owner's agent-written PR, or one labelled author/human, waits at 071 /
// 072 until its author pushes or an operator presses Adopt now. In a
// repository the owner listed, this timer is the one exception: once the
// PR has been idle past person_after (the abandon_after idle anchor, so the
// hive's own comments, label writes and CI results never reset it), the
// hive posts one notice, waits person_grace, reads the PR again and then
// hands it to the Adopt now path. The record keeps the countdown here; the
// stage and Reason are untouched.

import "time"

// AdoptForcedByPersonTimer is the Record.AdoptForcedBy the timer records in
// place of an operator's login when it hands a PR to the Adopt now path.
// A GitHub login never contains ':', so it cannot name a person.
const AdoptForcedByPersonTimer = "hive:person-timer"

// PersonTakeoverStage reports whether s is a park the timer applies to:
// 071 needs-author or 072 returned-to-contributor.
func PersonTakeoverStage(s Stage) bool {
	return s == NeedsAuthor || s == ReturnedToContributor
}

// PersonParkSince is when the record's current 071 / 072 run started: the
// earliest entry in the trailing run of History hops at 071 or 072 (a
// hand-off between the two is one run). Zero when the record is not at
// either stage. History is capped, so a long run reads from the oldest hop
// kept, which only makes the clock later.
func (r Record) PersonParkSince() time.Time {
	if !PersonTakeoverStage(r.Stage) {
		return time.Time{}
	}
	var since time.Time
	for i := len(r.History) - 1; i >= 0; i-- {
		h := r.History[i]
		if !PersonTakeoverStage(h.Stage) {
			break
		}
		if !h.At.IsZero() {
			since = h.At
		}
	}
	if since.IsZero() {
		since = r.EnteredAt
	}
	return since
}

// PersonIdleSince is when the timer's idle clock started: the later of the
// last outside activity (the idle anchor IdleSince uses: a head push the
// hive did not make, a new comment or review) and the start of the 071 /
// 072 run. Zero when the record is not at either stage.
func (r Record) PersonIdleSince() time.Time {
	since := r.PersonParkSince()
	if since.IsZero() {
		return since
	}
	if act := r.idleActivityAt(); act.After(since) {
		return act
	}
	return since
}

// PersonIdleFor is how long the PR has been idle at now for the timer (0
// when unknown or not at 071 / 072).
func (r Record) PersonIdleFor(now time.Time) time.Duration {
	since := r.PersonIdleSince()
	if since.IsZero() || !now.After(since) {
		return 0
	}
	return now.Sub(since)
}

// PersonCountdownStart is the earliest a notice belonging to the current
// countdown can have been posted: the later of the idle start and the last
// cancelled countdown (PersonNoticeResetAt). A notice of the hive's own
// posted at or after it is this countdown's.
func (r Record) PersonCountdownStart() time.Time {
	since := r.PersonIdleSince()
	if r.PersonNoticeResetAt.After(since) {
		return r.PersonNoticeResetAt
	}
	return since
}

// PersonActivitySinceNotice reports whether outside activity is on record
// after the takeover notice went out, or after the timer handed the PR to
// Adopt now while that adoption is still pending; either cancels it.
func (r Record) PersonActivitySinceNotice() bool {
	anchor := r.PersonNoticeAt
	if anchor.IsZero() && r.PersonTakeoverPending() {
		anchor = r.PersonTakeoverAt
	}
	return !anchor.IsZero() && r.idleActivityAt().After(anchor)
}

// PersonTakeoverDue reports whether the notice went out at least grace
// before now and nothing on record moved since. The caller still checks the
// PR's labels, its park reason, change requests and a fresh read of
// comments and reviews before it takes the PR over.
func (r Record) PersonTakeoverDue(grace time.Duration, now time.Time) bool {
	if !PersonTakeoverStage(r.Stage) || r.PersonNoticeAt.IsZero() || r.PersonActivitySinceNotice() {
		return false
	}
	return now.Sub(r.PersonNoticeAt) >= grace
}

// PersonCheckBackoff reports whether the read before the notice ran for the
// current idle stretch less than AbandonRecheckEvery before now and found
// no reason to warn (a person's change request) or failed: it is not
// repeated yet.
func (r Record) PersonCheckBackoff(now time.Time) bool {
	return !r.PersonCheckedAt.IsZero() && r.PersonCheckedAt.After(r.PersonIdleSince()) && now.Sub(r.PersonCheckedAt) < AbandonRecheckEvery
}

// NotePersonCheck records that the read before the notice ran at now and
// did not lead to a notice.
func (s *Store) NotePersonCheck(key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.PersonCheckedAt = now
	s.items[key] = r
	s.dirty = true
}

// MarkPersonNoticed records that the takeover notice was posted at at.
func (s *Store) MarkPersonNoticed(key string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || !r.PersonNoticeAt.IsZero() {
		return
	}
	r.PersonNoticeAt = at
	s.items[key] = r
	s.dirty = true
}

// ResetPersonNotice cancels a pending takeover countdown at now: the notice
// is dropped and now kept as PersonNoticeResetAt, so a notice posted before
// it is never taken for the next countdown's. A pending takeover the timer
// recorded (AdoptForcedBy AdoptForcedByPersonTimer, not yet attempted) is
// withdrawn too; an operator's Adopt now is left alone. It reports whether
// anything was cancelled.
func (s *Store) ResetPersonNotice(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	timerForced := ok && r.AdoptForcedBy == AdoptForcedByPersonTimer
	if !ok || (r.PersonNoticeAt.IsZero() && !timerForced) {
		return false
	}
	r.PersonNoticeAt = time.Time{}
	r.PersonNoticeResetAt = now
	if timerForced {
		r.AdoptForcedBy, r.AdoptForcedAt = "", time.Time{}
	}
	s.items[key] = r
	s.dirty = true
	return true
}

// NotePersonActivity records outside activity a fresh read found at at
// (a comment or review the lane's counts never saw): it moves the idle
// anchor and LastActivityAt, so the timer's idle clock restarts exactly as
// if the lane had seen it.
func (s *Store) NotePersonActivity(key string, at, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok || at.IsZero() {
		return
	}
	if at.After(now) {
		at = now
	}
	if at.After(r.AbandonActivityAt) {
		r.AbandonActivityAt = at
	}
	if at.After(r.LastActivityAt) {
		r.LastActivityAt = at
	}
	s.items[key] = r
	s.dirty = true
}

// MarkPersonTakeover hands the PR to the Adopt now path at now on the
// timer's behalf: the forced-adoption mark an operator's Adopt now records
// (NoteAdoptionForced), with AdoptForcedByPersonTimer as the actor, plus
// PersonTakeoverAt, which outlives the mark (MarkAdopted clears it) so the
// record keeps saying it was the timer, not an operator. The countdown is
// spent: the notice is dropped and now kept as PersonNoticeResetAt, so a
// PR that comes back to 071 / 072 later is warned again first.
func (s *Store) MarkPersonTakeover(key string, now time.Time) {
	s.NoteAdoptionForced(key, AdoptForcedByPersonTimer, now)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[key]
	if !ok {
		return
	}
	r.PersonTakeoverAt = now
	r.PersonNoticeAt, r.PersonNoticeResetAt = time.Time{}, now
	s.items[key] = r
	s.dirty = true
}

// PersonTakeoverPending reports whether the timer handed the PR to the
// Adopt now path and the adoption has not been attempted yet.
func (r Record) PersonTakeoverPending() bool {
	return r.AdoptForcedBy == AdoptForcedByPersonTimer
}
