package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestGuideFlowSignals(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	const day = 24 * time.Hour
	allowed := map[string]bool{"test-org/hive": true}
	merged := func(number int, age, since time.Duration) github.PullRequest {
		return github.PullRequest{Repo: "test-org/hive", Number: number, CreatedAt: now.Add(-since - age), MergedAt: now.Add(-since)}
	}
	tests := []struct {
		name  string
		a     *github.ActionableResult
		dwell time.Duration
		known bool
		want  []string
	}{
		{"missing", nil, 0, false, []string{"HIVE_FLOW: unknown", "surge=unknown", "mttm_sample=unknown", "oldest_actionable=unknown"}},
		{"no merges", &github.ActionableResult{GeneratedAt: now}, 0, true, []string{"HIVE_FLOW: unknown clogged_by=none", "merged_sample=0", "oldest_actionable=0m"}},
		{"surge boundary", nil, 3 * day, true, []string{"HIVE_FLOW: clogged clogged_by=surge ", "surge=4320m"}},
		{"healthy boundaries", &github.ActionableResult{GeneratedAt: now, PRs: github.PRResult{Attributed: []github.PullRequest{merged(1, 7*day, day)}}}, 3*day - time.Minute, true, []string{"HIVE_FLOW: normal clogged_by=none", "mttm_sample=10080m"}},
		{"slow merges", &github.ActionableResult{GeneratedAt: now, PRs: github.PRResult{Attributed: []github.PullRequest{merged(1, 8*day, day), merged(2, 10*day, 2*day)}}}, 0, true, []string{"HIVE_FLOW: clogged clogged_by=mttm ", "mttm_sample=12960m", "merged_sample=2"}},
		{"old issue", &github.ActionableResult{GeneratedAt: now, Issues: github.IssueResult{Items: []github.Issue{{Repo: "test-org/hive", AgeMinutes: 14 * 24 * 60}}}}, 0, true, []string{"HIVE_FLOW: clogged clogged_by=oldest ", "oldest_actionable=20160m"}},
		{"old pr", &github.ActionableResult{GeneratedAt: now, PRs: github.PRResult{Items: []github.PullRequest{{Repo: "test-org/hive", CreatedAt: now.Add(-14 * day)}}}}, 0, true, []string{"HIVE_FLOW: clogged clogged_by=oldest ", "oldest_actionable=20160m"}},
		// #20273: a long SURGE and one held PR with an empty merge sample
		// must name both breaches, so the guide can see no merge evidence.
		{"every breach named", &github.ActionableResult{GeneratedAt: now, PRs: github.PRResult{Items: []github.PullRequest{{Repo: "test-org/hive", CreatedAt: now.Add(-30 * day)}}}}, 4681 * time.Minute, true, []string{"HIVE_FLOW: clogged clogged_by=surge,oldest ", "surge=4681m", "mttm_sample=unknown", "merged_sample=0"}},
		// #20273: drafts and PRs waiting on their author do not age the flow.
		{"held prs ignored", &github.ActionableResult{GeneratedAt: now, PRs: github.PRResult{Items: []github.PullRequest{
			{Repo: "test-org/hive", Draft: true, CreatedAt: now.Add(-300 * day)},
			{Repo: "test-org/hive", CreatedAt: now.Add(-300 * day), Protection: &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionChangesRequested}},
			{Repo: "test-org/hive", CreatedAt: now.Add(-2 * day), Protection: &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionReviewRequired}},
		}}}, 0, true, []string{"HIVE_FLOW: unknown clogged_by=none", "oldest_actionable=2880m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := guideFlowLine(tt.a, allowed, tt.dwell, tt.known)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("%q missing %q", got, want)
				}
			}
		})
	}
	// Ignore out-of-window, future, unmerged, invalid and duplicate samples.
	valid := merged(1, day, 14*day)
	foreign := merged(2, 30*day, day)
	foreign.Repo = "other/repo"
	a := &github.ActionableResult{GeneratedAt: now, PRs: github.PRResult{Attributed: []github.PullRequest{
		valid, valid, foreign, merged(3, 30*day, 14*day+time.Second), merged(4, day, -day), merged(5, -day, day), {Repo: "test-org/hive", Number: 6},
	}, Items: []github.PullRequest{{Repo: "other/repo", CreatedAt: now.Add(-30 * day)}}}, Issues: github.IssueResult{Items: []github.Issue{{Repo: "other/repo", AgeMinutes: 999999}}}}
	got := guideFlowLine(a, allowed, 0, true)
	for _, want := range []string{"HIVE_FLOW: normal clogged_by=none", "merged_sample=1", "mttm_sample=1440m", "oldest_actionable=0m"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing %q", got, want)
		}
	}
}

func TestGuideFlowKickIntegration(t *testing.T) {
	s := newScheduler()
	s.SetSurgeDuration(func() (time.Duration, bool) { return 72 * time.Hour, true })
	a := &github.ActionableResult{GeneratedAt: time.Now()}
	kicks := s.BuildKickMessages(a, []string{"guide", "scanner"})
	if len(kicks) != 2 {
		t.Fatalf("got %d kicks", len(kicks))
	}
	if !strings.Contains(kicks[0].Message, "HIVE_FLOW: clogged") {
		t.Fatal("guide kick missing flow signal")
	}
	if strings.Contains(kicks[1].Message, "HIVE_FLOW:") {
		t.Fatal("flow signal leaked to scanner")
	}
	if got := s.addGuideFlow("guide", a, ""); got != "" {
		t.Fatal("empty kick became a flow kick")
	}
	// A scoped kick must retain only that repo's attributed merge sample.
	a.PRs.Attributed = []github.PullRequest{{Repo: "test-org/hive", Number: 1}, {Repo: "test-org/docs", Number: 2}}
	scoped := actionableForRepo(a, "test-org/hive")
	if len(scoped.PRs.Attributed) != 1 || scoped.PRs.Attributed[0].Number != 1 {
		t.Fatal("repo-scoped merge sample lost or leaked")
	}
}

func TestGuideFlowManualAndReplica(t *testing.T) {
	s := newScheduler()
	s.cfg.Agents = map[string]config.AgentConfig{"guide-2": {ReplicaOf: "guide"}}
	s.SetSurgeDuration(func() (time.Duration, bool) { return 72 * time.Hour, true })
	s.SetLastActionable(&github.ActionableResult{GeneratedAt: time.Now()})
	for _, agent := range []string{"guide", "guide-2"} {
		got := s.BuildAgentMessageFromLastActionable(agent)
		if !strings.Contains(got, "HIVE_FLOW: clogged clogged_by=surge surge=4320m") {
			t.Fatalf("%s manual kick missing flow signal", agent)
		}
	}
}
