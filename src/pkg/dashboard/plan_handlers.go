package dashboard

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/worksource"
)

// planEpicStore returns the bead store to mint issue-sourced epics into: the
// architect store when present, else any store (deterministic pick not required
// — findEpicStore locates the epic by ID afterward regardless of which store
// holds it). Returns (nil, "") when no bead stores are configured.
func (s *Server) planEpicStore() (*beads.Store, string) {
	if s.deps == nil || s.deps.BeadStores == nil {
		return nil, ""
	}
	if store, ok := s.deps.BeadStores[planning.ArchitectAgentName]; ok {
		return store, planning.ArchitectAgentName
	}
	for name, store := range s.deps.BeadStores {
		return store, name
	}
	return nil, ""
}

// decomposeKicker returns the manager as a planning.DecomposeKicker (nil when no
// manager is wired). A test-only override (decomposeKickerOverride) lets tests
// inject a fake kicker and exercise the paused / kicked / no-agent branches
// without a live tmux Manager; production never sets it.
func (s *Server) decomposeKicker() planning.DecomposeKicker {
	if s.decomposeKickerOverride != nil {
		return s.decomposeKickerOverride
	}
	if s.deps == nil || s.deps.AgentMgr == nil {
		return nil
	}
	return s.deps.AgentMgr
}

func (s *Server) designConfig() planning.DesignConfig {
	if s.deps == nil || s.deps.Config == nil {
		return planning.DefaultDesignConfig()
	}
	p := s.deps.Config.Planning
	return planning.DesignConfig{
		PlanLabels:    p.PlanLabelsOrDefault(),
		DesignLabels:  p.DesignLabelsOrDefault(),
		ApprovedLabel: p.DesignApprovedLabelOrDefault(),
		MaxRevisions:  p.MaxDesignRevisionsOrDefault(),
		MaxConcurrent: p.MaxConcurrentDesignsOrDefault(),
	}
}

func (s *Server) enforcePlanIssueBody(r *http.Request, text string) (string, bool) {
	if s.deps == nil || s.deps.Config == nil || !s.deps.Config.Ioscan.IsEnabled() {
		return text, true
	}
	sanitized, v := ioscan.EnforceInput(text)
	if v.Blocked {
		s.auditFromRequest(r, "ioscan_block", auditDetail("context", "plan_from_issue", "findings", strconv.Itoa(len(v.Findings))), planning.ArchitectAgentName)
	}
	level := detectACMMLevel(s.deps.Config)
	if s.deps.Config.Ioscan.FailClosedAtLevel(level) && v.HasCriticalInjection() {
		s.auditFromRequest(r, "ioscan_fail_closed", auditDetail("context", "plan_from_issue", "findings", strconv.Itoa(len(v.Findings))), planning.ArchitectAgentName)
		return sanitized, false
	}
	return sanitized, true
}

// requestArchitectDecompose hands a freshly-minted (pending) epic to the
// architect, RESPECTING its pause: a paused architect leaves the request queued
// and returns DecomposeQueuedPaused so the handler can tell the user WHY nothing
// is happening. Only SendKick/IsPaused are called — never the launch-path mutex.
// On a successful kick it records the governor kick. The epic stays
// decompose_pending until the architect materializes children.
func (s *Server) requestArchitectDecompose(epic *beads.Bead) planning.DecomposeState {
	// A click is an explicit human request: it bypasses the label trigger's
	// re-kick throttle and hands a stuck (decompose_failed) epic a fresh
	// attempt budget (hivecommons/hive#8010). The kick is recorded so the
	// label trigger's throttle sees it and does not double-kick next cycle.
	store, _ := s.planEpicStore()
	if store != nil {
		_ = planning.ResetDecomposeAttempts(store, epic.ID)
	}
	state := planning.RequestDecompose(s.decomposeKicker(), epic)
	switch state {
	case planning.DecomposeKicked:
		if store != nil {
			_ = planning.RecordDecomposeKick(store, epic.ID, time.Now())
		}
		if s.deps != nil && s.deps.Governor != nil {
			s.deps.Governor.RecordKick(planning.ArchitectAgentName)
		}
	case planning.DecomposeQueuedPaused:
		s.logger.Info("plan-from-issue: architect paused, plan queued", "epic", epic.ID)
	case planning.DecomposeQueuedNoAgent:
		s.logger.Warn("plan-from-issue: architect unavailable, plan queued", "epic", epic.ID)
	}
	return state
}

// handlePlanFromIssue serves POST /api/plan/from-issue: the "Plan this issue"
// dashboard action (Part A). Body: {repo, number, url, title, body}. It mints an
// epic from the issue (idempotent via planning.EpicFromIssue) and REQUESTS
// decomposition by kicking the architect out-of-band with the decomposition
// prompt — the same lock-safe SendKick path every other kick uses, never the
// agent-launch mutex. It does NOT decompose synchronously (that would block the
// HTTP handler on a live tmux agent); instead it returns the epic id so the UI
// polls GET /api/plan/{epicID} and the architect fills the plan in by running
// `bd decompose <epic> --plan <file>` from its own pane (the kick prompt tells
// it to; hivecommons/hive#8010), surfaced via the existing plan-review gate.
func (s *Server) handlePlanFromIssue(w http.ResponseWriter, r *http.Request) {
	// Deliberately NOT owner-gated (F16, TestF16PlanFromIssueStaysUngated):
	// this mints a DRAFT epic whose children decompose.go withholds from
	// Ready(), so proposing a plan releases no work. The owner gate lives on
	// approve/reject/child below — gating the "ask for a plan" action would
	// make planning owner-only end to end and break the contributor workflow.
	var body struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		URL    string `json:"url"`
		Title  string `json:"title"`
		Body   string `json:"body"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Gate: planning below ACMM L5 mints an epic that nothing decomposes — the
	// architect agent has no cadence until L5 (see planning.PlanningMinACMMLevel).
	// Reject with a clear explanation rather than stranding a decompose_pending
	// epic. Read from the same config source the status payload uses.
	if s.deps == nil || s.deps.Config == nil || !planning.PlanningAllowedAtLevel(detectACMMLevel(s.deps.Config)) {
		jsonError(w, planning.PlanningLevelGateMessage, http.StatusConflict)
		return
	}
	if sanitized, ok := s.enforcePlanIssueBody(r, body.Body); !ok {
		jsonError(w, "ioscan rejected critical injection in issue body", http.StatusUnprocessableEntity)
		return
	} else {
		body.Body = sanitized
	}

	issue := github.Issue{
		Repo:   sanitizeString(body.Repo),
		Number: body.Number,
		URL:    sanitizeString(body.URL),
		Title:  sanitizeString(body.Title),
	}
	// When the client sent only a repo+number (no title), resolve the full issue
	// from the last enumerated actionable set so we always mint a titled epic.
	if issue.Title == "" {
		if resolved, ok := s.resolveActionableIssue(issue.Repo, issue.Number, issue.URL); ok {
			if issue.Title == "" {
				issue.Title = resolved.Title
			}
			if issue.URL == "" {
				issue.URL = resolved.URL
			}
			if issue.Repo == "" {
				issue.Repo = resolved.Repo
			}
			issue.Labels = resolved.Labels
		}
	}
	if issue.Title == "" {
		jsonError(w, "issue title is required (or a resolvable repo+number)", http.StatusBadRequest)
		return
	}

	store, agentName := s.planEpicStore()
	if store == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	epic, err := planning.EpicFromIssue(store, issue, sanitizeString(body.Body))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if planning.DesignGated(epic) {
		jsonError(w, "design gate is still active for this epic; approve the design before planning", http.StatusConflict)
		return
	}

	// Hand the epic to the architect out-of-band, respecting its pause. Extracted
	// so the paused/kicked/queued bookkeeping is testable without a live tmux agent.
	state := s.requestArchitectDecompose(epic)
	kicked := state == planning.DecomposeKicked

	// Build a user-facing message. When the architect is paused, say so clearly and
	// point at the fix — the user paused it deliberately and must know that is why
	// the plan is not being built yet.
	message := "Plan queued — the architect will build it."
	if kicked {
		message = "Plan requested — the architect is decomposing this issue now."
	} else if state == planning.DecomposeQueuedPaused {
		message = planning.ArchitectPausedMessage
	}

	s.auditFromRequest(r, "plan_from_issue",
		auditDetail("epic", epic.ID, "ref", epic.ExternalRef, "state", string(state)), agentName)
	s.refreshAndPersist()

	jsonResponse(w, map[string]interface{}{
		"ok":              true,
		"epic_id":         epic.ID,
		"epic":            epic,
		"kicked":          kicked,
		"state":           string(state),
		"architectPaused": state == planning.DecomposeQueuedPaused,
		"message":         message,
		"poll_url":        "/api/plan/" + epic.ID,
	})
}

// handlePlanDesignApprove serves POST /api/plan/{epicID}/design/approve: owner-only
// approval of Gate 1. It mirrors the trust boundary in GitHub by applying the
// configured approval label to the source issue, then marks the local epic as
// design-approved so the next label pass can decompose it.
func (s *Server) handlePlanDesignApprove(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	epicID := r.PathValue("epicID")
	store, agentName := s.findEpicStore(epicID)
	if store == nil {
		jsonError(w, "epic not found in any bead store", http.StatusNotFound)
		return
	}
	epic, err := store.Get(epicID)
	if err != nil {
		jsonError(w, "epic not found", http.StatusNotFound)
		return
	}
	repo := epic.Meta(planning.MetaIssueRepo)
	number, _ := strconv.Atoi(epic.Meta(planning.MetaIssueNumber))
	if repo == "" || number <= 0 {
		jsonError(w, "epic is not linked to a GitHub issue", http.StatusBadRequest)
		return
	}
	switch planning.DesignStatus(epic) {
	case planning.DesignStatusRequested, planning.DesignStatusNeedsHuman:
	case "":
		jsonError(w, "epic has no design to approve", http.StatusBadRequest)
		return
	default:
		jsonError(w, "design has not been posted yet", http.StatusBadRequest)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil {
		jsonError(w, "github client not initialized", http.StatusServiceUnavailable)
		return
	}
	label := s.designConfig().ApprovedLabelOrDefault()
	if err := s.deps.GHClient.AddLabels(r.Context(), repo, number, []string{label}); err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := planning.ApproveDesign(store, epicID); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditFromRequest(r, "design_approved", auditDetail("epic", epicID, "label", label, "run", repo+"#"+strconv.Itoa(number), "surface", "design"), agentName)
	s.refreshAndPersist()
	tree, _ := planning.GetPlanTree(store, epicID)
	jsonResponse(w, map[string]interface{}{"ok": true, "status": "approved", "plan": tree})
}

// handleDesignFromIssue serves POST /api/plan/from-issue/design: the dashboard
// “design first” button. It mints the issue epic and enters Gate 1, but does not
// kick decomposition; the label loop sends the design prompt after seeing the
// configured design label on the issue.
func (s *Server) handleDesignFromIssue(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		Repo   string `json:"repo"`
		Number int    `json:"number"`
		URL    string `json:"url"`
		Title  string `json:"title"`
		Body   string `json:"body"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if s.deps == nil || s.deps.Config == nil || !planning.PlanningAllowedAtLevel(detectACMMLevel(s.deps.Config)) {
		jsonError(w, planning.PlanningLevelGateMessage, http.StatusConflict)
		return
	}
	if sanitized, ok := s.enforcePlanIssueBody(r, body.Body); !ok {
		jsonError(w, "ioscan rejected critical injection in issue body", http.StatusUnprocessableEntity)
		return
	} else {
		body.Body = sanitized
	}
	issue := github.Issue{Repo: sanitizeString(body.Repo), Number: body.Number, URL: sanitizeString(body.URL), Title: sanitizeString(body.Title)}
	if issue.Title == "" {
		if resolved, ok := s.resolveActionableIssue(issue.Repo, issue.Number, issue.URL); ok {
			if issue.Title == "" {
				issue.Title = resolved.Title
			}
			if issue.URL == "" {
				issue.URL = resolved.URL
			}
			if issue.Repo == "" {
				issue.Repo = resolved.Repo
			}
			issue.Labels = resolved.Labels
		}
	}
	if issue.Title == "" {
		jsonError(w, "issue title is required (or a resolvable repo+number)", http.StatusBadRequest)
		return
	}
	if issue.Repo == "" || issue.Number <= 0 {
		jsonError(w, "issue repo and number are required to apply the design label", http.StatusBadRequest)
		return
	}
	if s.deps.GHClient == nil {
		jsonError(w, "github client not initialized", http.StatusServiceUnavailable)
		return
	}
	store, agentName := s.planEpicStore()
	if store == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}
	if existing := store.FindByExternalRef(planning.IssueRef(issue)); existing != nil && !planning.DecomposePending(existing) {
		jsonError(w, "epic is already decomposed; nothing to design", http.StatusBadRequest)
		return
	}
	label := s.designConfig().DesignLabelOrDefault()
	if err := s.deps.GHClient.AddLabels(r.Context(), issue.Repo, issue.Number, []string{label}); err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	epic, err := planning.EpicFromIssue(store, issue, sanitizeString(body.Body))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := planning.RequestDesign(store, epic.ID); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditFromRequest(r, "design_requested", auditDetail("epic", epic.ID, "ref", epic.ExternalRef, "label", label), agentName)
	s.refreshAndPersist()
	jsonResponse(w, map[string]interface{}{
		"ok": true, "epic_id": epic.ID, "epic": epic, "state": planning.DesignStatusQueued,
		"message": "Design requested — the architect will post a design before breakdown.", "poll_url": "/api/plan/" + epic.ID,
	})
}

// resolveActionableIssue looks up an actionable issue by repo+number (or url)
// from the last enumerated status, so a click that sends only identifiers can
// still recover the title/labels. Returns (issue, true) on a match.
func (s *Server) resolveActionableIssue(repo string, number int, url string) (github.Issue, bool) {
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status == nil {
		return github.Issue{}, false
	}
	for _, repoStatus := range status.Repos {
		for _, raw := range repoStatus.ActionableIssues {
			iss, ok := raw.(github.Issue)
			if !ok {
				continue
			}
			if url != "" && iss.URL == url {
				return iss, true
			}
			if number > 0 && iss.Number == number &&
				(repo == "" || strings.EqualFold(iss.Repo, repo) || strings.HasSuffix(strings.ToLower(iss.Repo), "/"+strings.ToLower(repo))) {
				return iss, true
			}
		}
	}
	return github.Issue{}, false
}

// findEpicStore returns the bead store that contains an epic bead with epicID,
// or (nil, "") if no store holds it. Planning children live in the same store
// as their epic (Decompose creates them there), so the located store is where
// the whole plan lives.
func (s *Server) findEpicStore(epicID string) (*beads.Store, string) {
	if s.deps == nil || s.deps.BeadStores == nil {
		return nil, ""
	}
	for name, store := range s.deps.BeadStores {
		if b, err := store.Get(epicID); err == nil && b.Type == beads.TypeEpic {
			return store, name
		}
	}
	return nil, ""
}

// handlePlanList serves GET /api/plans: every plan across all bead stores,
// drafts (awaiting human review) first. This is the listing the dashboard plan
// view renders before drilling into GET /api/plan/{epicID} (#7537).
func (s *Server) handlePlanList(w http.ResponseWriter, r *http.Request) {
	var stores map[string]*beads.Store
	if s.deps != nil {
		stores = s.deps.BeadStores
	}
	plans := planning.ListPlans(stores)
	if plans == nil {
		plans = []planning.PlanSummary{}
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "plans": plans})
}

// handlePlanTree serves GET /api/plan/{epicID}: the review view of a decomposed
// epic (the epic + its children with execution tags and dependency edges).
func (s *Server) handlePlanTree(w http.ResponseWriter, r *http.Request) {
	epicID := r.PathValue("epicID")
	store, _ := s.findEpicStore(epicID)
	if store == nil {
		jsonError(w, "epic not found in any bead store", http.StatusNotFound)
		return
	}
	tree, err := planning.GetPlanTree(store, epicID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "plan": tree})
}

// handlePlanApprove serves POST /api/plan/{epicID}/approve: approve the plan,
// releasing its children through Ready().
//
// OWNER-ONLY. Approval is the whole point of the plan-review gate: decompose.go
// withholds a draft epic's children from Ready() "until a human approves the
// plan", and ApprovePlan is what releases them into agent execution. An
// un-gated approve makes the gate decorative. Audit F16 (2026-08-13).
func (s *Server) handlePlanApprove(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	epicID := r.PathValue("epicID")
	store, agentName := s.findEpicStore(epicID)
	if store == nil {
		jsonError(w, "epic not found in any bead store", http.StatusNotFound)
		return
	}
	runKey := ""
	if epic, err := store.Get(epicID); err == nil {
		repo, number := epic.Meta(planning.MetaIssueRepo), epic.Meta(planning.MetaIssueNumber)
		if repo != "" && number != "" {
			runKey = repo + "#" + number
		}
	}
	if err := planning.ApprovePlan(store, epicID); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditFromRequest(r, "plan_approve", auditDetail("epic", epicID, "run", runKey, "surface", "plan"), agentName)
	s.refreshAndPersist()
	tree, _ := planning.GetPlanTree(store, epicID)
	mirrored := s.mirrorPlanToIssue(r, store, tree, agentName)
	jsonResponse(w, map[string]interface{}{"ok": true, "status": "approved", "plan": tree, "mirrored": mirrored})
}

// handlePlanReject serves POST /api/plan/{epicID}/reject: return an approved (or
// draft) plan to draft state, re-gating its children.
//
// OWNER-ONLY. RejectPlan is documented as "the inverse of ApprovePlan". If
// approve is owner-only and reject is not, a read-write member can undo an
// owner's approval at will — the gate is bypassed from the other side, and the
// two ends of one state machine must sit at the same trust level.
// Audit F16 (2026-08-13).
func (s *Server) handlePlanReject(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	epicID := r.PathValue("epicID")
	store, agentName := s.findEpicStore(epicID)
	if store == nil {
		jsonError(w, "epic not found in any bead store", http.StatusNotFound)
		return
	}
	if err := planning.RejectPlan(store, epicID); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.resetRunLeaseAfterPlanReject(store, epicID); err != nil {
		jsonError(w, err.Error(), runResetErrorStatus(err))
		return
	}
	s.auditFromRequest(r, "plan_reject", auditDetail("epic", epicID), agentName)
	s.refreshAndPersist()
	tree, _ := planning.GetPlanTree(store, epicID)
	jsonResponse(w, map[string]interface{}{"ok": true, "status": "draft", "plan": tree})
}

func (s *Server) resetRunLeaseAfterPlanReject(store *beads.Store, epicID string) error {
	if s == nil || s.contributeHub == nil || store == nil {
		return nil
	}
	epic, err := store.Get(epicID)
	if err != nil || epic == nil {
		return nil
	}
	repo, number := strings.TrimSpace(epic.Meta(planning.MetaIssueRepo)), strings.TrimSpace(epic.Meta(planning.MetaIssueNumber))
	if repo == "" || number == "" {
		return nil
	}
	key := worksource.Ref{Repo: repo, ExternalID: number}.Key()
	if n, err := strconv.Atoi(number); err == nil && n > 0 {
		key = worksource.Ref{Repo: repo, Number: n}.Key()
	}
	now := time.Now()
	held, ok := s.contributeHub.runLeaseHolder(key, now)
	if !ok || leaseStageIndex(held.stage) <= leaseStageIndex(StagePlan) {
		return nil
	}
	_, err = s.contributeHub.resetLeaseStage(held.identity, held.taskID, StagePlan, "plan rejected", now)
	return err
}

// handlePlanChild serves POST /api/plan/{epicID}/child/{childID}: edit a child
// before approval. Body {"action":"retag","execution":"agent_suitable"} retags;
// {"action":"remove"} removes (closes) the child.
//
// OWNER-ONLY. This edits the plan that approval will release: "remove" closes a
// child bead outright and "retag" flips a child between human_required and
// agent_suitable — i.e. it can hand work the planner marked as needing a human
// to an agent. That is the same decision approve makes, taken one bead at a
// time, so it sits behind the same gate. Audit F16 (2026-08-13).
func (s *Server) handlePlanChild(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	epicID := r.PathValue("epicID")
	childID := r.PathValue("childID")
	store, agentName := s.findEpicStore(epicID)
	if store == nil {
		jsonError(w, "epic not found in any bead store", http.StatusNotFound)
		return
	}

	var body struct {
		Action    string `json:"action"`
		Execution string `json:"execution"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	switch body.Action {
	case "retag":
		if err := planning.RetagChild(store, epicID, childID, sanitizeString(body.Execution)); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.auditFromRequest(r, "plan_child_retag", auditDetail("epic", epicID, "child", childID, "execution", body.Execution), agentName)
	case "remove":
		if err := planning.RemoveChild(store, epicID, childID); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.auditFromRequest(r, "plan_child_remove", auditDetail("epic", epicID, "child", childID), agentName)
	default:
		jsonError(w, "action must be 'retag' or 'remove'", http.StatusBadRequest)
		return
	}

	s.refreshAndPersist()
	tree, _ := planning.GetPlanTree(store, epicID)
	jsonResponse(w, map[string]interface{}{"ok": true, "plan": tree})
}
