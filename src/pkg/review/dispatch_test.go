package review

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/outputschema"
)

func reviewer(name string) AgentCapability {
	return AgentCapability{Name: name, Enabled: true, UsesKick: true, Role: "reviewer"}
}

func dispatchPR(sha string) PullRequest {
	return PullRequest{Repo: "hive", Number: 7, Title: "fix bug", Author: "hive-bot[bot]", HeadSHA: sha, URL: "https://example.invalid/pr/7", Lane: "quality"}
}

func TestPlanDispatchStateMachine(t *testing.T) {
	now := time.Date(2026, 8, 7, 1, 2, 3, 0, time.UTC)
	tests := []struct {
		name        string
		opts        DispatchOptions
		state       DispatchState
		artifact    Artifact
		wantReviews int
		wantPending int
	}{
		{name: "fan out disabled is no-op", opts: DispatchOptions{RequireApproval: true, FanOut: false, Agents: []AgentCapability{reviewer("r1")}}, wantReviews: 0, wantPending: 0},
		{name: "parallel reviewers dispatch up to cap", opts: DispatchOptions{RequireApproval: true, FanOut: true, MaxParallelReviews: 3, Agents: []AgentCapability{reviewer("r1"), reviewer("r2"), reviewer("r3")}}, wantReviews: 3, wantPending: 3},
		{name: "single reviewer dispatches one sequential round", opts: DispatchOptions{RequireApproval: true, FanOut: true, Agents: []AgentCapability{reviewer("r1")}}, wantReviews: 1, wantPending: 1},
		{name: "fresh aggregate suppresses duplicate review", opts: DispatchOptions{RequireApproval: true, FanOut: true, Agents: []AgentCapability{reviewer("r1"), reviewer("r2")}}, artifact: Artifact{Items: []Aggregate{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Verdict: VerdictApprove}}}, wantReviews: 0, wantPending: 0},
		{name: "pending same head suppresses duplicate perspective", opts: DispatchOptions{RequireApproval: true, FanOut: true, MaxParallelReviews: 5, Agents: []AgentCapability{reviewer("r1"), reviewer("r2")}}, state: DispatchState{Pending: []PendingReview{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Agent: "r1"}}}, wantReviews: 4, wantPending: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.ProjectOrg = "acme"
			tt.opts.Now = now
			plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, tt.artifact, tt.state, tt.opts)
			if len(plan.ReviewKicks) != tt.wantReviews || len(plan.State.Pending) != tt.wantPending {
				t.Fatalf("reviews=%d pending=%d, want %d/%d", len(plan.ReviewKicks), len(plan.State.Pending), tt.wantReviews, tt.wantPending)
			}
		})
	}
}

func TestPlanDispatchSHAInvalidation(t *testing.T) {
	state := DispatchState{Pending: []PendingReview{{Repo: "acme/hive", Number: 7, HeadSHA: "old", Perspective: PerspectiveSecurity, Agent: "r1"}}}
	plan := PlanDispatch([]PullRequest{dispatchPR("new")}, Artifact{}, state, DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "acme", Agents: []AgentCapability{reviewer("r1"), reviewer("r2")}})
	if len(plan.State.Pending) != DefaultMaxParallelReviews {
		t.Fatalf("old head pending reviews were not invalidated: %+v", plan.State.Pending)
	}
	for _, p := range plan.State.Pending {
		if p.HeadSHA != "new" {
			t.Fatalf("stale pending entry survived: %+v", p)
		}
	}
}

func TestConfirmDeliveredRollsBackFailedKicks(t *testing.T) {
	planned := []DispatchKick{
		{Kind: "review", Agent: "r1", Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness},
		{Kind: "review", Agent: "r2", Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity},
		{Kind: "fix", Agent: "quality", Repo: "acme/hive", Number: 7, HeadSHA: "sha1"},
	}
	state := DispatchState{
		Pending: []PendingReview{
			{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Agent: "r1"},
			{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Agent: "r2"},
		},
		Fixes: []PendingFix{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Agent: "quality", Attempts: 1}},
	}
	got := ConfirmDelivered(state, planned, planned[:1])
	if len(got.Pending) != 1 || got.Pending[0].Perspective != PerspectiveCorrectness {
		t.Fatalf("undelivered review was not rolled back: %+v", got.Pending)
	}
	if len(got.Fixes) != 0 {
		t.Fatalf("undelivered fix was not rolled back: %+v", got.Fixes)
	}
}

func TestBuildFixPromptAndCapExhaustion(t *testing.T) {
	finding := PerspectiveFinding{Perspective: PerspectiveCorrectness, Finding: outputschema.Finding{Title: "nil panic", Severity: outputschema.SeverityMedium, Summary: "guard nil input"}}
	agg := Aggregate{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Verdict: VerdictChangesRequested, Findings: []PerspectiveFinding{finding}}
	pr := dispatchPR("sha1")
	pr.Repo = "acme/hive"
	prompt := BuildFixPrompt(pr, agg, 1, escalation.MaxReEngagements)
	if !strings.Contains(prompt, "nil panic") || !strings.Contains(prompt, "attempt 1") || !strings.Contains(prompt, "acme/hive#7") {
		t.Fatalf("fix prompt missing expected context:\n%s", prompt)
	}
	state := DispatchState{}
	for i := 1; i <= escalation.MaxReEngagements; i++ {
		state.Fixes = append(state.Fixes, PendingFix{Repo: "acme/hive", Number: 7, HeadSHA: "old", Attempts: i})
	}
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, state, DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "acme", Agents: []AgentCapability{reviewer("r1")}})
	if len(plan.FixKicks) != 0 || len(plan.State.Human) != 1 || !strings.Contains(plan.State.Human[0].Reason, "cap reached") {
		t.Fatalf("cap exhaustion did not require human: kicks=%d human=%+v", len(plan.FixKicks), plan.State.Human)
	}
}

func TestFixDispatchFallsBackToAvailableScanner(t *testing.T) {
	agg := Aggregate{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Verdict: VerdictChangesRequested}
	pr := dispatchPR("sha1")
	pr.Repo = "acme/hive"
	pr.Lane = "quality"
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, DispatchOptions{
		RequireApproval: true,
		FanOut:          true,
		ProjectOrg:      "acme",
		Agents: []AgentCapability{
			{Name: "quality", Enabled: false, UsesKick: true},
			{Name: DefaultFixerAgent, Enabled: true, UsesKick: true},
			reviewer("r1"),
		},
	})
	if len(plan.FixKicks) != 1 || plan.FixKicks[0].Agent != DefaultFixerAgent {
		t.Fatalf("fixer fallback = %+v, want scanner", plan.FixKicks)
	}
}

func TestConfigReviewDefaults(t *testing.T) {
	var cfg config.ReviewConfig
	if cfg.FanOut || cfg.RequireApproval {
		t.Fatalf("review fan-out/approval must default off: %+v", cfg)
	}
	if got := cfg.EffectiveMaxParallelReviews(); got != config.DefaultMaxParallelReviews {
		t.Fatalf("default max parallel = %d, want %d", got, config.DefaultMaxParallelReviews)
	}
	cfg.MaxParallelReviews = 2
	if got := cfg.EffectiveMaxParallelReviews(); got != 2 {
		t.Fatalf("configured max parallel = %d, want 2", got)
	}
}
