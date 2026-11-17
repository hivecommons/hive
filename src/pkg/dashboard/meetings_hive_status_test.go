package dashboard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestHiveStatusBrief pins hivecommons/hive#18590 finding 9: the meeting
// attendee answers hive-state questions from the dashboard's in-memory
// status, the same counts the Overview shows.
func TestHiveStatusBrief(t *testing.T) {
	s := &Server{}
	if _, err := s.HiveStatusBrief(context.Background()); !errors.Is(err, errHiveStatusUnavailable) {
		t.Fatalf("no snapshot: err = %v", err)
	}
	st := chatEvalStatus(time.Now())
	st.OpenItems.Buckets.Actionable = FrontendOpenItemKinds{Issues: 7, PRs: 3}
	st.OpenItems.Buckets.Held = FrontendOpenItemKinds{Issues: 4, PRs: 1}
	st.OpenItems.Buckets.Blocked = FrontendOpenItemKinds{Issues: 2}
	st.Agents = append(st.Agents, FrontendAgent{Name: "coder", State: "running", Busy: "working", Doing: "fixing #13990"})
	st.ActionableNow = FrontendActionableNow{Issues: 7, PRs: 3, Total: 10}
	st.OverviewCoverage = &FrontendOverviewCoverage{Coverage: 81, Target: 90}
	st.OverviewTotals.Issues = FrontendOverviewTotal{Forge: 40, Tracked: 12, Held: 4, Breakdown: map[string]int{"needs_human": 3}}
	s.status = st
	got, err := s.HiveStatusBrief(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Open now: 40 issues and 12 PRs.",
		"Actionable: 7 issues, 3 PRs. Held: 4 issues, 1 PRs. Blocked: 2 issues, 0 PRs.",
		"Board pressure (BP): 40/12, that is 40 open issues and 12 open PRs",
		"Actionable now: 10 (7 issues, 3 PRs).",
		"Test coverage: 81% (target 90%).",
		"Open issues by stage: tracked 12; held 4; needs human 3.",
		"Agents: 17, 2 working, 1 paused, 1 blocked, 14 idle.",
		"Working right now: coder (fixing #13990); reviewer-2.",
		"Blocked: reviewer-2.",
		"Recently finished: scanner: opened PR hivecommons/hive#14001",
		"Open PRs: ",
		"14002 fix: other (CI failure)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	if len(got) > hiveStatusMaxChars {
		t.Fatalf("brief is %d bytes", len(got))
	}

	st.StaleSince = "2026-11-16T10:00:00Z"
	st.OpenItems.IssuesSyncing = true
	got, _ = s.HiveStatusBrief(context.Background())
	if !strings.Contains(got, "at least 40 issues (the issue count is still syncing: a partial count, not a total)") || !strings.Contains(got, "last-good figures from 2026-11-16T10:00:00Z") {
		t.Fatalf("stale and syncing are said: %s", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.HiveStatusBrief(ctx); err == nil {
		t.Fatal("a cancelled context returns its error")
	}
}

func TestHiveStatusListCaps(t *testing.T) {
	var b strings.Builder
	hiveStatusList(&b, "L", []string{"1", "2", "3", "4", "5", "6", "7"})
	if got := b.String(); got != "L: 1; 2; 3; 4; 5 (and 2 more)." {
		t.Fatalf("got %q", got)
	}
	b.Reset()
	hiveStatusList(&b, "L", nil)
	if b.Len() != 0 {
		t.Fatal("an empty list writes nothing")
	}
}

// Scored live test: she said "0 open issues" while about 35 were open
// across the hive's 13 repos, because the brief read the raw snapshot,
// not the derived open-items set /api/status serves. The brief counts
// every configured repo and says the scope.
func TestHiveStatusBriefCountsAcrossRepos(t *testing.T) {
	s := &Server{}
	s.status = &StatusPayload{
		HiveID: "hive-test",
		Repos: []FrontendRepo{
			{Name: "hive", Full: "hivecommons/hive", Issues: 20, PRs: 7},
			{Name: "quorum", Full: "hivecommons/quorum", Issues: 10, PRs: 3},
			{Name: "vexa", Full: "hivecommons/vexa", Issues: 5, PRs: 2},
			{Name: "docs", Full: "hivecommons/docs"},
		},
	}
	got, err := s.HiveStatusBrief(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Open now: 35 issues and 12 PRs across 4 repos",
		"hive 20 issues, 7 PRs",
		"Board pressure (BP): 35/12",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "0 issues and") {
		t.Fatalf("a zero total must never be stated: %s", got)
	}
}

// Counts that are not in yet are never stated as zero.
func TestHiveStatusBriefNeverStatesUnbackedZero(t *testing.T) {
	st := chatEvalStatus(time.Now())
	st.OpenItems = FrontendOpenItems{}
	st.ActionableNow = FrontendActionableNow{}
	s := &Server{}
	s.status = st
	got, err := s.HiveStatusBrief(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "not available yet") || strings.Contains(got, "Open now: 0") || strings.Contains(got, "Board pressure (BP): 0") {
		t.Fatalf("unbacked zero stated: %s", got)
	}
}
