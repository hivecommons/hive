package dashboard

import (
	"strings"
	"testing"
)

func TestGovernorPRModelsUIRendersReworkStats(t *testing.T) {
	body, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"First-pass",
		"Review rounds",
		"Fix attempts",
		"Most reworked PRs",
		"most_reworked",
		"avg_review_rounds",
		"govPRModelsSort = 'effectiveness'",
		"setGovernorPRModelsSort",
		"PR runs",
		"No ship",
		"b.effectiveness",
		"Reviewer accuracy",
		"/api/reviewer/accuracy",
		"false approve",
		"Confidence calibration by model",
		"no runs recorded, not a measured 0%",
		"first_pass_merge_rate_ci",
		"closed_prs_partial",
		// Per-model PR list (#18722).
		"toggleGovernorPRModelPRs",
		"data-action=\"toggleGovernorPRModelPRs\"",
		"&model=${encodeURIComponent(model)}",
		"gov-pr-model-prlist",
		"prs_truncated",
		"b.efforts",
		"By effort:",
		"avg_fix_attempts",
		"sample_prs",
		"governorPRModelSliceDetail",
		"b?.repos",
		"b?.weeks",
		"week_start",
		"By repository:",
		"Weekly trend",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("static/index.html missing %q", want)
		}
	}
}
