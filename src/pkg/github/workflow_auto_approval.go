package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Auto-approval of GitHub's "approve and run workflows" gate
// (hivecommons/hive#16603). EnrichCIStatus detects gated runs on a pending
// head (hivecommons/hive#16599); SweepWorkflowApprovals, right after it,
// approves the ones a trust policy covers, at most once per PR head.

const workflowApprovalRESTCaller = "hive:workflow_auto_approval"

// workflowApprovalAttemptTTL bounds the ledger: an entry no sweep has
// touched for this long belongs to a merged, closed or superseded head.
const workflowApprovalAttemptTTL = 24 * time.Hour

type workflowApprovalKey struct {
	repo   string
	number int
	head   string
}

type workflowApprovalAttempt struct {
	attempted        bool
	deniedCounted    bool
	permissionDenied bool
	seen             time.Time
}

// workflowApprovalLedger is the process-local record of approval passes,
// one entry per PR head, plus the /api/status counters.
type workflowApprovalLedger struct {
	mu       sync.Mutex
	attempts map[workflowApprovalKey]workflowApprovalAttempt
	status   WorkflowApprovalsStatus
}

// WorkflowApprovalsStatus is /api/status.workflow_approvals: decisions
// since the process started.
type WorkflowApprovalsStatus struct {
	Approved         int                    `json:"approved"`
	Denied           int                    `json:"denied"`
	PermissionErrors int                    `json:"permission_errors"`
	Last             *WorkflowApprovalEvent `json:"last,omitempty"`
}

// WorkflowApprovalEvent is the most recent approval decision.
type WorkflowApprovalEvent struct {
	Repo    string    `json:"repo"`
	Number  int       `json:"number"`
	Head    string    `json:"head"`
	RunID   int64     `json:"run_id"`
	Outcome string    `json:"outcome"`
	At      time.Time `json:"at"`
}

// WorkflowApprovalsStatus returns a copy of the approval counters.
func (c *Client) WorkflowApprovalsStatus() WorkflowApprovalsStatus {
	if c == nil {
		return WorkflowApprovalsStatus{}
	}
	c.workflowApprovals.mu.Lock()
	defer c.workflowApprovals.mu.Unlock()
	out := c.workflowApprovals.status
	if out.Last != nil {
		last := *out.Last
		out.Last = &last
	}
	return out
}

// touchLocked returns the head's entry, marking it live, and evicts
// superseded heads of the same PR and entries older than the TTL.
func (l *workflowApprovalLedger) touchLocked(key workflowApprovalKey, now time.Time) workflowApprovalAttempt {
	if l.attempts == nil {
		l.attempts = map[workflowApprovalKey]workflowApprovalAttempt{}
	}
	for other, entry := range l.attempts {
		if other == key {
			continue
		}
		if (other.repo == key.repo && other.number == key.number) || now.Sub(entry.seen) >= workflowApprovalAttemptTTL {
			delete(l.attempts, other)
		}
	}
	attempt := l.attempts[key]
	attempt.seen = now
	l.attempts[key] = attempt
	return attempt
}

func (l *workflowApprovalLedger) record(key workflowApprovalKey, runID int64, outcome string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch outcome {
	case "approved":
		l.status.Approved++
	case "denied":
		l.status.Denied++
	case "permission_error":
		l.status.PermissionErrors++
		attempt := l.attempts[key]
		attempt.permissionDenied = true
		l.attempts[key] = attempt
	}
	l.status.Last = &WorkflowApprovalEvent{Repo: key.repo, Number: key.number, Head: key.head, RunID: runID, Outcome: outcome, At: time.Now().UTC()}
}

func (c *Client) workflowApprovalKey(repo string, number int, head string) workflowApprovalKey {
	owner, name := c.splitRepo(repo)
	return workflowApprovalKey{repo: owner + "/" + name, number: number, head: head}
}

// workflowApprovalWithLedger carries a recorded permission refusal onto the
// head's probe, so the "needs workflow approval" caption holds on every
// cycle, including the ones that skip the sweep. The cached probe is shared
// and never mutated.
func (c *Client) workflowApprovalWithLedger(repo string, number int, head string, wa *PRWorkflowApproval) *PRWorkflowApproval {
	if c == nil || wa == nil || wa.PermissionDenied {
		return wa
	}
	key := c.workflowApprovalKey(repo, number, head)
	c.workflowApprovals.mu.Lock()
	denied := c.workflowApprovals.attempts[key].permissionDenied
	c.workflowApprovals.mu.Unlock()
	if !denied {
		return wa
	}
	out := *wa
	out.PermissionDenied = true
	return &out
}

// workflowApprovalTrusted is the trust policy: the owner admitted the PR
// (automerge label), or the PR author or the run's triggering actor is the
// hive's App or in the allowlist. A trusted author covers every gated run
// on the head, since the code under test is theirs.
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

// workflowRunApprovable says whether a gated run belongs to this PR head
// and may be approved at all. The event filter lives here, not in the
// shared probe, so the #16599 caption still counts every gated run
// (hivecommons/hive#16682): only `pull_request` runs are approved, never
// `pull_request_target` or other events that run with repository secrets.
// headRepo is checked only when known.
func workflowRunApprovable(run *gh.WorkflowRun, number int, head, headRepo string) bool {
	if run == nil || run.GetID() <= 0 || run.GetEvent() != "pull_request" || !strings.EqualFold(run.GetHeadSHA(), head) {
		return false
	}
	if headRepo != "" && !strings.EqualFold(run.GetHeadRepository().GetFullName(), headRepo) {
		return false
	}
	if len(run.PullRequests) == 0 {
		return true
	}
	for _, linked := range run.PullRequests {
		if linked.GetNumber() == number {
			return true
		}
	}
	return false
}

func workflowTriggeringActor(run *gh.WorkflowRun) string {
	if login := run.GetTriggeringActor().GetLogin(); login != "" {
		return login
	}
	return run.GetActor().GetLogin()
}

func (c *Client) workflowApprovalHeld(labels []string) bool {
	return c.isHeld(labels) || c.isExempt(labels) || c.sentinelBlockedLabel(labels) != ""
}

func (c *Client) workflowApprovalAdmitted(labels []string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), c.AutoMergeLabel()) {
			return true
		}
	}
	return false
}

// splitTrustedWorkflowRuns partitions the approvable gated runs by policy.
func (c *Client) splitTrustedWorkflowRuns(runs []*gh.WorkflowRun, number int, head, headRepo, author string, labels, actors []string) (trusted, denied []*gh.WorkflowRun) {
	admitted := c.workflowApprovalAdmitted(labels)
	app := c.AppBotLogin()
	for _, run := range runs {
		if !workflowRunApprovable(run, number, head, headRepo) {
			continue
		}
		if workflowApprovalTrusted(author, workflowTriggeringActor(run), app, admitted, actors) {
			trusted = append(trusted, run)
		} else {
			denied = append(denied, run)
		}
	}
	return trusted, denied
}

// SweepWorkflowApprovals approves gated workflow runs that EnrichCIStatus
// found on the PRs' heads, when policy trusts them, at most once per PR
// head. Call it right after EnrichCIStatus on the same slice. A 403/404 is
// recorded once and surfaces as PermissionDenied instead of being retried.
func (c *Client) SweepWorkflowApprovals(ctx context.Context, prs []PullRequest, policy config.GovernorCIConfig) {
	if c == nil || c.client == nil || !policy.AutoApproveWorkflowsEnabled() {
		return
	}
	actors := policy.WorkflowApprovalActors()
	for i := range prs {
		pr := &prs[i]
		a := pr.CIActivity
		if a == nil || a.WorkflowApproval == nil || a.WorkflowApproval.Pending <= 0 || !strings.EqualFold(a.Head, pr.HeadSHA) {
			continue
		}
		if pr.Draft || pr.HeadSHA == "" || pr.Number <= 0 || (pr.State != "" && pr.State != "open") {
			continue
		}
		key := c.workflowApprovalKey(pr.Repo, pr.Number, pr.HeadSHA)
		if c.workflowApprovalHeld(pr.Labels) || c.RepoIsPaused(key.repo) {
			continue
		}
		wa := a.WorkflowApproval
		c.workflowApprovals.mu.Lock()
		previous := c.workflowApprovals.touchLocked(key, time.Now())
		c.workflowApprovals.mu.Unlock()
		if previous.attempted {
			continue
		}
		trusted, denied := c.splitTrustedWorkflowRuns(wa.runs, pr.Number, pr.HeadSHA, "", pr.Author, pr.Labels, actors)
		if len(denied) > 0 && !previous.deniedCounted {
			c.workflowApprovals.mu.Lock()
			attempt := c.workflowApprovals.attempts[key]
			attempt.deniedCounted = true
			c.workflowApprovals.attempts[key] = attempt
			c.workflowApprovals.mu.Unlock()
			for _, run := range denied {
				c.workflowApprovals.record(key, run.GetID(), "denied")
			}
		}
		if len(trusted) == 0 {
			continue
		}
		owner, name := c.splitRepo(pr.Repo)
		// The listing may be a cycle old. Re-read head, author and labels
		// before letting the head run with the repository's CI rights.
		fresh, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, workflowApprovalRESTCaller), owner, name, pr.Number)
		if err != nil || fresh == nil || fresh.GetState() != "open" || fresh.GetDraft() || fresh.GetHead().GetSHA() != pr.HeadSHA {
			continue
		}
		labels := labelNames(fresh.Labels)
		headRepo := fresh.GetHead().GetRepo().GetFullName()
		if c.workflowApprovalHeld(labels) || headRepo == "" {
			continue
		}
		trusted, _ = c.splitTrustedWorkflowRuns(wa.runs, pr.Number, pr.HeadSHA, headRepo, fresh.GetUser().GetLogin(), labels, actors)
		if len(trusted) == 0 {
			continue
		}
		item := c.WritebackItemForPR(*pr)
		item.Author, item.Labels = fresh.GetUser().GetLogin(), labels
		if !c.WritebackAllowed(key.repo, item, writeback.KindCIRerun, "approve gated workflow runs").Allowed {
			continue
		}
		// Claim before the first POST: neither a concurrent cycle nor an
		// unchanged head repeats the pass, even after a partial failure.
		c.workflowApprovals.mu.Lock()
		attempt := c.workflowApprovals.attempts[key]
		claimed := !attempt.attempted
		attempt.attempted = true
		c.workflowApprovals.attempts[key] = attempt
		c.workflowApprovals.mu.Unlock()
		if !claimed {
			continue
		}
		approved := false
		for _, run := range trusted {
			err := c.approveWorkflowRun(ctx, owner, name, run.GetID())
			if err == nil {
				approved = true
				c.workflowApprovals.record(key, run.GetID(), "approved")
				c.logger.Info("approved gated workflow run", "repo", key.repo, "pr", pr.Number, "head", pr.HeadSHA, "run_id", run.GetID(), "actor", workflowTriggeringActor(run))
				continue
			}
			var apiErr *gh.ErrorResponse
			if errors.As(err, &apiErr) && apiErr.Response != nil && (apiErr.Response.StatusCode == http.StatusForbidden || apiErr.Response.StatusCode == http.StatusNotFound) {
				c.workflowApprovals.record(key, run.GetID(), "permission_error")
				c.logger.Warn("workflow approval refused; needs workflow approval on GitHub (App lacks actions: write?)", "repo", key.repo, "pr", pr.Number, "head", pr.HeadSHA, "run_id", run.GetID(), "error", err)
				refused := *wa
				refused.PermissionDenied = true
				activity := cloneCIActivity(a)
				activity.WorkflowApproval = &refused
				pr.CIActivity = activity
				break
			}
			c.logger.Warn("failed to approve gated workflow run", "repo", key.repo, "pr", pr.Number, "head", pr.HeadSHA, "run_id", run.GetID(), "error", err)
		}
		if approved {
			// Let the next cycle observe the runs starting.
			forgetWorkflowApproval(pr.Repo, pr.HeadSHA)
		}
	}
}

// approveWorkflowRun calls POST /repos/{o}/{r}/actions/runs/{id}/approve,
// which go-github v72 does not bind.
func (c *Client) approveWorkflowRun(ctx context.Context, owner, repo string, runID int64) error {
	req, err := c.client.NewRequest(http.MethodPost, fmt.Sprintf("repos/%s/%s/actions/runs/%d/approve", owner, repo, runID), nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(WithRESTCaller(ctx, workflowApprovalRESTCaller), req, nil)
	c.noteWritebackOutcome(owner+"/"+repo, resp, err)
	return err
}
