package hub

// Branch tests for small pure helpers whose edge branches had no coverage:
// hexVal's letter/invalid classes, clampScore's actual clamps, percentileRank's
// small-fleet floor, medianOf's empty/odd cases, the effective* nil-cluster and
// override branches, truncateReachRunes's truncation, isFailingCheckStatus's
// full status table, overallHealthStatus's critical threshold, and
// makeCanonical's length ceiling. Each branch encodes an invariant a future
// refactor could silently drop (e.g. an unclamped score would leak >100 into
// the quadrant UI; an unbounded canonical id would break the on-disk name
// budget), so they are pinned here.

import (
	"strings"
	"testing"
)

func TestHexValAllClasses(t *testing.T) {
	cases := []struct {
		c    byte
		want int
	}{
		{'0', 0}, {'9', 9},
		{'A', 10}, {'F', 15},
		{'a', 10}, {'f', 15},
		{'g', -1}, {'G', -1}, {'/', -1}, {':', -1}, {' ', -1},
	}
	for _, c := range cases {
		if got := hexVal(c.c); got != c.want {
			t.Errorf("hexVal(%q) = %d, want %d", c.c, got, c.want)
		}
	}
}

func TestClampScoreBounds(t *testing.T) {
	cases := []struct{ in, want int }{
		{-1, 0}, {-100, 0},
		{0, 0}, {50, 50}, {100, 100},
		{101, 100}, {1000, 100},
	}
	for _, c := range cases {
		if got := clampScore(c.in); got != c.want {
			t.Errorf("clampScore(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// A fleet smaller than minFleetForPercentile must score 0, not a percentile
// computed over a statistically meaningless population.
func TestPercentileRankSmallFleetReturnsZero(t *testing.T) {
	small := make([]float64, minFleetForPercentile-1)
	for i := range small {
		small[i] = float64(i)
	}
	if got := percentileRank(10, small, true); got != 0 {
		t.Errorf("percentileRank(small fleet) = %d, want 0", got)
	}
}

func TestMedianOfEmptyAndOdd(t *testing.T) {
	if got := medianOf(nil); got != 0 {
		t.Errorf("medianOf(nil) = %d, want 0", got)
	}
	if got := medianOf([]int{9, 1, 5}); got != 5 {
		t.Errorf("medianOf(odd) = %d, want 5", got)
	}
	if got := medianOf([]int{1, 3}); got != 2 {
		t.Errorf("medianOf(even) = %d, want 2", got)
	}
}

// A nil cluster must resolve to the disabled/unlimited zero value on every
// effective* knob, and a dashboard override must beat the clusters.json value.
func TestEffectiveScaleKnobsNilAndOverride(t *testing.T) {
	helperRedirectScaleSettings(t)

	if got := effectiveMaxHives(nil); got != 0 {
		t.Errorf("effectiveMaxHives(nil) = %d, want 0", got)
	}
	if got := effectivePoolMin(nil); got != 0 {
		t.Errorf("effectivePoolMin(nil) = %d, want 0", got)
	}
	if got := effectivePoolTarget(nil); got != 0 {
		t.Errorf("effectivePoolTarget(nil) = %d, want 0", got)
	}

	// No override saved: clusters.json values pass through.
	cluster := &ClusterConfig{ID: "branch-c1", PoolMin: 4, PoolTarget: 6, MaxHives: 9}
	if got := effectivePoolMin(cluster); got != 4 {
		t.Errorf("effectivePoolMin(no override) = %d, want 4", got)
	}

	// Override saved: it wins over clusters.json for the overridden knob.
	seven := 7
	if err := saveScaleSettings(ScaleSettings{
		Clusters: map[string]ClusterScaleOverride{
			"branch-c1": {PoolMin: &seven},
		},
	}); err != nil {
		t.Fatalf("saveScaleSettings: %v", err)
	}
	if got := effectivePoolMin(cluster); got != 7 {
		t.Errorf("effectivePoolMin(override) = %d, want 7", got)
	}
	// Knobs without an override still fall back to clusters.json.
	if got := effectivePoolTarget(cluster); got != 6 {
		t.Errorf("effectivePoolTarget(no override) = %d, want 6", got)
	}
	if got := effectiveMaxHives(cluster); got != 9 {
		t.Errorf("effectiveMaxHives(no override) = %d, want 9", got)
	}
}

// The rune cap must truncate over-length values without splitting a multi-byte
// sequence, and pass short values through untouched.
func TestTruncateReachRunes(t *testing.T) {
	if got := truncateReachRunes("short", 10); got != "short" {
		t.Errorf("truncateReachRunes(short) = %q, want passthrough", got)
	}
	got := truncateReachRunes("héllo wörld", 5)
	if got != "héllo" {
		t.Errorf("truncateReachRunes = %q, want %q", got, "héllo")
	}
	if !strings.HasPrefix("héllo wörld", got) {
		t.Errorf("truncation %q is not a prefix — multi-byte rune was split", got)
	}
}

// The failing-status set must match the dashboard's FAILING_CHECK_STATUSES
// exactly: all four failing statuses true, pass/skip/unknown false.
func TestIsFailingCheckStatusTable(t *testing.T) {
	failing := []string{
		healthCheckStatusFail, healthCheckStatusWarn,
		healthCheckStatusCritical, healthCheckStatusError,
	}
	for _, st := range failing {
		if !isFailingCheckStatus(st) {
			t.Errorf("isFailingCheckStatus(%q) = false, want true", st)
		}
	}
	for _, st := range []string{"pass", healthCheckStatusSkip, "", "bogus"} {
		if isFailingCheckStatus(st) {
			t.Errorf("isFailingCheckStatus(%q) = true, want false", st)
		}
	}
}

// overallHealthStatus must mirror the spoke's HealthSummary thresholds,
// including the critical cliff at MORE than healthCriticalFailThreshold fails.
func TestOverallHealthStatusThresholds(t *testing.T) {
	cases := []struct {
		fails, warns int
		want         string
	}{
		{0, 0, healthStatusOK},
		{0, 1, healthStatusWarning},
		{1, 0, healthStatusDegraded},
		{healthCriticalFailThreshold, 5, healthStatusDegraded},
		{healthCriticalFailThreshold + 1, 0, healthStatusCritical},
	}
	for _, c := range cases {
		if got := overallHealthStatus(c.fails, c.warns); got != c.want {
			t.Errorf("overallHealthStatus(%d,%d) = %q, want %q", c.fails, c.warns, got, c.want)
		}
	}
}

// The wire form must respect the on-disk name budget: a subject that pushes
// "provider:subject" past maxCanonicalLen is rejected, one byte under passes.
func TestMakeCanonicalLengthCeiling(t *testing.T) {
	prefixLen := len("github") + len(canonicalSeparator)
	atLimit := strings.Repeat("a", maxCanonicalLen-prefixLen)
	if _, err := makeCanonical("github", atLimit); err != nil {
		t.Fatalf("makeCanonical(at limit) unexpectedly failed: %v", err)
	}
	if _, err := makeCanonical("github", atLimit+"a"); err == nil {
		t.Fatal("makeCanonical(over limit) = nil error, want length rejection")
	}
}
