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
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("static/index.html missing %q", want)
		}
	}
}
