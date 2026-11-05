package github

import (
	"context"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// PRWorkflowApproval is evidence of GitHub's approve-and-run gate, which
// can hold workflow runs before they create any check runs.
type PRWorkflowApproval struct {
	Pending          int        `json:"pending"`
	RunIDs           []int64    `json:"run_ids,omitempty"`
	Actor            string     `json:"actor,omitempty"`
	ObservedAt       *time.Time `json:"observed_at,omitempty"`
	PermissionDenied bool       `json:"permission_denied,omitempty"`
	runs             []*gh.WorkflowRun
}

// WorkflowApprovalStatus is GitHub's status/conclusion for approval-gated runs.
const WorkflowApprovalStatus = "action_required"
const workflowApprovalCacheTTL = 90 * time.Second

type workflowApprovalEntry struct {
	at  time.Time
	val *PRWorkflowApproval
}

type workflowApprovalKey struct {
	repo   string
	number int
	head   string
}

type workflowApprovalAttempt struct {
	attempted        bool
	denied           bool
	permissionDenied bool
	seen             time.Time
}

// workflowApprovalAttemptTTL bounds the attempts ledger: an entry no sweep
// has observed for this long belongs to a merged, closed or superseded head.
const workflowApprovalAttemptTTL = 24 * time.Hour

type workflowApprovalState struct {
	mu       sync.Mutex
	probes   map[string]workflowApprovalEntry
	attempts map[workflowApprovalKey]workflowApprovalAttempt
	status   WorkflowApprovalsStatus
}

// WorkflowApprovalsStatus counts decisions since this client was created.
type WorkflowApprovalsStatus struct {
	Approved         int                    `json:"approved"`
	Denied           int                    `json:"denied"`
	PermissionErrors int                    `json:"permission_errors"`
	Last             *WorkflowApprovalEvent `json:"last,omitempty"`
}

// WorkflowApprovalEvent describes the most recent approval decision.
type WorkflowApprovalEvent struct {
	Repo    string    `json:"repo"`
	Number  int       `json:"number"`
	Head    string    `json:"head"`
	RunID   int64     `json:"run_id"`
	Outcome string    `json:"outcome"`
	At      time.Time `json:"at"`
}

// WorkflowApprovalsStatus returns a snapshot suitable for /api/status.
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

func (s *workflowApprovalState) record(key workflowApprovalKey, runID int64, outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch outcome {
	case "approved":
		s.status.Approved++
	case "denied":
		s.status.Denied++
	case "permission_error":
		s.status.PermissionErrors++
		attempt := s.attempts[key]
		attempt.permissionDenied = true
		s.attempts[key] = attempt
	}
	s.status.Last = &WorkflowApprovalEvent{Repo: key.repo, Number: key.number, Head: key.head, RunID: runID, Outcome: outcome, At: time.Now().UTC()}
}

// touchAttempt marks a gated head as live in the attempts ledger and returns
// its entry. Superseded heads of the same PR and entries no sweep has observed
// within workflowApprovalAttemptTTL are evicted. The caller holds s.mu.
func (s *workflowApprovalState) touchAttempt(key workflowApprovalKey, now time.Time) workflowApprovalAttempt {
	if s.attempts == nil {
		s.attempts = map[workflowApprovalKey]workflowApprovalAttempt{}
	}
	for other, entry := range s.attempts {
		if other == key {
			continue
		}
		if (other.repo == key.repo && other.number == key.number) || now.Sub(entry.seen) >= workflowApprovalAttemptTTL {
			delete(s.attempts, other)
		}
	}
	attempt := s.attempts[key]
	attempt.seen = now
	s.attempts[key] = attempt
	return attempt
}

// workflowApprovalForPR returns a copy of the head's approval probe with the
// attempts ledger applied, so a permission refusal keeps its caption on cycles
// that skip SweepWorkflowApprovals. Cached probes are never mutated.
func (c *Client) workflowApprovalForPR(ctx context.Context, repo string, number int, sha string) *PRWorkflowApproval {
	probe := c.workflowRunsAwaitingApproval(ctx, repo, sha)
	if probe == nil {
		return nil
	}
	wa := *probe
	owner, name := c.splitRepo(repo)
	key := workflowApprovalKey{repo: owner + "/" + name, number: number, head: sha}
	c.workflowApprovals.mu.Lock()
	wa.PermissionDenied = c.workflowApprovals.attempts[key].permissionDenied
	c.workflowApprovals.mu.Unlock()
	return &wa
}

// workflowRunsAwaitingApproval memoizes the read for a short window. The
// caller validates each run's head repository and SHA before any mutation.
func (c *Client) workflowRunsAwaitingApproval(ctx context.Context, repo, sha string) *PRWorkflowApproval {
	if c == nil || c.client == nil || strings.TrimSpace(sha) == "" {
		return nil
	}
	key := repo + "@" + sha
	c.workflowApprovals.mu.Lock()
	cached, ok := c.workflowApprovals.probes[key]
	c.workflowApprovals.mu.Unlock()
	if ok && time.Since(cached.at) < workflowApprovalCacheTTL {
		return cached.val
	}
	owner, name := c.splitRepo(repo)
	opts := &gh.ListWorkflowRunsOptions{
		HeadSHA: sha, Event: "pull_request", ListOptions: gh.ListOptions{PerPage: 100},
	}
	var current []*gh.WorkflowRun
	for {
		runs, resp, err := c.client.Actions.ListRepositoryWorkflowRuns(WithRESTCaller(ctx, "hive:workflow_approval"), owner, name, opts)
		if err != nil || runs == nil {
			c.logger.Debug("failed to list workflow runs for head", "repo", repo, "sha", sha, "error", err)
			return nil
		}
		// Do not turn a response for a different head into approval evidence.
		for _, run := range runs.WorkflowRuns {
			if run != nil && strings.EqualFold(run.GetHeadSHA(), sha) && run.GetEvent() == "pull_request" {
				current = append(current, run)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	val := workflowApprovalFromRuns(current, time.Now().UTC())
	c.workflowApprovals.mu.Lock()
	if c.workflowApprovals.probes == nil {
		c.workflowApprovals.probes = map[string]workflowApprovalEntry{}
	}
	// Bound the probe cache to heads seen in the current TTL window.
	c.workflowApprovals.evictStaleProbes(time.Now())
	c.workflowApprovals.probes[key] = workflowApprovalEntry{at: time.Now(), val: val}
	c.workflowApprovals.mu.Unlock()
	return val
}

func workflowApprovalFromRuns(runs []*gh.WorkflowRun, now time.Time) *PRWorkflowApproval {
	var out *PRWorkflowApproval
	for _, run := range runs {
		if run == nil || run.GetID() <= 0 || (run.GetStatus() != WorkflowApprovalStatus && run.GetConclusion() != WorkflowApprovalStatus) {
			continue
		}
		if out == nil {
			at := now.UTC()
			out = &PRWorkflowApproval{ObservedAt: &at}
		}
		out.Pending++
		out.RunIDs = append(out.RunIDs, run.GetID())
		out.runs = append(out.runs, run)
		if out.Actor == "" {
			out.Actor = workflowTriggeringActor(run)
		}
	}
	return out
}

func workflowTriggeringActor(run *gh.WorkflowRun) string {
	if login := run.GetTriggeringActor().GetLogin(); login != "" {
		return login
	}
	return run.GetActor().GetLogin()
}

// evictStaleProbes bounds the per-client cache to heads seen in the current
// TTL window. The caller holds s.mu.
func (s *workflowApprovalState) evictStaleProbes(now time.Time) {
	for key, entry := range s.probes {
		if now.Sub(entry.at) >= workflowApprovalCacheTTL {
			delete(s.probes, key)
		}
	}
}

// forgetWorkflowApproval drops a head's probe after CI webhooks or successful
// approvals, so the next cycle observes the workflow gate again.
func (c *Client) forgetWorkflowApproval(repo, sha string) {
	c.workflowApprovals.mu.Lock()
	defer c.workflowApprovals.mu.Unlock()
	delete(c.workflowApprovals.probes, repo+"@"+sha)
}
