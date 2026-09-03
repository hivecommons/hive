package governor

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// #3498: the mode ladder gates a queue depth that grows with repo count, so
// fixed thresholds put a large hive in permanent SURGE while a small one idles.
// These tests cover the governor side — that the repo count reaches
// thresholdFor, that the mode ladder moves with it, and that a governor which
// was never told a repo count behaves exactly as it always did.

func scalingLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// A governor constructed without SetRepoCount must ladder on the historical
// absolute numbers. Every existing test in this package relies on it, and so
// does any embedder that has not been updated.
func TestThresholdFor_DefaultsToUnscaledWhenRepoCountUnset(t *testing.T) {
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, slog.Default())

	if got := g.thresholdFor("surge"); got != 20 {
		t.Errorf("surge = %d, want the historical 20", got)
	}
	if got := g.thresholdFor("busy"); got != 10 {
		t.Errorf("busy = %d, want the historical 10", got)
	}
	if got := g.thresholdFor("quiet"); got != 2 {
		t.Errorf("quiet = %d, want the historical 2", got)
	}
}

func TestSetRepoCount_ScalesDefaultThresholds(t *testing.T) {
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, slog.Default())
	g.SetRepoCount(39)

	if got := g.thresholdFor("surge"); got != 780 {
		t.Errorf("39-repo surge = %d, want 780", got)
	}
	if got := g.thresholdFor("busy"); got != 390 {
		t.Errorf("39-repo busy = %d, want 390", got)
	}
	if got := g.thresholdFor("quiet"); got != 78 {
		t.Errorf("39-repo quiet = %d, want 78", got)
	}
}

func TestSetRepoCount_FloorsAtOne(t *testing.T) {
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, slog.Default())

	for _, n := range []int{0, -1} {
		g.SetRepoCount(n)
		if got := g.thresholdFor("surge"); got != 20 {
			t.Errorf("SetRepoCount(%d): surge = %d, want the unscaled 20 — a zero would pin the hive in SURGE", n, got)
		}
	}
}

// The reported scenario, end to end: a 39-repo hive carrying ~210 actionable
// items sat in SURGE permanently. With scaled defaults the same queue is no
// longer a surge, because 210 items across 39 repos is ~5 per repo.
func TestComputeMode_LargeHiveLeavesPermanentSurge(t *testing.T) {
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, slog.Default())

	g.SetRepoCount(1)
	if got := g.computeMode(210); got != ModeSurge {
		t.Fatalf("1-repo hive at queue 210 = %s, want SURGE (the pre-fix behavior)", got)
	}

	g.SetRepoCount(39)
	if got := g.computeMode(210); got == ModeSurge {
		t.Error("39-repo hive at queue 210 is still SURGE — the reported bug is not fixed")
	}
}

// The same per-repo pressure must produce the same mode regardless of hive
// size. That equivalence is the point of the feature: the ladder stops
// encoding an implicit repo count.
func TestComputeMode_SameMoodAtSamePerRepoPressure(t *testing.T) {
	small := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, slog.Default())
	small.SetRepoCount(3)
	large := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, slog.Default())
	large.SetRepoCount(39)

	// 15 items per repo is comfortably into BUSY (base busy=10, surge=20).
	if got, want := small.computeMode(15*3), large.computeMode(15*39); got != want {
		t.Errorf("3-repo hive = %s but 39-repo hive = %s at the same per-repo pressure", got, want)
	}
	// 25 per repo clears the surge base on both.
	if got := small.computeMode(25 * 3); got != ModeSurge {
		t.Errorf("3-repo hive at 25/repo = %s, want SURGE", got)
	}
	if got := large.computeMode(25 * 39); got != ModeSurge {
		t.Errorf("39-repo hive at 25/repo = %s, want SURGE", got)
	}
}

// Hand-tuned hives must see no behavior change whatsoever — that promise is
// what makes this a defaults-only change.
func TestComputeMode_ExplicitThresholdsIgnoreRepoCount(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"surge": {Threshold: 300},
			"busy":  {Threshold: 200},
			"quiet": {Threshold: 100},
		},
	}
	g := New(cfg, map[string]config.AgentConfig{}, slog.Default())

	for _, repos := range []int{1, 39, 500} {
		g.SetRepoCount(repos)
		if got := g.computeMode(210); got != ModeBusy {
			t.Errorf("repos=%d: hand-tuned hive at queue 210 = %s, want BUSY", repos, got)
		}
	}
}

func TestThresholdFor_ScalingNoneRestoresAbsoluteBehavior(t *testing.T) {
	cfg := config.GovernorConfig{ThresholdScaling: config.ThresholdScalingNone}
	g := New(cfg, map[string]config.AgentConfig{}, slog.Default())
	g.SetRepoCount(39)

	if got := g.thresholdFor("surge"); got != 20 {
		t.Errorf("scaling none = %d, want the absolute 20", got)
	}
	if got := g.computeMode(210); got != ModeSurge {
		t.Errorf("scaling none at queue 210 = %s, want SURGE (opted out)", got)
	}
}

func TestThresholdFor_SqrtScaling(t *testing.T) {
	cfg := config.GovernorConfig{ThresholdScaling: config.ThresholdScalingSqrt}
	g := New(cfg, map[string]config.AgentConfig{}, slog.Default())
	g.SetRepoCount(39)

	if got := g.thresholdFor("surge"); got != 140 {
		t.Errorf("sqrt surge = %d, want 140", got)
	}
}

// An explicit threshold is never scaled, so mixing one with scaled defaults can
// leave BUSY above SURGE — computeMode tests surge first and returns, making
// BUSY unreachable. The governor must say so rather than let a mode silently
// disappear.
func TestSetRepoCount_WarnsOnInvertedLadder(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{"surge": {Threshold: 70}},
	}
	g := New(cfg, map[string]config.AgentConfig{}, scalingLogger(&buf))
	g.SetRepoCount(39)

	out := buf.String()
	if !strings.Contains(out, "not in descending order") {
		t.Fatalf("no inversion warning logged; got %q", out)
	}
	// The warning has to carry the numbers, or an operator cannot act on it.
	for _, want := range []string{"surge=70", "busy=390", "repo_count=39"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning does not include %q; got %q", want, out)
		}
	}

	// And the inversion is real: BUSY cannot be entered.
	if got := g.computeMode(200); got != ModeSurge {
		t.Errorf("queue 200 with the inverted ladder = %s, want SURGE (busy unreachable)", got)
	}
}

func TestSetRepoCount_NoWarningOnHealthyLadder(t *testing.T) {
	var buf bytes.Buffer
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, scalingLogger(&buf))
	g.SetRepoCount(39)

	if strings.Contains(buf.String(), "not in descending order") {
		t.Errorf("warned about a correctly ordered ladder: %q", buf.String())
	}
}

// UpdateConfig can introduce an inversion on its own (new explicit thresholds,
// or a changed scaling curve), so it must run the same check.
func TestUpdateConfig_WarnsOnInvertedLadder(t *testing.T) {
	var buf bytes.Buffer
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, scalingLogger(&buf))
	g.SetRepoCount(39)
	buf.Reset()

	g.UpdateConfig(config.GovernorConfig{
		Modes: map[string]config.ModeConfig{"surge": {Threshold: 70}},
	})

	if !strings.Contains(buf.String(), "not in descending order") {
		t.Errorf("UpdateConfig did not re-check the ladder; got %q", buf.String())
	}
}

// SetRepoCount is called on every config reload, including reloads that did not
// touch the repo list. Re-warning on each one would spam the log.
func TestSetRepoCount_UnchangedCountIsQuiet(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{"surge": {Threshold: 70}},
	}
	g := New(cfg, map[string]config.AgentConfig{}, scalingLogger(&buf))
	g.SetRepoCount(39)
	buf.Reset()

	g.SetRepoCount(39)
	if buf.Len() != 0 {
		t.Errorf("re-setting the same repo count logged again: %q", buf.String())
	}
}

// UpdateConfig runs on EVERY periodic config reload (main.go's ~2-minute loop)
// and again on every dashboard config save, not only when something changed.
// Re-warning each time would emit a WARN every couple of minutes forever on a
// hive whose ladder is inverted — and an inverted ladder is exactly what the
// #3498 reporter's hive has (explicit surge=70 beside a scaled busy). That is
// against this codebase's own norm for the reload path, which is deliberately
// kept quiet so a healthy hive does not spam.
//
// The warning must fire on the TRANSITION into an inverted ladder, then stay
// quiet while nothing about it changes.
func TestUpdateConfig_InvertedLadderWarnsOnceNotEveryReload(t *testing.T) {
	var buf bytes.Buffer
	inverted := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{"surge": {Threshold: 70}},
	}
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, scalingLogger(&buf))
	g.SetRepoCount(39)

	buf.Reset()
	g.UpdateConfig(inverted)
	if !strings.Contains(buf.String(), "not in descending order") {
		t.Fatalf("first UpdateConfig did not warn; got %q", buf.String())
	}

	// Ten more reloads that change nothing at all.
	buf.Reset()
	for i := 0; i < 10; i++ {
		g.UpdateConfig(inverted)
	}
	if buf.Len() != 0 {
		t.Errorf("re-warned on an unchanged inverted ladder %d times: %q",
			strings.Count(buf.String(), "not in descending order"), buf.String())
	}
}

// Suppression must be keyed on the ladder itself, not latched forever: a
// DIFFERENT inversion is new information and has to be reported, or an operator
// who edits one threshold and makes things worse hears nothing.
func TestUpdateConfig_ChangedInversionWarnsAgain(t *testing.T) {
	var buf bytes.Buffer
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, scalingLogger(&buf))
	g.SetRepoCount(39)
	g.UpdateConfig(config.GovernorConfig{
		Modes: map[string]config.ModeConfig{"surge": {Threshold: 70}},
	})

	buf.Reset()
	g.UpdateConfig(config.GovernorConfig{
		Modes: map[string]config.ModeConfig{"surge": {Threshold: 50}},
	})
	if !strings.Contains(buf.String(), "not in descending order") {
		t.Errorf("a different inversion was suppressed; got %q", buf.String())
	}
}

// And a ladder that gets FIXED and later breaks again must warn again — the
// suppression is a dedupe, not a one-shot.
func TestUpdateConfig_ReinversionAfterRepairWarnsAgain(t *testing.T) {
	var buf bytes.Buffer
	inverted := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{"surge": {Threshold: 70}},
	}
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, scalingLogger(&buf))
	g.SetRepoCount(39)

	g.UpdateConfig(inverted)
	g.UpdateConfig(config.GovernorConfig{}) // repaired: all three scale together
	buf.Reset()

	g.UpdateConfig(inverted)
	if !strings.Contains(buf.String(), "not in descending order") {
		t.Errorf("re-inversion after a repair was suppressed; got %q", buf.String())
	}
}
