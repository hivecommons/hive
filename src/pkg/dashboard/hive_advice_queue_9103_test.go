package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hiveadvisor"
)

// TestBuildHiveAdvisorQueueUsesOverviewClassifier9103 pins that the advisor
// sees exactly what the Overview export sees: same items (open + held), same
// bands, and the per-item causes the queue-health rules split on.
func TestBuildHiveAdvisorQueueUsesOverviewClassifier9103(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{Dashboard: config.DashboardConfig{IssueBands: config.DashboardIssueBandsConfig{StaleDays: 3}}}}
	status := parityStatus9102(t)
	now := mustTime9102(t, "2026-09-27T00:00:00Z")
	status.Repos[0].OpenPrs = append(status.Repos[0].OpenPrs, FrontendPR{
		PullRequest:  github.PullRequest{Number: 16, Title: "verdict blocked", Author: "bob", HiveAgent: "scanner", Labels: []string{}, UpdatedAt: mustTime9102(t, "2026-09-20T00:00:00Z"), CreatedAt: mustTime9102(t, "2026-09-10T00:00:00Z")},
		MergeVerdict: &github.MergeVerdict{State: github.MergeVerdictBlocked, Reason: "base protection"},
	})

	q := s.buildHiveAdvisorQueue(status, now)

	rows, _ := s.overviewPRRows(status, s.issueBandsConfig(), overviewFilters{}, now)
	if len(q.PRs) != len(rows) || len(q.PRs) != 8 {
		t.Fatalf("advisor saw %d PRs, export saw %d rows (want 8: 7 open + 1 held)", len(q.PRs), len(rows))
	}
	issueRows, _ := s.overviewIssueRows(status, s.issueBandsConfig(), overviewFilters{}, now)
	if len(q.Issues) != len(issueRows) || len(q.Issues) != 8 {
		t.Fatalf("advisor saw %d issues, export saw %d rows", len(q.Issues), len(issueRows))
	}
	if q.StaleDays != 3 {
		t.Fatalf("stale days = %d, want the configured 3", q.StaleDays)
	}

	byNum := map[int]hiveadvisor.PRItem{}
	for _, pr := range q.PRs {
		byNum[pr.Number] = pr
	}
	blocked := byNum[12]
	if blocked.Band != "blocked" || !blocked.CIFailing || !blocked.Conflict || blocked.VerdictBlocked || strings.Join(blocked.FailingChecks, ",") != "ci/test" || !blocked.Stale {
		t.Fatalf("PR 12 = %+v", blocked)
	}
	if blocked.URL != "https://github.com/octo/demo/pull/12" || blocked.Repo != "octo/demo" || blocked.AgeDays != 25 || blocked.IdleDays != 24 {
		t.Fatalf("PR 12 identity/age = %+v", blocked)
	}
	verdict := byNum[16]
	if verdict.Band != "blocked" || !verdict.VerdictBlocked || verdict.VerdictReason != "base protection" || verdict.CIFailing || verdict.Lane != "scanner" || verdict.Author != "bob" {
		t.Fatalf("PR 16 = %+v", verdict)
	}
	if held := byNum[9]; held.Band != "waiting" || !held.Held || held.NeedsHuman {
		t.Fatalf("held PR 9 = %+v", held)
	}
	if nh := byNum[10]; nh.Band != "waiting" || nh.Held || !nh.NeedsHuman || nh.Lane != "" {
		t.Fatalf("needs-human PR 10 = %+v", nh)
	}
	// The classifier ranks blocked above draft, and eligible above blocked:
	// the advisor must take the band as classified, not re-derive it.
	if byNum[15].Band != "blocked" || byNum[11].Band != "eligible" || !byNum[11].Conflict {
		t.Fatalf("draft-failing/eligible-conflict bands: %+v %+v", byNum[15], byNum[11])
	}

	byIssue := map[int]hiveadvisor.IssueItem{}
	for _, is := range q.Issues {
		byIssue[is.Number] = is
	}
	if byIssue[6].Band != "done" || byIssue[5].Band != "waiting" || byIssue[3].Band != "agent-filed" || byIssue[2].Band != "in-progress" {
		t.Fatalf("issue bands: %+v", byIssue)
	}
	if byIssue[8].Repo != "octo/demo" || byIssue[8].URL != "https://github.com/octo/demo/issues/8" {
		t.Fatalf("held issue 8 = %+v", byIssue[8])
	}

	// Band rule text is the same the Overview tooltips print.
	var blockedRule hiveadvisor.BandRule
	for _, b := range q.PRBands {
		if b.Key == "blocked" {
			blockedRule = b
		}
	}
	if blockedRule.Kind != "pr" || blockedRule.Label != "Blocked" || blockedRule.Rule != prBandSpec("blocked", s.issueBandsConfig()).Rule {
		t.Fatalf("blocked band rule = %+v", blockedRule)
	}
}

// TestAttachHiveAdviceExplainsBlockedQueueInIdle9103: governor IDLE with an
// actionable queue of 0 and a chart full of blocked PRs must lead with the
// blocked-PR advice, name the cause, link the CSV, and quote both counts.
func TestAttachHiveAdviceExplainsBlockedQueueInIdle9103(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{}}
	status := minimalPayload()
	status.Governor.Mode = "idle"
	status.Governor.Issues = 2
	status.Governor.PRs = 0
	status.Agents = []FrontendAgent{{Name: "guide", Enabled: true, NoCadence: true}}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	prs := make([]any, 0, 4)
	for i := 1; i <= 3; i++ {
		prs = append(prs, FrontendPR{PullRequest: github.PullRequest{Number: i, CIStatus: "failing", FailingChecks: []string{"go-security-analysis"}, HiveAgent: "scanner", CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-24 * time.Hour)}})
	}
	prs = append(prs, FrontendPR{PullRequest: github.PullRequest{Number: 4, ReviewURL: "https://reviews/4"}})
	status.Repos = []FrontendRepo{{Full: "octo/demo", OpenPrs: prs}}

	res := s.AttachHiveAdvice(status, now)
	if res.Frozen || len(res.Recommendations) == 0 || res.Recommendations[0].ID != "reduce-blocked-prs" {
		t.Fatalf("recommendations = %+v", res.Recommendations)
	}
	top := res.Recommendations[0]
	if !strings.Contains(top.Rationale, "3 of 4 open PRs (75%) are Blocked. 3 of 3 fail check `go-security-analysis`") {
		t.Fatalf("rationale = %q", top.Rationale)
	}
	if len(top.Links) == 0 || top.Links[0].URL != "/api/overview/prs.csv?band=blocked" {
		t.Fatalf("links = %+v", top.Links)
	}
	if len(top.Items) != 3 || top.Items[0].URL != "https://github.com/octo/demo/pull/1" {
		t.Fatalf("items = %+v", top.Items)
	}
	if res.Counts != (hiveadvisor.Counts{GovernorIssues: 2, GovernorPRs: 0, OverviewIssues: 0, OverviewPRs: 4}) {
		t.Fatalf("counts = %+v", res.Counts)
	}
	if len(res.Bands) != 1 || res.Bands[0].Label != "Blocked" || !strings.Contains(res.Bands[0].Rule, "failing CI") {
		t.Fatalf("bands = %+v", res.Bands)
	}

	// Custom thresholds from governor.advisory.queue_health are honoured.
	s.deps.Config.Governor.Advisory.QueueHealth.BlockedPRPct = 80
	s.hiveAdviceEpoch, s.hiveAdviceLast = nil, nil
	strict := s.AttachHiveAdvice(status, now)
	for _, r := range strict.Recommendations {
		if r.ID == "reduce-blocked-prs" {
			t.Fatalf("75%% blocked fired against an 80%% threshold: %v", strict.Recommendations)
		}
	}
}
