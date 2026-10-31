package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
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
	// PRs, on an issue, names the open pull requests nested under it as the
	// tracker row showed them. Two or more make the job decide each PR on
	// its own and report every outcome (delegatedDecisionItem) instead of
	// deciding the issue — an issue row with several PRs never silently
	// picks one (hivecommons/hive#15009). Empty: the dashboard's own
	// snapshot is consulted the same way.
	PRs []int `json:"prs,omitempty"`
	// Force re-runs the decision past the "already recorded" dedup (the
	// `decide:delegate:rerun` option on a stuck decision).
	Force bool `json:"force,omitempty"`
	// Chosen names the preview button that sent this request
	// (hivecommons/hive#15126); the ledger records it as the action's note.
	Chosen string `json:"chosen,omitempty"`
	// Head is the PR head the tracker row showed; it only keys the
	// preview's plan cache, so a push asks GitHub afresh.
	Head string `json:"head,omitempty"`
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
	// Result is the shared row-action outcome (what was decided, on which
	// item, what happens next), added for the dashboard's result toast.
	Result *RowActionResult `json:"result,omitempty"`
	// Items is set when an issue job decided each of its nested PRs
	// separately: one entry per PR, in number order, each with its own
	// decision, notes and result. Result then summarises them
	// (rowActionDecidedEach) and Decision is unset.
	Items []delegatedDecisionItem `json:"items,omitempty"`
}

// delegatedDecisionItem is one nested PR's outcome inside an issue job.
type delegatedDecisionItem struct {
	Number   int                        `json:"number"`
	Type     string                     `json:"type"`
	Status   string                     `json:"status"`
	Decision *delegatedecision.Decision `json:"decision,omitempty"`
	Notes    []string                   `json:"notes,omitempty"`
	Error    string                     `json:"error,omitempty"`
	Result   *RowActionResult           `json:"result,omitempty"`
}

// delegatedOutcome is what deciding one item produced, before it is
// written into a job or an item.
type delegatedOutcome struct {
	Decision *delegatedecision.Decision
	Notes    []string
	Result   *RowActionResult
	Err      error
}

type delegatedDecisionJobs struct {
	mu   sync.Mutex
	jobs map[string]*delegatedDecisionJob
	// plans is the preview's short-lived plan cache
	// (api_repo_item_decide_delegated_plan.go).
	plans delegatedPlanCache
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

// stageAdoptionEnabled mirrors the adoption engine's policy gate
// (governor.labels.stage + .adoption); unreadable config counts as on so
// the request is not refused on a dashboard that cannot see the policy.
func (s *Server) stageAdoptionEnabled() bool {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return true
	}
	stage := s.deps.Config.Governor.Labels.Stage
	return stage.IsEnabled() && stage.Adoption.IsEnabled()
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
	if started {
		// The decision is being recorded: a cached preview would hide it.
		s.delegatedJobs.plans.forget(repoFull, number)
	}
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
		// The response wrapper records the pending action after this handler
		// returns. Starting sooner lets a fast job settle a nonexistent row.
		start := func() { go run() }
		if recorder, ok := w.(*operatorActionWriter); ok {
			recorder.afterRecord = start
		} else {
			start()
		}
		w.WriteHeader(http.StatusAccepted)
		jsonResponse(w, snapshot)
		return
	}
	run()
	done, _ := s.delegatedJobs.get(key)
	jsonResponse(w, done)
}

// runDelegatedDecision gathers, decides, executes and records one job. An
// issue with two or more open nested PRs is decided per PR (see
// delegatedDecisionRequest.PRs); anything else is decided as one item.
func (s *Server) runDelegatedDecision(ctx context.Context, key, repoFull string, number int, itemType, user string, mergerRole bool, req delegatedDecisionRequest) {
	if itemType == "issue" {
		if prs := s.delegatedNestedOpenPRs(repoFull, number, req.PRs); len(prs) >= delegatedFanOutMinPRs {
			s.runDelegatedDecisionPerPR(ctx, key, repoFull, number, prs, user, mergerRole, req)
			return
		}
	}
	out := s.decideDelegatedOne(ctx, repoFull, number, itemType, user, mergerRole, req)
	s.delegatedJobs.finish(key, func(j *delegatedDecisionJob) {
		j.Decision = out.Decision
		j.Notes = out.Notes
		j.Result = out.Result
		if out.Err != nil {
			j.Status = delegatedJobStatusFailed
			j.Error = out.Err.Error()
			return
		}
		j.Status = delegatedJobStatusDone
	})
	s.settleDelegatedJob(key, repoFull, number)
}

// settleDelegatedJob records the finished job's outcome on the operator-
// action ledger so the row's decision badge can show what hive did next.
func (s *Server) settleDelegatedJob(key, repoFull string, number int) {
	if done, ok := s.delegatedJobs.get(key); ok {
		s.settleOperatorAction(repoFull, number, &done)
	}
}

// delegatedFanOutMinPRs is the number of open nested PRs from which an
// issue's delegated decision is made per PR rather than on the issue.
const delegatedFanOutMinPRs = 2

// runDelegatedDecisionPerPR decides each nested PR on its own — each under
// its own job key too, so a GET on the PR's path reads it and a concurrent
// click on that PR is answered as already running — and writes the per-PR
// outcomes and a summary into the issue's job.
func (s *Server) runDelegatedDecisionPerPR(ctx context.Context, key, repoFull string, number int, prs []int, user string, mergerRole bool, req delegatedDecisionRequest) {
	items := make([]delegatedDecisionItem, 0, len(prs))
	prReq := req
	prReq.PRs = nil
	for _, pr := range prs {
		item := delegatedDecisionItem{Number: pr, Type: "pr", Status: delegatedJobStatusDone}
		prKey := delegatedJobKey(repoFull, pr)
		now := time.Now()
		sub := &delegatedDecisionJob{
			ID: prKey + "@" + strconv.FormatInt(now.UnixNano(), 36), Repo: repoFull, Number: pr, Type: "pr",
			Status: delegatedJobStatusRunning, Requester: user, StartedAt: now, Marker: delegatedecision.Marker(s.delegateHiveID()),
		}
		if _, started := s.delegatedJobs.start(prKey, sub, now); !started {
			item.Status = delegatedJobStatusFailed
			item.Error = "a delegated decision is already running for this pull request"
			items = append(items, item)
			continue
		}
		out := s.decideDelegatedOne(ctx, repoFull, pr, "pr", user, mergerRole, prReq)
		item.Decision, item.Notes, item.Result = out.Decision, out.Notes, out.Result
		if out.Err != nil {
			item.Status = delegatedJobStatusFailed
			item.Error = out.Err.Error()
		}
		s.delegatedJobs.finish(prKey, func(j *delegatedDecisionJob) {
			j.Decision, j.Notes, j.Result, j.Status, j.Error = item.Decision, item.Notes, item.Result, item.Status, item.Error
		})
		s.settleDelegatedJob(prKey, repoFull, pr)
		items = append(items, item)
	}
	result := delegatedPerPRResult(repoFull, number, items)
	s.delegatedJobs.finish(key, func(j *delegatedDecisionJob) {
		j.Items = items
		j.Result = &result
		j.Status = delegatedJobStatusDone
		failed := 0
		for _, it := range items {
			if it.Error != "" {
				failed++
				if j.Error == "" {
					j.Error = fmt.Sprintf("#%d: %s", it.Number, it.Error)
				}
			}
			j.Notes = append(j.Notes, fmt.Sprintf("#%d: %s", it.Number, delegatedItemWord(it)))
		}
		if failed == len(items) {
			j.Status = delegatedJobStatusFailed
		} else {
			j.Error = ""
		}
	})
	s.settleDelegatedJob(key, repoFull, number)
	numbers := make([]string, 0, len(prs))
	for _, pr := range prs {
		numbers = append(numbers, strconv.Itoa(pr))
	}
	s.audit.LogRecord(user, auditRepoItemDecisionDelegated, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "type", "issue",
		"per_pr", strings.Join(numbers, ","), "policy", delegatedecision.PolicyName), "", repoFull, number)
}

// delegatedNestedOpenPRs is the open PRs nested under an issue: the
// dashboard's hint when it sent one, else the tracker's own pairing from
// the last enumeration (the same linked_prs the row drew). Sorted, deduped,
// never the issue itself.
func (s *Server) delegatedNestedOpenPRs(repoFull string, number int, hint []int) []int {
	if len(hint) == 0 && s != nil {
		s.statusMu.RLock()
		status := s.status
		s.statusMu.RUnlock()
		if status != nil {
			hint = delegatedNestedPRNumbers(s.buildStageTracker(status, s.stageStoreOrShared(), time.Now().UTC()).Items, repoFull, number)
		}
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(hint))
	for _, n := range hint {
		if n <= 0 || n == number || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// delegatedNestedPRNumbers reads an issue's linked_prs out of tracker items.
func delegatedNestedPRNumbers(items []StageTrackerItem, repoFull string, number int) []int {
	for _, it := range items {
		if it.Kind != "issue" || it.Number != number || !strings.EqualFold(it.Repo, repoFull) {
			continue
		}
		out := make([]int, 0, len(it.LinkedPRs))
		for _, lp := range it.LinkedPRs {
			out = append(out, lp.Number)
		}
		return out
	}
	return nil
}

// delegatedItemWord is one PR's outcome in a few words for the summary.
func delegatedItemWord(it delegatedDecisionItem) string {
	switch {
	case it.Error != "":
		return "failed — " + it.Error
	case it.Result != nil && it.Result.Action == rowActionNoop:
		return "no change (already decided)"
	case it.Result != nil:
		word := it.Result.Action
		if it.Result.Rule != "" {
			word += " (" + strings.ReplaceAll(it.Result.Rule, "-", " ") + ")"
		}
		return word
	}
	return "decided"
}

// delegatedPerPRResult summarises the per-PR outcomes for the issue row's
// toast: "#17 approved (small low risk); #20 recommended (sentinel
// untrusted)". Each PR's own result carries its outcome and next step.
func delegatedPerPRResult(repoFull string, number int, items []delegatedDecisionItem) RowActionResult {
	parts := make([]string, 0, len(items))
	decided, noop, failed := 0, 0, 0
	for _, it := range items {
		parts = append(parts, "#"+strconv.Itoa(it.Number)+" "+delegatedItemWord(it))
		switch {
		case it.Error != "":
			failed++
		case it.Result != nil && it.Result.Action == rowActionNoop:
			noop++
		default:
			decided++
		}
	}
	out := RowActionResult{Action: rowActionDecidedEach, Target: rowActionTarget(repoFull, rowActionTargetIssue, number)}
	out.Outcome = fmt.Sprintf("Decided each of the %d nested pull requests: %s.", len(items), strings.Join(parts, "; "))
	out.Next = fmt.Sprintf("%d sent · %d decided · %d no-op", len(items), decided, noop)
	if failed > 0 {
		out.Next += fmt.Sprintf(" · %d failed", failed)
	}
	out.Next += ". Each PR's own toast says what happens next."
	return out
}

// decideDelegatedOne gathers, decides, executes and audits one item.
func (s *Server) decideDelegatedOne(ctx context.Context, repoFull string, number int, itemType, user string, mergerRole bool, req delegatedDecisionRequest) delegatedOutcome {
	in, err := s.gatherDelegatedInput(ctx, repoFull, number, itemType, req)
	if err != nil {
		s.audit.LogRecord(user, auditRepoItemDecisionDelegated+"_failed", auditDetail("repo", repoFull, "number", strconv.Itoa(number), "error", err.Error()), "", repoFull, number)
		return delegatedOutcome{Err: err}
	}
	// A structured blocker — the App lacks a permission, a CLA must be
	// signed — is not a question the hive can answer: a direction would
	// un-park the issue only for the agent to re-park it with the same
	// reason (hivecommons/hive#14976). Say what would unblock it instead
	// and write nothing (hivecommons/hive#15099).
	if in.Kind == delegatedecision.KindIssue {
		if b := s.delegatedBlocker(ctx, repoFull, number, s.delegatedCommentTexts(ctx, repoFull, number)); b != nil {
			held := slices.Contains(in.Labels, delegatedecision.LabelHold)
			res := blockerNoEffectResult(rowActionTarget(repoFull, rowActionTargetIssue, number), *b, held, s.blockerOptionsContext())
			d := delegatedecision.Decision{Verdict: delegatedecision.VerdictRefused, Rule: "blocker:" + b.Kind, Text: res.Outcome, Rationale: res.Outcome, KeepsPark: true}
			notes := []string{"blocked: " + b.Short + "; nothing written"}
			s.audit.LogRecord(user, auditRepoItemDecisionDelegated, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "type", itemType,
				"verdict", string(d.Verdict), "rule", d.Rule, "actions", "", "noop", "no-effect:blocker:"+b.Kind, "policy", delegatedecision.PolicyName, "decision", d.Text), "", repoFull, number)
			return delegatedOutcome{Decision: &d, Notes: notes, Result: &res}
		}
	}
	// A PR at 090 held only for its author's trust is decided here: approve
	// when nothing is wrong with it, adopt otherwise (hivecommons/hive#15173).
	// Any other 090 PR is one no policy run changes, so answer at once in
	// plain words with the actions that do (hivecommons/hive#15078).
	d, deciderNotes, humanMergeDecided := s.delegatedHumanMergeDecision(in, req, user, mergerRole)
	if !humanMergeDecided {
		if out, ok := s.delegatedHumanMergeOutcome(repoFull, number, itemType, user, mergerRole, in, req); ok {
			return out
		}
		d, deciderNotes = s.decideDelegated(ctx, in)
	} else if d.Verdict == delegatedecision.VerdictApprove {
		deciderNotes = append(deciderNotes, s.admitDelegatedHumanMergeHead(ctx, repoFull, number, user))
	}
	// An adoption whose copy was closed unmerged (hivecommons/hive#15052)
	// overrides the policy: the recorded decision no longer stands, so the
	// answer is to re-adopt, not "already decided".
	var orphan orphanedAdoption
	if o, ok := s.delegatedOrphanedAdoption(ctx, repoFull, number, in, time.Now()); ok {
		orphan = o
		d = o.decision("#" + strconv.Itoa(number))
		deciderNotes = append(deciderNotes, fmt.Sprintf("adoption orphaned: hive's copy #%d is closed without merging", o.Dead))
	}
	// The same rule firing again on an item the hive already decided would
	// only repeat the comment: report it as a no-op instead of writing —
	// unless the recorded decision never took effect, in which case say so
	// in plain words with the options (hivecommons/hive#14822), or the
	// operator asked to re-run it. A re-adoption request older than the
	// latest adoption marker was carried out and then orphaned again
	// (hivecommons/hive#15052): it is not a repeat.
	prior := s.delegatedPriorDecisionState(ctx, repoFull, number, d.Rule)
	if prior.Found && !req.Force {
		// A park label put back after the recorded decision is a new ask,
		// not that decision failing to take effect (hivecommons/hive#15021:
		// approve at 20:41Z lifted 045; the scanner re-parked at 01:16Z
		// with its own question; the dedup then called the fresh park
		// "decided, but it hasn't taken effect" for hours).
		// A decision that kept the park never lifted the label, so a label
		// event after it is not a new ask: re-deciding an operator-action
		// self-report must not post the same direction again (the #15021
		// loop); it answers "already decided; waiting on <who>".
		if repark, ok := s.delegatedReparkedAfter(ctx, repoFull, number, prior.At); ok && !d.KeepsPark {
			deciderNotes = append(deciderNotes, "parked again at "+repark.At.UTC().Truncate(time.Second).Format(time.RFC3339)+" ("+repark.Label+") after the decision recorded at "+prior.At.UTC().Truncate(time.Second).Format(time.RFC3339)+": a new ask, deciding anew")
			prior = delegatedPriorState{}
		}
	}
	if prior.Found && !req.Force && (orphan.Dead == 0 || prior.At.After(orphan.MarkerAt)) {
		kind := rowActionTargetIssue
		if in.Kind == delegatedecision.KindPR {
			kind = rowActionTargetPR
		}
		now := delegatedDecisionNow()
		timeout := s.delegatedEffectTimeout()
		why := s.delegatedStuckWhy(in, d, req, prior)
		options := delegatedStuckOptions(in, d, mergerRole, why)
		res := delegatedDecisionEffectResult(rowActionTarget(repoFull, kind, number), in, d, prior.At, now, timeout, why, options)
		notes := append(deciderNotes, "already applied; nothing written")
		noop := "already-applied"
		if res.Action == rowActionNoEffect {
			notes = append(notes, "decision has not taken effect ("+why+")", s.postDelegatedStalledComment(ctx, in, d, prior, why, options))
			noop = "no-effect:" + why
		}
		s.audit.LogRecord(user, auditRepoItemDecisionDelegated, auditDetail("repo", repoFull, "number", strconv.Itoa(number), "type", itemType,
			"verdict", string(d.Verdict), "rule", d.Rule, "actions", "", "noop", noop, "policy", delegatedecision.PolicyName, "decision", d.Text), "", repoFull, number)
		return delegatedOutcome{Decision: &d, Notes: notes, Result: &res}
	}
	s.logDelegatedLadder(repoFull, number, d)
	notes, execErr := s.executeDelegatedDecision(ctx, in, d, user, mergerRole)
	notes = append(deciderNotes, notes...)
	result := delegatedRowActionResult(in, d, notes)
	if orphan.Dead > 0 && !orphan.Looped && execErr == nil {
		result.Outcome = delegatedOrphanOutcome(orphan.Dead)
	}
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
	return delegatedOutcome{Decision: &d, Notes: notes, Result: &result, Err: execErr}
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
	in.Author = author
	in.HiveAuthored = client.IsHiveAuthorLogin(author)
	in.TrustedAuthor = in.HiveAuthored
	if issue.IsPullRequest() || itemType == "pr" {
		in.Kind = delegatedecision.KindPR
		pr, _, err := api.PullRequests.Get(ctx, owner, repoName, number)
		if err != nil {
			return in, fmt.Errorf("reading PR %s#%d: %w", repoFull, number, err)
		}
		for _, l := range pr.Labels {
			if name := l.GetName(); name != "" && !slices.Contains(in.Labels, name) {
				in.Labels = append(in.Labels, name)
			}
		}
		author = strings.TrimSpace(pr.GetUser().GetLogin())
		in.Author = author
		in.HiveAuthored = client.IsHiveAuthorLogin(author)
		in.TrustedAuthor = in.HiveAuthored
		if err := s.gatherDelegatedPRContext(ctx, repoFull, pr, &in); err != nil {
			return in, err
		}
		if err := s.gatherDelegatedLadderInput(ctx, repoFull, pr, &in); err != nil {
			return in, err
		}
		in.DiffEmpty = ghpkg.PRChangedFilesClaimsEmpty(pr)
		state := strings.ToLower(pr.GetMergeableState())
		in.Conflicting = state == "dirty" || (pr.Mergeable != nil && !pr.GetMergeable())
		ci, err := delegatedHeadCI(ctx, api, owner, repoName, pr, state)
		if err != nil {
			return in, fmt.Errorf("reading checks for %s#%d: %w", repoFull, number, err)
		}
		in.CIGreen, in.CIPending = ci.green, ci.pending
		s.gatherDelegatedRecordedPark(repoFull, number, pr.GetHead().GetSHA(), &in)
		if files, _, err := api.PullRequests.ListFiles(ctx, owner, repoName, number, &gh.ListOptions{PerPage: delegatedDecisionGatherLimit}); err == nil && len(files) > 0 {
			in.LowRiskPaths = true
			for _, f := range files {
				if !delegatedLowRiskPath(f.GetFilename()) {
					in.LowRiskPaths = false
				}
				if delegatedWorkflowPath(f.GetFilename()) {
					in.WorkflowPath = true
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
	if in.Kind == delegatedecision.KindPR && in.HiveAuthored && s.hiveSupersedingAutoMerge() {
		in.HiveSuperseding = ghpkg.HiveSupersedingPR(true, in.Body)
		in.HiveSupersederHardStop = stagelabels.HiveSupersederHardStopReason(reason)
	}
	in.AdoptionStalled = strings.HasPrefix(lowerReason, stagelabels.LaneReasonAdoptionStalledPrefix)
	if in.Kind == delegatedecision.KindPR && !in.HiveAuthored {
		in.AdoptionGraceElapsed = s.delegatedAdoptionGraceElapsed(repoFull, number)
	}
	if strings.Contains(lowerReason, stageengineprstage.ContributorLaneReasonWorkflowPath) {
		in.WorkflowPath = true
	}
	in.ReporterTrustHold = strings.Contains(lowerReason, "reporter-trust") || strings.Contains(lowerReason, "reporter trust")
	in.DCOMissing = strings.Contains(lowerReason, "dco")
	if in.Kind == delegatedecision.KindIssue {
		in.Advisory = delegatedAdvisoryChecklist(in.Labels, in.Body)
	}
	return in, nil
}

// delegatedAdvisoryChecklist is the advisory block of a hive advisory issue
// (hivecommons/hive#15754): the same classification the tracker draws
// (stageTrackerAdvisoryFor), with the checklist's progress. nil for an
// ordinary issue.
func delegatedAdvisoryChecklist(labels []string, body string) *delegatedecision.AdvisoryChecklist {
	adv := stageTrackerAdvisoryFor(labels, body, nil)
	if adv == nil {
		return nil
	}
	return &delegatedecision.AdvisoryChecklist{Resolved: adv.Resolved, Total: adv.Total}
}

// delegatedAdoptionGraceElapsed reads the stage store: adoption at the PR's
// nudged head came due and the engine held it (hivecommons/hive#15038).
func (s *Server) delegatedAdoptionGraceElapsed(repoFull string, number int) bool {
	rec, ok := s.stageStoreOrShared().Get(stagelabels.Key(repoFull, number))
	if !ok || rec.NudgeHead == "" {
		return false
	}
	return rec.AdoptionBlocked(rec.NudgeHead) != ""
}

// delegatedHeadCIState is what the head's checks say: green (every
// gating check passed), pending (some still running, none failed), or
// neither (a check failed, or none ever reported).
type delegatedHeadCIState struct {
	green, pending bool
}

// delegatedHeadCI decides Input.CIGreen / CIPending for the PR under
// decision. mergeable_state alone cannot carry it: GitHub reports "blocked"
// both for a missing required review and for red required checks,
// "unstable" whenever ANY context is pending or failing — a non-required
// merge-metadata context like tide included — and "unknown" / "behind" /
// "" say nothing about the head at all. Only "clean" and "has_hooks"
// assert a green head; every other state is settled from the head's
// combined status and check runs (meta contexts excluded, the way
// gatherDelegatedPRContext settles a competing PR): green is no red,
// nothing pending and at least one green result; pending is no red with
// a check still running. hivecommons/hive#15722 read "unstable" as red
// while every check on the head was green and only tide was pending, and
// the preview told the operator "CI is red".
func delegatedHeadCI(ctx context.Context, api *gh.Client, owner, repoName string, pr *gh.PullRequest, state string) (delegatedHeadCIState, error) {
	if state == "clean" || state == "has_hooks" {
		return delegatedHeadCIState{green: true}, nil
	}
	sha := strings.TrimSpace(pr.GetHead().GetSHA())
	if sha == "" {
		return delegatedHeadCIState{}, nil
	}
	summary, err := ghpkg.SummarizeCommitChecks(ctx, api, owner, repoName, sha)
	if err != nil {
		return delegatedHeadCIState{}, err
	}
	if len(summary.Red) > 0 {
		return delegatedHeadCIState{}, nil
	}
	return delegatedHeadCIState{green: summary.Pending == 0 && summary.Green > 0, pending: summary.Pending > 0}, nil
}

// gatherDelegatedRecordedPark reads what the stage engine recorded about
// the PR — the reason it parked it, whether the hive adopted it in place,
// and the reviewer verdict on its head — so the delegate decides on the
// same facts the 085 entry comment named, not only on the words the
// tracker row sent along (hivecommons/hive#15722: the row said reviewer
// requires_human / high-risk, the plan said "CI is red").
func (s *Server) gatherDelegatedRecordedPark(repoFull string, number int, head string, in *delegatedecision.Input) {
	head = strings.TrimSpace(head)
	recorded := ""
	if rec, ok := s.stageStoreOrShared().Get(stagelabels.Key(repoFull, number)); ok {
		recorded = strings.TrimSpace(rec.Reason)
		if recorded != "" && !slices.Contains(in.Reasons, recorded) {
			in.Reasons = append(in.Reasons, recorded)
		}
		in.AdoptedByHive = rec.AdoptionLive(head)
	}
	for _, r := range in.Reasons {
		lower := strings.ToLower(r)
		if strings.Contains(lower, stageengineprstage.ContributorLaneReasonAdoptedByHive) {
			in.AdoptedByHive = true
		}
		if stagelabels.ReasonClass(lower) == stageengine.StageReasonVerdictRequiresHuman || strings.Contains(lower, "requires_human") {
			in.ReviewerRequiresHuman = true
		}
	}
	art, err := review.LoadArtifact("")
	if err != nil {
		return
	}
	lookup := stageTrackerDecisionVerdictLookup(&art)
	agg, ok := lookup(repoFull, number, head, in.ReviewerRequiresHuman)
	if !ok {
		_, short, _ := strings.Cut(repoFull, "/")
		agg, ok = lookup(short, number, head, in.ReviewerRequiresHuman)
	}
	if !ok {
		return
	}
	if agg.RequiresHuman || agg.DecisionRequested {
		in.ReviewerRequiresHuman = true
	}
	if in.Question == "" {
		in.Question = strings.TrimSpace(agg.DecisionQuestion)
	}
	in.ReviewerFinding = strings.TrimSpace(agg.DecisionFinding)
	in.SecurityFinding = delegatedSecurityFinding(agg)
}

// delegatedSecurityFinding: the reviewer's finding is the security
// review's own — that perspective withheld approval, or the finding names
// a security / credentials question. The reviewer's generic high-risk
// marker alone is not one: it is a risk-class flag the fixer addresses.
func delegatedSecurityFinding(agg review.Aggregate) bool {
	if agg.Perspectives[review.PerspectiveSecurity] == review.VerdictRequiresHuman {
		return true
	}
	lower := strings.ToLower(agg.DecisionFinding)
	for _, term := range []string{"security", "credential", "secret", "permission", "privilege", "vulnerab", "insecure", "malicious", "supply chain", "supply-chain"} {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

// delegatedWorkflowPath reports a file under .github/workflows: the adoption
// fence refuses to publish such edits from the App (no `workflows`
// permission), so an adopt-supersede on the PR cannot execute
// (hivecommons/hive#15188).
func delegatedWorkflowPath(path string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(path)), ".github/workflows/")
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
	comment := delegatedecision.CommentBodyWith(d, hiveID, user, delegatedecision.CommentOptions{Kind: in.Kind, EffectTimeout: s.delegatedEffectTimeout()})
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
					s.noteDelegatedUnpark(repoFull, number)
				}
			} else if d.Verdict == delegatedecision.VerdictDirection && (in.HiveAuthored || in.AdoptedByHive) {
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
				queued, err := client.QueuePRAutoMergeWithNoteResult(ctx, repoFull, number, user, marker)
				if err != nil {
					return notes, fmt.Errorf("approving PR head: %w", err)
				}
				if queued.ReviewSubmitted {
					notes = append(notes, "Approve review submitted on the head and queued for auto-merge (the engine merges per repo settings)")
				} else {
					// The hive's own PR: no review is possible, so the
					// operator's approval goes on record the way Approve
					// records it and the self-authored lane merges on it.
					if _, err := client.RecordOperatorPullRequestApprovalWith(ctx, repoFull, number, user, ghpkg.PullRequestApprovalOptions{
						HumanChangeRequests: repoItemDecisionChangeRequests(),
						LiftNeedsHuman:      true,
						SentinelLabel:       s.sentinelLabel(),
					}); err != nil {
						return notes, fmt.Errorf("approving PR: %w", err)
					}
					notes = append(notes, "approval recorded on the hive's own head and queued for auto-merge (the App cannot review its own pull request; the self-authored lane merges once CI is green)")
				}
			} else {
				if _, err := client.RecordOperatorPullRequestApproval(ctx, repoFull, number, user); err != nil {
					return notes, fmt.Errorf("approving PR: %w", err)
				}
				notes = append(notes, "`/lgtm` + `/approve` recorded; hive's merge sweep merges it once CI is green and the head has soaked")
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
				if err := delegatedRemoveLabel(ctx, client, repoFull, number, l); err != nil {
					return notes, fmt.Errorf("removing label %s: %w", l, err)
				}
			}
			notes = append(notes, "labels removed: "+strings.Join(a.Labels, ", "))
		case delegatedecision.ActionCloseDuplicate, delegatedecision.ActionCloseNotNow, delegatedecision.ActionCloseCompleted:
			// All share one comment-then-close path so the writeback
			// ratchet (pkg/writeback/testdata) sees no new write sites.
			notNow := a.Kind == delegatedecision.ActionCloseNotNow
			completed := a.Kind == delegatedecision.ActionCloseCompleted
			body := fmt.Sprintf("%s\n\nRationale: %s.\n\n_Closed by the hive as the owner's delegate (rule `%s`)._\n%s", a.Text, d.Rationale, d.Rule, marker)
			reason := "closed as duplicate of #" + strconv.Itoa(a.Target) + " by the hive delegate"
			what := "duplicate"
			stateReason := issueStateReasonNotPlanned
			if notNow || completed {
				if in.Kind != delegatedecision.KindIssue {
					return notes, fmt.Errorf("%s closes issues only", a.Kind)
				}
				body = comment
				reason = "declined for now by the hive delegate: no incident or recurrence justifies the cost yet"
				what = "not-now"
			}
			if completed {
				reason, what, stateReason = "advisory closed as completed by the hive delegate: its items are resolved", "advisory", issueStateReasonCompleted
			}
			if err := client.CreateIssueComment(ctx, repoFull, number, body); err != nil {
				return notes, fmt.Errorf("posting %s note: %w", what, err)
			}
			if in.Kind == delegatedecision.KindPR {
				if err := client.CloseSupersededPR(ctx, repoFull, number); err != nil {
					return notes, fmt.Errorf("closing superseded PR: %w", err)
				}
			} else if err := client.CloseIssue(ctx, repoFull, number, ghpkg.IssueCloseOptions{
				OverrideReason:          reason,
				SuppressOverrideComment: true,
				StateReason:             stateReason,
			}); err != nil {
				return notes, fmt.Errorf("closing %s: %w", what, err)
			}
			if completed {
				notes = append(notes, "closed as completed (advisory resolved)")
			} else if notNow {
				notes = append(notes, "closed as not planned (declined for now)")
			} else {
				notes = append(notes, fmt.Sprintf("closed in favour of #%d", a.Target))
			}
		case delegatedecision.ActionLink:
			body := fmt.Sprintf("Cross-link: %s (#%d).\n%s", a.Text, a.Target, marker)
			if err := client.CreateIssueComment(ctx, repoFull, number, body); err != nil {
				return notes, fmt.Errorf("cross-linking: %w", err)
			}
			notes = append(notes, fmt.Sprintf("cross-linked #%d", a.Target))
		case delegatedecision.ActionAdoptSupersede:
			if in.Kind != delegatedecision.KindPR || in.HiveAuthored {
				return notes, fmt.Errorf("supersede adoption requires a contributor PR")
			}
			if in.PersonAuthored() {
				return notes, fmt.Errorf("a person pushed %s#%d themselves (@%s); the hive asks the author instead of adopting it", repoFull, number, in.Author)
			}
			if !s.stageAdoptionEnabled() {
				// The engine only consumes the request label with adoption
				// on; queuing it would be a silent no-op with a stale label.
				return notes, fmt.Errorf("supersede adoption requires governor.labels.stage.adoption to be enabled on this hive")
			}
			if err := client.EnsureIssueLabel(ctx, repoFull, delegatedecision.LabelAdoptSupersede, "5319e7", "Owner-delegate request: adopt now in supersede mode"); err != nil {
				return notes, fmt.Errorf("ensuring adoption request label: %w", err)
			}
			if err := client.AddLabels(ctx, repoFull, number, []string{delegatedecision.LabelAdoptSupersede}); err != nil {
				return notes, fmt.Errorf("queuing supersede adoption: %w", err)
			}
			if d.Rule == delegatedecision.RuleAdoptNow {
				// Grace already elapsed: run it as Adopt now — the forced
				// mark bypasses presence and maintainer_policy and the
				// sweep is nudged (api_repo_item_decision_reroute.go).
				store := s.stageStoreOrShared()
				key := stagelabels.Key(repoFull, number)
				ensureStageRecord(store, key)
				store.NoteAdoptionForced(key, user, time.Now())
				if superseding := s.nudgeSweepAndAwaitAdoption(ctx, store, key, repoFull, number); superseding > 0 {
					notes = append(notes, fmt.Sprintf("adopted now → #%d (hive's copy)", superseding))
				} else {
					notes = append(notes, "adoption forced (adopt-now): the hive opens its copy on the pass now running")
				}
				break
			}
			notes = append(notes, "supersede adoption queued for the adoption engine's next pass")
		case delegatedecision.ActionRecommendOnly:
			if err := client.CreateIssueComment(ctx, repoFull, number, comment); err != nil {
				return notes, fmt.Errorf("posting recommendation: %w", err)
			}
			park := delegatedecision.LabelNeedsDecision
			if in.Kind == delegatedecision.KindIssue {
				park = delegatedecision.LabelIssueNeedsDecision
			}
			if d.Verdict == delegatedecision.VerdictOperatorAction {
				notes = append(notes, "operator direction posted; `"+park+"` kept, no approved-direction, no implementer")
			} else {
				notes = append(notes, "recommendation posted; `"+park+"` kept for the owner")
			}
		}
	}
	if lifted := s.liftDelegatedParkLabels(ctx, in, d); len(lifted) > 0 {
		notes = append(notes, "park labels lifted: "+strings.Join(lifted, ", "))
	}
	for _, r := range d.Refusals {
		notes = append(notes, "refused: "+r)
	}
	return notes, nil
}

// delegatedParkLabels are the labels that asked a person to decide and
// that a resolving delegated decision answers: the needs-human ask and the
// 085 needs-decision stage label (PR and issue forms). `hold` is never
// among them.
var delegatedParkLabels = []string{config.DefaultNeedsHumanLabel, stagelabels.NeedsDecision.Label(), ghpkg.NeedsDecisionStageLabel}

// delegatedDecisionResolves reports whether verdict settles the park
// (approve / direction / adopt / close); recommend-only, owner-only and
// refused keep the item with the person.
func delegatedDecisionResolves(v delegatedecision.Verdict) bool {
	switch v {
	case delegatedecision.VerdictApprove, delegatedecision.VerdictDirection, delegatedecision.VerdictAdoptSupersede,
		delegatedecision.VerdictCloseDuplicate, delegatedecision.VerdictCloseSuperseded,
		delegatedecision.VerdictQuarantineFlaky, delegatedecision.VerdictAcceptRatchet, delegatedecision.VerdictAcceptProposal,
		delegatedecision.VerdictNotNow, delegatedecision.VerdictClose:
		return true
	}
	return false
}

// liftDelegatedParkLabels takes the delegatedParkLabels still on a PR off
// once the delegate has resolved it (hivecommons/hive#15096 N13: the
// decision landed, `needs-human` stayed, the label-derived pass re-parked
// the row in 085 and the popover re-asked). Returns the labels removed; a
// failed removal is logged, not returned — the decision record stands and
// the tracker projection ranks it above the stale label.
func (s *Server) liftDelegatedParkLabels(ctx context.Context, in delegatedecision.Input, d delegatedecision.Decision) []string {
	if in.Kind != delegatedecision.KindPR || !delegatedDecisionResolves(d.Verdict) {
		return nil
	}
	var lifted []string
	for _, label := range delegatedParkLabels {
		if !slices.ContainsFunc(in.Labels, func(l string) bool { return strings.EqualFold(l, label) }) {
			continue
		}
		if err := delegatedRemoveLabel(ctx, s.deps.GHClient, in.Repo, in.Number, label); err != nil {
			if s.deps.Logger != nil {
				s.deps.Logger.Warn("delegated decision: park label not lifted", "repo", in.Repo, "pr", in.Number, "label", label, "error", err)
			}
			continue
		}
		lifted = append(lifted, label)
	}
	return lifted
}

// delegatedRemoveLabel is the one label-removal write of the delegate: a
// solicited write (the operator chose Let hive decide), shared by the
// decision's own label actions and the park-label lift.
func delegatedRemoveLabel(ctx context.Context, client *ghpkg.Client, repo string, number int, label string) error {
	return client.RemoveLabel(ctx, repo, number, label)
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
