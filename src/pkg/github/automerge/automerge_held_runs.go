package automerge

// Held and never-started required checks on trusted-lane PRs.
//
// A required context can be missing from a PR head for two reasons that
// polling will never cure (hivecommons/hive#11552, #11553, #11877):
//
//  1. GitHub created the `pull_request` workflow runs but holds them for
//     approval (`conclusion=action_required`). This happens when the head was
//     pushed by `github-actions[bot]` — e.g. a GITHUB_TOKEN autofix commit onto
//     a Dependabot branch — under the repository's "require approval for
//     first-time contributors" policy. The run exists, the required check does
//     not, and nothing will start it until a person clicks "Approve and run".
//  2. GitHub created no `pull_request` runs for the head at all, so the
//     required check can never appear.
//
// The App's own PRs and configured trusted bots are the only lanes this file
// acts for (trySweepSelfAuthoredPR); human-authored lanes are untouched so a
// first-time contributor's held run is never released by automation. Only
// runs whose head repository is the base repository are approved — a fork head
// is never released here even for a trusted login.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const (
	// requiredCheckMissingPrefix is the commit-CI reason this file refines.
	requiredCheckMissingPrefix = "required-check-missing:"
	// requiredCheckHeldPrefix: the head's workflow runs exist but are held for
	// approval and the sweep could not (or already did) release them.
	requiredCheckHeldPrefix = "required-check-held:"
	// requiredCheckNeverStartedPrefix: no `pull_request` run exists for a head
	// older than heldRunNeverStartedGrace; polling cannot make one appear.
	requiredCheckNeverStartedPrefix = "required-check-never-started:"
	// approvedHeldWorkflowRunsReason is returned on the tick that released
	// held runs; the next tick re-evaluates the (now running) checks.
	approvedHeldWorkflowRunsReason = "approved-held-workflow-runs"
	// workflowRunConclusionActionRequired is GitHub's conclusion for a run
	// waiting on a person's approval.
	workflowRunConclusionActionRequired = "action_required"
	// heldRunNeverStartedGrace bounds how long a head with zero workflow runs
	// is still treated as "GitHub has not scheduled them yet".
	heldRunNeverStartedGrace = 30 * time.Minute
	workflowRunsPageSize     = 100
)

// resolveMissingRequiredCheck refines a `required-check-missing:<ctx>` skip
// for a trusted-lane PR. It returns the reason the caller should log: the
// original reason when nothing is known, a held/never-started reason when
// polling cannot progress, or approvedHeldWorkflowRunsReason after releasing
// held runs. It never returns an error: inability to inspect or approve runs
// degrades to the original reason so the sweep keeps its existing behavior.
func (c *Engine) resolveMissingRequiredCheck(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, number int, headSHA, reason string, headPushedAt time.Time) string {
	if c == nil || c.gh == nil || !strings.HasPrefix(reason, requiredCheckMissingPrefix) {
		return reason
	}
	missing := strings.TrimPrefix(reason, requiredCheckMissingPrefix)
	if c.isForkPR(pr) {
		return reason
	}
	runs, _, err := c.gh.Actions.ListRepositoryWorkflowRuns(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, &gh.ListWorkflowRunsOptions{
		HeadSHA:     headSHA,
		Event:       "pull_request",
		ListOptions: gh.ListOptions{PerPage: workflowRunsPageSize},
	})
	if err != nil || runs == nil {
		c.warn("automerge could not list workflow runs for head", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
		return reason
	}
	held := heldSameRepoWorkflowRuns(runs.WorkflowRuns, owner+"/"+repo)
	if len(held) == 0 {
		if len(runs.WorkflowRuns) == 0 && headOlderThan(c.now, headPushedAt, heldRunNeverStartedGrace) {
			return requiredCheckNeverStartedPrefix + missing
		}
		return reason
	}
	if !c.markHeldRunApprovalAttempted(owner, repo, number, headSHA) {
		// Already released (or refused) for this head; the runs are either
		// starting or need a person. Do not spend another approve call.
		return requiredCheckHeldPrefix + missing
	}
	approved := 0
	for _, run := range held {
		if err := c.approveWorkflowRunEffect(ctx, owner, repo, number, headSHA, run); err != nil {
			if isGitHubStatus(err, http.StatusForbidden) {
				c.warn("automerge cannot approve held workflow runs; the App needs Actions: write — approve them in the Actions tab or `gh run approve`",
					"repo", displayRepo, "pr", number, "sha", headSHA, "run", run.GetID(), "workflow", run.GetName(), "held", len(held))
				return requiredCheckHeldPrefix + missing
			}
			c.warn("automerge failed to approve held workflow run", "repo", displayRepo, "pr", number, "run", run.GetID(), "workflow", run.GetName(), "error", err)
			continue
		}
		approved++
	}
	if approved == 0 {
		return requiredCheckHeldPrefix + missing
	}
	c.info("automerge approved held workflow runs", "repo", displayRepo, "pr", number, "sha", headSHA, "approved", approved, "held", len(held), "missing", missing)
	return approvedHeldWorkflowRunsReason
}

// heldSameRepoWorkflowRuns returns the runs waiting on approval whose head
// repository is fullName. A run without a head repository is never released.
func heldSameRepoWorkflowRuns(runs []*gh.WorkflowRun, fullName string) []*gh.WorkflowRun {
	var held []*gh.WorkflowRun
	for _, run := range runs {
		if run == nil || !strings.EqualFold(run.GetConclusion(), workflowRunConclusionActionRequired) {
			continue
		}
		if run.GetHeadRepository() == nil || !strings.EqualFold(run.GetHeadRepository().GetFullName(), fullName) {
			continue
		}
		held = append(held, run)
	}
	return held
}

func headOlderThan(now func() time.Time, headPushedAt time.Time, grace time.Duration) bool {
	if headPushedAt.IsZero() {
		return false
	}
	if now == nil {
		now = time.Now
	}
	return now().Sub(headPushedAt) > grace
}

// markHeldRunApprovalAttempted records that approval was attempted for this
// PR head and reports whether this is the first attempt.
func (c *Engine) markHeldRunApprovalAttempted(owner, repo string, number int, headSHA string) bool {
	key := c.evaluatedHeadKey(owner, repo, number) + "@" + strings.ToLower(headSHA)
	c.heldRunApprovalsMu.Lock()
	defer c.heldRunApprovalsMu.Unlock()
	if c.heldRunApprovals == nil {
		c.heldRunApprovals = make(map[string]bool)
	}
	if c.heldRunApprovals[key] {
		return false
	}
	c.heldRunApprovals[key] = true
	return true
}

// approveWorkflowRunEffect routes POST /actions/runs/{id}/approve through the
// mutation boundary. go-github v72 has no binding for this endpoint.
func (c *Engine) approveWorkflowRunEffect(ctx context.Context, owner, repo string, number int, headSHA string, run *gh.WorkflowRun) error {
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindWorkflowRunApprove,
		Target: strconv.FormatInt(run.GetID(), 10),
		Actor:  "automerge",
		Inputs: map[string]string{"pr": strconv.Itoa(number), "head_sha": headSHA, "workflow": run.GetName()},
	}, func(ctx context.Context) (effects.Result, error) {
		path := fmt.Sprintf("repos/%s/%s/actions/runs/%d/approve", owner, repo, run.GetID())
		req, err := c.gh.NewRequest(http.MethodPost, path, nil)
		if err != nil {
			return effects.Result{}, effects.NotApplied(err)
		}
		if _, err := c.gh.Do(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), req, nil); err != nil {
			return effects.Result{}, effects.NotApplied(err)
		}
		return effects.Result{Provenance: path}, nil
	})
	return err
}
