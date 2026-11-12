package github

import (
	"context"
	"fmt"

	"github.com/hivecommons/hive/pkg/writeback"
)

// TransientCIRerunOutcome is what RerunTransientCIFailures did for one PR.
type TransientCIRerunOutcome struct {
	// Settled means the head's one transient re-run is spent: jobs were
	// re-run, the write-back gate or GitHub refused, or nothing failed is
	// left to re-run. Unsettled (with an error) means a later pass may retry.
	Settled bool
	// RunIDs are the workflow runs whose failed jobs GitHub accepted for a
	// re-run.
	RunIDs []int64
	// Skipped names why nothing was re-run ("" when RunIDs is non-empty):
	// a write-back gate reason, "rerun-forbidden" or "no-failed-runs".
	Skipped string
}

// RerunTransientCIFailures re-runs the failed jobs of every failed workflow
// run on pr's head, for a red PR an agent triaged as transient
// (PullRequest.TransientCITriaged). It is the deterministic repair the fix
// loop uses instead of kicking a fixer that can only comment, or pushing a
// "retrigger CI" commit that re-arms fences and can dismiss approvals
// (projectbluefin/common#1647). It never pushes. The re-run is an
// unsolicited write, so it consults the write-back gate (KindCIRerun) first;
// a hive-authored PR passes unless the repository is paused, off or
// issues-only, and a trusted dependency-bot PR (cmd/hive
// dependency_bot_transient_rerun.go) is not the hive's own item, so it needs
// the kind's level and opt-in label like any other. Once any re-run request reached GitHub the attempt is settled
// whatever came back, so a head gets at most one round of re-run calls.
func (c *Client) RerunTransientCIFailures(ctx context.Context, pr PullRequest) (TransientCIRerunOutcome, error) {
	if c == nil || c.client == nil {
		return TransientCIRerunOutcome{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(pr.Repo)
	repo := owner + "/" + name
	preview := fmt.Sprintf("re-run the failed jobs on %s#%d at %s (transient CI triage)", repo, pr.Number, shortSHA(pr.HeadSHA))
	if d := c.WritebackAllowed(repo, c.WritebackItemForPR(pr), writeback.KindCIRerun, preview); !d.Allowed {
		return TransientCIRerunOutcome{Settled: true, Skipped: d.Reason}, nil
	}
	if c.sharedCIRerunRefused(repo) {
		return TransientCIRerunOutcome{Settled: true, Skipped: "rerun-forbidden"}, nil
	}
	ctx = WithRESTCaller(ctx, "hive:transient_ci_rerun")
	runs, err := c.latestFailedWorkflowRuns(ctx, owner, name, pr.HeadSHA)
	if err != nil {
		return TransientCIRerunOutcome{}, err
	}
	if len(runs) == 0 {
		// Failed on a check no Actions run backs (an external CI), or it was
		// already re-run by hand: nothing the hive can re-run.
		return TransientCIRerunOutcome{Settled: true, Skipped: "no-failed-runs"}, nil
	}
	out := TransientCIRerunOutcome{Settled: true}
	for _, run := range runs {
		if _, err := c.client.Actions.RerunFailedJobsByID(ctx, owner, name, run.GetID()); err != nil { // writeback:gated — WritebackAllowed(KindCIRerun) consulted above
			if isWriteForbiddenError(err) {
				if c.markSharedCIRerunForbidden(repo) {
					c.logger.Warn("transient CI rerun forbidden: the App installation lacks Actions write permission for rerun-failed-jobs", "repo", repo, "pr", pr.Number, "run", run.GetID(), "error", err)
				}
				out.Skipped = "rerun-forbidden"
				break
			}
			c.logger.Warn("transient CI rerun failed", "repo", repo, "pr", pr.Number, "run", run.GetID(), "error", err)
			continue
		}
		out.RunIDs = append(out.RunIDs, run.GetID())
	}
	if len(out.RunIDs) > 0 {
		out.Skipped = ""
	} else if out.Skipped == "" {
		out.Skipped = "rerun-failed"
	}
	return out, nil
}
