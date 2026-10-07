package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestPRBandReporterTrustHoldReason(t *testing.T) {
	reason := (github.ReporterTrust{Held: true, Issue: 42, Reporter: "outsider"}).NeedsHumanReason()
	pr := github.PullRequest{Labels: []string{"hold", "needs-human"}, HiveAttributed: true, ReporterTrustReason: reason}
	info := PRBand(pr, true, config.DashboardIssueBandsConfig{}, time.Now())
	if info.Band != "waiting" || !info.Held || !strings.Contains(info.HoldReason, reason) {
		t.Fatalf("reporter-trust hold missing from waiting band: %+v", info)
	}
	for _, wrong := range []string{"automated fix attempts exhausted", "ACMM level gate"} {
		if strings.Contains(info.HoldReason, wrong) {
			t.Fatalf("reporter-trust hold misdescribed: %s", info.HoldReason)
		}
	}
	pr.ReporterTrustReason = ""
	info = PRBand(pr, true, config.DashboardIssueBandsConfig{}, time.Now())
	if !strings.Contains(info.HoldReason, "automated fix attempts exhausted") {
		t.Fatalf("existing fix-loop hold fallback changed: %s", info.HoldReason)
	}
}
