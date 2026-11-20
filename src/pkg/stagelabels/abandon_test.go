package stagelabels

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// abandonFixture is a store at a fixed, movable clock with one contributor
// PR parked at an adoption-stalled 085.
type abandonFixture struct {
	t   *testing.T
	now time.Time
	st  *Store
	key string
}

func newAbandonFixture(t *testing.T, stage Stage, reason string) *abandonFixture {
	t.Helper()
	f := &abandonFixture{t: t, now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), key: Key("example/testrepo", 42)}
	f.st = NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return f.now })
	f.st.ObserveHead(f.key, "aaa", stage, reason)
	f.st.NoteActivity(f.key, Activity{Head: "aaa", Comments: 3, Reviews: 1, Digest: "verdict:approve|ci:red|", TrackIdle: true})
	return f
}

func (f *abandonFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

func (f *abandonFixture) rec() Record {
	f.t.Helper()
	r, ok := f.st.Get(f.key)
	if !ok {
		f.t.Fatal("record missing")
	}
	return r
}

const stalledReason = LaneReasonAdoptionStalledPrefix + "check-failing:unit"

func TestAbandonCandidate(t *testing.T) {
	cases := []struct {
		stage  Stage
		reason string
		want   bool
	}{
		{NeedsAuthor, "check-failing:unit", true},
		{NeedsDecision, stalledReason, true},
		{NeedsDecision, "sentinel-alert", false},
		{NeedsDecision, "", false},
		{AdoptedByHive, stalledReason, false},
		{ReturnedToContributor, "check-failing:unit", false},
		{ReviewPending, "", false},
	}
	for _, c := range cases {
		if got := AbandonCandidate(c.stage, c.reason); got != c.want {
			t.Errorf("AbandonCandidate(%s, %q) = %v, want %v", c.stage, c.reason, got, c.want)
		}
	}
}

// The hive's own comment and its label churn do not restart the idle
// clock; an author push or an author comment does.
func TestIdleClockIgnoresHiveActivity(t *testing.T) {
	f := newAbandonFixture(t, NeedsDecision, stalledReason)
	start := f.rec().IdleSince()
	if start.IsZero() {
		t.Fatal("idle clock did not start")
	}

	f.advance(24 * time.Hour)
	// The hive posts a comment: the reconciler credits it, and the next
	// fetch (which includes it) reads no change.
	f.st.CountOwnComments(f.key, 1)
	// Owned stage labels never enter the digest, so a stage label rewrite
	// carries the same digest.
	if f.st.NoteActivity(f.key, Activity{Head: "aaa", Comments: 4, Reviews: 1, Digest: "verdict:approve|ci:red|", TrackIdle: true}) {
		t.Fatal("hive comment read as activity")
	}
	if got := f.rec().IdleSince(); !got.Equal(start) {
		t.Fatalf("hive comment moved the idle clock: %v -> %v", start, got)
	}
	// Unknown counts (the lane did not fetch the PR) never read as activity.
	f.st.NoteActivity(f.key, Activity{Head: "aaa", Comments: -1, Reviews: -1, Digest: "verdict:approve|ci:red|", TrackIdle: true})
	if got := f.rec().IdleSince(); !got.Equal(start) {
		t.Fatalf("an unknown count moved the idle clock: %v", got)
	}
	// A digest change (verdict, CI state, a non-hive label) moves
	// LastActivityAt for stage/stalled but not the idle clock.
	f.st.NoteActivity(f.key, Activity{Head: "aaa", Comments: 4, Reviews: 1, Digest: "verdict:?|ci:none|someone-label", TrackIdle: true})
	if r := f.rec(); !r.IdleSince().Equal(start) || !r.LastActivityAt.Equal(f.now) {
		t.Fatalf("digest change: idle since %v (want %v), last activity %v", r.IdleSince(), start, r.LastActivityAt)
	}
	// A head the hive pushed itself is not the author's push.
	f.st.MarkAdoptedInPlace(f.key, "aaa", "contributor-offline", f.now)
	f.st.MarkHivePushedHead(f.key, "hive1")
	f.st.NoteActivity(f.key, Activity{Head: "hive1", Comments: 4, Reviews: 1, Digest: "verdict:?|ci:none|someone-label", TrackIdle: true})
	if got := f.rec().IdleSince(); !got.Equal(start) {
		t.Fatalf("the hive's own push moved the idle clock: %v", got)
	}
	if idle := f.rec().IdleFor(f.now); idle != 24*time.Hour {
		t.Fatalf("idle = %v, want 24h", idle)
	}

	// The author comments.
	f.advance(time.Hour)
	f.st.NoteActivity(f.key, Activity{Head: "aaa", Comments: 5, Reviews: 1, Digest: "verdict:approve|ci:red|", TrackIdle: true})
	if got := f.rec().IdleSince(); !got.Equal(f.now) {
		t.Fatalf("author comment did not restart the clock: %v, want %v", got, f.now)
	}

	// The author pushes.
	f.advance(48 * time.Hour)
	f.st.NoteActivity(f.key, Activity{Head: "bbb", Comments: 5, Reviews: 1, Digest: "verdict:approve|ci:red|", TrackIdle: true})
	if got := f.rec().IdleSince(); !got.Equal(f.now) {
		t.Fatalf("author push did not restart the clock: %v, want %v", got, f.now)
	}
}

// An 085↔074 flap (the adoption retried and stalled again) keeps the park
// run's start, driven through NoteActivity the way the stage pass does: the
// digest flips with the stage (085 "adoption-stalled:…" reads CI none, 074
// "check-failing:…" reads red), which moves LastActivityAt but must not move
// the idle clock.
func TestIdleClockSurvivesAdoptionFlap(t *testing.T) {
	f := newAbandonFixture(t, AdoptedByHive, "check-failing:unit")
	note := func(digest string) {
		f.st.NoteActivity(f.key, Activity{Head: "aaa", Comments: 3, Reviews: 1, Digest: digest, TrackIdle: true})
	}
	f.advance(time.Hour)
	f.st.ObserveHead(f.key, "aaa", NeedsDecision, stalledReason)
	note("verdict:?|ci:none|")
	parked := f.now
	for i := 0; i < 3; i++ {
		f.advance(6 * time.Hour)
		f.st.ObserveHead(f.key, "aaa", AdoptedByHive, "check-failing:unit")
		note("verdict:?|ci:red|")
		f.advance(6 * time.Hour)
		f.st.ObserveHead(f.key, "aaa", NeedsDecision, stalledReason)
		note("verdict:?|ci:none|")
	}
	r := f.rec()
	if r.EnteredAt.Equal(parked) || !r.LastActivityAt.After(parked) {
		t.Fatal("fixture did not flap through NoteActivity")
	}
	if got := r.ParkRunSince(); !got.Equal(parked) {
		t.Fatalf("park run restarted on the flap: %v, want %v", got, parked)
	}
	if got := r.IdleSince(); !got.Equal(parked) {
		t.Fatalf("idle since = %v, want %v", got, parked)
	}

	// Leaving the park for review starts a new run on the next park.
	f.advance(time.Hour)
	f.st.ObserveHead(f.key, "ccc", ReviewPending, "")
	f.advance(time.Hour)
	f.st.ObserveHead(f.key, "ccc", NeedsDecision, stalledReason)
	if got := f.rec().ParkRunSince(); !got.Equal(f.now) {
		t.Fatalf("a new park kept the old run: %v, want %v", got, f.now)
	}
}

// An earlier 085 for another decision does not start the clock: a PR that
// sat ten days on a reviewer's requires_human, was adopted (074) and then
// stalled (085 adoption-stalled) starts its run at the stall.
func TestParkRunIgnoresEarlierDecision085(t *testing.T) {
	f := newAbandonFixture(t, NeedsDecision, "reviewer-verdict:requires_human")
	f.advance(10 * 24 * time.Hour)
	f.st.ObserveHead(f.key, "aaa", AdoptedByHive, "check-failing:unit")
	f.advance(3 * time.Hour)
	f.st.ObserveHead(f.key, "aaa", NeedsDecision, stalledReason)
	stalled := f.now
	r := f.rec()
	if got := r.ParkRunSince(); !got.Equal(stalled) {
		t.Fatalf("park run = %v, want the stall at %v", got, stalled)
	}
	if got := r.AbandonStep(7*24*time.Hour, true, f.now); got != AbandonKeep {
		t.Fatalf("flagged on the first pass after the stall: %v", got)
	}

	// The same 085 visit turning adoption-stalled without a hop starts
	// the run at the reason change.
	g := newAbandonFixture(t, NeedsDecision, "reviewer-verdict:requires_human")
	g.advance(10 * 24 * time.Hour)
	g.st.ObserveHead(g.key, "aaa", NeedsDecision, stalledReason)
	if got := g.rec().ParkRunSince(); !got.Equal(g.now) {
		t.Fatalf("reason change without a hop: park run = %v, want %v", got, g.now)
	}
}

// A flagged adoption-stalled PR that goes back to 074 keeps its flag and
// its pending notice, so the flap does not post a new warning.
func TestAbandonFlagSurvivesAdoptionRetry(t *testing.T) {
	const after = 7 * 24 * time.Hour
	f := newAbandonFixture(t, NeedsDecision, stalledReason)
	f.advance(after + time.Hour)
	f.st.MarkAbandoned(f.key, f.now)
	f.st.MarkAbandonNoticed(f.key, f.now)
	noticed := f.now
	f.advance(time.Hour)
	f.st.ObserveHead(f.key, "aaa", AdoptedByHive, "check-failing:unit")
	f.st.NoteActivity(f.key, Activity{Head: "aaa", Comments: 3, Reviews: 1, Digest: "verdict:?|ci:red|", TrackIdle: true})
	r := f.rec()
	if r.Abandoned() {
		t.Fatal("a 074 row read as abandoned")
	}
	if got := r.AbandonStep(after, true, f.now); got != AbandonKeep {
		t.Fatalf("074 inside the run: step = %v, want keep", got)
	}
	f.advance(time.Hour)
	f.st.ObserveHead(f.key, "aaa", NeedsDecision, stalledReason)
	r = f.rec()
	if !r.Abandoned() || !r.AbandonNoticeAt.Equal(noticed) {
		t.Fatalf("flag or notice lost across the flap: %+v", r)
	}
	if got := r.AbandonStep(after, true, f.now); got != AbandonKeep {
		t.Fatalf("back at 085: step = %v, want keep", got)
	}
}

// Raising abandon_after past how long a flagged PR has been idle clears the
// flag and its pending close.
func TestAbandonRaisedThresholdClears(t *testing.T) {
	f := newAbandonFixture(t, NeedsDecision, stalledReason)
	f.advance(25 * time.Hour)
	if got := f.rec().AbandonStep(24*time.Hour, true, f.now); got != AbandonFlag {
		t.Fatalf("step = %v, want flag", got)
	}
	f.st.MarkAbandoned(f.key, f.now)
	f.st.MarkAbandonNoticed(f.key, f.now)
	if got := f.rec().AbandonStep(24*time.Hour, true, f.now); got != AbandonKeep {
		t.Fatalf("same threshold: step = %v, want keep", got)
	}
	if got := f.rec().AbandonStep(7*24*time.Hour, true, f.now); got != AbandonClear {
		t.Fatalf("raised threshold: step = %v, want clear", got)
	}
	f.st.ClearAbandoned(f.key)
	if r := f.rec(); !r.AbandonedAt.IsZero() || !r.AbandonNoticeAt.IsZero() {
		t.Fatalf("clear kept state: %+v", r)
	}
}

// With the setting off nothing abandon-related is persisted.
func TestAbandonFieldsOmittedWhenOff(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	st := NewStore("", func() time.Time { return now })
	key := Key("example/testrepo", 9)
	st.ObserveHead(key, "aaa", NeedsDecision, stalledReason)
	st.NoteActivity(key, Activity{Head: "aaa", Comments: 1, Reviews: 0})
	r, _ := st.Get(key)
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "abandon") {
		t.Fatalf("setting-off record persists abandon fields: %s", data)
	}
}

// A record outside the parks has no idle clock.
func TestIdleClockOnlyForParks(t *testing.T) {
	f := newAbandonFixture(t, NeedsDecision, "sentinel-alert")
	f.advance(30 * 24 * time.Hour)
	r := f.rec()
	if !r.IdleSince().IsZero() || r.IdleFor(f.now) != 0 {
		t.Fatalf("non-park 085 has an idle clock: %v", r.IdleSince())
	}
	if got := r.AbandonStep(7*24*time.Hour, true, f.now); got != AbandonKeep {
		t.Fatalf("step = %v, want keep", got)
	}
}

// abandon_after unset or 0: nothing changes, however long the PR idles.
func TestAbandonStepOff(t *testing.T) {
	f := newAbandonFixture(t, NeedsDecision, stalledReason)
	f.advance(365 * 24 * time.Hour)
	for _, after := range []time.Duration{0, -time.Hour} {
		if got := f.rec().AbandonStep(after, true, f.now); got != AbandonKeep {
			t.Fatalf("after=%v: step = %v, want keep", after, got)
		}
	}
	r := f.rec()
	if r.Abandoned() || r.AbandonedReason(f.now) != "" {
		t.Fatalf("flag set with the setting off: %+v", r)
	}
	// A flag left from when the setting was on is cleared once it is off.
	f.st.MarkAbandoned(f.key, f.now)
	if got := f.rec().AbandonStep(0, true, f.now); got != AbandonClear {
		t.Fatalf("setting off with a flag: step = %v, want clear", got)
	}
}

func TestAbandonStepThreshold(t *testing.T) {
	const after = 7 * 24 * time.Hour
	for _, tc := range []struct {
		name   string
		stage  Stage
		reason string
	}{
		{"085 adoption-stalled", NeedsDecision, stalledReason},
		{"071 silent author", NeedsAuthor, "check-failing:unit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAbandonFixture(t, tc.stage, tc.reason)
			f.advance(after - time.Minute)
			if got := f.rec().AbandonStep(after, true, f.now); got != AbandonKeep {
				t.Fatalf("below threshold: step = %v, want keep", got)
			}
			f.advance(2 * time.Minute)
			if got := f.rec().AbandonStep(after, true, f.now); got != AbandonFlag {
				t.Fatalf("past threshold with counts: step = %v, want flag", got)
			}
			if got := f.rec().AbandonStep(after, false, f.now); got != AbandonConfirm {
				t.Fatalf("past threshold without counts: step = %v, want confirm", got)
			}
			if !f.st.MarkAbandoned(f.key, f.now) {
				t.Fatal("MarkAbandoned refused")
			}
			r := f.rec()
			if !r.Abandoned() {
				t.Fatal("flag not set")
			}
			// Stage and reason are untouched: the adoption-stalled: prefix
			// stays for every consumer that matches it.
			if r.Stage != tc.stage || r.Reason != tc.reason {
				t.Fatalf("stage/reason changed: %s %q", r.Stage, r.Reason)
			}
			want := "abandoned 7d: check-failing:unit"
			if got := r.AbandonedReason(f.now); got != want {
				t.Fatalf("reason = %q, want %q", got, want)
			}
			if got := r.AbandonStep(after, true, f.now); got != AbandonKeep {
				t.Fatalf("flagged and quiet: step = %v, want keep", got)
			}

			// The author comments: the flag clears and the clock restarts.
			f.advance(time.Hour)
			f.st.NoteActivity(f.key, Activity{Head: "aaa", Comments: 9, Reviews: 1, Digest: "verdict:approve|ci:red|", TrackIdle: true})
			if got := f.rec().AbandonStep(after, true, f.now); got != AbandonClear {
				t.Fatalf("activity after the flag: step = %v, want clear", got)
			}
			f.st.ClearAbandoned(f.key)
			r = f.rec()
			if r.Abandoned() || !r.AbandonedAt.IsZero() || r.AbandonedBlocker != "" {
				t.Fatalf("clear left the flag: %+v", r)
			}
			if got := r.IdleFor(f.now); got != 0 {
				t.Fatalf("clock did not restart: %v", got)
			}
		})
	}
}

// The confirming read: activity it finds restarts the clock; a failed
// read backs off for AbandonRecheckEvery instead of repeating every sweep.
func TestAbandonConfirmingRead(t *testing.T) {
	const after = 7 * 24 * time.Hour
	f := newAbandonFixture(t, NeedsAuthor, "check-failing:unit")
	f.advance(8 * 24 * time.Hour)
	if got := f.rec().AbandonStep(after, false, f.now); got != AbandonConfirm {
		t.Fatalf("step = %v, want confirm", got)
	}
	// The read found the author's comment two days ago.
	seen := f.now.Add(-48 * time.Hour)
	f.st.NoteAbandonCheck(f.key, seen, f.now)
	if got := f.rec().IdleSince(); !got.Equal(seen) {
		t.Fatalf("read activity did not move the clock: %v", got)
	}
	if got := f.rec().AbandonStep(after, false, f.now); got != AbandonKeep {
		t.Fatalf("after read activity: step = %v, want keep", got)
	}

	// Past the threshold again; this read fails (nothing found, no flag).
	f.advance(6 * 24 * time.Hour)
	f.st.NoteAbandonCheck(f.key, time.Time{}, f.now)
	f.advance(10 * time.Minute)
	if got := f.rec().AbandonStep(after, false, f.now); got != AbandonKeep {
		t.Fatalf("within the recheck window: step = %v, want keep", got)
	}
	f.advance(AbandonRecheckEvery)
	if got := f.rec().AbandonStep(after, false, f.now); got != AbandonConfirm {
		t.Fatalf("after the recheck window: step = %v, want confirm", got)
	}
}

// Leaving the park drops the flag's meaning at once and the step clears it.
func TestAbandonFlagClearsOffPark(t *testing.T) {
	const after = 24 * time.Hour
	f := newAbandonFixture(t, NeedsDecision, stalledReason)
	f.advance(2 * after)
	f.st.MarkAbandoned(f.key, f.now)
	f.st.ObserveHead(f.key, "bbb", ReviewPending, "")
	r := f.rec()
	if r.Abandoned() {
		t.Fatal("flag reads on a record that left the park")
	}
	if got := r.AbandonStep(after, true, f.now); got != AbandonClear {
		t.Fatalf("step = %v, want clear", got)
	}
}

func TestAbandonCloseDue(t *testing.T) {
	const grace = 24 * time.Hour
	f := newAbandonFixture(t, NeedsDecision, stalledReason)
	f.advance(10 * 24 * time.Hour)
	if f.rec().AbandonCloseDue(grace, f.now) {
		t.Fatal("close due without a flag")
	}
	f.st.MarkAbandoned(f.key, f.now)
	if f.rec().AbandonCloseDue(grace, f.now) {
		t.Fatal("close due without a notice")
	}
	f.st.MarkAbandonNoticed(f.key, f.now)
	f.advance(grace - time.Minute)
	if f.rec().AbandonCloseDue(grace, f.now) {
		t.Fatal("close due inside the grace")
	}
	f.advance(time.Minute)
	if !f.rec().AbandonCloseDue(grace, f.now) {
		t.Fatal("close not due after the grace")
	}
	f.st.MarkAbandonClosed(f.key, f.now)
	if f.rec().AbandonCloseDue(grace, f.now) {
		t.Fatal("close due twice")
	}

	// Activity after the notice cancels the close.
	g := newAbandonFixture(t, NeedsDecision, stalledReason)
	g.advance(10 * 24 * time.Hour)
	g.st.MarkAbandoned(g.key, g.now)
	g.st.MarkAbandonNoticed(g.key, g.now)
	g.advance(time.Hour)
	g.st.NoteActivity(g.key, Activity{Head: "bbb", Comments: 3, Reviews: 1, Digest: "verdict:approve|ci:red|", TrackIdle: true})
	g.advance(2 * grace)
	if g.rec().AbandonCloseDue(grace, g.now) {
		t.Fatal("close due despite activity after the notice")
	}

	// A refused close backs off for AbandonRecheckEvery.
	h := newAbandonFixture(t, NeedsDecision, stalledReason)
	h.advance(10 * 24 * time.Hour)
	h.st.MarkAbandoned(h.key, h.now)
	h.st.MarkAbandonNoticed(h.key, h.now)
	h.advance(2 * grace)
	h.st.MarkAbandonCloseTried(h.key, h.now)
	if h.rec().AbandonCloseDue(grace, h.now.Add(time.Minute)) {
		t.Fatal("close retried inside the back-off")
	}
	if !h.rec().AbandonCloseDue(grace, h.now.Add(AbandonRecheckEvery)) {
		t.Fatal("close not retried after the back-off")
	}
}

func TestAbandonedReasonText(t *testing.T) {
	cases := []struct {
		idle    time.Duration
		blocker string
		want    string
	}{
		{9*24*time.Hour + 5*time.Hour, "check-failing:unit", "abandoned 9d: check-failing:unit"},
		{5 * time.Hour, "contributor-lane:workflow-path", "abandoned 5h: contributor-lane:workflow-path"},
		{time.Minute, "", "abandoned <1h"},
	}
	for _, c := range cases {
		if got := AbandonedReasonText(c.idle, c.blocker); got != c.want {
			t.Errorf("AbandonedReasonText(%v, %q) = %q, want %q", c.idle, c.blocker, got, c.want)
		}
	}
	if got := AbandonBlocker(stalledReason); got != "check-failing:unit" {
		t.Errorf("AbandonBlocker = %q", got)
	}
	if got := AbandonBlocker("reviewer-verdict:changes_requested"); !strings.HasPrefix(got, "reviewer-verdict") {
		t.Errorf("AbandonBlocker(071) = %q", got)
	}
}
