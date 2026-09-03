package governor

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Regression tests for #2573 (boot-kick cadence bypass): startup used to call
// a ClearLastKicks method that wiped the persisted last-kick timestamps right
// after they were restored, so EVERY eligible agent was kicked on the first
// eval of every pod boot. On hosted hives, hub-managed auto-upgrades roll the
// Deployment into a brand-new pod (container restart count 0), so agents on
// 4h/6h cadences were re-kicked at roll frequency — burning backend tokens
// ("Bob coins") far beyond any configured cadence. The fix removes the wipe:
// restored timestamps gate the first eval exactly like any other eval.

// bootTestGovernor mirrors the reporter's setup: bob-backed agents on long
// cadences plus a paused supervisor, pinned to a fixed clock.
func bootTestGovernor(base time.Time) *Governor {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{
				"scanner":    "6h",
				"outreach":   "4h",
				"supervisor": "pause",
			}},
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner":    {Backend: "bob", Enabled: true},
		"outreach":   {Backend: "bob", Enabled: true},
		"supervisor": {Backend: "bob", Enabled: true},
	}
	g := New(cfg, agents, testLogger())
	g.now = func() time.Time { return base }
	return g
}

// TestBootSeededLastKick_WithinCadence_NotDue is the core #2573 boot
// regression: an agent whose persisted last kick is INSIDE its cadence
// interval must NOT be kicked on the first eval after a pod boot.
func TestBootSeededLastKick_WithinCadence_NotDue(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	g := bootTestGovernor(base)

	// The pod rolled 1h after the last kick — 6h/4h cadences have NOT elapsed.
	g.SeedLastKicks(map[string]time.Time{
		"scanner":  base.Add(-1 * time.Hour),
		"outreach": base.Add(-1 * time.Hour),
	})

	due := dueSet(g)
	if due["scanner"] || due["outreach"] {
		t.Errorf("boot must not bypass cadence: agents kicked 1h ago on 6h/4h cadences came up due = %v", due)
	}
	if due["supervisor"] {
		t.Error("paused supervisor must never be due")
	}
}

// TestBootSeededLastKick_CadenceElapsed_Due is the positive control against
// over-blocking (the paused-blocks-everything failure mode): an agent whose
// cadence HAS elapsed — including across downtime longer than the interval —
// is kicked on the first eval.
func TestBootSeededLastKick_CadenceElapsed_Due(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	g := bootTestGovernor(base)

	g.SeedLastKicks(map[string]time.Time{
		"scanner":  base.Add(-7 * time.Hour), // 6h cadence elapsed
		"outreach": base.Add(-3 * time.Hour), // 4h cadence NOT elapsed
	})

	due := dueSet(g)
	if !due["scanner"] {
		t.Error("scanner's 6h cadence elapsed while the pod was down — it must be due on first eval")
	}
	if due["outreach"] {
		t.Error("outreach's 4h cadence has not elapsed — it must not be due")
	}
}

// TestBootNoSeededKicks_AllCadencedAgentsDue: a fresh hive (no persisted
// state, nothing seeded) keeps the original behavior — every cadenced,
// unpaused agent is kicked on the first eval.
func TestBootNoSeededKicks_AllCadencedAgentsDue(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	g := bootTestGovernor(base)

	due := dueSet(g)
	if !due["scanner"] || !due["outreach"] {
		t.Errorf("fresh hive with no persisted kicks must kick all cadenced agents on first eval, got %v", due)
	}
	if due["supervisor"] {
		t.Error("paused supervisor must never be due, even on a fresh hive")
	}
}

// TestSeedLastKicks_ClampsFutureTimestamps: a future-dated snapshot value
// (node clock skew, corrupt state) is clamped to now, so it can delay an
// agent by at most one full cadence interval from the current wall clock.
func TestSeedLastKicks_ClampsFutureTimestamps(t *testing.T) {
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	g := bootTestGovernor(base)

	g.SeedLastKicks(map[string]time.Time{"scanner": base.Add(3 * time.Hour)})

	if lk := g.GetState().LastKick["scanner"]; !lk.Equal(base) {
		t.Fatalf("future timestamp must be clamped to now: got %v, want %v", lk, base)
	}

	// Not due immediately (clamped to "just kicked")...
	if due := dueSet(g); due["scanner"] {
		t.Error("scanner should not be due immediately after the clamp")
	}
	// ...but due once one full interval passes — never blocked past that.
	g.now = func() time.Time { return base.Add(6*time.Hour + time.Minute) }
	if due := dueSet(g); !due["scanner"] {
		t.Error("scanner must be due one cadence interval after the clamped timestamp")
	}
}

// TestStartup_NoClearLastKicksWipe asserts the fix is present in SOURCE: the
// startup path must not wipe restored last-kick state, and the governor must
// not offer a wipe primitive for it to call. Sync/port merges have silently
// reverted shipped fixes before — this fails loudly if the wipe comes back.
func TestStartup_NoClearLastKicksWipe(t *testing.T) {
	for _, path := range []string{"governor.go", "../../cmd/hive/main.go"} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if strings.Contains(string(src), "ClearLastKicks()") {
			t.Errorf("%s references ClearLastKicks() — the #2573 boot-kick cadence bypass has been reintroduced: "+
				"startup must honor persisted LastKick state instead of wiping it", path)
		}
	}
}
