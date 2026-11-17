package workflowmodel

import "fmt"

// Post-merge range attribution (hivecommons/hive#18356, #18644).
//
// A release, deploy or default-branch CI run of revision R for one
// pipeline/environment is attributed to every merged PR whose merge commit
// lies in prev..R, where prev is the revision of that pipeline/environment's
// previous successful run. With no previous success the lower bound is the
// revision at which the workflow model was enabled, so PRs merged before
// then are never credited. The first successful run whose range contains a
// PR's merge commit sets the PR's column to success; a failed run whose
// range contains it sets it to failed until a later success contains it.
// Nothing is guessed: a run whose revision or range cannot be determined
// leaves the column unknown and records why.
//
// The function is pure and agnostic of GitHub Actions and Tekton: observers
// resolve revisions and conclusions and hand them over in run order.

// RunConclusion is the outcome of a finished post-merge run.
type RunConclusion string

// Run conclusions. Any other value (queued, running, cancelled, skipped)
// neither credits nor fails a PR and does not advance prev.
const (
	RunSuccess RunConclusion = "success"
	RunFailure RunConclusion = "failure"
)

// PostMergeRun is one observed run of a pipeline/environment.
type PostMergeRun struct {
	// ID identifies the run for the record (run id, PipelineRun name).
	ID string
	// Revision is the commit the run built or deployed; empty when the
	// observer could not determine it.
	Revision   string
	Conclusion RunConclusion
}

// MergedPR is a merged pull request and its merge commit on the default
// branch.
type MergedPR struct {
	Number      int
	MergeCommit string
}

// AttributionStatus is a PR's status in one post-merge column.
type AttributionStatus string

// Attribution statuses.
const (
	AttributionSuccess AttributionStatus = "success"
	AttributionFailed  AttributionStatus = "failed"
	AttributionUnknown AttributionStatus = "unknown"
)

// Attribution is a PR's status in one column, the run that set it, and,
// when unknown, why.
type Attribution struct {
	Status AttributionStatus
	RunID  string
	Reason string
}

// AttributionInput is everything AttributeRuns needs.
type AttributionInput struct {
	// History is the default branch's first-parent commits, oldest first.
	// It orders commits so prev..R is the slice after prev up to and
	// including R.
	History []string
	// EnabledAt is the default-branch revision at which the workflow model
	// was enabled. It is the lower bound for runs with no previous success;
	// when empty or not in History such ranges are undeterminable.
	EnabledAt string
	// Columns maps a column (pipeline/environment) to its runs, oldest
	// first.
	Columns map[string][]PostMergeRun
	// PRs are the merged PRs to attribute.
	PRs []MergedPR
}

// AttributeRuns returns, per PR number and column, the PR's post-merge
// status under the range attribution rules.
func AttributeRuns(in AttributionInput) map[int]map[string]Attribution {
	pos := make(map[string]int, len(in.History))
	for i, sha := range in.History {
		if _, dup := pos[sha]; !dup {
			pos[sha] = i
		}
	}
	enabled, enabledKnown := pos[in.EnabledAt]
	if in.EnabledAt == "" {
		enabledKnown = false
	}

	out := make(map[int]map[string]Attribution, len(in.PRs))
	for _, pr := range in.PRs {
		out[pr.Number] = make(map[string]Attribution, len(in.Columns))
	}

	for column, runs := range in.Columns {
		// at[i] is the History index of in.PRs[i]'s merge commit, or -1
		// when the PR is not attributable in this column at all.
		at := make([]int, len(in.PRs))
		for i, pr := range in.PRs {
			a := Attribution{Status: AttributionUnknown, Reason: "no finished run's range contains the merge commit"}
			idx, ok := pos[pr.MergeCommit]
			switch {
			case pr.MergeCommit == "" || !ok:
				a.Reason = "merge commit is not on the observed default-branch history"
				idx = -1
			case enabledKnown && idx <= enabled:
				a.Reason = "merged before the workflow model was enabled"
				idx = -1
			}
			at[i] = idx
			out[pr.Number][column] = a
		}

		prev, havePrev := -1, false
		for _, run := range runs {
			if run.Conclusion != RunSuccess && run.Conclusion != RunFailure {
				continue
			}
			lower, lowerKnown := prev, havePrev
			if !havePrev {
				lower, lowerKnown = enabled, enabledKnown
			}
			rev, revKnown := pos[run.Revision]
			if run.Revision == "" {
				revKnown = false
			}

			var why string
			switch {
			case !revKnown && run.Revision == "":
				why = fmt.Sprintf("run %s: revision could not be determined", run.ID)
			case !revKnown:
				why = fmt.Sprintf("run %s: revision %s is not on the observed default-branch history", run.ID, run.Revision)
			case !lowerKnown:
				why = fmt.Sprintf("run %s: no previous successful run and the model's enablement revision is unknown", run.ID)
			}
			if why != "" {
				// The run may cover any PR not yet credited after the
				// last known lower bound; record why without guessing.
				for i, pr := range in.PRs {
					a := out[pr.Number][column]
					if at[i] < 0 || a.Status != AttributionUnknown || (lowerKnown && at[i] <= lower) {
						continue
					}
					a.Reason = why
					out[pr.Number][column] = a
				}
				if run.Conclusion == RunSuccess && revKnown {
					prev, havePrev = rev, true
				}
				continue
			}

			for i, pr := range in.PRs {
				if at[i] <= lower || at[i] > rev {
					continue
				}
				a := out[pr.Number][column]
				if a.Status == AttributionSuccess {
					continue
				}
				switch run.Conclusion {
				case RunSuccess:
					a = Attribution{Status: AttributionSuccess, RunID: run.ID}
				case RunFailure:
					a = Attribution{Status: AttributionFailed, RunID: run.ID}
				}
				out[pr.Number][column] = a
			}
			if run.Conclusion == RunSuccess {
				prev, havePrev = rev, true
			}
		}
	}
	return out
}
