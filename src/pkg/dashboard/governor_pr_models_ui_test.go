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
		"b.efforts",
		"By effort:",
		"avg_fix_attempts",
		"sample_prs",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("static/index.html missing %q", want)
		}
	}
}
