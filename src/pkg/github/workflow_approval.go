package github

import (
	"context"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// PRWorkflowApproval records workflow runs on a PR head that GitHub holds
// behind its "approve and run workflows" gate (status action_required). Such
// runs create no check runs, so from the check-run listing alone the required
// checks simply look unreported; this is the evidence that they are in fact
// waiting on a person clicking Approve (hivecommons/hive#16599).
type PRWorkflowApproval struct {
	Pending    int        `json:"pending"`
	RunIDs     []int64    `json:"run_ids,omitempty"`
	Actor      string     `json:"actor,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// WorkflowApprovalStatus is the workflow-run status GitHub reports while a
// run waits for a maintainer to approve it.
const WorkflowApprovalStatus = "action_required"

// workflowApprovalCacheTTL bounds the extra workflow-run listing to one call
// per head per window; the cycle calls EnrichCIStatus far more often than a
// human approves workflows.
const workflowApprovalCacheTTL = 90 * time.Second

type workflowApprovalEntry struct {
	at  time.Time
	val *PRWorkflowApproval
}

var workflowApprovalCache sync.Map // repo@sha → workflowApprovalEntry

// workflowRunsAwaitingApproval lists the head's workflow runs and keeps the
// ones gated on approval. It returns nil when none are, or when the listing
// failed: absence of evidence is not evidence.
func (c *Client) workflowRunsAwaitingApproval(ctx context.Context, repo, sha string) *PRWorkflowApproval {
	if c == nil || c.client == nil || strings.TrimSpace(sha) == "" {
		return nil
	}
	key := repo + "@" + sha
	if cached, ok := workflowApprovalCache.Load(key); ok {
		if e, ok := cached.(workflowApprovalEntry); ok && time.Since(e.at) < workflowApprovalCacheTTL {
			return e.val
		}
	}
	owner, name := c.splitRepo(repo)
	runs, _, err := c.client.Actions.ListRepositoryWorkflowRuns(WithRESTCaller(ctx, "hive:workflow_approval"), owner, name, &gh.ListWorkflowRunsOptions{
		HeadSHA:     sha,
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil || runs == nil {
		c.logger.Debug("failed to list workflow runs for head", "repo", repo, "sha", sha, "error", err)
		return nil
	}
	val := workflowApprovalFromRuns(runs.WorkflowRuns, time.Now().UTC())
	workflowApprovalCache.Store(key, workflowApprovalEntry{at: time.Now(), val: val})
	return val
}

// workflowApprovalFromRuns is the pure half: the approval-gated subset of a
// head's workflow runs, or nil when the head has none.
func workflowApprovalFromRuns(runs []*gh.WorkflowRun, now time.Time) *PRWorkflowApproval {
	var out *PRWorkflowApproval
	for _, run := range runs {
		if run == nil {
			continue
		}
		if run.GetStatus() != WorkflowApprovalStatus && run.GetConclusion() != WorkflowApprovalStatus {
			continue
		}
		if out == nil {
			at := now.UTC()
			out = &PRWorkflowApproval{ObservedAt: &at}
		}
		out.Pending++
		if id := run.GetID(); id != 0 {
			out.RunIDs = append(out.RunIDs, id)
		}
		if out.Actor == "" {
			if actor := run.GetActor().GetLogin(); actor != "" {
				out.Actor = actor
			} else if actor := run.GetTriggeringActor().GetLogin(); actor != "" {
				out.Actor = actor
			}
		}
	}
	return out
}

// forgetWorkflowApproval drops the cached probe for a head so the next
// cycle re-lists after an approval was issued.
func forgetWorkflowApproval(repo, sha string) {
	workflowApprovalCache.Delete(repo + "@" + sha)
}
