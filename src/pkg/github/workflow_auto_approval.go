package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/writeback"
)

// workflowApprovalTrusted follows hivecommons/hive#16603: a trusted PR
// author authorizes every gated run on the head, because the code under
// test is theirs. Otherwise trust is per run — a trusted triggering actor
// (or the App) authorizes only the runs it triggered, and an admitted PR
// authorizes all of them.
func workflowApprovalTrusted(author, actor, app string, admitted bool, actors []string) bool {
	if admitted {
		return true
	}
	for _, login := range []string{author, actor} {
		login = strings.TrimSpace(login)
		if login == "" {
			continue
		}
		if app != "" && strings.EqualFold(login, app) {
			return true
		}
		for _, trusted := range actors {
			if strings.EqualFold(login, strings.TrimSpace(trusted)) {
				return true
			}
		}
	}
	return false
}

// SweepWorkflowApprovals runs after CI enrichment. It observes the workflow
// approval gate and approves trusted runs at most once per PR head in this
// client. Permission refusals remain display evidence without repeated writes.
func (c *Client) SweepWorkflowApprovals(ctx context.Context, prs []PullRequest, policy config.GovernorCIConfig) {
	if c == nil || c.client == nil {
		return
	}
	for i := range prs {
		pr := &prs[i]
		if pr.Draft || pr.HeadSHA == "" || pr.Number <= 0 || pr.CIStatus == "success" || (pr.State != "" && pr.State != "open") {
			continue
		}
		owner, name := c.splitRepo(pr.Repo)
		repo := owner + "/" + name
		if c.workflowApprovalHeld(pr.Labels) || c.RepoIsPaused(repo) {
			continue
		}
		if pr.CIActivity != nil && strings.EqualFold(pr.CIActivity.Head, pr.HeadSHA) {
			activity := *pr.CIActivity
			activity.WorkflowApproval = nil
			pr.CIActivity = &activity
		}
		probe := c.workflowApprovalForPR(ctx, repo, pr.Number, pr.HeadSHA)
		if probe == nil {
			continue
		}
		wa := *probe
		if pr.CIActivity == nil {
			pr.CIActivity = &PRCIActivity{Head: pr.HeadSHA, Source: "github-workflow-runs"}
		}
		if !strings.EqualFold(pr.CIActivity.Head, pr.HeadSHA) {
			continue
		}
		pr.CIActivity.WorkflowApproval = &wa
		key := workflowApprovalKey{repo: repo, number: pr.Number, head: pr.HeadSHA}
		c.workflowApprovals.mu.Lock()
		previous := c.workflowApprovals.touchAttempt(key, time.Now())
		c.workflowApprovals.mu.Unlock()
		if previous.attempted || !policy.AutoApproveWorkflowsEnabled() {
			continue
		}
		// Enumeration can be cached. Re-read the head, author and admission
		// before trusting them to execute code with the repository's CI rights.
		fresh, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:workflow_approval"), owner, name, pr.Number)
		if err != nil || fresh == nil || fresh.GetState() != "open" || fresh.GetDraft() || fresh.GetHead().GetSHA() != pr.HeadSHA {
			continue
		}
		labels := labelNames(fresh.Labels)
		if c.workflowApprovalHeld(labels) {
			continue
		}
		admitted := false
		for _, label := range labels {
			admitted = admitted || strings.EqualFold(label, c.AutoMergeLabel())
		}
		var trusted, denied []*gh.WorkflowRun
		headRepo := fresh.GetHead().GetRepo().GetFullName()
		for _, run := range wa.runs {
			if headRepo == "" || !strings.EqualFold(run.GetHeadRepository().GetFullName(), headRepo) {
				continue
			}
			if len(run.PullRequests) > 0 {
				matches := false
				for _, linked := range run.PullRequests {
					matches = matches || linked.GetNumber() == pr.Number
				}
				if !matches {
					continue
				}
			}
			if workflowApprovalTrusted(fresh.GetUser().GetLogin(), workflowTriggeringActor(run), c.AppBotLogin(), admitted, policy.WorkflowApprovalActors()) {
				trusted = append(trusted, run)
			} else {
				denied = append(denied, run)
			}
		}
		c.workflowApprovals.mu.Lock()
		attempt := c.workflowApprovals.attempts[key]
		recordDenied := !attempt.denied
		attempt.denied = attempt.denied || len(denied) > 0
		c.workflowApprovals.attempts[key] = attempt
		c.workflowApprovals.mu.Unlock()
		if recordDenied {
			for _, run := range denied {
				c.workflowApprovals.record(key, run.GetID(), "denied")
			}
		}
		item := c.WritebackItemForPR(*pr)
		item.Author, item.Labels = fresh.GetUser().GetLogin(), labels
		if len(trusted) == 0 || !c.WritebackAllowed(repo, item, writeback.KindCIRerun, "approve gated workflow runs").Allowed {
			continue
		}
		// Claim before the first POST, including partial failures. A concurrent
		// cycle and an unchanged head must not repeat the approval pass.
		c.workflowApprovals.mu.Lock()
		attempt = c.workflowApprovals.attempts[key]
		claimed := !attempt.attempted
		attempt.attempted = true
		c.workflowApprovals.attempts[key] = attempt
		c.workflowApprovals.mu.Unlock()
		if !claimed {
			continue
		}
		for _, run := range trusted {
			if err := c.approveGatedWorkflowRun(ctx, owner, name, run.GetID()); err != nil {
				var apiErr *gh.ErrorResponse
				if errors.As(err, &apiErr) && apiErr.Response != nil && (apiErr.Response.StatusCode == http.StatusForbidden || apiErr.Response.StatusCode == http.StatusNotFound) {
					c.workflowApprovals.record(key, run.GetID(), "permission_error")
					wa.PermissionDenied = true
					c.logger.Warn("workflow approval refused; needs workflow approval on GitHub", "repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "run_id", run.GetID(), "error", err)
					break
				}
				c.logger.Warn("failed to approve gated workflow run", "repo", repo, "pr", pr.Number, "run_id", run.GetID(), "error", err)
				continue
			}
			c.workflowApprovals.record(key, run.GetID(), "approved")
			c.logger.Info("approved gated workflow run", "repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "run_id", run.GetID())
			c.forgetWorkflowApproval(repo, pr.HeadSHA)
		}
	}
}

func (c *Client) workflowApprovalHeld(labels []string) bool {
	return c.isHeld(labels) || c.isExempt(labels) || c.sentinelBlockedLabel(labels) != ""
}

// go-github v72 does not bind POST /actions/runs/{id}/approve.
func (c *Client) approveGatedWorkflowRun(ctx context.Context, owner, repo string, runID int64) error {
	req, err := c.client.NewRequest(http.MethodPost, fmt.Sprintf("repos/%s/%s/actions/runs/%d/approve", owner, repo, runID), nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(WithRESTCaller(ctx, "hive:workflow_approval"), req, nil)
	c.noteWritebackOutcome(owner+"/"+repo, resp, err)
	return err
}
