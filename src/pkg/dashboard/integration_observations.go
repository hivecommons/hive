package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
)

const (
	integrationReconcileInterval       = 3 * time.Minute
	integrationReconcileTimeout        = 45 * time.Second
	integrationReconcileMaxRuns        = 8
	integrationReconcilePageSize       = 100
	integrationReconcileMaxJobReads    = 24
	integrationReconcileMaxPolicyReads = 4
)

type integrationWebhookPayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest *gh.PullRequest `json:"pull_request"`
	WorkflowRun *gh.WorkflowRun `json:"workflow_run"`
	MergeGroup  struct {
		BaseRef string `json:"base_ref"`
		BaseSHA string `json:"base_sha"`
		HeadRef string `json:"head_ref"`
		HeadSHA string `json:"head_sha"`
	} `json:"merge_group"`
	WorkflowJob struct {
		ID           int64     `json:"id"`
		RunID        int64     `json:"run_id"`
		RunAttempt   int       `json:"run_attempt"`
		HeadSHA      string    `json:"head_sha"`
		HeadBranch   string    `json:"head_branch"`
		WorkflowName string    `json:"workflow_name"`
		RunnerID     int64     `json:"runner_id"`
		Status       string    `json:"status"`
		Conclusion   string    `json:"conclusion"`
		StartedAt    time.Time `json:"started_at"`
		CompletedAt  time.Time `json:"completed_at"`
	} `json:"workflow_job"`
}

func integrationBranchBase(branch string, scopes []config.IntegrationScope, repo string) string {
	for _, scope := range scopes {
		if !strings.EqualFold(scope.Repo, repo) {
			continue
		}
		if branch == scope.Base || strings.HasPrefix(branch, "gh-readonly-queue/"+scope.Base+"/") {
			return scope.Base
		}
	}
	return ""
}

func (s *Server) observeIntegrationWebhook(event, delivery string, body []byte, now time.Time) error {
	if err := s.observeIntegrationExecutionWebhook(event, delivery, body, now); err != nil {
		return err
	}
	cfg := s.integrationConfig()
	if cfg.Mode == config.IntegrationOff {
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	var p integrationWebhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return err
	}
	repo := p.Repository.FullName
	enrolled := false
	for _, scope := range cfg.Scopes {
		if strings.EqualFold(scope.Repo, repo) {
			enrolled = true
		}
	}
	if !enrolled {
		return nil
	}
	store, err := s.integrationStore()
	if err != nil {
		return err
	}
	if event == "workflow_run" && p.WorkflowRun != nil {
		if err := s.observeIntegrationRun(p.WorkflowRun, repo, delivery, now); err != nil {
			return err
		}
		if p.WorkflowRun.GetEvent() == "merge_group" {
			return s.observeIntegrationPolicyRun(context.Background(), p.WorkflowRun, repo, delivery, now)
		}
		return s.observeIntegrationDirectPolicyRun(context.Background(), p.WorkflowRun, repo, delivery, now)
	}
	if store.HasEvent("github/" + delivery) {
		return nil
	}
	if event == "merge_group" {
		g := p.MergeGroup
		base := strings.TrimPrefix(g.BaseRef, "refs/heads/")
		if !cfg.Allows(repo, base) {
			return nil
		}
		if p.Action == "destroyed" {
			return store.InvalidateGroup(repo, g.HeadRef, g.HeadSHA, delivery, "native group destroyed", now)
		}
		ctx, cancel := context.WithTimeout(context.Background(), integrationReconcileTimeout)
		defer cancel()
		if s.deps == nil || s.deps.GHClient == nil {
			return fmt.Errorf("native group observation client unavailable")
		}
		group, readErr := s.deps.GHClient.ReadNativeIntegrationGroup(ctx, repo, base, g.BaseSHA, g.HeadRef, g.HeadSHA)
		subject := integration.Subject{Version: integration.SchemaVersion, Repo: repo, Base: base, Engine: "native",
			Authority: g.HeadRef, BaseSHA: group.BaseSHA, TestedSHA: g.HeadSHA, TestedTree: group.Tree,
			Policy: "unverified-policy", Producer: "github-merge-group-webhook", Workflow: "merge_group", Attempt: 1, Generation: 1}
		for _, m := range group.Members {
			subject.Members = append(subject.Members, integration.Member{PR: m.PR, Head: m.Head, Entry: m.Entry, Position: m.Position, BaseCommit: m.BaseCommit, HeadCommit: m.HeadCommit})
		}
		state := integration.Waiting
		reason := "membership and tested tree observed; required policy/workflow/producer proof remains unknown"
		if readErr != nil {
			state = integration.Unknown
			reason = "authoritative group observation unavailable: " + readErr.Error()
		}
		evidence := integration.Evidence{Subject: subject, State: state, ObservedAt: now, Source: "github-merge-group-v1",
			Reason: reason}
		return store.Append(integration.Event{Version: integration.SchemaVersion, ID: "github/" + delivery, At: now, Evidence: &evidence})
	}

	if event == "pull_request" || event == "pull_request_review" {
		if pr := p.PullRequest; pr != nil {
			reason := "member eligibility or review changed"
			head := ""
			if p.Action == "synchronize" {
				head = pr.GetHead().GetSHA()
				reason = "member head changed"
			}
			if event == "pull_request" && (p.Action == "opened" || p.Action == "reopened") {
				return nil
			}
			// A merged predecessor is NOT member removal: GitHub can retain a
			// documented prefix. Only an unmerged close invalidates it here.
			if pr.GetMerged() {
				receipt := integration.IntegrationReceipt{Repo: repo, PR: pr.GetNumber(), Head: pr.GetHead().GetSHA(), MergeSHA: pr.GetMergeCommitSHA(),
					Base: pr.GetBase().GetRef(), MergedAt: pr.GetMergedAt().Time, Source: "github-signed-merged-webhook-v1"}
				return store.Append(integration.Event{Version: integration.SchemaVersion, ID: "github/" + delivery, At: now, Integrated: &receipt})
			}
			return store.InvalidateMember(repo, pr.GetNumber(), head, reason, delivery, now)
		}
	}
	if event != "workflow_job" {
		return nil
	}
	j := p.WorkflowJob
	attempt := j.RunAttempt
	// Older GitHub payloads omit run_attempt. Do not silently call it attempt 1:
	// the bounded reconciliation resolves the actual attempt.
	if attempt < 1 {
		return fmt.Errorf("workflow job attempt unavailable; reconciliation required")
	}
	job := integration.Job{Repo: repo, Base: integrationBranchBase(j.HeadBranch, cfg.Scopes, repo),
		ID: j.ID, RunID: j.RunID, Attempt: attempt, Head: j.HeadSHA, Workflow: j.WorkflowName, RunnerID: j.RunnerID,
		Status: j.Status, Conclusion: j.Conclusion, StartedAt: j.StartedAt, CompletedAt: j.CompletedAt, ObservedAt: now}
	return store.Append(integration.Event{Version: integration.SchemaVersion, ID: "github/" + delivery, At: now, Job: &job})
}

func (s *Server) handleIntegrationReconcile(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if err := s.ReconcileIntegration(r.Context()); err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, map[string]string{"status": "observed; completeness is recorded per scope"})
}

func (s *Server) observeIntegrationRun(run *gh.WorkflowRun, repo, delivery string, now time.Time) error {
	if run.GetEvent() != "pull_request" && run.GetEvent() != "merge_group" {
		return nil
	}
	cfg := s.integrationConfig()
	var pr *gh.PullRequest
	base, engine := integrationBranchBase(run.GetHeadBranch(), cfg.Scopes, repo), "native"
	if run.GetEvent() == "pull_request" {
		if len(run.PullRequests) != 1 || run.PullRequests[0] == nil {
			return nil
		}
		pr, engine = run.PullRequests[0], "direct"
		base = pr.GetBase().GetRef()
	}
	if !cfg.Allows(repo, base) {
		return nil
	}
	store, err := s.integrationStore()
	if err != nil {
		return err
	}
	subject := integration.Subject{Version: integration.SchemaVersion, Repo: repo, Base: base, Engine: engine,
		Authority: fmt.Sprintf("workflow-run/%d", run.GetID()),
		Policy:    "unverified-policy", Producer: "github-actions-observation", Workflow: run.GetPath(), WorkflowSourceRepo: run.GetHeadRepository().GetFullName(), Attempt: run.GetRunAttempt(), Generation: 1}
	if pr != nil {
		if pr.GetNumber() > 0 {
			subject.Authority = fmt.Sprintf("pull/%d", pr.GetNumber())
		}
		subject.BaseSHA = pr.GetBase().GetSHA()
		subject.Members = []integration.Member{{PR: pr.GetNumber(), Head: pr.GetHead().GetSHA(), Entry: fmt.Sprintf("pull/%d", pr.GetNumber()), Position: 1}}
	}
	if run.GetID() > 0 {
		subject.Generation = uint64(run.GetID())
	}
	evidence := integration.Evidence{Subject: subject, State: integration.Unknown, ObservedAt: now, Source: "github-workflow-run-v1", RunID: run.GetID(), SuiteID: run.GetCheckSuiteID(),
		Reason: "author workflow observed; actual checkout commit/tree and combined-base policy evidence unavailable"}
	if !store.HasEvent("github/" + delivery) {
		if err := store.Append(integration.Event{Version: integration.SchemaVersion, ID: "github/" + delivery, At: now, Evidence: &evidence}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) observeIntegrationPolicyRun(parent context.Context, run *gh.WorkflowRun, repo, delivery string, now time.Time) error {
	cfg := s.integrationConfig()
	if run == nil || run.GetEvent() != "merge_group" {
		return nil
	}
	if run.GetID() < 1 || run.GetRunAttempt() < 1 {
		return fmt.Errorf("native workflow generation unavailable")
	}
	store, err := s.integrationStore()
	if err != nil {
		return err
	}
	if s.deps == nil || s.deps.GHClient == nil {
		return fmt.Errorf("policy observation client unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, integrationReconcileTimeout)
	defer cancel()
	for _, policy := range cfg.Policies {
		if !strings.EqualFold(policy.Repo, repo) || policy.Engine != "native" || strings.Split(run.GetPath(), "@")[0] != policy.Workflow ||
			integrationBranchBase(run.GetHeadBranch(), cfg.Scopes, repo) != policy.Base {
			continue
		}
		eventID := "policy/" + delivery + "/" + policy.Workflow
		if store.HasEvent(eventID) {
			continue
		}
		var subject *integration.Subject
		for _, evidence := range store.ForTestedHead(repo, run.GetHeadSHA()) {
			candidate := evidence.Subject
			if candidate.Repo == repo && candidate.Base == policy.Base && candidate.Engine == "native" &&
				candidate.TestedSHA == run.GetHeadSHA() && evidence.State != integration.Superseded && len(candidate.Members) > 0 {
				if subject != nil && (subject.Authority != candidate.Authority || subject.BaseSHA != candidate.BaseSHA) {
					return fmt.Errorf("ambiguous native tested subject")
				}
				subject = &candidate
			}
		}
		if subject == nil {
			authority := "refs/heads/" + strings.TrimPrefix(run.GetHeadBranch(), "refs/heads/")
			group, groupErr := s.deps.GHClient.ReadNativeIntegrationGroup(ctx, repo, policy.Base, "", authority, run.GetHeadSHA())
			if groupErr != nil {
				return groupErr
			}
			recovered := integration.Subject{Version: integration.SchemaVersion, Repo: repo, Base: policy.Base, Engine: "native", Authority: authority,
				BaseSHA: group.BaseSHA, TestedSHA: group.HeadSHA, TestedTree: group.Tree, Policy: "unverified-policy", Producer: "github-queue-read", Workflow: "merge_group", Attempt: 1, Generation: 1}
			for _, member := range group.Members {
				recovered.Members = append(recovered.Members, integration.Member{PR: member.PR, Head: member.Head, Entry: member.Entry, Position: member.Position, BaseCommit: member.BaseCommit, HeadCommit: member.HeadCommit})
			}
			subject = &recovered
		}
		group, groupErr := s.deps.GHClient.ReadNativeIntegrationGroup(ctx, repo, policy.Base, "", subject.Authority, subject.TestedSHA)
		if groupErr != nil {
			return groupErr
		}
		if !integrationNativeMembershipCompatible(*subject, group) {
			return fmt.Errorf("native tested membership/tree changed")
		}
		proof, proofErr := s.deps.GHClient.ReadIntegrationWorkflowProof(ctx, policy, run)
		subject.Policy, subject.Producer, subject.Workflow = policy.Fingerprint(), fmt.Sprintf("github-app/%d", policy.AppID), fmt.Sprintf("%s@%d", policy.Workflow, run.GetWorkflowID())
		subject.WorkflowSourceRepo = run.GetHeadRepository().GetFullName()
		subject.Attempt, subject.Generation = run.GetRunAttempt(), uint64(run.GetID())
		observedAt := run.GetUpdatedAt().Time
		if observedAt.IsZero() {
			observedAt = now
		}
		observation := integration.Evidence{Subject: *subject, State: integration.Unknown, Source: "github-actions-bound-v1", ObservedAt: observedAt,
			RunID: proof.RunID, SuiteID: proof.SuiteID, CheckIDs: proof.CheckIDs, Reason: proof.Reason}
		if proofErr != nil {
			observation.Reason = proofErr.Error()
		} else if proof.Passed {
			observation.State = integration.Passed
		} else if proof.Rejected {
			observation.State = integration.Rejected
		} else {
			observation.State = integration.Running
		}
		if err := store.Append(integration.Event{Version: integration.SchemaVersion, ID: eventID, At: now, Evidence: &observation}); err != nil {
			return err
		}
		if proofErr != nil {
			return proofErr
		}
	}
	return nil
}

// ReconcileIntegration repairs missed job events with one bounded batch per
// interval, not a per-run poller. Overflow and ambiguous base attribution stay
// incomplete. It never approves/cancels/reruns jobs or modifies native entries.
func (s *Server) ReconcileIntegration(ctx context.Context) error {
	if s == nil {
		return nil
	}
	cfg := s.integrationConfig()
	if cfg.Mode == config.IntegrationOff {
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	s.integration.mu.Lock()
	now := time.Now()
	if now.Sub(s.integration.lastReconcile) < integrationReconcileInterval {
		s.integration.mu.Unlock()
		return nil
	}
	s.integration.lastReconcile = now
	start := s.integration.reconcileScopeOffset % len(cfg.Scopes)
	s.integration.reconcileScopeOffset = (start + 1) % len(cfg.Scopes)
	s.integration.mu.Unlock()
	if s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		return fmt.Errorf("GitHub workflow observations unavailable")
	}
	store, err := s.integrationStore()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, integrationReconcileTimeout)
	defer cancel()
	client := s.deps.GHClient.GoGitHub()
	jobReads := 0
	policyReads := 0
	scopes := append(append([]config.IntegrationScope(nil), cfg.Scopes[start:]...), cfg.Scopes[:start]...)
	for _, scope := range scopes {
		parts := strings.SplitN(scope.Repo, "/", 2)
		from := now.Add(-cfg.Window)
		c := integration.Coverage{Repo: scope.Repo, Base: scope.Base, From: from, Through: now, Source: "github-actions-rest-v1", Complete: true}
		// Creation time is not execution time: older completed runs can overlap
		// this window. Inspect the bounded unfiltered cohort, or remain unknown.
		runs, resp, readErr := client.Actions.ListRepositoryWorkflowRuns(ctx, parts[0], parts[1], &gh.ListWorkflowRunsOptions{
			ListOptions: gh.ListOptions{PerPage: integrationReconcilePageSize}})
		if readErr != nil || runs == nil {
			c.Complete, c.Reason = false, "workflow listing unavailable"
		} else {
			if resp != nil && resp.NextPage != 0 {
				c.Complete, c.Reason = false, "bounded reconciliation overflow"
			}
			observedRuns := 0
			for _, run := range runs.WorkflowRuns {
				if run == nil {
					c.Complete = false
					continue
				}
				if run.GetStatus() == "completed" && !run.GetUpdatedAt().Time.IsZero() && run.GetUpdatedAt().Time.Before(from) {
					continue
				}
				if observedRuns >= integrationReconcileMaxRuns || jobReads >= integrationReconcileMaxJobReads {
					c.Complete, c.Reason = false, "global bounded job reconciliation budget exhausted"
					continue
				}
				base := integrationBranchBase(run.GetHeadBranch(), cfg.Scopes, scope.Repo)
				for _, pr := range run.PullRequests {
					if pr != nil && pr.GetBase().GetRef() == scope.Base {
						base = scope.Base
					}
				}
				if base == "" {
					c.Complete, c.Reason = false, "workflow base attribution unknown"
					continue
				}
				if base != scope.Base {
					continue
				}
				observedRuns++
				delivery := fmt.Sprintf("reconcile-run/%s/%d/%d/%d", scope.Repo, run.GetID(), run.GetRunAttempt(), run.GetUpdatedAt().UnixNano())
				if err := s.observeIntegrationRun(run, scope.Repo, delivery, now); err != nil {
					return err
				}
				for _, policy := range cfg.Policies {
					engine := "direct"
					if run.GetEvent() == "merge_group" {
						engine = "native"
					}
					if !strings.EqualFold(policy.Repo, scope.Repo) || policy.Base != scope.Base || policy.Engine != engine || strings.Split(run.GetPath(), "@")[0] != policy.Workflow {
						continue
					}
					if policyReads >= integrationReconcileMaxPolicyReads {
						s.webhookLogger().Warn("integration policy reconciliation budget exhausted; missing receipts remain unknown", "repo", scope.Repo, "run", run.GetID())
						break
					}
					policyReads++
					var policyErr error
					if engine == "native" {
						policyErr = s.observeIntegrationPolicyRun(ctx, run, scope.Repo, delivery, now)
					} else {
						policyErr = s.observeIntegrationDirectPolicyRun(ctx, run, scope.Repo, delivery, now)
					}
					if policyErr != nil {
						s.webhookLogger().Warn("integration policy reconciliation unavailable", "repo", scope.Repo, "run", run.GetID(), "error", policyErr)
					}
				}
				attempts := run.GetRunAttempt()
				if attempts < 1 {
					c.Complete, c.Reason = false, "run attempt unknown"
					continue
				}
				for attempt := 1; attempt <= attempts; attempt++ {
					if attempt > cfg.MaxAttempts || jobReads >= integrationReconcileMaxJobReads {
						c.Complete, c.Reason = false, "attempt cohort exceeds bounded reconciliation"
						break
					}
					jobReads++
					request, requestErr := client.NewRequest("GET", fmt.Sprintf("repos/%s/%s/actions/runs/%d/attempts/%d/jobs?per_page=%d", parts[0], parts[1], run.GetID(), attempt, integrationReconcilePageSize), nil)
					if requestErr != nil {
						return requestErr
					}
					jobs := new(gh.Jobs)
					jobsResp, jobsErr := client.Do(ctx, request, jobs)
					if jobsErr != nil || jobs == nil {
						c.Complete, c.Reason = false, "workflow jobs unavailable"
						continue
					}
					if jobsResp != nil && jobsResp.NextPage != 0 {
						c.Complete, c.Reason = false, "job fanout exceeds reconciliation bound"
					}
					for _, j := range jobs.Jobs {
						if j == nil {
							c.Complete = false
							continue
						}
						job := integration.Job{Repo: scope.Repo, Base: base, ID: j.GetID(), RunID: run.GetID(), Attempt: attempt,
							Head: run.GetHeadSHA(), Workflow: run.GetPath(), RunnerID: j.GetRunnerID(), Status: j.GetStatus(), Conclusion: j.GetConclusion(),
							StartedAt: j.GetStartedAt().Time, CompletedAt: j.GetCompletedAt().Time, ObservedAt: now}
						e := integration.Event{Version: integration.SchemaVersion, ID: fmt.Sprintf("reconcile/%s/%d", job.Key(), now.UnixNano()), At: now, Job: &job}
						if err := store.Append(e); err != nil {
							return err
						}
					}
				}
			}
		}
		if readErr == nil {
			if err := s.reconcileIntegrationMerges(ctx, scope, from, now, &c); err != nil {
				return err
			}
		}
		if err := store.Append(integration.Event{Version: integration.SchemaVersion, ID: fmt.Sprintf("coverage/%s/%d", scope.Repo+"#"+scope.Base, now.UnixNano()), At: now, Coverage: &c}); err != nil {
			return err
		}
		if readErr != nil {
			return fmt.Errorf("integration workflow listing: %w", readErr)
		}
	}
	return nil
}
