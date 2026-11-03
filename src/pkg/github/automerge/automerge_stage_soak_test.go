package automerge

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestSoakWindowFor: the soak the pass records for a head — the lane's
// bare `contributor-lane:soaking` reason with the tracker's first-green
// time (095), the pre-record `soaking(remaining=…)` reason still read for
// stored records, RouteHuman's soak evidence on a green trust-blocked head
// (060/080, including a head first seen green this pass and one past the
// window), and nothing for a head that is not green, has no soak, or is
// blank.
func TestSoakWindowFor(t *testing.T) {
	now := time.Date(2026, 10, 28, 20, 30, 0, 0, time.UTC)
	window := 30 * time.Minute
	green := stagelabels.CheckSummary{Total: 3, Green: 3, Known: true}
	var noClock time.Time

	// 095 with the tracker's clock: the soak started when the head went green.
	firstGreen := now.Add(-18 * time.Minute)
	at, win, ok := soakWindowFor("contributor-lane:soaking", "abc", nil, window, firstGreen, now)
	if !ok || win != window || !at.Equal(firstGreen) {
		t.Fatalf("095 reason + tracker: %v %v %v", at, win, ok)
	}
	// Same inputs a pass later: the same clock (nothing to re-stamp).
	if at2, _, _ := soakWindowFor("contributor-lane:soaking", "abc", nil, window, firstGreen, now.Add(time.Minute)); !at2.Equal(at) {
		t.Fatalf("clock moved between passes: %v → %v", at, at2)
	}
	// Tracker cold (restart): the soak reads as starting now; noteSoak
	// keeps a record's own clock in that case (TestNoteSoakKeepsRecordedClock).
	if at, _, ok = soakWindowFor("contributor-lane:soaking", "abc", nil, window, noClock, now); !ok || !at.Equal(now) {
		t.Fatalf("cold tracker: %v %v", at, ok)
	}
	// A first-green in the future (clock skew) is not trusted either.
	if at, _, ok = soakWindowFor("contributor-lane:soaking", "abc", nil, window, now.Add(time.Minute), now); !ok || !at.Equal(now) {
		t.Fatalf("future first green: %v %v", at, ok)
	}
	// The pre-record reason shape still reads as a clock, aged from its remaining.
	at, win, ok = soakWindowFor("contributor-lane:soaking(remaining=12m0s)", "abc", nil, window, noClock, now)
	if !ok || win != window || !at.Equal(now.Add(-18*time.Minute)) {
		t.Fatalf("legacy 095 reason: %v %v %v", at, win, ok)
	}
	// Remaining longer than the window (config shrank): clamp to the window.
	if at, _, ok = soakWindowFor("contributor-lane:soaking(remaining=45m0s)", "abc", nil, window, noClock, now); !ok || !at.Equal(now) {
		t.Fatalf("clamped: %v %v", at, ok)
	}
	// The reason without a configured window is not a clock.
	if _, _, ok = soakWindowFor("contributor-lane:soaking", "abc", nil, 0, firstGreen, now); ok {
		t.Fatal("no window: must not soak")
	}
	route := &stagelabels.Evidence{Soak: window, SoakElapsed: 5 * time.Minute, Checks: green}
	if at, win, ok = soakWindowFor("contributor-lane:claim-tier-untrusted", "abc", route, window, noClock, now); !ok || win != window || !at.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("060 route: %v %v %v", at, win, ok)
	}
	first := &stagelabels.Evidence{Soak: window, Checks: green}
	if at, _, ok = soakWindowFor("contributor-lane:claim-tier-untrusted", "abc", first, window, noClock, now); !ok || !at.Equal(now) {
		t.Fatalf("first seen green this pass must start the clock now: %v %v", at, ok)
	}
	done := &stagelabels.Evidence{Soak: window, SoakElapsed: 40 * time.Minute, Checks: green}
	if _, _, ok = soakWindowFor("contributor-lane:claim-tier-untrusted", "abc", done, window, noClock, now); !ok {
		t.Fatal("a head past the window still carries its clock")
	}
	for name, e := range map[string]*stagelabels.Evidence{
		"pending": {Soak: window, Checks: stagelabels.CheckSummary{Total: 3, Green: 2, Pending: 1, Known: true}},
		"red":     {Soak: window, Checks: stagelabels.CheckSummary{Total: 3, Green: 2, Red: []string{"lint"}, Known: true}},
		"unknown": {Soak: window, Checks: stagelabels.CheckSummary{Total: 3, Green: 3}},
		"no-ci":   {Soak: window, Checks: stagelabels.CheckSummary{Known: true}},
		"no-soak": {Checks: green, SoakElapsed: time.Minute},
	} {
		if _, _, ok = soakWindowFor("contributor-lane:claim-tier-untrusted", "abc", e, window, noClock, now); ok {
			t.Errorf("%s must not soak", name)
		}
	}
	if _, _, ok = soakWindowFor("contributor-lane:soaking", "", nil, window, firstGreen, now); ok {
		t.Fatal("blank head must not soak")
	}
}

// TestNoteSoakWritesAndClears: noteSoak stamps the record's clock on a
// soaking pass and clears it once the head stops soaking.
func TestNoteSoakWritesAndClears(t *testing.T) {
	now := time.Date(2026, 10, 28, 20, 30, 0, 0, time.UTC)
	store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), func() time.Time { return now })
	key := stagelabels.Key("acme/widget", 15174)
	firstGreen := now.Add(-18 * time.Minute)
	store.Observe(key, stagelabels.PreMergeSoak, "contributor-lane:soaking")
	noteSoak(store, key, "contributor-lane:soaking", "abc1234def", nil, 30*time.Minute, firstGreen, now)
	rec, _ := store.Get(key)
	if head, at, win, ok := rec.Soak(); !ok || head != "abc1234def" || win != 30*time.Minute || !at.Equal(firstGreen) {
		t.Fatalf("soak = %q %v %v ok=%v", head, at, win, ok)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	// The next pass, same head, same clock: the record is untouched and
	// the store has nothing to write (hivecommons/hive#15729).
	now = now.Add(time.Minute)
	store.Observe(key, stagelabels.PreMergeSoak, "contributor-lane:soaking")
	noteSoak(store, key, "contributor-lane:soaking", "abc1234def", nil, 30*time.Minute, firstGreen, now)
	if again, _ := store.Get(key); !again.SoakStartedAt.Equal(rec.SoakStartedAt) || again.SoakHead != rec.SoakHead || again.SoakWindowSeconds != rec.SoakWindowSeconds {
		t.Fatalf("unchanged pass re-stamped the soak: %+v → %+v", rec, again)
	}
	if store.Dirty() {
		t.Fatal("an unchanged soaking pass must not dirty the store")
	}
	noteSoak(store, key, "contributor-lane:merged", "abc1234def", nil, 30*time.Minute, firstGreen, now)
	if rec, _ := store.Get(key); rec.SoakHead != "" {
		t.Fatalf("a pass that is not soaking must clear: %+v", rec)
	}
	noteSoak(nil, key, "contributor-lane:soaking", "abc", nil, 30*time.Minute, firstGreen, now) // nil store: no panic
}

// TestNoteSoakKeepsRecordedClock: after a restart the in-memory tracker
// has no first-green for the head, but the record does; the pass keeps
// the record's clock rather than restarting the countdown from now. A
// different head, or a record with no clock, starts from now.
func TestNoteSoakKeepsRecordedClock(t *testing.T) {
	now := time.Date(2026, 10, 28, 20, 30, 0, 0, time.UTC)
	store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), func() time.Time { return now })
	key := stagelabels.Key("acme/widget", 15730)
	started := now.Add(-20 * time.Minute)
	store.Observe(key, stagelabels.PreMergeSoak, "contributor-lane:soaking")
	store.NoteSoak(key, "abc", started, 30*time.Minute)
	var cold time.Time
	noteSoak(store, key, "contributor-lane:soaking", "abc", nil, 30*time.Minute, cold, now)
	if rec, _ := store.Get(key); !rec.SoakStartedAt.Equal(started) {
		t.Fatalf("cold tracker restarted the recorded clock: %v, want %v", rec.SoakStartedAt, started)
	}
	noteSoak(store, key, "contributor-lane:soaking", "def", nil, 30*time.Minute, cold, now)
	if rec, _ := store.Get(key); rec.SoakHead != "def" || !rec.SoakStartedAt.Equal(now) {
		t.Fatalf("a new head must start its own clock: %+v", rec)
	}
	// The tracker knowing better wins over the record.
	noteSoak(store, key, "contributor-lane:soaking", "def", nil, 30*time.Minute, now.Add(-5*time.Minute), now)
	if rec, _ := store.Get(key); !rec.SoakStartedAt.Equal(now.Add(-5 * time.Minute)) {
		t.Fatalf("tracker clock must replace the provisional one: %+v", rec)
	}
}
