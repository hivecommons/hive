package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestReporterTrustHoldSurfacesHumanReason(t *testing.T) {
	const reason = "reporter-trust hold — issue #581 filed by @stranger (o/r)"
	pr := github.PullRequest{Repo: "o/r", Number: 42, Labels: []string{"hold", "needs-human"}, HiveAttributed: true, ReporterTrustReason: reason}
	band := PRBand(pr, true, config.DashboardIssueBandsConfig{}, time.Now())
	if band.Band != "waiting" || !strings.Contains(band.HoldReason, reason) || strings.Contains(band.HoldReason, "attempts exhausted") {
		t.Fatalf("band=%+v", band)
	}
	found := false
	for _, signal := range band.Signals {
		if signal.Label == reason {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing reporter reason: %+v", band.Signals)
	}
	status := &StatusPayload{Repos: []FrontendRepo{{Name: "o/r", HeldPrs: []any{FrontendPR{PullRequest: pr}}}}}
	queue := (&Server{}).buildHiveAdvisorQueue(status, time.Now())
	if len(queue.PRs) != 1 || !queue.PRs[0].NeedsHuman || queue.PRs[0].NeedsHumanReason != reason {
		t.Fatalf("advice queue=%+v", queue.PRs)
	}
}
