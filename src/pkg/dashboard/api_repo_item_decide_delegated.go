package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// "Let hive decide" (Decide… › Let hive decide) on an activity-monitor row
// parked on a person. Where the sibling verbs in api_repo_item_decision.go
// record what the operator chose, this one has the hive choose as the
// owner's delegate under delegatedecision.PolicyText and records the result
// exactly as a human decision would be recorded — the `/hive decision <text>`
// comment, the parking labels lifted (Client.RecordOperatorIssueDecision) or
// the queue approval on the PR head — stamped with delegatedecision.Marker so
// the audit log can count delegated decisions apart from human ones.
//
// POST …/items/{number}/decide-delegated starts a job and answers 202 with
// its id; GET the same path reads the job. The job gathers the item from
// GitHub, runs delegatedecision.Run (policy + rails) — or, when
// decisions.delegate.llm.enabled is on and the rules fall through to the
// reversible default, the owner-delegate decider turn
// (api_repo_item_decide_delegated_llm.go) — and executes the allowed actions
// through the existing GitHub helpers. Nothing here merges, lifts a
// hold or clears the sentinel label; the auto-merge engine, a person and the
// sentinel sweep respectively own those.
//
// Authorisation is the repo-hold bar (canToggleRepoHold) plus a signed-in
// user, like the sibling decision endpoint; the setting
// decisions.delegate.enabled (default on) and the per-repository override
// decisions.delegate.repos.<key>.enabled gate it.
const (
	auditRepoItemDecisionDelegated = "repo_item_decision_delegated"

	delegatedDecisionMaxBody = 16 << 10

	delegatedDecisionJobTTL      = 30 * time.Minute
	delegatedDecisionGatherLimit = 100

	delegatedJobStatusRunning = "running"
	delegatedJobStatusDone    = "done"
	delegatedJobStatusFailed  = "failed"

	delegatedLabelColorNeedsOwner = "b60205"
	delegatedLabelColorFlaky      = "fbca04"
	delegatedLabelColorTriage     = "0e8a16"
)

// delegatedDecisionRunAsync runs jobs on a goroutine; tests set it false to
// execute inline.
var delegatedDecisionRunAsync = true

type delegatedDecisionRequest struct {
	// Type is "issue" or "pr"; unknown reads as issue.
	Type string `json:"type"`
	// Reason / Caption / SentinelReason are the tracker row's own words for
	// the park, so the decider sees what the operator saw.
	Reason         string `json:"reason"`
	Caption        string `json:"caption"`
	SentinelReason string `json:"sentinel_reason"`
}

// delegatedDecisionJob is the wire shape of one job.
type delegatedDecisionJob struct {
	ID        string                     `json:"id"`
	Repo      string                     `json:"repo"`
	Number    int                        `json:"number"`
	Type      string                     `json:"type"`
	Status    string                     `json:"status"`
	Requester string                     `json:"requester"`
	StartedAt time.Time                  `json:"started_at"`
	EndedAt   *time.Time                 `json:"ended_at,omitempty"`
	Decision  *delegatedecision.Decision `json:"decision,omitempty"`
	Notes     []string                   `json:"notes,omitempty"`
	Error     string                     `json:"error,omitempty"`
	Marker    string                     `json:"marker,omitempty"`
}

type delegatedDecisionJobs struct {
	mu   sync.Mutex
	jobs map[string]*delegatedDecisionJob
}

func delegatedJobKey(repo string, number int) string {
	return strings.ToLower(repo) + "#" + strconv.Itoa(number)
}

func (j *delegatedDecisionJobs) get(key string) (delegatedDecisionJob, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.jobs[key]
	if !ok {
		return delegatedDecisionJob{}, false
	}
	return *job, true
}

// start registers a job unless one is already running for the key.
func (j *delegatedDecisionJobs) start(key string, job *delegatedDecisionJob, now time.Time) (delegatedDecisionJob, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.jobs == nil {
		j.jobs = make(map[string]*delegatedDecisionJob)
	}
	for k, old := range j.jobs {
		if old.EndedAt != nil && now.Sub(*old.EndedAt) > delegatedDecisionJobTTL {
			delete(j.jobs, k)
		}
	}
	if old, ok := j.jobs[key]; ok && old.Status == delegatedJobStatusRunning {
		return *old, false
	}
	j.jobs[key] = job
	return *job, true
}

func (j *delegatedDecisionJobs) finish(key string, fn func(*delegatedDecisionJob)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job, ok := j.jobs[key]; ok {
		fn(job)
		now := time.Now()
		job.EndedAt = &now
	}
}

func (s *Server) delegateDecisionEnabled(repoFull string) bool {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return config.DelegateDecisionConfig{}.IsEnabledFor(repoFull)
	}
	return s.deps.Config.Decisions.Delegate.IsEnabledFor(repoFull)
}

func (s *Server) delegateHiveID() string {
	if s != nil && s.deps != nil && s.deps.Config != nil && strings.TrimSpace(s.deps.Config.HiveID) != "" {
		return strings.TrimSpace(s.deps.Config.HiveID)
	}
	return "hive"
}

func (s *Server) handleRepoItemDecideDelegatedStatus(w http.ResponseWriter, r *http.Request) {
	owner, repoName, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	job, found := s.delegatedJobs.get(delegatedJobKey(owner+"/"+repoName, number))
	if !found {
		jsonError(w, "no delegated decision for this item", http.StatusNotFound)
		return
	}
	jsonResponse(w, job)
}

func (s *Server) handleRepoItemDecideDelegated(w http.ResponseWriter, r *http.Request) {
	owner, repoName, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	repoFull := owner + "/" + repoName
	if !s.delegateDecisionEnabled(repoFull) {
		jsonError(w, "delegated decisions are disabled (decisions.delegate.enabled, or decisions.delegate.repos.<repo>.enabled for this repository)", http.StatusForbidden)
		return
	}
	allowed, status, msg := s.canToggleRepoHold(r, owner, repoName)
	if !allowed {
		jsonError(w, msg, status)
		return
	}
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if user == "" {
		jsonError(w, "GitHub user session required to delegate a decision", http.StatusUnauthorized)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	var body delegatedDecisionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, delegatedDecisionMaxBody)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	itemType := strings.ToLower(strings.TrimSpace(body.Type))
	if itemType != "pr" {
		itemType = "issue"
	}
	now := time.Now()
	key := delegatedJobKey(repoFull, number)
	job := &delegatedDecisionJob{
		ID: key + "@" + strconv.FormatInt(now.UnixNano(), 36), Repo: repoFull, Number: number, Type: itemType,
		Status: delegatedJobStatusRunning, Requester: user, StartedAt: now, Marker: delegatedecision.Marker(s.delegateHiveID()),
	}
	snapshot, started := s.delegatedJobs.start(key, job, now)
	if !started {
		w.WriteHeader(http.StatusConflict)
		jsonResponse(w, snapshot)
		return
	}
	mergerRole := config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleMerger) &&
		(!isOwnerRole(r.Header.Get("X-Hive-Role")) || r.Header.Get(ownerRoleVerifiedHeader) == "true")
	run := func() {
		ctx := context.Background()
		if s.deps.Ctx != nil {
			ctx = s.deps.Ctx
		}
		s.runDelegatedDecision(ctx, key, repoFull, number, itemType, user, mergerRole, body)
	}
	if delegatedDecisionRunAsync {
		go run()
		w.WriteHeader(http.StatusAccepted)
		jsonResponse(w, snapshot)
		return
	}
	run()
	done, _ := s.delegatedJobs.get(key)
	jsonResponse(w, done)
}

// runDelegatedDecision gathers, decides, executes and records one job.
func (s *Server) runDelegatedDecision(ctx context.Context, key, repoFull string, number int, itemType, user string, mergerRole bool, req delegatedDecisionRequest) {
	in, err := s.gatherDelegatedInput(ctx, repoFull, number, itemType, req)
	if err != nil {
		s.delegatedJobs.finish(key, func(j *delegatedDecisionJob) {
			j.Status = delegatedJobStatusFailed
			j.Error = err.Error()
		})
		s.audit.LogRecord(user, auditRepoItemDecisionDelegated+"_failed", auditDetail("repo", repoFull, "number", strconv.Itoa(number), "error", err.Error()), "", repoFull, number)
		return
	}
	d, deciderNotes := s.decideDelegated(ctx, in)
	notes, execErr := s.executeDelegatedDecision(ctx, in, d, user, mergerRole)
	notes = append(deciderNotes, notes...)
	s.delegatedJobs.finish(key, func(j *delegatedDecisionJob) {
		j.Decision = &d
		j.Notes = notes
		if execErr != nil {
			j.Status = delegatedJobStatusFailed
			j.Error = execErr.Error()
			return
		}
		j.Status = delegatedJobStatusDone
	})
	kinds := make([]string, 0, len(d.Actions))
	for _, a := range d.Actions {
		kinds = append(kinds, string(a.Kind))
	}
	action := auditRepoItemDecisionDelegated
	if execErr != nil {
		action += "_failed"
	}
	detail := auditDetail("repo", repoFull, "number", strconv.Itoa(number), "type", itemType,
		"verdict", string(d.Verdict), "rule", d.Rule, "actions", strings.Join(kinds, ","),
		"keeps_park", strconv.FormatBool(d.KeepsPark), "policy", delegatedecision.PolicyName, "decision", d.Text)
	if execErr != nil {
		detail = auditDetail("repo", repoFull, "number", strconv.Itoa(number), "verdict", string(d.Verdict), "rule", d.Rule, "error", execErr.Error())
	}
	s.audit.LogRecord(user, action, detail, "", repoFull, number)
}

// gatherDelegatedInput reads what the decider needs from GitHub.
func (s *Server) gatherDelegatedInput(ctx context.Context, repoFull string, number int, itemType string, req delegatedDecisionRequest) (delegatedecision.Input, error) {
	client := s.deps.GHClient
	api := client.GoGitHub()
	owner, repoName, _ := strings.Cut(repoFull, "/")
	in := delegatedecision.Input{Repo: repoFull, Number: number, Kind: delegatedecision.KindIssue, ContributorOnline: true}
	reason := strings.TrimSpace(req.Reason)
	if reason != "" {
		in.Reasons = append(in.Reasons, reason)
	}
	if c := strings.TrimSpace(req.Caption); c != "" && c != reason {
		in.Reasons = append(in.Reasons, c)
	}
	issue, _, err := api.Issues.Get(ctx, owner, repoName, number)
	if err != nil {
		return in, fmt.Errorf("reading %s#%d: %w", repoFull, number, err)
	}
	if issue.GetState() != "" && !strings.EqualFold(issue.GetState(), "open") {
		return in, fmt.Errorf("%s#%d is not open", repoFull, number)
	}
	in.Title = issue.GetTitle()
	in.Body = issue.GetBody()
	for _, l := range issue.Labels {
		in.Labels = append(in.Labels, l.GetName())
	}
	author := strings.TrimSpace(issue.GetUser().GetLogin())
	in.HiveAuthored = client.IsHiveAuthorLogin(author)
	in.TrustedAuthor = in.HiveAuthored
	if issue.IsPullRequest() || itemType == "pr" {
		in.Kind = delegatedecision.KindPR
		pr, _, err := api.PullRequests.Get(ctx, owner, repoName, number)
		if err != nil {
			return in, fmt.Errorf("reading PR %s#%d: %w", repoFull, number, err)
		}
		state := strings.ToLower(pr.GetMergeableState())
		in.Conflicting = state == "dirty" || (pr.Mergeable != nil && !pr.GetMergeable())
		in.CIGreen = !in.Conflicting && state != "unstable"
		if files, _, err := api.PullRequests.ListFiles(ctx, owner, repoName, number, &gh.ListOptions{PerPage: delegatedDecisionGatherLimit}); err == nil && len(files) > 0 {
			in.LowRiskPaths = true
			for _, f := range files {
				if !delegatedLowRiskPath(f.GetFilename()) {
					in.LowRiskPaths = false
					break
				}
			}
		}
		if !in.TrustedAuthor && author != "" {
			if perm, _, err := api.Repositories.GetPermissionLevel(ctx, owner, repoName, author); err == nil {
				switch strings.ToLower(perm.GetPermission()) {
				case "admin", "write", "maintain":
					in.TrustedAuthor = true
				}
			}
		}
	}
	sentinel := s.sentinelLabel()
	for _, l := range in.Labels {
		if strings.EqualFold(strings.TrimSpace(l), sentinel) {
			in.SentinelAlert = true
		}
	}
	in.SentinelReason = strings.TrimSpace(req.SentinelReason)
	lowerReason := strings.ToLower(reason)
	in.AdoptionStalled = strings.HasPrefix(lowerReason, stagelabels.LaneReasonAdoptionStalledPrefix)
	if in.AdoptionStalled || strings.Contains(lowerReason, "offline") {
		in.ContributorOnline = false
	}
	in.ReporterTrustHold = strings.Contains(lowerReason, "reporter-trust") || strings.Contains(lowerReason, "reporter trust")
	in.DCOMissing = strings.Contains(lowerReason, "dco")
	return in, nil
}

// delegatedLowRiskPath reports a file whose change the policy calls low-risk:
// CI / workflow config, docs and text, changelog fragments.
func delegatedLowRiskPath(path string) bool {
	p := strings.ToLower(strings.TrimSpace(path))
	switch {
	case strings.HasPrefix(p, ".github/"), strings.HasPrefix(p, "docs/"), strings.Contains(p, "/docs/"),
		strings.HasPrefix(p, "changelog.d/"), strings.HasSuffix(p, ".md"), strings.HasSuffix(p, ".txt"),
		strings.HasSuffix(p, ".yml") && strings.Contains(p, "ci"), strings.HasSuffix(p, ".yaml") && strings.Contains(p, "ci"):
		return true
	}
	return false
}

// executeDelegatedDecision runs the allowed actions through the existing
// GitHub helpers. It stops at the first failure and reports what landed.
func (s *Server) executeDelegatedDecision(ctx context.Context, in delegatedecision.Input, d delegatedecision.Decision, user string, mergerRole bool) ([]string, error) {
	client := s.deps.GHClient
	repoFull, number := in.Repo, in.Number
	hiveID := s.delegateHiveID()
	marker := delegatedecision.Marker(hiveID)
	comment := delegatedecision.CommentBody(d, hiveID, user)
	var notes []string
	if d.Verdict == delegatedecision.VerdictRefused {
		return append(notes, "refused: "+d.Rationale), nil
	}
	for _, a := range d.Actions {
		switch a.Kind {
		case delegatedecision.ActionCommentDecision:
			if err := client.CreateIssueComment(ctx, repoFull, number, comment); err != nil {
				return notes, fmt.Errorf("posting decision: %w", err)
			}
			notes = append(notes, "posted `/hive decision` with the delegated marker")
			if in.Kind == delegatedecision.KindIssue {
				_, err := client.RecordOperatorIssueDecision(ctx, repoFull, number, user, d.Text)
				switch {
				case errors.Is(err, ghpkg.ErrIssueNotParked):
					notes = append(notes, "issue carried no parking label; comment only")
				case err != nil:
					return notes, fmt.Errorf("un-parking issue: %w", err)
				default:
					notes = append(notes, "parking labels lifted, `"+ghpkg.HumanAckLabel+"` applied")
				}
			} else if d.Verdict == delegatedecision.VerdictDirection && in.HiveAuthored {
				if _, err := client.RecordOperatorPullRequestDecision(ctx, repoFull, number, user, d.Text, repoItemDecisionChangeRequests()); err != nil && !errors.Is(err, ghpkg.ErrPullRequestNotHiveAuthored) {
					return notes, fmt.Errorf("recording PR direction: %w", err)
				} else if err == nil {
					notes = append(notes, "direction recorded for the author agent")
				}
			}
		case delegatedecision.ActionApproveReview:
			if in.Kind != delegatedecision.KindPR {
				continue
			}
			if in.SentinelAlert {
				notes = append(notes, "queue approval deferred: the sentinel label is cleared only by a person; once it is removed the approval follows")
				continue
			}
			if mergerRole {
				if err := client.QueuePRAutoMergeWithNote(ctx, repoFull, number, user, marker); err != nil {
					return notes, fmt.Errorf("approving PR head: %w", err)
				}
				notes = append(notes, "Approve review submitted on the head and queued for auto-merge (the engine merges per repo settings)")
			} else {
				if _, err := client.RecordOperatorPullRequestApproval(ctx, repoFull, number, user); err != nil {
					return notes, fmt.Errorf("approving PR: %w", err)
				}
				notes = append(notes, "`/lgtm` + `/approve` recorded; the merge gate takes it from here")
			}
		case delegatedecision.ActionLabelsAdd:
			for _, l := range a.Labels {
				color, desc := delegatedLabelMeta(l)
				if err := client.EnsureIssueLabel(ctx, repoFull, l, color, desc); err != nil {
					return notes, fmt.Errorf("ensuring label %s: %w", l, err)
				}
			}
			if err := client.AddLabels(ctx, repoFull, number, a.Labels); err != nil {
				return notes, fmt.Errorf("adding labels: %w", err)
			}
			notes = append(notes, "labels added: "+strings.Join(a.Labels, ", "))
		case delegatedecision.ActionLabelsRemove:
			for _, l := range a.Labels {
				if err := client.RemoveLabel(ctx, repoFull, number, l); err != nil {
					return notes, fmt.Errorf("removing label %s: %w", l, err)
				}
			}
			notes = append(notes, "labels removed: "+strings.Join(a.Labels, ", "))
		case delegatedecision.ActionCloseDuplicate:
			body := fmt.Sprintf("%s\n\nRationale: %s.\n\n_Closed by the hive as the owner's delegate (rule `%s`)._\n%s", a.Text, d.Rationale, d.Rule, marker)
			if err := client.CreateIssueComment(ctx, repoFull, number, body); err != nil {
				return notes, fmt.Errorf("posting duplicate note: %w", err)
			}
			if in.Kind == delegatedecision.KindPR {
				if err := client.CloseSupersededPR(ctx, repoFull, number); err != nil {
					return notes, fmt.Errorf("closing superseded PR: %w", err)
				}
			} else if err := client.CloseIssue(ctx, repoFull, number, ghpkg.IssueCloseOptions{
				OverrideReason:          "closed as duplicate of #" + strconv.Itoa(a.Target) + " by the hive delegate",
				SuppressOverrideComment: true,
				StateReason:             issueStateReasonNotPlanned,
			}); err != nil {
				return notes, fmt.Errorf("closing duplicate: %w", err)
			}
			notes = append(notes, fmt.Sprintf("closed in favour of #%d", a.Target))
		case delegatedecision.ActionLink:
			body := fmt.Sprintf("Cross-link: %s (#%d).\n%s", a.Text, a.Target, marker)
			if err := client.CreateIssueComment(ctx, repoFull, number, body); err != nil {
				return notes, fmt.Errorf("cross-linking: %w", err)
			}
			notes = append(notes, fmt.Sprintf("cross-linked #%d", a.Target))
		case delegatedecision.ActionAdoptSupersede:
			notes = append(notes, "adopt-in-supersede-mode recorded as the decision; the adoption engine picks it up on its next pass")
		case delegatedecision.ActionRecommendOnly:
			if err := client.CreateIssueComment(ctx, repoFull, number, comment); err != nil {
				return notes, fmt.Errorf("posting recommendation: %w", err)
			}
			notes = append(notes, "recommendation posted; `"+delegatedecision.LabelNeedsDecision+"` kept for the owner")
		}
	}
	for _, r := range d.Refusals {
		notes = append(notes, "refused: "+r)
	}
	return notes, nil
}

func delegatedLabelMeta(label string) (color, description string) {
	switch strings.ToLower(label) {
	case delegatedecision.LabelNeedsOwner:
		return delegatedLabelColorNeedsOwner, "Only the hive owner can act on this (secrets, tokens, org permissions)"
	case delegatedecision.LabelFlakyTest:
		return delegatedLabelColorFlaky, "Quarantined flaky test: retried, deflake task filed"
	case delegatedecision.LabelTriageAccepted:
		return delegatedLabelColorTriage, "Triage accepted by a maintainer"
	}
	return "", ""
}
