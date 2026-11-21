package stagelabels

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The person-PR takeover timer (governor.labels.stage.adoption.person_after,
// hivecommons/hive#19395) runs on 071 / 072 and on the abandon_after idle
// anchor: the hive's own label and CI churn never restart it, a push or a
// comment does.
func TestPersonIdleSince(t *testing.T) {
	f := newAbandonFixture(t, ReturnedToContributor, "check-failing:unit")
	start := f.now
	if !PersonTakeoverStage(NeedsAuthor) || !PersonTakeoverStage(ReturnedToContributor) || PersonTakeoverStage(NeedsDecision) || PersonTakeoverStage(AdoptedByHive) {
		t.Fatal("PersonTakeoverStage")
	}
	if got := f.rec().PersonIdleSince(); !got.Equal(start) {
		t.Fatalf("idle since = %v, want the park start %v", got, start)
	}

	// A 072 -> 071 hand-off is one run.
	f.advance(time.Hour)
	f.st.ObserveHead(f.key, "aaa", NeedsAuthor, "check-failing:unit")
	if got := f.rec().PersonIdleSince(); !got.Equal(start) {
		t.Fatalf("after 072 -> 071: idle since = %v, want %v", got, start)
	}

	// The hive's verdict / CI / label digest flipping is not activity.
	f.advance(time.Hour)
	f.st.NoteActivity(f.key, Activity{Head: "aaa", Comments: 3, Reviews: 1, Digest: "verdict:approve|ci:pending|", TrackIdle: true})
	if got := f.rec().PersonIdleFor(f.now); got != 2*time.Hour {
		t.Fatalf("idle for %v after a digest flip, want 2h", got)
	}

	// A comment restarts it.
	f.advance(time.Hour)
	f.st.NoteActivity(f.key, Activity{Head: "aaa", Comments: 4, Reviews: 1, Digest: "verdict:approve|ci:pending|", TrackIdle: true})
	if got := f.rec().PersonIdleSince(); !got.Equal(f.now) {
		t.Fatalf("idle since = %v after a comment, want %v", got, f.now)
	}

	// Leaving 071 / 072 ends the run; coming back starts a new one.
	f.advance(time.Hour)
	f.st.ObserveHead(f.key, "aaa", ReviewPending, "")
	if got := f.rec().PersonIdleSince(); !got.IsZero() {
		t.Fatalf("idle since at 050 = %v, want zero", got)
	}
	f.advance(time.Hour)
	f.st.ObserveHead(f.key, "aaa", ReturnedToContributor, "check-failing:unit")
	if got := f.rec().PersonParkSince(); !got.Equal(f.now) {
		t.Fatalf("new run starts %v, want %v", got, f.now)
	}
}

// Notice, due, cancel and hand-over: the countdown's record.
func TestPersonTakeoverCountdown(t *testing.T) {
	f := newAbandonFixture(t, ReturnedToContributor, "check-failing:unit")
	grace := 24 * time.Hour
	f.advance(25 * time.Hour)
	noticed := f.now
	f.st.MarkPersonNoticed(f.key, noticed)
	f.st.MarkPersonNoticed(f.key, noticed.Add(time.Minute))
	if r := f.rec(); !r.PersonNoticeAt.Equal(noticed) {
		t.Fatalf("the first notice must stick: %v", r.PersonNoticeAt)
	}
	if r := f.rec(); !r.PersonCountdownStart().Equal(r.PersonIdleSince()) {
		t.Fatalf("countdown start = %v", r.PersonCountdownStart())
	}
	f.advance(grace - time.Minute)
	if f.rec().PersonTakeoverDue(grace, f.now) {
		t.Fatal("due inside the grace")
	}
	f.advance(time.Minute)
	if !f.rec().PersonTakeoverDue(grace, f.now) {
		t.Fatal("not due at the grace")
	}

	// Activity on record after the notice cancels it.
	f.st.NoteActivity(f.key, Activity{Head: "bbb", Comments: 3, Reviews: 1, Digest: "verdict:approve|ci:red|", TrackIdle: true})
	if r := f.rec(); !r.PersonActivitySinceNotice() || r.PersonTakeoverDue(grace, f.now) {
		t.Fatal("a push after the notice did not cancel the takeover")
	}
	if !f.st.ResetPersonNotice(f.key, f.now) {
		t.Fatal("reset reported nothing to cancel")
	}
	if r := f.rec(); !r.PersonNoticeAt.IsZero() || !r.PersonNoticeResetAt.Equal(f.now) || !r.PersonCountdownStart().Equal(f.now) {
		t.Fatalf("after the reset: %+v", r)
	}
	if f.st.ResetPersonNotice(f.key, f.now) {
		t.Fatal("a second reset cancelled something")
	}

	// A fresh read's activity moves the anchor.
	f.advance(time.Hour)
	seen := f.now.Add(-10 * time.Minute)
	f.st.NotePersonActivity(f.key, seen, f.now)
	if r := f.rec(); !r.AbandonActivityAt.Equal(seen) || !r.PersonIdleSince().Equal(seen) {
		t.Fatalf("fresh-read activity not noted: %+v", r)
	}

	// The hand-over records the Adopt now mark under the timer's actor and
	// spends the countdown; an operator's mark is never withdrawn by a reset.
	f.st.MarkPersonNoticed(f.key, f.now)
	f.advance(grace)
	f.st.MarkPersonTakeover(f.key, f.now)
	r := f.rec()
	if by, ok := r.AdoptionForced(); !ok || by != AdoptForcedByPersonTimer || !r.PersonTakeoverPending() || !r.PersonTakeoverAt.Equal(f.now) || !r.PersonNoticeAt.IsZero() {
		t.Fatalf("after the hand-over: %+v", r)
	}
	if strings.Contains(AdoptForcedByPersonTimer, "@") || !strings.Contains(AdoptForcedByPersonTimer, ":") {
		t.Fatalf("AdoptForcedByPersonTimer %q could pass for a GitHub login", AdoptForcedByPersonTimer)
	}
	f.advance(time.Minute)
	if !f.st.ResetPersonNotice(f.key, f.now) {
		t.Fatal("a pending timer mark was not withdrawn")
	}
	if r := f.rec(); r.PersonTakeoverPending() || r.AdoptForcedBy != "" || r.PersonTakeoverAt.IsZero() {
		t.Fatalf("after withdrawing the timer's mark: %+v", r)
	}
	f.st.NoteAdoptionForced(f.key, "operator", f.now)
	if f.st.ResetPersonNotice(f.key, f.now) {
		t.Fatal("a reset withdrew an operator's Adopt now")
	}
	if r := f.rec(); r.AdoptForcedBy != "operator" {
		t.Fatalf("operator mark lost: %+v", r)
	}
}

// A read before the notice that found nothing to warn about backs off an
// hour for the same idle stretch; new activity ends the back-off.
func TestPersonCheckBackoff(t *testing.T) {
	f := newAbandonFixture(t, ReturnedToContributor, "check-failing:unit")
	f.advance(25 * time.Hour)
	if f.rec().PersonCheckBackoff(f.now) {
		t.Fatal("back-off with no check on record")
	}
	f.st.NotePersonCheck(f.key, f.now)
	f.advance(AbandonRecheckEvery - time.Minute)
	if !f.rec().PersonCheckBackoff(f.now) {
		t.Fatal("no back-off inside the hour")
	}
	f.advance(time.Minute)
	if f.rec().PersonCheckBackoff(f.now) {
		t.Fatal("back-off past the hour")
	}
	f.st.NotePersonCheck(f.key, f.now)
	f.advance(time.Minute)
	f.st.NoteActivity(f.key, Activity{Head: "aaa", Comments: 4, Reviews: 1, Digest: "verdict:approve|ci:red|", TrackIdle: true})
	if f.rec().PersonCheckBackoff(f.now) {
		t.Fatal("a comment after the check kept the back-off")
	}
}

// With the timer off nothing is written: the new fields are omitzero.
func TestPersonTakeoverFieldsOmitZero(t *testing.T) {
	data, err := json.Marshal(Record{Stage: ReturnedToContributor})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"person_notice_at", "person_notice_reset_at", "person_takeover_at", "person_checked_at"} {
		if strings.Contains(string(data), key) {
			t.Errorf("zero record writes %s: %s", key, data)
		}
	}
}
