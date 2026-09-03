package classify

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// makeIssue is a helper to build a github.Issue with only the fields relevant
// to classification.
func makeIssue(title string, labels ...string) github.Issue {
	return github.Issue{Title: title, Labels: labels}
}

// ---------------------------------------------------------------------------
// Tier + Model tests
// ---------------------------------------------------------------------------

func TestClassify_SimpleKeywordsYieldHaiku(t *testing.T) {
	cases := []struct {
		title string
	}{
		{"Fix typo in README"},
		{"Rename package foo to bar"},
		{"Add i18n support for tooltips"},
		{"Add const for max retries"},
		{"Update label on close button"},
		{"Fix badge colour in dark mode"},
		{"Add tooltip to help icon"},
		{"Update placeholder text in search"},
		{"Fix aria label for modal"},
		{"Add alt text to logo image"},
	}

	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			got := Classify(makeIssue(tc.title))
			if got.Tier != TierSimple {
				t.Errorf("title %q: want TierSimple, got %v", tc.title, got.Tier)
			}
			if got.Model != ModelHaiku {
				t.Errorf("title %q: want ModelHaiku, got %v", tc.title, got.Model)
			}
		})
	}
}

func TestClassify_AutoQALabelsYieldSimple(t *testing.T) {
	cases := []string{"auto-qa", "auto-qa-finding"}
	for _, label := range cases {
		t.Run(label, func(t *testing.T) {
			issue := makeIssue("Some unrelated title about architecture", label)
			got := Classify(issue)
			if got.Tier != TierSimple {
				t.Errorf("label %q: want TierSimple, got %v", label, got.Tier)
			}
			if got.Model != ModelHaiku {
				t.Errorf("label %q: want ModelHaiku, got %v", label, got.Model)
			}
		})
	}
}

func TestClassify_ComplexSignalsInTitleYieldOpus(t *testing.T) {
	cases := []struct {
		title string
	}{
		{"Investigate race condition in event loop"},
		{"Fix deadlock in goroutine scheduler"},
		{"Fix memory leak in WebSocket handler"},
		{"Performance degradation after upgrade"},
		{"Breaking api change in v3 client"},
	}

	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			got := Classify(makeIssue(tc.title))
			if got.Tier != TierComplex {
				t.Errorf("title %q: want TierComplex, got %v", tc.title, got.Tier)
			}
			if got.Model != ModelOpus {
				t.Errorf("title %q: want ModelOpus, got %v", tc.title, got.Model)
			}
		})
	}
}

func TestClassify_ComplexLabelsYieldOpus(t *testing.T) {
	complexLabels := []struct {
		label string
	}{
		{"kind/security"},
		{"kind/regression"},
	}

	for _, tc := range complexLabels {
		t.Run(tc.label, func(t *testing.T) {
			issue := makeIssue("Some ordinary looking title", tc.label)
			got := Classify(issue)
			if got.Tier != TierComplex {
				t.Errorf("label %q: want TierComplex, got %v", tc.label, got.Tier)
			}
			if got.Model != ModelOpus {
				t.Errorf("label %q: want ModelOpus, got %v", tc.label, got.Model)
			}
		})
	}
}

func TestClassify_DefaultTierIsMediumWithSonnet(t *testing.T) {
	issue := makeIssue("Improve error handling in dashboard loader")
	got := Classify(issue)
	if got.Tier != TierMedium {
		t.Errorf("want TierMedium, got %v", got.Tier)
	}
	if got.Model != ModelSonnet {
		t.Errorf("want ModelSonnet, got %v", got.Model)
	}
}

// Simple keywords take priority over complex labels — the tier check in
// classifyTier checks simpleKeywords before label-based complex signals.
func TestClassify_SimpleTitleBeatsComplexLabel(t *testing.T) {
	issue := makeIssue("Fix typo in security guide", "kind/security")
	got := Classify(issue)
	if got.Tier != TierSimple {
		t.Errorf("want TierSimple (simple keyword wins), got %v", got.Tier)
	}
	if got.Model != ModelHaiku {
		t.Errorf("want ModelHaiku, got %v", got.Model)
	}
}

// ---------------------------------------------------------------------------
// Lane tests
// ---------------------------------------------------------------------------

func TestClassify_ArchitectLane(t *testing.T) {
	cases := []struct {
		title string
	}{
		{"RFC: redesign the webhook subsystem"},
		{"Architecture document for v3 api"},
		{"Refactor authentication middleware"},
		{"Migration plan for Postgres"},
		{"Breaking change proposal for protocol"},
		{"Api design discussion for new endpoints"},
	}

	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			got := Classify(makeIssue(tc.title))
			if got.Lane != LaneArchitect {
				t.Errorf("title %q: want LaneArchitect, got %v", tc.title, got.Lane)
			}
		})
	}
}

func TestClassify_ReviewerLane(t *testing.T) {
	cases := []struct {
		desc  string
		title string
		label string
	}{
		{desc: "workflow-failure in title", title: "workflow-failure detected in release pipeline"},
		{desc: "ci-failure in title", title: "ci-failure on main branch"},
		{desc: "nightly in title", title: "nightly e2e failing since yesterday"},
		{desc: "coverage in title", title: "Increase coverage for pkg/classify"},
		{desc: "regression in title", title: "Performance regression after PR merge"},
		{desc: "ga4 in title", title: "GA4 event not firing on checkout"},
		{desc: "analytics in title", title: "Analytics dashboard not showing data"},
		{desc: "nightly label", title: "Something", label: "nightly"},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			var issue github.Issue
			if tc.label != "" {
				issue = makeIssue(tc.title, tc.label)
			} else {
				issue = makeIssue(tc.title)
			}
			got := Classify(issue)
			if got.Lane != LaneCIMaintainer {
				t.Errorf("%s: want LaneCIMaintainer, got %v", tc.desc, got.Lane)
			}
		})
	}
}

func TestClassify_OutreachLane(t *testing.T) {
	cases := []struct {
		title string
	}{
		{"Update ADOPTERS list for Q2"},
		{"Outreach to CNCF projects"},
		{"Community engagement plan for KubeCon"},
		{"Engagement tracker for new adopters"},
	}

	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			got := Classify(makeIssue(tc.title))
			if got.Lane != LaneOutreach {
				t.Errorf("title %q: want LaneOutreach, got %v", tc.title, got.Lane)
			}
		})
	}
}

func TestClassify_OperabilityLanes(t *testing.T) {
	SetLanes([]LaneConfig{
		{Name: "telemetry", Keywords: []string{"observability", "opentelemetry", "prometheus", "grafana", "tracing", "metrics", "structured-logging", "servicemonitor", "podmonitor"}},
		{Name: "operations", Keywords: []string{"healthz", "readyz", "readiness", "slo-", "sli-", "service-level-objective", "service-level-indicator", "error-budget", "runbook", "incident-response", "rollback", "alerting"}},
	})
	defer SetLanes(nil)
	tests := []struct {
		title string
		want  Lane
	}{
		{"Add an OpenTelemetry collector configuration", Lane("telemetry")},
		{"Create a readiness runbook and rollback procedure", Lane("operations")},
		{"Define an SLO for API availability", Lane("scanner")},
		{"Add a service-level-objective for API availability", Lane("operations")},
		{"Fix the slow login redirect", Lane("scanner")},
		{"Fix the sliding window rate limiter", Lane("scanner")},
		{"Fix an array slice panic", Lane("scanner")},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			if got := Classify(makeIssue(tt.title)).Lane; got != tt.want {
				t.Errorf("lane = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassify_DefaultLanesExcludeOperabilityAgents(t *testing.T) {
	SetLanes(nil)
	for _, title := range []string{
		"Add an OpenTelemetry collector configuration",
		"Create a readiness runbook and rollback procedure",
	} {
		if got := Classify(makeIssue(title)).Lane; got != Lane("scanner") {
			t.Fatalf("default lane for %q = %q, want scanner", title, got)
		}
	}
}

func TestClassify_TitlePrefixRoutesToLane(t *testing.T) {
	cases := []struct {
		title    string
		wantLane Lane
	}{
		{"[quality] kernel-swap.sh has no test coverage", LaneQuality},
		{"[quality] Add unit tests for Python scripts", LaneQuality},
		{"[architect] Redesign the webhook subsystem", LaneArchitect},
		{"[ci-maintainer] Fix nightly build", LaneCIMaintainer},
		{"[outreach] Contact CNCF project leads", LaneOutreach},
	}

	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			got := Classify(makeIssue(tc.title))
			if got.Lane != tc.wantLane {
				t.Errorf("title %q: want %v, got %v", tc.title, tc.wantLane, got.Lane)
			}
		})
	}
}

func TestClassify_LabelRoutesToLane(t *testing.T) {
	cases := []struct {
		desc     string
		title    string
		label    string
		wantLane Lane
	}{
		{"quality label", "Some issue about testing", "quality", LaneQuality},
		{"architect label", "Improve error handling", "architect", LaneArchitect},
		{"outreach label", "Follow up with users", "outreach", LaneOutreach},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got := Classify(makeIssue(tc.title, tc.label))
			if got.Lane != tc.wantLane {
				t.Errorf("%s: want %v, got %v", tc.desc, tc.wantLane, got.Lane)
			}
		})
	}
}

func TestClassify_PrefixBeatsKeywordMismatch(t *testing.T) {
	// Title has [quality] prefix but no quality keywords — prefix should win
	issue := makeIssue("[quality] Build scripts lack test parity with bluefin main")
	got := Classify(issue)
	if got.Lane != LaneQuality {
		t.Errorf("want LaneQuality (prefix routing), got %v", got.Lane)
	}
}

func TestClassify_DefaultLaneIsScanner(t *testing.T) {
	issue := makeIssue("Fix edge case in issue parser")
	got := Classify(issue)
	if got.Lane != LaneScanner {
		t.Errorf("want LaneScanner, got %v", got.Lane)
	}
}

// Architect keywords win over ci-maintainer keywords (architect is checked first).
func TestClassify_ArchitectBeatsReviewer(t *testing.T) {
	// "refactor" is architect, "regression" is ci-maintainer — architect wins
	issue := makeIssue("Refactor regression test helpers")
	got := Classify(issue)
	if got.Lane != LaneArchitect {
		t.Errorf("want LaneArchitect (checked before ci-maintainer), got %v", got.Lane)
	}
}

// ---------------------------------------------------------------------------
// Cluster key extraction
// ---------------------------------------------------------------------------

func TestClassify_ClusterKeyExtraction(t *testing.T) {
	cases := []struct {
		title   string
		wantKey string
	}{
		{"Dashboard loading spinner is broken", "dashboard"},
		{"Card border radius too large", "card"},
		{"Sidebar collapses on route change", "sidebar"},
		{"Navbar hides behind modal overlay", "navbar"},
		{"Modal dialog does not close on Esc", "modal"},
		{"API rate limit exceeded error", "api"},
		{"Webhook payload not received", "webhook"},
		{"CI pipeline fails on PR open", "ci"},
		{"Nightly job failing since Friday", "nightly"},
		{"Drasi integration not working", "drasi"},
		{"Benchmark results missing from report", "benchmark"},
		{"GPU memory allocation fails", "gpu"},
		{"Mission install fails on OpenShift", "mission"},
		{"Studio layout broken on small screens", "studio"},
		{"Random unrelated issue", ""},
	}

	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			got := Classify(makeIssue(tc.title))
			if got.ClusterKey != tc.wantKey {
				t.Errorf("title %q: want ClusterKey %q, got %q", tc.title, tc.wantKey, got.ClusterKey)
			}
		})
	}
}

// The first matching prefix wins (dashboard before card in the prefix list).
func TestClassify_ClusterKeyFirstMatchWins(t *testing.T) {
	// "dashboard" appears before "card" in prefixes
	issue := makeIssue("Dashboard card hover state broken")
	got := Classify(issue)
	if got.ClusterKey != "dashboard" {
		t.Errorf("want ClusterKey %q, got %q", "dashboard", got.ClusterKey)
	}
}

// ---------------------------------------------------------------------------
// ClassifyAll tests
// ---------------------------------------------------------------------------

func TestClassifyAll_MutatesSlice(t *testing.T) {
	issues := []github.Issue{
		makeIssue("Fix typo in help text"),
		makeIssue("Investigate race condition in cache layer"),
		makeIssue("Update error message wording"),
	}

	result := ClassifyAll(issues)

	// ClassifyAll returns the same (mutated) slice. The underlying array
	// might differ due to slice semantics, so the mutation is verified by
	// checking values below rather than pointer identity.

	if result[0].ComplexityTier != string(TierSimple) {
		t.Errorf("issue[0]: want ComplexityTier %q, got %q", TierSimple, result[0].ComplexityTier)
	}
	if result[0].ModelRec != string(ModelHaiku) {
		t.Errorf("issue[0]: want ModelRec %q, got %q", ModelHaiku, result[0].ModelRec)
	}

	if result[1].ComplexityTier != string(TierComplex) {
		t.Errorf("issue[1]: want ComplexityTier %q, got %q", TierComplex, result[1].ComplexityTier)
	}
	if result[1].ModelRec != string(ModelOpus) {
		t.Errorf("issue[1]: want ModelRec %q, got %q", ModelOpus, result[1].ModelRec)
	}

	if result[2].ComplexityTier != string(TierMedium) {
		t.Errorf("issue[2]: want ComplexityTier %q, got %q", TierMedium, result[2].ComplexityTier)
	}
	if result[2].ModelRec != string(ModelSonnet) {
		t.Errorf("issue[2]: want ModelRec %q, got %q", ModelSonnet, result[2].ModelRec)
	}
}

func TestClassifyAll_LaneFieldSet(t *testing.T) {
	issues := []github.Issue{
		makeIssue("RFC: redesign storage layer"),
		makeIssue("Nightly build failed on main"),
		makeIssue("Community outreach for CNCF"),
		makeIssue("Handle nil pointer in parser"),
	}

	result := ClassifyAll(issues)

	wantLanes := []string{
		string(LaneArchitect),
		string(LaneCIMaintainer),
		string(LaneOutreach),
		string(LaneScanner),
	}

	for i, want := range wantLanes {
		if result[i].Lane != want {
			t.Errorf("issue[%d] %q: want Lane %q, got %q", i, issues[i].Title, want, result[i].Lane)
		}
	}
}

func TestClassifyAll_EmptySlice(t *testing.T) {
	result := ClassifyAll([]github.Issue{})
	if len(result) != 0 {
		t.Errorf("want empty result, got len=%d", len(result))
	}
}

func TestClassifyAll_OriginalSliceIsMutated(t *testing.T) {
	issues := []github.Issue{
		makeIssue("Fix typo"),
	}
	ClassifyAll(issues)
	// ClassifyAll mutates via index range, so issues[0] itself should be updated
	if issues[0].ComplexityTier != string(TierSimple) {
		t.Errorf("original slice not mutated: want %q, got %q", TierSimple, issues[0].ComplexityTier)
	}
}

// ---------------------------------------------------------------------------
// Case-insensitivity tests
// ---------------------------------------------------------------------------

func TestClassify_CaseInsensitiveTitle(t *testing.T) {
	cases := []struct {
		title    string
		wantTier Tier
	}{
		{"Fix TYPO in docs", TierSimple},
		{"Investigate RACE CONDITION in scheduler", TierComplex},
		{"RENAME the package", TierSimple},
		{"MEMORY LEAK in cache", TierComplex},
	}
	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			got := Classify(makeIssue(tc.title))
			if got.Tier != tc.wantTier {
				t.Errorf("title %q: want %v, got %v", tc.title, tc.wantTier, got.Tier)
			}
		})
	}
}

func TestClassify_CaseInsensitiveLabel(t *testing.T) {
	// Labels are joined and lowercased before matching, so uppercase label
	// values should still match.
	issue := makeIssue("Ordinary issue title", "KIND/SECURITY")
	got := Classify(issue)
	if got.Tier != TierComplex {
		t.Errorf("uppercase kind/security label: want TierComplex, got %v", got.Tier)
	}
}

// ---------------------------------------------------------------------------
// tierToModel direct tests (edge coverage for the switch)
// ---------------------------------------------------------------------------

func TestTierToModel(t *testing.T) {
	cases := []struct {
		tier  Tier
		model ModelRecommendation
	}{
		{TierSimple, ModelHaiku},
		{TierMedium, ModelSonnet},
		{TierComplex, ModelOpus},
		{"unknown", ModelSonnet}, // default branch
	}
	for _, tc := range cases {
		got := tierToModel(tc.tier)
		if got != tc.model {
			t.Errorf("tierToModel(%q): want %q, got %q", tc.tier, tc.model, got)
		}
	}
}
