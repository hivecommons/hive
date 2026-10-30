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
	integrationReconcileInterval    = 3 * time.Minute
	integrationReconcileTimeout     = 45 * time.Second
	integrationReconcileMaxRuns     = 8
	integrationReconcilePageSize    = 100
	integrationReconcileMaxJobReads = 24
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
	cfg := s.integrationConfig()
	if cfg.Mode == config.IntegrationOff {
		return nil
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
	if store.HasEvent("github/" + delivery) {
		return nil
	}
	if event == "workflow_run" && p.WorkflowRun != nil {
		if p.WorkflowRun.GetEvent() == "merge_group" {
			return s.observeIntegrationPolicyRun(context.Background(), p.WorkflowRun, repo, delivery, now)
		}
		return s.observeIntegrationRun(p.WorkflowRun, repo, delivery, now)
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
			Authority: g.HeadRef, BaseSHA: g.BaseSHA, TestedSHA: g.HeadSHA, TestedTree: group.Tree,
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
				return nil
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
	if run.GetEvent() != "pull_request" || len(run.PullRequests) != 1 {
		return nil
	}
	cfg := s.integrationConfig()
	pr := run.PullRequests[0]
	if pr == nil || !cfg.Allows(repo, pr.GetBase().GetRef()) {
		return nil
	}
	store, err := s.integrationStore()
	if err != nil {
		return err
	}
	subject := integration.Subject{Version: integration.SchemaVersion, Repo: repo, Base: pr.GetBase().GetRef(), Engine: "direct",
		Authority: fmt.Sprintf("workflow-run/%d", run.GetID()), BaseSHA: pr.GetBase().GetSHA(),
		Policy: "unverified-policy", Producer: "github-actions-observation", Workflow: run.GetPath(), Attempt: run.GetRunAttempt(), Generation: 1,
		Members: []integration.Member{{PR: pr.GetNumber(), Head: pr.GetHead().GetSHA(), Entry: fmt.Sprintf("pull/%d", pr.GetNumber()), Position: 1}}}
	evidence := integration.Evidence{Subject: subject, State: integration.Unknown, ObservedAt: now, Source: "github-workflow-run-v1", RunID: run.GetID(), SuiteID: run.GetCheckSuiteID(),
		Reason: "author workflow observed; actual checkout commit/tree and combined-base policy evidence unavailable"}
	return store.Append(integration.Event{Version: integration.SchemaVersion, ID: "github/" + delivery, At: now, Evidence: &evidence})
}

func (s *Server) observeIntegrationPolicyRun(parent context.Context, run *gh.WorkflowRun, repo, delivery string, now time.Time) error {
	cfg := s.integrationConfig()
	if run == nil || run.GetEvent() != "merge_group" {
		return nil
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
		if !strings.EqualFold(policy.Repo, repo) || policy.Engine != "native" || strings.Split(run.GetPath(), "@")[0] != policy.Workflow {
			continue
		}
		eventID := "policy/" + delivery + "/" + policy.Workflow
		if store.HasEvent(eventID) {
			continue
		}
		var subject *integration.Subject
		for _, evidence := range store.Snapshot(now).Evidence {
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
			return fmt.Errorf("native group membership not observed; reconcile group delivery first")
		}
		group, groupErr := s.deps.GHClient.ReadNativeIntegrationGroup(ctx, repo, policy.Base, subject.BaseSHA, subject.Authority, subject.TestedSHA)
		if groupErr != nil {
			return groupErr
		}
		if len(group.Members) != len(subject.Members) || group.Tree != subject.TestedTree {
			return fmt.Errorf("native tested membership/tree changed")
		}
		for index, m := range group.Members {
			old := subject.Members[index]
			if old.PR != m.PR || old.Head != m.Head || old.Entry != m.Entry || old.Position != m.Position {
				return fmt.Errorf("native exact member changed")
			}
		}
		proof, proofErr := s.deps.GHClient.ReadIntegrationWorkflowProof(ctx, policy, run)
		subject.Policy, subject.Producer, subject.Workflow = policy.Blob, fmt.Sprintf("github-app/%d", policy.AppID), policy.Workflow
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
	s.integration.mu.Lock()
	now := time.Now()
	if now.Sub(s.integration.lastReconcile) < integrationReconcileInterval {
		s.integration.mu.Unlock()
		return nil
	}
	s.integration.lastReconcile = now
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
	for _, scope := range cfg.Scopes {
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
		if err := store.Append(integration.Event{Version: integration.SchemaVersion, ID: fmt.Sprintf("coverage/%s/%d", scope.Repo+"#"+scope.Base, now.UnixNano()), At: now, Coverage: &c}); err != nil {
			return err
		}
		if readErr != nil {
			return fmt.Errorf("integration workflow listing: %w", readErr)
		}
	}
	return nil
}
