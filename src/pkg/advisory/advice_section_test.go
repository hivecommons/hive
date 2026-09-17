package advisory

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hiveadvisor"
)

func TestFormatDigestMarkdownIncludesAdviceSection(t *testing.T) {
	end := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	advice := &hiveadvisor.Result{
		Epoch: hiveadvisor.Epoch{Mode: "SURGE", End: end},
		Recommendations: []hiveadvisor.Recommendation{{
			ID:        "throttle-pr-producing-lanes",
			Title:     "Throttle PR-producing lanes",
			Rationale: "Reduce new PR creation until pressure falls.",
			Signals:   []hiveadvisor.Signal{{Name: "mode", Value: "SURGE"}},
		}},
		NextReviewInDays: 3,
	}
	md := FormatDigestMarkdown(BuildDigest(nil, "SURGE"), DigestOptions{ShowEmpty: true, Advice: advice})
	for _, want := range []string{"## Advice", "Throttle PR-producing lanes", "mode=SURGE", "next review in 3 day(s)"} {
		if !strings.Contains(md, want) {
			t.Fatalf("digest missing %q:\n%s", want, md)
		}
	}
}
