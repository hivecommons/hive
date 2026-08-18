package config

import (
	"strings"
	"testing"
)

// Governor mode thresholds gate a queue depth that grows with the number of
// repos a hive watches, so a fixed threshold encodes an implicit repo count
// (#3498). These tests pin the scaling curves and, more importantly, the
// resolution order — an explicitly configured threshold must never be scaled.

func TestScaleThreshold_Linear(t *testing.T) {
	tests := []struct {
		repos int
		base  int
		want  int
	}{
		{repos: 1, base: 20, want: 20},
		{repos: 3, base: 20, want: 60},
		{repos: 39, base: 20, want: 780},
		{repos: 39, base: 10, want: 390},
		{repos: 39, base: 2, want: 78},
		// A hive with no repos configured still watches its primary repo.
		{repos: 0, base: 20, want: 20},
		{repos: -5, base: 20, want: 20},
	}
	for _, tt := range tests {
		if got := ScaleThreshold(tt.base, tt.repos, ThresholdScalingLinear); got != tt.want {
			t.Errorf("ScaleThreshold(%d, %d, linear) = %d, want %d", tt.base, tt.repos, got, tt.want)
		}
	}
}

func TestScaleThreshold_Sqrt(t *testing.T) {
	tests := []struct {
		repos int
		base  int
		want  int
	}{
		{repos: 1, base: 20, want: 20},   // ceil(sqrt(1)) = 1
		{repos: 3, base: 20, want: 40},   // ceil(sqrt(3)) = 2
		{repos: 4, base: 20, want: 40},   // ceil(sqrt(4)) = 2
		{repos: 39, base: 20, want: 140}, // ceil(sqrt(39)) = 7
	}
	for _, tt := range tests {
		if got := ScaleThreshold(tt.base, tt.repos, ThresholdScalingSqrt); got != tt.want {
			t.Errorf("ScaleThreshold(%d, %d, sqrt) = %d, want %d", tt.base, tt.repos, got, tt.want)
		}
	}
}

// sqrt must never exceed linear, and must be strictly below it once the curves
// separate — otherwise the option is mislabeled in the config docs and the UI.
//
// They TIE at 1 and 2 repos, because ceil(sqrt(2)) == 2: the ceiling cannot
// produce a factor below 2 until 5 repos, where ceil(sqrt(5)) == 3. Asserting
// strict inequality everywhere fails at repos=2, which is how this test found
// the real shape of the curve rather than the one assumed when writing it.
func TestScaleThreshold_SqrtIsNeverHarsherThanLinear(t *testing.T) {
	for _, repos := range []int{1, 2, 3, 4, 5, 10, 39, 100} {
		lin := ScaleThreshold(20, repos, ThresholdScalingLinear)
		sq := ScaleThreshold(20, repos, ThresholdScalingSqrt)
		if sq > lin {
			t.Errorf("repos=%d: sqrt (%d) exceeds linear (%d)", repos, sq, lin)
		}
		if sq < 20 {
			t.Errorf("repos=%d: sqrt (%d) fell below the base threshold", repos, sq)
		}
	}

	// Where the curves have separated, the gap must be real and must widen —
	// that separation is the entire reason sqrt is offered.
	for _, repos := range []int{3, 5, 10, 39, 100} {
		lin := ScaleThreshold(20, repos, ThresholdScalingLinear)
		sq := ScaleThreshold(20, repos, ThresholdScalingSqrt)
		if sq >= lin {
			t.Errorf("repos=%d: sqrt (%d) should be strictly below linear (%d)", repos, sq, lin)
		}
	}
}

func TestScaleThreshold_None(t *testing.T) {
	for _, repos := range []int{1, 3, 39, 500} {
		if got := ScaleThreshold(20, repos, ThresholdScalingNone); got != 20 {
			t.Errorf("repos=%d: scaling none = %d, want the base 20", repos, got)
		}
	}
}

// A zero or negative base has no meaningful scaling; multiplying it would still
// be zero, but returning it untouched keeps "0 means unset" intact for callers.
func TestScaleThreshold_NonPositiveBaseUnchanged(t *testing.T) {
	for _, base := range []int{0, -1} {
		if got := ScaleThreshold(base, 39, ThresholdScalingLinear); got != base {
			t.Errorf("ScaleThreshold(%d, 39, linear) = %d, want %d", base, got, base)
		}
	}
}

func TestThresholdScalingMode_DefaultsToLinear(t *testing.T) {
	tests := map[string]string{
		"":       ThresholdScalingLinear,
		"linear": ThresholdScalingLinear,
		"sqrt":   ThresholdScalingSqrt,
		"none":   ThresholdScalingNone,
		// Config load rejects these; reaching here means validation was
		// bypassed, and the documented default beats a silent opt-out.
		"nonsense": ThresholdScalingLinear,
		"LINEAR":   ThresholdScalingLinear,
	}
	for in, want := range tests {
		g := GovernorConfig{ThresholdScaling: in}
		if got := g.ThresholdScalingMode(); got != want {
			t.Errorf("ThresholdScalingMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// THE core guarantee of #3498: a hand-tuned threshold is returned verbatim.
// Scaling an operator's explicit 300 by a 39-repo hive would produce 11700 and
// silently break every hive that already worked around this bug by hand.
func TestEffectiveThreshold_ExplicitIsNeverScaled(t *testing.T) {
	g := GovernorConfig{
		Modes: map[string]ModeConfig{
			"surge": {Threshold: 300},
			"busy":  {Threshold: 200},
			"quiet": {Threshold: 100},
		},
	}
	for _, repos := range []int{1, 39, 500} {
		if got := g.EffectiveThreshold("surge", repos); got != 300 {
			t.Errorf("repos=%d: explicit surge = %d, want 300", repos, got)
		}
		if got := g.EffectiveThreshold("busy", repos); got != 200 {
			t.Errorf("repos=%d: explicit busy = %d, want 200", repos, got)
		}
		if got := g.EffectiveThreshold("quiet", repos); got != 100 {
			t.Errorf("repos=%d: explicit quiet = %d, want 100", repos, got)
		}
	}
}

func TestEffectiveThreshold_DefaultsScale(t *testing.T) {
	g := GovernorConfig{}

	// One repo must reproduce the pre-#3498 numbers exactly.
	if got := g.EffectiveThreshold("surge", 1); got != 20 {
		t.Errorf("1-repo surge = %d, want the historical 20", got)
	}
	if got := g.EffectiveThreshold("busy", 1); got != 10 {
		t.Errorf("1-repo busy = %d, want the historical 10", got)
	}
	if got := g.EffectiveThreshold("quiet", 1); got != 2 {
		t.Errorf("1-repo quiet = %d, want the historical 2", got)
	}

	// The reported hive: 39 repos.
	if got := g.EffectiveThreshold("surge", 39); got != 780 {
		t.Errorf("39-repo surge = %d, want 780", got)
	}
}

// A one-repo hive must get the historical constants under EVERY curve — the
// curves only diverge in how they grow, never at the identity point.
func TestEffectiveThreshold_IdentityAtOneRepoForEveryCurve(t *testing.T) {
	for _, scaling := range []string{"", ThresholdScalingLinear, ThresholdScalingSqrt, ThresholdScalingNone} {
		g := GovernorConfig{ThresholdScaling: scaling}
		if got := g.EffectiveThreshold("surge", 1); got != 20 {
			t.Errorf("scaling=%q: 1-repo surge = %d, want 20", scaling, got)
		}
		if got := g.EffectiveThreshold("busy", 1); got != 10 {
			t.Errorf("scaling=%q: 1-repo busy = %d, want 10", scaling, got)
		}
		if got := g.EffectiveThreshold("quiet", 1); got != 2 {
			t.Errorf("scaling=%q: 1-repo quiet = %d, want 2", scaling, got)
		}
	}
}

// A mode entry that exists only to carry cadences has Threshold 0. Treating
// that as an explicit zero would put every non-empty queue in that mode.
func TestEffectiveThreshold_ZeroMeansUnset(t *testing.T) {
	g := GovernorConfig{
		Modes: map[string]ModeConfig{
			"surge": {Threshold: 0, Cadences: map[string]Cadence{"scanner": "5m"}},
		},
	}
	if got := g.EffectiveThreshold("surge", 3); got != 60 {
		t.Errorf("threshold 0 with cadences = %d, want the scaled default 60", got)
	}
}

// computeMode only ladders over surge/busy/quiet; idle is the fallthrough. A
// threshold on any other mode has never been consulted and must not start
// being invented here.
func TestEffectiveThreshold_UnknownModesHaveNoDefault(t *testing.T) {
	g := GovernorConfig{}
	for _, name := range []string{"idle", "", "custom", "unknown"} {
		if got := g.EffectiveThreshold(name, 39); got != 0 {
			t.Errorf("EffectiveThreshold(%q) = %d, want 0", name, got)
		}
	}
	// An explicit threshold on a custom mode is still returned, unscaled —
	// pkg/governor's thresholdFor has always honored one.
	g.Modes = map[string]ModeConfig{"custom": {Threshold: 7}}
	if got := g.EffectiveThreshold("custom", 39); got != 7 {
		t.Errorf("explicit custom threshold = %d, want 7", got)
	}
}

// Mixed sources are legal and are the case most likely to surprise: the
// explicit value stays put while the others scale around it.
func TestEffectiveThreshold_MixedExplicitAndScaled(t *testing.T) {
	g := GovernorConfig{Modes: map[string]ModeConfig{"surge": {Threshold: 70}}}

	if got := g.EffectiveThreshold("surge", 39); got != 70 {
		t.Errorf("explicit surge = %d, want 70", got)
	}
	if got := g.EffectiveThreshold("busy", 39); got != 390 {
		t.Errorf("scaled busy = %d, want 390", got)
	}
	// This ladder is inverted (busy above surge); pkg/governor warns about it.
	// Recorded here so the arithmetic behind that warning is pinned.
	if g.EffectiveThreshold("busy", 39) <= g.EffectiveThreshold("surge", 39) {
		t.Fatal("fixture no longer produces the inverted ladder it exists to document")
	}
}

func TestEffectiveThreshold_ScalingNoneRestoresAbsoluteBehavior(t *testing.T) {
	g := GovernorConfig{ThresholdScaling: ThresholdScalingNone}
	if got := g.EffectiveThreshold("surge", 39); got != 20 {
		t.Errorf("scaling none on a 39-repo hive = %d, want the absolute 20", got)
	}
}

func TestRepoCount_FloorsAtOne(t *testing.T) {
	if got := (ProjectConfig{}).RepoCount(); got != 1 {
		t.Errorf("no repos = %d, want 1", got)
	}
	if got := (ProjectConfig{Repos: []string{"a", "b", "c"}}).RepoCount(); got != 3 {
		t.Errorf("3 repos = %d, want 3", got)
	}
}

func TestValidateThresholdScaling(t *testing.T) {
	for _, v := range []string{"", "linear", "sqrt", "none"} {
		if !ValidateThresholdScaling(v) {
			t.Errorf("ValidateThresholdScaling(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"Linear", "SQRT", "log", "true", "off", "1"} {
		if ValidateThresholdScaling(v) {
			t.Errorf("ValidateThresholdScaling(%q) = true, want false", v)
		}
	}
}

func TestValidate_RejectsBadThresholdScaling(t *testing.T) {
	c := &Config{
		Project:  ProjectConfig{Org: "my-org"},
		GitHub:   GitHubConfig{Token: "t"},
		Agents:   map[string]AgentConfig{"scanner": {Backend: "claude"}},
		Governor: GovernorConfig{ThresholdScaling: "logarithmic"},
	}
	err := c.validate()
	if err == nil {
		t.Fatal("validate() accepted an invalid threshold_scaling")
	}
	for _, want := range []string{"threshold_scaling", "logarithmic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestValidate_AcceptsEveryThresholdScaling(t *testing.T) {
	for _, v := range []string{"", "linear", "sqrt", "none"} {
		c := &Config{
			Project:  ProjectConfig{Org: "my-org"},
			GitHub:   GitHubConfig{Token: "t"},
			Agents:   map[string]AgentConfig{"scanner": {Backend: "claude"}},
			Governor: GovernorConfig{ThresholdScaling: v},
		}
		if err := c.validate(); err != nil {
			t.Errorf("validate() rejected threshold_scaling %q: %v", v, err)
		}
	}
}
