package github

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// PRWorkflowApproval records workflow runs on a PR head that GitHub holds
// behind its "approve and run workflows" gate (status action_required). Such
// runs create no check runs, so from the check-run listing alone the required
// checks simply look unreported; this is the evidence that they are in fact
// waiting on a person clicking Approve (hivecommons/hive#16599).
//
// PermissionDenied is set once the hive tried to approve the runs itself and
// GitHub refused for want of `actions: write`, so the click is a person's
// (hivecommons/hive#16603).
type PRWorkflowApproval struct {
	Pending          int        `json:"pending"`
	RunIDs           []int64    `json:"run_ids,omitempty"`
	Actor            string     `json:"actor,omitempty"`
	ObservedAt       *time.Time `json:"observed_at,omitempty"`
	PermissionDenied bool       `json:"permission_denied,omitempty"`
	// Denied is set once the approval sweep decided the hive will not
	// approve this head's runs: its trust policy does not cover the head, or
	// governor.ci.auto_approve_workflows is off. Like PermissionDenied it is
	// kept per head in the sweep's ledger, so it holds on the cycles that
	// skip the sweep.
	Denied bool `json:"denied,omitempty"`
	// runs are the gated runs themselves, read-only, for the auto-approval
	// decision (triggering actor, event, head repository).
	runs []*gh.WorkflowRun
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

// workflowApprovalEvictAfter is how long a head's probe stays cached once
// it is no longer refreshed: heads that got pushed over or merged stop
// being probed, and their entries go with the next sweep of the map.
const workflowApprovalEvictAfter = 30 * time.Minute

var workflowApprovalLastEvict atomic.Int64 // unix seconds

// evictStaleWorkflowApprovals drops entries older than
// workflowApprovalEvictAfter, at most once per eviction window, so the
// package-global map stays bounded by the heads probed in the last half hour.
func evictStaleWorkflowApprovals(now time.Time) {
	last := workflowApprovalLastEvict.Load()
	if now.Unix()-last < int64(workflowApprovalEvictAfter/time.Second) {
		return
	}
	if !workflowApprovalLastEvict.CompareAndSwap(last, now.Unix()) {
		return
	}
	workflowApprovalCache.Range(func(key, value any) bool {
		if e, ok := value.(workflowApprovalEntry); !ok || now.Sub(e.at) > workflowApprovalEvictAfter {
			workflowApprovalCache.Delete(key)
		}
		return true
	})
}

// workflowRunsAwaitingApproval lists the head's workflow runs and keeps the
// ones gated on approval. It returns nil when none are, or when the listing
// failed: absence of evidence is not evidence.
func (c *Client) workflowRunsAwaitingApproval(ctx context.Context, repo, sha string) *PRWorkflowApproval {
	if c == nil || c.client == nil || strings.TrimSpace(sha) == "" {
		return nil
	}
	key := repo + "@" + sha
	evictStaleWorkflowApprovals(time.Now())
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
		out.runs = append(out.runs, run)
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

// workflowApprovalProbeWanted reports whether pr's head may have workflow
// runs GitHub holds for approval, so that it is worth the listing: CI still
// pending, or green on what did run while a required check never reported
// at all. The second is a fork head whose `pull_request_target` workflows
// ran (Prow) while its `pull_request` runs are held: CIStatus reads success,
// and a pending-only probe never saw the held runs (projectbluefin/server#686).
// A red head is not probed; its failure is the blocker.
func workflowApprovalProbeWanted(pr PullRequest) bool {
	if pr.CIActivity == nil {
		return false
	}
	switch pr.CIStatus {
	case "pending":
		return true
	case "success":
		return pr.Protection != nil && len(pr.Protection.MissingRequiredChecks) > 0
	}
	return false
}

// HeldWorkflowRunIDs names the workflow runs on pr's current head that
// GitHub holds behind its approve-workflows gate and that the hive decided
// it will not approve (Denied) or was refused permission to approve
// (PermissionDenied): a person's click is what moves the PR. Nil when there
// are none, when the evidence is for another head, or while the sweep has
// not decided — a trusted head waits for the sweep, not for a person.
func (p PullRequest) HeldWorkflowRunIDs() []int64 {
	a := p.CIActivity
	if a == nil || a.WorkflowApproval == nil || a.WorkflowApproval.Pending <= 0 || len(a.WorkflowApproval.RunIDs) == 0 {
		return nil
	}
	if !a.WorkflowApproval.Denied && !a.WorkflowApproval.PermissionDenied {
		return nil
	}
	if a.Head != "" && p.HeadSHA != "" && !strings.EqualFold(a.Head, p.HeadSHA) {
		return nil
	}
	return append([]int64(nil), a.WorkflowApproval.RunIDs...)
}

// forgetWorkflowApproval drops the cached probe for a head so the next
// cycle re-lists: called when a CI webhook for the head arrives (someone
// approved, a run started) and after the hive approves runs itself.
func forgetWorkflowApproval(repo, sha string) {
	workflowApprovalCache.Delete(repo + "@" + sha)
}
