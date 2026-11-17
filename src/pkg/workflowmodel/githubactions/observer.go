package githubactions

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Post-merge run observer (hivecommons/hive#18645, part of #18356).
//
// The observer turns GitHub Actions workflow runs into the post-merge
// columns an ingested model declares (Main CI, Release, Deploy: <env>,
// Deploy) and feeds them through workflowmodel.AttributeRuns, so each
// merged PR gets a real pipeline.status. It is pure: the caller lists the
// runs and the default branch's first-parent history and hands them over.
// Nothing is guessed. A run the observer cannot place in a column, or
// whose revision it cannot tell, leaves the column unknown and the reason
// is recorded.

// Pipeline status values. They are the spellings of the pipeline.status
// fact.
const (
	PipelineGreen   = "green"
	PipelineRed     = "red"
	PipelineRunning = "running"
	PipelineUnknown = "unknown"
)

// Run status and conclusion spellings the Actions REST API uses.
const (
	runStatusCompleted  = "completed"
	runConclusionOK     = "success"
	runConclusionFailed = "failure"
	runConclusionTimout = "timed_out"
	runConclusionError  = "startup_failure"
)

// WorkflowRun is one GitHub Actions workflow run as the REST API reports
// it (GET /repos/{owner}/{repo}/actions/runs).
type WorkflowRun struct {
	ID int64
	// Path is the workflow file, `.github/workflows/x.yml`. An `@ref`
	// suffix the API sometimes adds is ignored.
	Path       string
	Event      string
	HeadBranch string
	HeadSHA    string
	// Status is queued, in_progress, waiting, requested, pending or
	// completed.
	Status string
	// Conclusion is set once Status is completed: success, failure,
	// timed_out, startup_failure, cancelled, skipped, neutral, ...
	Conclusion string
	CreatedAt  time.Time
	// Environments are the deployment environments the run targeted, nil
	// when the caller did not observe them. They only matter for a
	// workflow bound to more than one deploy environment.
	Environments []string
}

// PostMergeInput is everything ObservePostMerge needs.
type PostMergeInput struct {
	// Model is the repository's workflow model; its ingested post-merge
	// states and the GitHub Actions pipelines bound into them define the
	// columns.
	Model *workflowmodel.Model
	// DefaultBranch is the repository's default branch.
	DefaultBranch string
	// History is the default branch's first-parent commits, oldest first.
	History []string
	// EnabledAt is the default-branch revision at which the model was
	// enabled (see workflowmodel.AttributionInput).
	EnabledAt string
	// Runs are the observed workflow runs, in any order.
	Runs []WorkflowRun
	// PRs are the merged PRs to report on.
	PRs []workflowmodel.MergedPR
}

// ColumnStatus is one PR's status in one post-merge column.
type ColumnStatus struct {
	Column string
	workflowmodel.Attribution
	// Running is set when the column is not settled and a run that covers
	// the merge commit is still queued or in progress.
	Running bool
}

// PostMergeStatus is one PR's pipeline.status: the status of the first
// post-merge column, in chain order, that has not succeeded, or green when
// every column has. Reason says why it is not green.
type PostMergeStatus struct {
	Status  string
	Column  string
	Reason  string
	Columns []ColumnStatus
}

// PostMergeColumn is one post-merge column of a model and the GitHub
// Actions pipelines that report into it.
type PostMergeColumn struct {
	// ID is the state id (main-ci, release, deploy-<env>, deploy).
	ID string
	// PushVerify is set for Main CI: a push-verify pipeline reports into
	// the column, so only default-branch push runs count.
	PushVerify bool
	// Environment is the deployment environment the column's guard names,
	// "" when the column is not environment-specific.
	Environment string
	// Workflows are the bound workflow file paths.
	Workflows []string
}

// PostMergeColumns returns the model's post-merge columns that GitHub
// Actions pipelines report into, in chain order. A state counts when it
// is ingested, follows the merge (it is not the pre-merge CI state) and
// binds at least one GitHub Actions pipeline.
func PostMergeColumns(m *workflowmodel.Model) []PostMergeColumn {
	if m == nil {
		return nil
	}
	envOf := map[string]string{}
	for _, g := range m.Spec.Guards {
		if g.Kind == workflowmodel.GuardEnvironmentApproval {
			envOf[g.ID] = g.Params["environment"]
		}
	}
	var out []PostMergeColumn
	for _, s := range m.Spec.States {
		if s.Origin != workflowmodel.OriginIngested || s.ID == workflowmodel.StateCI || len(s.Pipelines) == 0 {
			continue
		}
		col := PostMergeColumn{ID: s.ID, Environment: envOf[s.GuardRef], PushVerify: s.ID == workflowmodel.StateMainCI}
		for _, id := range s.Pipelines {
			p := m.Pipeline(id)
			if p == nil || p.System != workflowmodel.SystemGitHubActions {
				continue
			}
			col.Workflows = append(col.Workflows, workflowPath(p.Path))
		}
		if len(col.Workflows) > 0 {
			out = append(out, col)
		}
	}
	return out
}

// ObservePostMerge returns every PR's pipeline.status under the range
// attribution rules, keyed by PR number.
func ObservePostMerge(in PostMergeInput) map[int]PostMergeStatus {
	cols := PostMergeColumns(in.Model)
	out := make(map[int]PostMergeStatus, len(in.PRs))
	if len(cols) == 0 {
		for _, pr := range in.PRs {
			out[pr.Number] = PostMergeStatus{Status: PipelineUnknown, Reason: "the workflow model binds no GitHub Actions post-merge pipelines"}
		}
		return out
	}

	// A workflow bound to several environment columns is told apart only
	// by the environments its runs report.
	envCols := map[string]int{}
	for _, c := range cols {
		if c.Environment == "" {
			continue
		}
		for _, w := range c.Workflows {
			envCols[w]++
		}
	}

	runs := append([]WorkflowRun(nil), in.Runs...)
	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.Before(runs[j].CreatedAt)
		}
		return runs[i].ID < runs[j].ID
	})

	columns := make(map[string][]workflowmodel.PostMergeRun, len(cols))
	pending := make(map[string][]WorkflowRun, len(cols))
	exact := make(map[string]map[string][]WorkflowRun, len(cols))
	for _, c := range cols {
		bound := map[string]bool{}
		for _, w := range c.Workflows {
			bound[w] = true
		}
		for _, r := range runs {
			p := workflowPath(r.Path)
			if !bound[p] || !postMergeEvent(c, r, in.DefaultBranch) {
				continue
			}
			pmr := workflowmodel.PostMergeRun{ID: runID(r), Revision: strings.TrimSpace(r.HeadSHA), Conclusion: conclusion(r)}
			if c.Environment != "" && envCols[p] > 1 {
				switch {
				case r.Environments == nil:
					// Which environment this run deployed is not
					// observed: never credit it to one of them.
					if r.Status != runStatusCompleted {
						continue
					}
					pmr.ID += " (deployment environment not reported)"
					pmr.Revision = ""
				case !containsFold(r.Environments, c.Environment):
					continue
				}
			}
			if r.Status != runStatusCompleted {
				pending[c.ID] = append(pending[c.ID], r)
				continue
			}
			columns[c.ID] = append(columns[c.ID], pmr)
			if c.PushVerify && pmr.Revision != "" && (pmr.Conclusion == workflowmodel.RunSuccess || pmr.Conclusion == workflowmodel.RunFailure) {
				if exact[c.ID] == nil {
					exact[c.ID] = map[string][]WorkflowRun{}
				}
				exact[c.ID][pmr.Revision] = append(exact[c.ID][pmr.Revision], r)
			}
		}
		if _, ok := columns[c.ID]; !ok {
			columns[c.ID] = nil
		}
	}

	attr := workflowmodel.AttributeRuns(workflowmodel.AttributionInput{
		History:   in.History,
		EnabledAt: in.EnabledAt,
		Columns:   columns,
		PRs:       in.PRs,
	})

	pos := make(map[string]int, len(in.History))
	for i, sha := range in.History {
		if _, dup := pos[sha]; !dup {
			pos[sha] = i
		}
	}

	for _, pr := range in.PRs {
		st := PostMergeStatus{Status: PipelineGreen}
		settled := false
		for _, c := range cols {
			a := attr[pr.Number][c.ID]
			if a.Status == "" {
				a = workflowmodel.Attribution{Status: workflowmodel.AttributionUnknown, Reason: "no run observed"}
			}
			cs := ColumnStatus{Column: c.ID, Attribution: a}
			if a.Status == workflowmodel.AttributionUnknown && c.PushVerify {
				// Main CI: the push run of the exact merge commit decides
				// when the range cannot.
				if rs := exact[c.ID][pr.MergeCommit]; len(rs) > 0 {
					cs.Attribution = exactAttribution(rs)
				}
			}
			if cs.Status == workflowmodel.AttributionUnknown {
				if r, ok := coveringPending(pending[c.ID], pr.MergeCommit, pos); ok {
					cs.Running = true
					cs.Reason = fmt.Sprintf("run %s is %s", runID(r), r.Status)
				}
			}
			st.Columns = append(st.Columns, cs)
			if settled || cs.Status == workflowmodel.AttributionSuccess {
				continue
			}
			settled = true
			st.Column = c.ID
			switch {
			case cs.Status == workflowmodel.AttributionFailed:
				st.Status = PipelineRed
				st.Reason = fmt.Sprintf("%s: run %s failed", c.ID, cs.RunID)
			case cs.Running:
				st.Status = PipelineRunning
				st.Reason = fmt.Sprintf("%s: %s", c.ID, cs.Reason)
			default:
				st.Status = PipelineUnknown
				st.Reason = fmt.Sprintf("%s: %s", c.ID, cs.Reason)
			}
		}
		out[pr.Number] = st
	}
	return out
}

// postMergeEvent reports whether run r is a post-merge run for column c.
// Pull-request and merge-queue runs never are; Main CI only counts pushes
// to the default branch.
func postMergeEvent(c PostMergeColumn, r WorkflowRun, defaultBranch string) bool {
	switch r.Event {
	case EventPullRequest, EventPullRequestTarget, EventMergeGroup:
		return false
	}
	if c.PushVerify {
		return r.Event == EventPush && defaultBranch != "" && r.HeadBranch == defaultBranch
	}
	// Release and deploy runs (release, tag push, deployment, dispatch,
	// workflow_run) are kept whatever ref they ran on: AttributeRuns
	// checks the revision against the default-branch history and records
	// why when it is not there.
	return true
}

func conclusion(r WorkflowRun) workflowmodel.RunConclusion {
	if r.Status != runStatusCompleted {
		return workflowmodel.RunConclusion(r.Status)
	}
	switch r.Conclusion {
	case runConclusionOK:
		return workflowmodel.RunSuccess
	case runConclusionFailed, runConclusionTimout, runConclusionError:
		return workflowmodel.RunFailure
	}
	// cancelled, skipped, neutral, action_required, stale: neither credits
	// nor fails (see workflowmodel.RunConclusion).
	return workflowmodel.RunConclusion(r.Conclusion)
}

// exactAttribution decides Main CI from the finished push runs of the
// exact merge commit, oldest first: any success is success, otherwise the
// latest failure.
func exactAttribution(rs []WorkflowRun) workflowmodel.Attribution {
	var a workflowmodel.Attribution
	for _, r := range rs {
		switch conclusion(r) {
		case workflowmodel.RunSuccess:
			return workflowmodel.Attribution{Status: workflowmodel.AttributionSuccess, RunID: runID(r)}
		case workflowmodel.RunFailure:
			a = workflowmodel.Attribution{Status: workflowmodel.AttributionFailed, RunID: runID(r)}
		}
	}
	return a
}

// coveringPending returns the oldest unfinished run whose revision is the
// merge commit or a later default-branch commit.
func coveringPending(rs []WorkflowRun, merge string, pos map[string]int) (WorkflowRun, bool) {
	at, ok := pos[merge]
	if merge == "" || !ok {
		return WorkflowRun{}, false
	}
	for _, r := range rs {
		if p, ok := pos[strings.TrimSpace(r.HeadSHA)]; ok && p >= at {
			return r, true
		}
	}
	return WorkflowRun{}, false
}

func runID(r WorkflowRun) string { return strconv.FormatInt(r.ID, 10) }

// workflowPath strips the `@ref` suffix the API adds to reusable or
// dynamic workflow paths.
func workflowPath(p string) string {
	if i := strings.Index(p, "@"); i >= 0 {
		p = p[:i]
	}
	return path.Clean(strings.TrimSpace(p))
}

func containsFold(xs []string, x string) bool {
	for _, y := range xs {
		if strings.EqualFold(strings.TrimSpace(y), x) {
			return true
		}
	}
	return false
}
