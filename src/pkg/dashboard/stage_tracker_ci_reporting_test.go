package dashboard

import (
	"strings"
	"testing"
)

func TestCIReportingUsesExistingReasonWithoutPromotingStage(t *testing.T) {
	for _, prefix := range []string{"stalled", "unverified"} {
		reason := "ci-reporting-" + prefix + ":check=\"build-and-test\" run=21 attempt=2 owner-action-required"
		got := humanizeStageReason(reason)
		if !strings.HasPrefix(got, "CI reporting "+prefix+" — ") ||
			!strings.Contains(got, "run=21 attempt=2") || !strings.Contains(got, "owner-action-required") {
			t.Fatalf("reporting provenance disappeared from existing tooltip reason: %s", got)
		}
	}
}
