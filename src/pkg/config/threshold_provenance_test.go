package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Threshold provenance: pack-seeded values scale, operator-typed values do not
// (#4037).
//
// #3898 shipped repo-count scaling with a hole: ACMM packs seed explicit
// thresholds on apply, "explicit always wins" returned them verbatim, and
// nothing distinguished a pack-seeded default from a number an operator typed.
// Scaling therefore engaged only on hives that had never applied a level —
// which is not the normal path, and plausibly excluded the 39-repo fleet shape
// that motivated #3498 in the first place.

const testRepos = 39

func packSeeded(modes map[string]ModeConfig) GovernorConfig {
	return GovernorConfig{Modes: modes, ThresholdsSource: ThresholdSourcePack}
}

func operatorSet(modes map[string]ModeConfig) GovernorConfig {
	return GovernorConfig{Modes: modes}
}

func l4Modes() map[string]ModeConfig {
	// What pkg/config/packs/level-4.yaml seeds.
	return map[string]ModeConfig{
		"surge": {Threshold: 10},
		"busy":  {Threshold: 5},
		"quiet": {Threshold: 2},
	}
}

// --- the bug ------------------------------------------------------------------

func TestPackSeededThresholdsScale(t *testing.T) {
	g := packSeeded(l4Modes())
	for _, tc := range []struct {
		mode string
		want int
	}{
		{"surge", 10 * testRepos},
		{"busy", 5 * testRepos},
		{"quiet", 2 * testRepos},
	} {
		if got := g.EffectiveThreshold(tc.mode, testRepos); got != tc.want {
			t.Errorf("%s on a %d-repo pack-applied hive = %d, want %d — scaling must reach a pack-applied hive",
				tc.mode, testRepos, got, tc.want)
		}
	}
}

func TestPackSeededKeepsPerLevelTuning(t *testing.T) {
	// L3 seeds 15/10/3 where L4-L6 seed 10/5/2. Scaling must preserve that
	// difference rather than collapse both onto the built-in default base —
	// losing per-level tuning was one of the rejected workarounds in #3898.
	l3 := packSeeded(map[string]ModeConfig{"surge": {Threshold: 15}})
	l4 := packSeeded(map[string]ModeConfig{"surge": {Threshold: 10}})
	gotL3 := l3.EffectiveThreshold("surge", testRepos)
	gotL4 := l4.EffectiveThreshold("surge", testRepos)
	if gotL3 != 15*testRepos || gotL4 != 10*testRepos {
		t.Fatalf("L3 surge = %d (want %d), L4 surge = %d (want %d)", gotL3, 15*testRepos, gotL4, 10*testRepos)
	}
	if gotL3 == gotL4 {
		t.Error("per-level tuning collapsed: L3 and L4 produced the same threshold")
	}
}

// --- the guarantee that must not regress ---------------------------------------

func TestOperatorSetThresholdIsNeverScaled(t *testing.T) {
	// The #3498 guarantee, and the reason #3898 could not simply scale every
	// explicit value: 300 * 39 = 11700.
	g := operatorSet(map[string]ModeConfig{"surge": {Threshold: 300}})
	if got := g.EffectiveThreshold("surge", testRepos); got != 300 {
		t.Errorf("hand-tuned surge = %d, want 300 unscaled (300*%d = %d would be the bug)",
			got, testRepos, 300*testRepos)
	}
}

func TestAbsentMarkerReadsAsOperatorSet(t *testing.T) {
	// MIGRATION. Every hive that applied a pack before #4037 has seeded
	// thresholds and no marker. Reading those as pack-seeded would multiply
	// them by the repo count the first time the new code ran — a silent mode-
	// ladder change on upgrade. They must stay exactly as they are.
	g := operatorSet(l4Modes())
	if got := g.EffectiveThreshold("surge", testRepos); got != 10 {
		t.Errorf("pre-#4037 seeded surge = %d, want 10 unchanged on upgrade", got)
	}
	if g.ThresholdsArePackSeeded() {
		t.Error("an empty ThresholdsSource must not read as pack-seeded")
	}
}

func TestUnrecognizedSourceReadsAsOperatorSet(t *testing.T) {
	// Only the exact marker enables scaling; anything else fails safe onto the
	// verbatim path rather than multiplying numbers nobody expected to move.
	g := GovernorConfig{Modes: l4Modes(), ThresholdsSource: "something-else"}
	if g.ThresholdsArePackSeeded() {
		t.Fatal("only ThresholdSourcePack may read as pack-seeded")
	}
	if got := g.EffectiveThreshold("surge", testRepos); got != 10 {
		t.Errorf("surge = %d, want 10 verbatim", got)
	}
}

// --- interaction with the rest of the resolution rules -------------------------

func TestUnsetThresholdStillUsesBuiltinBase(t *testing.T) {
	// A pack that seeds only some modes leaves the rest on the built-in bases.
	g := packSeeded(map[string]ModeConfig{"surge": {Threshold: 10}})
	if got := g.EffectiveThreshold("busy", testRepos); got != DefaultThresholdBusy*testRepos {
		t.Errorf("unseeded busy = %d, want the scaled built-in base %d", got, DefaultThresholdBusy*testRepos)
	}
}

func TestZeroThresholdCountsAsUnsetUnderBothProvenances(t *testing.T) {
	// A literal zero has always meant UNSET — mode entries often exist only to
	// carry cadences. The marker must not change that.
	for name, g := range map[string]GovernorConfig{
		"pack":     packSeeded(map[string]ModeConfig{"surge": {Threshold: 0}}),
		"operator": operatorSet(map[string]ModeConfig{"surge": {Threshold: 0}}),
	} {
		if got := g.EffectiveThreshold("surge", testRepos); got != DefaultThresholdSurge*testRepos {
			t.Errorf("%s: zero threshold = %d, want the scaled default %d",
				name, got, DefaultThresholdSurge*testRepos)
		}
	}
}

func TestScalingCurvesApplyToPackBases(t *testing.T) {
	modes := map[string]ModeConfig{"surge": {Threshold: 10}}
	// none: the pack's absolute value, which is the pre-#3898 behavior.
	g := GovernorConfig{Modes: modes, ThresholdsSource: ThresholdSourcePack, ThresholdScaling: ThresholdScalingNone}
	if got := g.EffectiveThreshold("surge", testRepos); got != 10 {
		t.Errorf("scaling=none surge = %d, want 10", got)
	}
	// sqrt: ceil(sqrt(39)) = 7.
	g.ThresholdScaling = ThresholdScalingSqrt
	if got := g.EffectiveThreshold("surge", testRepos); got != 70 {
		t.Errorf("scaling=sqrt surge = %d, want 70", got)
	}
}

func TestSingleRepoHiveSeesPackValueUnchanged(t *testing.T) {
	// The common small hive: scaling by 1 is a no-op, so a pack-applied hive
	// watching one repo behaves exactly as it did before this change.
	g := packSeeded(l4Modes())
	if got := g.EffectiveThreshold("surge", 1); got != 10 {
		t.Errorf("single-repo surge = %d, want the pack value 10 unchanged", got)
	}
}

func TestPackSeededUnknownModeStillHasNoThreshold(t *testing.T) {
	// computeMode ladders only over surge/busy/quiet; a threshold on idle or a
	// custom mode has never been consulted and the marker must not change that.
	g := packSeeded(map[string]ModeConfig{"idle": {Threshold: 7}})
	if got := g.EffectiveThreshold("idle", testRepos); got != 0 {
		t.Errorf("idle = %d, want 0", got)
	}
}

// --- why the marker is whole-set rather than per-mode --------------------------

func TestWholeSetProvenanceCannotInvertTheModeLadder(t *testing.T) {
	// Per-mode provenance was the obvious design and it inverts the ladder: an
	// operator hand-tuning ONLY surge to 30 would leave a pack-seeded busy of 5
	// scaling to 195 on a 39-repo hive, putting busy ABOVE surge. Because the
	// marker covers the whole set, editing any threshold makes all of them
	// verbatim, so surge >= busy >= quiet is preserved.
	afterOperatorEdit := operatorSet(map[string]ModeConfig{
		"surge": {Threshold: 30}, "busy": {Threshold: 5}, "quiet": {Threshold: 2},
	})
	surge := afterOperatorEdit.EffectiveThreshold("surge", testRepos)
	busy := afterOperatorEdit.EffectiveThreshold("busy", testRepos)
	quiet := afterOperatorEdit.EffectiveThreshold("quiet", testRepos)
	if surge < busy || busy < quiet {
		t.Errorf("mode ladder inverted: surge=%d busy=%d quiet=%d", surge, busy, quiet)
	}
	// And the pack-seeded set scales as a set, which also cannot invert.
	p := packSeeded(l4Modes())
	surge, busy, quiet = p.EffectiveThreshold("surge", testRepos), p.EffectiveThreshold("busy", testRepos), p.EffectiveThreshold("quiet", testRepos)
	if surge < busy || busy < quiet {
		t.Errorf("scaled ladder inverted: surge=%d busy=%d quiet=%d", surge, busy, quiet)
	}
}

// --- the marker has to survive a save/load round trip ---------------------------

func TestThresholdsSourceRoundTripsThroughYAML(t *testing.T) {
	// The whole feature depends on this surviving Config.Save and the next
	// load. GovernorConfig has no custom marshaller today, so the struct tag
	// carries it — but ModeConfig next door DOES have one, and a future
	// marshaller on GovernorConfig that forgot this field would silently switch
	// scaling back off on every pack-applied hive with nothing to show for it.
	in := GovernorConfig{
		ThresholdsSource: ThresholdSourcePack,
		Modes:            map[string]ModeConfig{"surge": {Threshold: 10}},
	}
	blob, err := yaml.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out GovernorConfig
	if err := yaml.Unmarshal(blob, &out); err != nil {
		t.Fatalf("Unmarshal: %v\n%s", err, blob)
	}
	if !out.ThresholdsArePackSeeded() {
		t.Fatalf("thresholds_source did not survive the round trip; got %q in:\n%s",
			out.ThresholdsSource, blob)
	}
	if got := out.EffectiveThreshold("surge", testRepos); got != 10*testRepos {
		t.Errorf("after round trip surge = %d, want %d", got, 10*testRepos)
	}
}

func TestThresholdsSourceIsOmittedWhenUnset(t *testing.T) {
	// omitempty: a hive that never applied a pack must not grow a
	// `thresholds_source: ""` line in its config on the next save.
	blob, err := yaml.Marshal(GovernorConfig{Modes: map[string]ModeConfig{}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := string(blob); strings.Contains(got, "thresholds_source") {
		t.Errorf("unset marker should be omitted, got:\n%s", got)
	}
}
