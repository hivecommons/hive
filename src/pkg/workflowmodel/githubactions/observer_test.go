package githubactions

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

const (
	wfMain    = ".github/workflows/main.yml"
	wfRelease = ".github/workflows/release.yml"
	wfDeploy  = ".github/workflows/deploy.yml"
)

var (
	history = []string{"c0", "c1", "c2", "c3", "c4", "c5"}
	prs     = []workflowmodel.MergedPR{{Number: 1, MergeCommit: "c1"}, {Number: 2, MergeCommit: "c2"}, {Number: 3, MergeCommit: "c3"}}
	t0      = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
)

func pipeline(path string, role workflowmodel.Role, envs ...string) workflowmodel.Pipeline {
	p := workflowmodel.Pipeline{
		ID:     workflowmodel.PipelineID(workflowmodel.SystemGitHubActions, path),
		Name:   baseName(path),
		System: workflowmodel.SystemGitHubActions,
		Path:   path,
		Role:   role,
	}
	for _, e := range envs {
		p.Nodes = append(p.Nodes, workflowmodel.Node{ID: "deploy-" + e, Kind: workflowmodel.NodeJob, Environment: e})
	}
	return p
}

func model(pipelines ...workflowmodel.Pipeline) *workflowmodel.Model {
	return workflowmodel.Bind(workflowmodel.Scope{Forge: "github", Repository: "o/r"}, pipelines, nil, workflowmodel.Coverage{})
}

func mainAndRelease() *workflowmodel.Model {
	return model(pipeline(wfMain, workflowmodel.RolePushVerify), pipeline(wfRelease, workflowmodel.RoleRelease))
}

func run(id int64, path, event, branch, sha, status, conclusion string) WorkflowRun {
	return WorkflowRun{ID: id, Path: path, Event: event, HeadBranch: branch, HeadSHA: sha, Status: status, Conclusion: conclusion, CreatedAt: t0.Add(time.Duration(id) * time.Minute)}
}

func push(id int64, sha, conclusion string) WorkflowRun {
	return run(id, wfMain, EventPush, "main", sha, "completed", conclusion)
}

func release(id int64, sha, conclusion string) WorkflowRun {
	return run(id, wfRelease, EventRelease, "v1."+sha, sha, "completed", conclusion)
}

func observe(m *workflowmodel.Model, enabledAt string, runs ...WorkflowRun) map[int]PostMergeStatus {
	return ObservePostMerge(PostMergeInput{Model: m, DefaultBranch: "main", History: history, EnabledAt: enabledAt, Runs: runs, PRs: prs})
}

func column(t *testing.T, st PostMergeStatus, id string) ColumnStatus {
	t.Helper()
	for _, c := range st.Columns {
		if c.Column == id {
			return c
		}
	}
	t.Fatalf("no column %q in %+v", id, st.Columns)
	return ColumnStatus{}
}

func TestPostMergeColumnsFollowTheModelChain(t *testing.T) {
	m := model(pipeline(wfMain, workflowmodel.RolePushVerify), pipeline(wfRelease, workflowmodel.RoleRelease), pipeline(wfDeploy, workflowmodel.RoleDeploy, "prod", "staging"))
	cols := PostMergeColumns(m)
	var ids []string
	for _, c := range cols {
		ids = append(ids, c.ID)
	}
	want := []string{workflowmodel.StateMainCI, workflowmodel.StateRelease, workflowmodel.DeployStateID("prod"), workflowmodel.DeployStateID("staging")}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("columns %v, want %v", ids, want)
	}
	if !cols[0].PushVerify || cols[1].PushVerify {
		t.Fatalf("only Main CI is push-verify: %+v", cols)
	}
	if cols[2].Environment != "prod" || cols[3].Environment != "staging" {
		t.Fatalf("deploy environments %+v", cols[2:])
	}
}

func TestObservePostMergeGreenWhenEveryColumnSucceeded(t *testing.T) {
	got := observe(mainAndRelease(), "c0", push(1, "c3", "success"), release(2, "c3", "success"))
	for _, pr := range prs {
		st := got[pr.Number]
		if st.Status != PipelineGreen || st.Reason != "" {
			t.Fatalf("PR %d: %+v, want green", pr.Number, st)
		}
	}
}

func TestObservePostMergeBatchedReleaseAndFailureThenSuccess(t *testing.T) {
	m := mainAndRelease()
	ci := push(1, "c3", "success")

	got := observe(m, "c0", ci, release(2, "c2", "failure"))
	for _, n := range []int{1, 2} {
		if st := got[n]; st.Status != PipelineRed || st.Column != workflowmodel.StateRelease {
			t.Fatalf("PR %d after failed release: %+v, want red in release", n, st)
		}
	}
	if st := got[3]; st.Status != PipelineUnknown || st.Column != workflowmodel.StateRelease {
		t.Fatalf("PR 3 not yet released: %+v, want unknown", st)
	}

	got = observe(m, "c0", ci, release(2, "c2", "failure"), release(3, "c3", "success"))
	for _, pr := range prs {
		if st := got[pr.Number]; st.Status != PipelineGreen {
			t.Fatalf("PR %d after the batched release succeeded: %+v, want green", pr.Number, st)
		}
		if c := column(t, got[pr.Number], workflowmodel.StateRelease); c.RunID != "3" {
			t.Fatalf("PR %d credited to run %q, want 3", pr.Number, c.RunID)
		}
	}
}

func TestObservePostMergeFirstRunOnlyCreditsPRsSinceEnablement(t *testing.T) {
	got := observe(mainAndRelease(), "c1", push(1, "c3", "success"), release(2, "c3", "success"))
	st := got[1]
	if st.Status != PipelineUnknown || !strings.Contains(st.Reason, "before the workflow model was enabled") {
		t.Fatalf("PR 1 merged at enablement: %+v", st)
	}
	if got[2].Status != PipelineGreen || got[3].Status != PipelineGreen {
		t.Fatalf("PRs after enablement: %+v %+v", got[2], got[3])
	}
}

func TestObservePostMergeMainCIUsesExactMergeSHA(t *testing.T) {
	// No enablement revision: the range of the first run cannot be told,
	// but the push run of PR 2's exact merge commit decides Main CI.
	got := observe(mainAndRelease(), "", push(1, "c2", "success"))
	c := column(t, got[2], workflowmodel.StateMainCI)
	if c.Status != workflowmodel.AttributionSuccess || c.RunID != "1" {
		t.Fatalf("PR 2 Main CI: %+v, want success from run 1", c)
	}
	if got[2].Column != workflowmodel.StateRelease {
		t.Fatalf("PR 2 should wait on release: %+v", got[2])
	}
	c = column(t, got[1], workflowmodel.StateMainCI)
	if c.Status != workflowmodel.AttributionUnknown || !strings.Contains(c.Reason, "enablement revision is unknown") {
		t.Fatalf("PR 1 Main CI must stay unknown with a reason: %+v", c)
	}
}

func TestObservePostMergeIgnoresPullRequestAndOtherBranchRuns(t *testing.T) {
	got := observe(mainAndRelease(), "c0",
		run(1, wfMain, EventPullRequest, "feature", "c3", "completed", "success"),
		run(2, wfMain, EventPush, "feature", "c3", "completed", "success"),
		run(3, wfMain, EventMergeGroup, "gh-readonly-queue/main/pr-3", "c3", "completed", "success"),
	)
	for _, pr := range prs {
		if c := column(t, got[pr.Number], workflowmodel.StateMainCI); c.Status != workflowmodel.AttributionUnknown {
			t.Fatalf("PR %d Main CI credited by a non-post-merge run: %+v", pr.Number, c)
		}
	}
}

func TestObservePostMergeRunning(t *testing.T) {
	got := observe(mainAndRelease(), "c0", run(1, wfMain, EventPush, "main", "c3", "in_progress", ""))
	st := got[3]
	if st.Status != PipelineRunning || st.Column != workflowmodel.StateMainCI || !strings.Contains(st.Reason, "in_progress") {
		t.Fatalf("PR 3: %+v, want running in main-ci", st)
	}
}

func TestObservePostMergeUnknownRevisionRecordsReason(t *testing.T) {
	got := observe(mainAndRelease(), "c0", push(1, "c3", "success"), release(2, "", "success"))
	st := got[2]
	if st.Status != PipelineUnknown || !strings.Contains(st.Reason, "revision could not be determined") {
		t.Fatalf("PR 2: %+v", st)
	}
}

func TestObservePostMergeDeployEnvironments(t *testing.T) {
	m := model(pipeline(wfDeploy, workflowmodel.RoleDeploy, "prod", "staging"))
	prod, staging := workflowmodel.DeployStateID("prod"), workflowmodel.DeployStateID("staging")

	dep := run(1, wfDeploy, EventWorkflowDispatch, "main", "c3", "completed", "success")
	got := observe(m, "c0", dep)
	if st := got[3]; st.Status != PipelineUnknown || !strings.Contains(st.Reason, "deployment environment not reported") {
		t.Fatalf("a run without environments must not credit either column: %+v", st)
	}

	dep.Environments = []string{"staging"}
	got = observe(m, "c0", dep)
	if c := column(t, got[3], staging); c.Status != workflowmodel.AttributionSuccess {
		t.Fatalf("staging: %+v", c)
	}
	if c := column(t, got[3], prod); c.Status != workflowmodel.AttributionUnknown {
		t.Fatalf("prod must not be credited by a staging deploy: %+v", c)
	}
	if got[3].Column != prod {
		t.Fatalf("PR 3 decided by %q, want %q", got[3].Column, prod)
	}
}

func TestObservePostMergeNoColumns(t *testing.T) {
	got := observe(model(), "c0", push(1, "c3", "success"))
	if st := got[1]; st.Status != PipelineUnknown || st.Reason == "" {
		t.Fatalf("%+v", st)
	}
}
