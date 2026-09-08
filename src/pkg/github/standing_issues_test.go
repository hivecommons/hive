package github

import (
	"context"
	"net/http/httptest"
	"testing"
)

// These tests pin the standing-meta-issue skip at THE choice point —
// EnumerateActionable/fetchIssues — so the hive's own advisory report and bot
// dependency dashboards never enter the actionable set the dashboard counts
// and the scheduler feeds into kick prompts. The contribute queue already
// denies these to human contributors by default; this is the agent-path half
// of the same judgment.

func standingTestIssues() []wireIssue {
	return []wireIssue{
		// Real work — the positive control. A title merely MENTIONING
		// dependencies must never be swallowed by the dashboard-title match.
		{Number: 1, Title: "fix dependency dashboard rendering in docs", User: wireUser{"alice"},
			Labels: []wireLabel{{Name: "bug"}}, CreatedAt: hoursAgo(3)},
		// The hive's own advisory report, exactly as advisory.go files it.
		{Number: 2, Title: "🐝 Hive Advisory Report", User: wireUser{"some-app[bot]"},
			Labels: []wireLabel{{Name: "hive/advisory"}}, CreatedAt: hoursAgo(2)},
		// Renovate's control panel: bot author + dashboard title, no labels
		// (Renovate creates it unlabeled by default, so no label-based
		// exemption can catch it).
		{Number: 3, Title: "Dependency Dashboard", User: wireUser{"renovate[bot]"},
			CreatedAt: hoursAgo(2)},
		// An advisory report that lost its label but kept the exact title.
		{Number: 4, Title: "🐝 Hive Advisory Report", User: wireUser{"some-app[bot]"},
			CreatedAt: hoursAgo(1)},
		// A HUMAN using a dashboard-ish title stays actionable: the bot arm
		// requires the author to be a known bot.
		{Number: 5, Title: "Dependency Dashboard is confusing, redesign it", User: wireUser{"bob"},
			CreatedAt: hoursAgo(1)},
		// The advisory report with a hold label must vanish entirely, not
		// surface in the Hold list.
		{Number: 6, Title: "🐝 Hive Advisory Report", User: wireUser{"some-app[bot]"},
			Labels: []wireLabel{{Name: "hive/advisory"}, {Name: "hold"}}, CreatedAt: hoursAgo(1)},
	}
}

func enumerateStanding(t *testing.T) *ActionableResult {
	t.Helper()
	org, repo := "testorg", "testrepo"
	mux := buildMux(t, org, repo, standingTestIssues(), nil)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	return result
}

func TestEnumerateActionable_SkipsStandingMetaIssues(t *testing.T) {
	result := enumerateStanding(t)
	nums := actionableNumbers(result)

	if !nums[1] {
		t.Error("positive control failed: issue #1 (real work mentioning a dashboard) is not actionable")
	}
	if !nums[5] {
		t.Error("issue #5 (human-authored, dashboard-ish title) must stay actionable — the bot arm requires a bot author")
	}
	for _, n := range []int{2, 3, 4, 6} {
		if nums[n] {
			t.Errorf("issue #%d is a standing meta issue but entered the actionable set", n)
		}
	}
	if got := result.Issues.Count; got != 2 {
		t.Errorf("Issues.Count = %d, want 2 (issues #1 and #5)", got)
	}
	for _, h := range result.Hold.Items {
		if h.Number == 6 {
			t.Error("issue #6 (held advisory report) surfaced in the Hold list; standing meta issues must vanish entirely")
		}
	}
}

func TestStandingMetaIssueReason(t *testing.T) {
	cases := []struct {
		name, title, author string
		labels              []string
		wantSkip            bool
	}{
		{"advisory by exact title", "🐝 Hive Advisory Report", "anyone", nil, true},
		{"advisory by label, retitled", "weekly digest", "anyone", []string{"HIVE/Advisory"}, true},
		{"renovate dashboard", "Dependency Dashboard", "renovate[bot]", nil, true},
		{"renovate dashboard, case-insensitive author", "Renovate Dashboard", "Renovate[bot]", nil, true},
		{"dependabot dashboard", "Dependency Dashboard", "dependabot[bot]", nil, true},
		{"human with dashboard title", "Dependency Dashboard", "alice", nil, false},
		{"bot with non-dashboard title", "chore: update lockfile", "renovate[bot]", nil, false},
		{"ordinary issue", "fix the frobnicator", "bob", []string{"bug"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := standingMetaIssueReason(tc.title, tc.author, tc.labels)
			if (reason != "") != tc.wantSkip {
				t.Errorf("standingMetaIssueReason(%q, %q, %v) = %q, wantSkip=%v",
					tc.title, tc.author, tc.labels, reason, tc.wantSkip)
			}
		})
	}
}
