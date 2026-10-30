package automerge

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestSoakWindowFor: the soak the pass records for a head — the lane's
// `soaking(remaining=…)` reason after admission (095), RouteHuman's soak
// evidence on a green trust-blocked head (060/080, including a head first
// seen green this pass and one past the window), and nothing for a head
// that is not green, has no soak, or is blank.
func TestSoakWindowFor(t *testing.T) {
	now := time.Date(2026, 10, 28, 20, 30, 0, 0, time.UTC)
	window := 30 * time.Minute
	green := stagelabels.CheckSummary{Total: 3, Green: 3, Known: true}

	at, win, ok := soakWindowFor("contributor-lane:soaking(remaining=12m0s)", "abc", nil, window, now)
	if !ok || win != window || !at.Equal(now.Add(-18*time.Minute)) {
		t.Fatalf("095 reason: %v %v %v", at, win, ok)
	}
	// Remaining longer than the window (config shrank): clamp to the window.
	if at, _, ok = soakWindowFor("contributor-lane:soaking(remaining=45m0s)", "abc", nil, window, now); !ok || !at.Equal(now) {
		t.Fatalf("clamped: %v %v", at, ok)
	}
	// The reason without a configured window is not a clock.
	if _, _, ok = soakWindowFor("contributor-lane:soaking(remaining=12m0s)", "abc", nil, 0, now); ok {
		t.Fatal("no window: must not soak")
	}
	route := &stagelabels.Evidence{Soak: window, SoakElapsed: 5 * time.Minute, Checks: green}
	if at, win, ok = soakWindowFor("contributor-lane:claim-tier-untrusted", "abc", route, window, now); !ok || win != window || !at.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("060 route: %v %v %v", at, win, ok)
	}
	first := &stagelabels.Evidence{Soak: window, Checks: green}
	if at, _, ok = soakWindowFor("contributor-lane:claim-tier-untrusted", "abc", first, window, now); !ok || !at.Equal(now) {
		t.Fatalf("first seen green this pass must start the clock now: %v %v", at, ok)
	}
	done := &stagelabels.Evidence{Soak: window, SoakElapsed: 40 * time.Minute, Checks: green}
	if _, _, ok = soakWindowFor("contributor-lane:claim-tier-untrusted", "abc", done, window, now); !ok {
		t.Fatal("a head past the window still carries its clock")
	}
	for name, e := range map[string]*stagelabels.Evidence{
		"pending": {Soak: window, Checks: stagelabels.CheckSummary{Total: 3, Green: 2, Pending: 1, Known: true}},
		"red":     {Soak: window, Checks: stagelabels.CheckSummary{Total: 3, Green: 2, Red: []string{"lint"}, Known: true}},
		"unknown": {Soak: window, Checks: stagelabels.CheckSummary{Total: 3, Green: 3}},
		"no-ci":   {Soak: window, Checks: stagelabels.CheckSummary{Known: true}},
		"no-soak": {Checks: green, SoakElapsed: time.Minute},
	} {
		if _, _, ok = soakWindowFor("contributor-lane:claim-tier-untrusted", "abc", e, window, now); ok {
			t.Errorf("%s must not soak", name)
		}
	}
	if _, _, ok = soakWindowFor("contributor-lane:soaking(remaining=12m0s)", "", nil, window, now); ok {
		t.Fatal("blank head must not soak")
	}
}

// TestNoteSoakWritesAndClears: noteSoak stamps the record's clock on a
// soaking pass and clears it once the head stops soaking.
func TestNoteSoakWritesAndClears(t *testing.T) {
	now := time.Date(2026, 10, 28, 20, 30, 0, 0, time.UTC)
	store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), func() time.Time { return now })
	key := stagelabels.Key("acme/widget", 15174)
	store.Observe(key, stagelabels.PreMergeSoak, "contributor-lane:soaking(remaining=12m0s)")
	noteSoak(store, key, "contributor-lane:soaking(remaining=12m0s)", "abc1234def", nil, 30*time.Minute, now)
	rec, _ := store.Get(key)
	if head, at, win, ok := rec.Soak(); !ok || head != "abc1234def" || win != 30*time.Minute || !at.Equal(now.Add(-18*time.Minute)) {
		t.Fatalf("soak = %q %v %v ok=%v", head, at, win, ok)
	}
	noteSoak(store, key, "contributor-lane:merged", "abc1234def", nil, 30*time.Minute, now)
	if rec, _ := store.Get(key); rec.SoakHead != "" {
		t.Fatalf("a pass that is not soaking must clear: %+v", rec)
	}
	noteSoak(nil, key, "contributor-lane:soaking(remaining=12m0s)", "abc", nil, 30*time.Minute, now) // nil store: no panic
}
