package main

// Tests for the cmd/hive review-swarm wiring layer: planReviewDispatch,
// refreshReviewVerdicts, and persistReviewDispatchState (main.go). These
// functions translate hive config + the actionable PR snapshot into
// pkg/review dispatch plans and persist the resulting state. They were at
// 0-10% coverage; the pkg/review engine itself is already well covered, so
// these tests focus on the glue: config gating, PR/agent capability mapping,
// and artifact/state file round-trips.
//
// All review state paths are redirected into t.TempDir() so the tests are
// hermetic on hosts with a live /var/run/hive-metrics.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

const reviewTestSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// redirectReviewPaths points every package-level review/outputschema path at
// a fresh temp dir and restores the originals on cleanup.
func redirectReviewPaths(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldState := review.ReviewDispatchStatePath
	oldVerdicts := review.ReviewVerdictsPath
	oldReports := outputschema.AgentReportDir
	review.ReviewDispatchStatePath = filepath.Join(dir, review.ReviewDispatchStateFile)
	review.ReviewVerdictsPath = filepath.Join(dir, review.ReviewVerdictsFile)
	outputschema.AgentReportDir = dir
	t.Cleanup(func() {
		review.ReviewDispatchStatePath = oldState
		review.ReviewVerdictsPath = oldVerdicts
		outputschema.AgentReportDir = oldReports
	})
	return dir
}

func reviewSwarmConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Org: "kubestellar", AIAuthor: "hive-bot"},
		Review: config.ReviewConfig{
			RequireApproval: true,
			FanOut:          true,
			ReviewerAgents:  []string{"reviewer"},
			FixerAgent:      "scanner",
		},
		Agents: map[string]config.AgentConfig{
			"reviewer": {Enabled: true, Role: "review specialist"},
			"scanner":  {Enabled: true, Role: "scanner"},
			"disabled": {Enabled: false, Role: "review"},
		},
	}
}

func actionableWithPR(author string) *github.ActionableResult {
	return &github.ActionableResult{
		PRs: github.PRResult{Items: []github.PullRequest{{
			Repo:    "hivecommons/hive",
			Number:  4321,
			Title:   "test: add coverage",
			Author:  author,
			HeadSHA: reviewTestSHA,
			URL:     "https://github.com/hivecommons/hive/pull/4321",
		}}},
	}
}

// Config/nil gating of planReviewDispatch is covered by
// review_dispatch_gates_test.go (TestPlanReviewDispatchRequiresBothToggles,
// TestPlanReviewDispatchNilInputs); these tests cover the dispatch paths past
// the gate.

func TestPlanReviewDispatch_KicksReviewerForAgentAuthoredPR(t *testing.T) {
	redirectReviewPaths(t)
	plan := planReviewDispatch(reviewSwarmConfig(), actionableWithPR("hive-bot"), nil, restoreTestLogger())

	// A single reviewer agent is throttled to one perspective per cycle.
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("expected 1 review kick for a single reviewer, got %d", len(plan.ReviewKicks))
	}
	kick := plan.ReviewKicks[0]
	if kick.Agent != "reviewer" {
		t.Errorf("kick agent = %q, want %q", kick.Agent, "reviewer")
	}
	if kick.Repo != "hivecommons/hive" || kick.Number != 4321 || kick.HeadSHA != reviewTestSHA {
		t.Errorf("kick PR identity = %s#%d@%s, want hivecommons/hive#4321@%s",
			kick.Repo, kick.Number, kick.HeadSHA, reviewTestSHA)
	}
	if kick.Kind != "review" {
		t.Errorf("kick kind = %q, want %q", kick.Kind, "review")
	}
	if len(plan.State.Pending) != 1 {
		t.Fatalf("expected 1 pending review recorded in state, got %d", len(plan.State.Pending))
	}
	if plan.State.Pending[0].Agent != "reviewer" || plan.State.Pending[0].Number != 4321 {
		t.Errorf("pending review = %+v, want agent reviewer on PR 4321", plan.State.Pending[0])
	}
}

func TestPlanReviewDispatch_SkipsHumanAuthoredPR(t *testing.T) {
	redirectReviewPaths(t)
	plan := planReviewDispatch(reviewSwarmConfig(), actionableWithPR("some-human"), nil, restoreTestLogger())
	if len(plan.ReviewKicks) != 0 || len(plan.FixKicks) != 0 {
		t.Fatalf("human-authored PR must not be dispatched, got %d review kicks and %d fix kicks",
			len(plan.ReviewKicks), len(plan.FixKicks))
	}
}

func TestPlanReviewDispatch_PausedAgentConfigExcludesReviewer(t *testing.T) {
	redirectReviewPaths(t)
	cfg := reviewSwarmConfig()
	reviewer := cfg.Agents["reviewer"]
	reviewer.Paused = true
	cfg.Agents["reviewer"] = reviewer

	plan := planReviewDispatch(cfg, actionableWithPR("hive-bot"), nil, restoreTestLogger())
	if len(plan.ReviewKicks) != 0 {
		t.Fatalf("paused reviewer must not receive kicks, got %d", len(plan.ReviewKicks))
	}
}

func TestPlanReviewDispatch_ChangesRequestedVerdictDispatchesFixer(t *testing.T) {
	redirectReviewPaths(t)
	artifact := review.Artifact{
		GeneratedAt: time.Now().UTC(),
		Items: []review.Aggregate{{
			Repo:    "hivecommons/hive",
			Number:  4321,
			HeadSHA: reviewTestSHA,
			Verdict: review.VerdictChangesRequested,
		}},
	}
	if err := review.WriteArtifact("", artifact); err != nil {
		t.Fatalf("seed verdict artifact: %v", err)
	}

	plan := planReviewDispatch(reviewSwarmConfig(), actionableWithPR("hive-bot"), nil, restoreTestLogger())
	if len(plan.ReviewKicks) != 0 {
		t.Errorf("PR with an aggregate verdict must not be re-reviewed, got %d review kicks", len(plan.ReviewKicks))
	}
	if len(plan.FixKicks) != 1 {
		t.Fatalf("expected 1 fix kick, got %d", len(plan.FixKicks))
	}
	if plan.FixKicks[0].Agent != "scanner" {
		t.Errorf("fix kick agent = %q, want configured fixer %q", plan.FixKicks[0].Agent, "scanner")
	}
	if len(plan.State.Fixes) != 1 || plan.State.Fixes[0].Attempts != 1 {
		t.Errorf("state fixes = %+v, want one pending fix with 1 attempt", plan.State.Fixes)
	}
}

func TestRefreshReviewVerdicts_NilOrDisabledConfigIsNoOp(t *testing.T) {
	dir := redirectReviewPaths(t)
	refreshReviewVerdicts(nil, restoreTestLogger())
	cfg := &config.Config{}
	refreshReviewVerdicts(cfg, restoreTestLogger())
	if _, err := os.Stat(filepath.Join(dir, review.ReviewVerdictsFile)); !os.IsNotExist(err) {
		t.Fatalf("verdict artifact must not be written when review approval is off (stat err=%v)", err)
	}
}

func TestRefreshReviewVerdicts_MissingReportDirIsQuiet(t *testing.T) {
	dir := redirectReviewPaths(t)
	outputschema.AgentReportDir = filepath.Join(dir, "does-not-exist")
	cfg := &config.Config{Review: config.ReviewConfig{RequireApproval: true}}
	refreshReviewVerdicts(cfg, restoreTestLogger()) // must not panic or write
	if _, err := os.Stat(review.ReviewVerdictsPath); !os.IsNotExist(err) {
		t.Fatalf("no artifact expected when report dir is missing (stat err=%v)", err)
	}
}

func TestRefreshReviewVerdicts_CollectsReportsIntoArtifact(t *testing.T) {
	dir := redirectReviewPaths(t)
	report := review.PerspectiveReport{
		AgentReport: outputschema.AgentReport{
			Lane:       "review-swarm",
			Kind:       outputschema.KindReview,
			Findings:   []outputschema.Finding{},
			PRsOpened:  []outputschema.PROpened{},
			BeadsFiled: []outputschema.BeadFiled{},
			Summary:    "security review summary",
		},
		Perspective: review.PerspectiveSecurity,
		Verdict:     review.VerdictApprove,
		Repo:        "hivecommons/hive",
		Number:      4321,
		HeadSHA:     reviewTestSHA,
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	reportPath := filepath.Join(dir, review.ReviewReportFilePrefix+"security"+review.ReviewReportFileSuffix)
	if err := os.WriteFile(reportPath, raw, 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}

	cfg := &config.Config{Review: config.ReviewConfig{RequireApproval: true}}
	refreshReviewVerdicts(cfg, restoreTestLogger())

	artifact, err := review.LoadArtifact("")
	if err != nil {
		t.Fatalf("load refreshed artifact: %v", err)
	}
	if len(artifact.Items) != 1 {
		t.Fatalf("artifact aggregates = %d, want 1", len(artifact.Items))
	}
	agg := artifact.Items[0]
	if agg.Repo != "hivecommons/hive" || agg.Number != 4321 || agg.HeadSHA != reviewTestSHA {
		t.Errorf("aggregate identity = %s#%d@%s, want hivecommons/hive#4321@%s",
			agg.Repo, agg.Number, agg.HeadSHA, reviewTestSHA)
	}
	if agg.Perspectives[review.PerspectiveSecurity] != review.VerdictApprove {
		t.Errorf("security perspective verdict = %q, want approve", agg.Perspectives[review.PerspectiveSecurity])
	}
}

func TestPersistReviewDispatchState_EmptyPlanWritesNothing(t *testing.T) {
	redirectReviewPaths(t)
	persistReviewDispatchState(review.DispatchPlan{}, nil, restoreTestLogger())
	if _, err := os.Stat(review.ReviewDispatchStatePath); !os.IsNotExist(err) {
		t.Fatalf("empty plan must not persist state (stat err=%v)", err)
	}
}

func TestPersistReviewDispatchState_KeepsOnlyDeliveredKicks(t *testing.T) {
	redirectReviewPaths(t)
	now := time.Now().UTC()
	delivered := review.DispatchKick{
		Kind: "review", Agent: "reviewer", Repo: "hivecommons/hive",
		Number: 4321, HeadSHA: reviewTestSHA, Perspective: review.PerspectiveSecurity,
	}
	dropped := review.DispatchKick{
		Kind: "review", Agent: "reviewer", Repo: "hivecommons/hive",
		Number: 4321, HeadSHA: reviewTestSHA, Perspective: review.PerspectiveStyle,
	}
	plan := review.DispatchPlan{
		ReviewKicks: []review.DispatchKick{delivered, dropped},
		State: review.DispatchState{
			GeneratedAt: now,
			Pending: []review.PendingReview{
				{Repo: delivered.Repo, Number: delivered.Number, HeadSHA: delivered.HeadSHA, Perspective: delivered.Perspective, Agent: delivered.Agent, Dispatched: now},
				{Repo: dropped.Repo, Number: dropped.Number, HeadSHA: dropped.HeadSHA, Perspective: dropped.Perspective, Agent: dropped.Agent, Dispatched: now},
			},
		},
	}

	persistReviewDispatchState(plan, []review.DispatchKick{delivered}, restoreTestLogger())

	state, err := review.LoadDispatchState("")
	if err != nil {
		t.Fatalf("load persisted state: %v", err)
	}
	if len(state.Pending) != 1 {
		t.Fatalf("persisted pending = %d, want only the delivered kick", len(state.Pending))
	}
	if state.Pending[0].Perspective != review.PerspectiveSecurity {
		t.Errorf("persisted perspective = %q, want %q", state.Pending[0].Perspective, review.PerspectiveSecurity)
	}
}

func TestPersistReviewDispatchState_WriteFailureIsNonFatal(t *testing.T) {
	dir := redirectReviewPaths(t)
	// Point the state file inside a plain file so MkdirAll/rename must fail.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	review.ReviewDispatchStatePath = filepath.Join(blocker, "state.json")

	plan := review.DispatchPlan{State: review.DispatchState{GeneratedAt: time.Now().UTC()}}
	persistReviewDispatchState(plan, nil, restoreTestLogger()) // must not panic
}
