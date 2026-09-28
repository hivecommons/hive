package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/timeline"
)

// RunCheckpointSummaryMaxBytes is the hard mobile payload budget for the
// plain-text checkpoint summary. Keep notification bodies small and predictable.
const RunCheckpointSummaryMaxBytes = 2048

const (
	runCheckpointDecisionApprove = "approve"
	runCheckpointDecisionReject  = "reject"
)

var errRunCheckpointNotHeld = errors.New("run checkpoint is not held for review")

// RunCheckpointPayload is the single compact review contract consumed by
// mobile, chat, push, and PWA surfaces.
type RunCheckpointPayload struct {
	RunKey          string                    `json:"run_key"`
	Stage           string                    `json:"stage"`
	Gen             uint64                    `json:"gen"`
	Repo            string                    `json:"repo"`
	Title           string                    `json:"title"`
	PlanEpicID      string                    `json:"plan_epic_id,omitempty"`
	Summary         string                    `json:"summary"`
	SummaryMaxBytes int                       `json:"summary_max_bytes"`
	DetailURL       string                    `json:"detail_url,omitempty"`
	Decisions       []RunCheckpointDecision   `json:"decisions"`
	DashboardURL    string                    `json:"dashboard_url"`
	Staleness       RunCheckpointStaleness    `json:"staleness"`
	Approvers       RunCheckpointApproverRule `json:"approvers"`
	Approval        map[string]string         `json:"approval,omitempty"`
}

type RunCheckpointDecision struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	Method string `json:"method"`
	URL    string `json:"url"`
}

type RunCheckpointStaleness struct {
	Fence string `json:"fence"`
	Gen   uint64 `json:"gen"`
}

type RunCheckpointApproverRule struct {
	Role                  string `json:"role"`
	VerifiedOwnerRequired bool   `json:"verified_owner_required"`
}

type runCheckpointDecisionRequest struct {
	Action string `json:"action"`
	Gen    uint64 `json:"gen"`
}

func (s *Server) handleRunCheckpointGet(w http.ResponseWriter, r *http.Request) {
	payload, err := s.RunCheckpointPayload(pathRunKey(r))
	if err != nil {
		jsonError(w, err.Error(), runCheckpointStatus(err))
		return
	}
	jsonResponse(w, payload)
}

func (s *Server) handleRunCheckpointDecision(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	key := pathRunKey(r)
	var req runCheckpointDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	action := strings.TrimSpace(strings.ToLower(req.Action))
	if action != runCheckpointDecisionApprove && action != runCheckpointDecisionReject {
		jsonError(w, "action must be approve or reject", http.StatusBadRequest)
		return
	}
	payload, err := s.RunCheckpointPayload(key)
	if err != nil {
		jsonError(w, err.Error(), runCheckpointStatus(err))
		return
	}
	if req.Gen != payload.Gen {
		jsonError(w, "stale checkpoint generation", http.StatusConflict)
		return
	}
	if payload.Stage == StageSpec && !s.designCheckpointPending(payload.PlanEpicID) {
		s.decideSpecCheckpointLease(w, r, payload, action)
		return
	}
	store, agentName := s.findEpicStore(payload.PlanEpicID)
	if store == nil {
		jsonError(w, "checkpoint plan not found", http.StatusNotFound)
		return
	}
	// The checkpoint's stage picks the decision, never the epic's design
	// metadata: a design epic whose Spec checkpoint was skipped still reaches
	// the plan checkpoint with design_status pending (hivecommons/hive#9181).
	if payload.Stage == StageSpec {
		epic, err := store.Get(payload.PlanEpicID)
		if err != nil || epic.Meta(planning.MetaDesignVia) != planning.DesignViaSpektacular {
			jsonError(w, "spec checkpoint has no Spektacular design to decide", http.StatusConflict)
			return
		}
	}
	now := time.Now()
	switch {
	case action == runCheckpointDecisionApprove && payload.Stage == StageSpec:
		// Advance the exact generation the owner reviewed first: it is the
		// transition, and the only step that can refuse. Recording the design
		// approval (and signalling the forge) after it keeps a failed or
		// retried approval from stranding the lease at spec.
		runKey, err := s.advanceCheckpointLease(payload.RunKey, payload.PlanEpicID, StageSpec, StagePlan, payload.Gen, requestUser(r), now)
		if err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		if err := s.approveRunDesign(r.Context(), store, payload.PlanEpicID, runKey, requestUser(r)); err != nil {
			jsonError(w, "run advanced to plan but recording the design approval failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.auditFromRequest(r, "design_approved", auditDetail("epic", payload.PlanEpicID, "run", payload.RunKey, "surface", "run_checkpoint"), agentName)
	case action == runCheckpointDecisionApprove:
		if err := planning.ApprovePlan(store, payload.PlanEpicID); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if payload.Stage == StagePlan {
			if _, err := s.advanceCheckpointLease(payload.RunKey, payload.PlanEpicID, StagePlan, StageImplement, payload.Gen, requestUser(r), now); err != nil {
				// Undo the approval so the refused decision leaves the run
				// exactly as the owner found it and a retry sees it held.
				if rbErr := planning.RejectPlan(store, payload.PlanEpicID); rbErr != nil {
					s.logger.Warn("[runs] rolling back plan approval after refused lease advance failed", "run", payload.RunKey, "epic", payload.PlanEpicID, "error", rbErr)
				}
				jsonError(w, err.Error(), http.StatusConflict)
				return
			}
		}
		s.auditFromRequest(r, "plan_approve", auditDetail("epic", payload.PlanEpicID, "run", payload.RunKey, "surface", "run_checkpoint"), agentName)
	case payload.Stage == StageSpec:
		if err := store.SetMetadata(payload.PlanEpicID, planning.MetaDesignStatus, planning.DesignStatusQueued); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.auditFromRequest(r, "design_reject", auditDetail("epic", payload.PlanEpicID, "run", payload.RunKey, "surface", "run_checkpoint"), agentName)
	default:
		if err := planning.RejectPlan(store, payload.PlanEpicID); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.resetRunLeaseAfterPlanReject(store, payload.PlanEpicID); err != nil {
			jsonError(w, err.Error(), runResetErrorStatus(err))
			return
		}
		s.auditFromRequest(r, "plan_reject", auditDetail("epic", payload.PlanEpicID, "run", payload.RunKey, "surface", "run_checkpoint"), agentName)
	}
	s.refreshAndPersist()
	jsonResponse(w, map[string]any{"ok": true, "status": action, "run_key": payload.RunKey, "gen": req.Gen})
}

// designCheckpointPending reports whether epicID names a Spektacular design
// epic whose design still awaits approval. Only such a spec checkpoint is
// decided on the epic; every other held spec is decided on its lease.
func (s *Server) designCheckpointPending(epicID string) bool {
	if epicID == "" {
		return false
	}
	store, _ := s.findEpicStore(epicID)
	if store == nil {
		return false
	}
	epic, err := store.Get(epicID)
	return err == nil && epic.Meta(planning.MetaDesignVia) == planning.DesignViaSpektacular && planning.DesignStatus(epic) != planning.DesignStatusApproved
}

// decideSpecCheckpointLease decides a spec checkpoint that has no design epic
// to approve - the run was admitted through triage, non-design POST
// /api/runs/spec, nous or inception (hivecommons/hive#9182). Approval releases
// the parked lease to plan and records the stage_approval; rejection re-mints
// the spec generation so the stage is offered for another attempt instead of
// staying parked behind the rejected receipt.
func (s *Server) decideSpecCheckpointLease(w http.ResponseWriter, r *http.Request, payload RunCheckpointPayload, action string) {
	now := time.Now()
	held, ok := s.heldSpecCheckpointLease(payload.RunKey, now)
	if !ok {
		jsonError(w, errRunCheckpointNotHeld.Error(), http.StatusConflict)
		return
	}
	if held.gen != payload.Gen {
		jsonError(w, "stale checkpoint generation", http.StatusConflict)
		return
	}
	detail := auditDetail("run", payload.RunKey, "gen", strconv.FormatUint(held.gen, 10), "surface", "run_checkpoint")
	switch action {
	case runCheckpointDecisionApprove:
		if _, err := s.contributeHub.advanceLeaseStageAt(held.identity, held.taskID, StagePlan, now, now.Add(time.Millisecond)); err != nil {
			jsonError(w, err.Error(), runResetErrorStatus(err))
			return
		}
		s.recordRunCheckpointApproval(payload.RunKey, payload.PlanEpicID, StageSpec, requestUser(r), held.gen, now, nil)
		s.auditFromRequest(r, "spec_approve", detail, "")
	case runCheckpointDecisionReject:
		if _, err := s.contributeHub.retryLeaseStage(held.identity, held.taskID, now); err != nil {
			jsonError(w, err.Error(), runResetErrorStatus(err))
			return
		}
		s.auditFromRequest(r, "spec_reject", detail, "")
	}
	s.refreshAndPersist()
	jsonResponse(w, map[string]any{"ok": true, "status": action, "run_key": payload.RunKey, "gen": payload.Gen})
}

// RunCheckpointPayload returns the compact checkpoint review payload. Chat and
// notification integrations call this same builder instead of formatting their
// own partial copies of the run state.
func (s *Server) RunCheckpointPayload(key string) (RunCheckpointPayload, error) {
	run, err := s.runCheckpointRun(key)
	if err != nil {
		return RunCheckpointPayload{}, err
	}
	payload := RunCheckpointPayload{
		RunKey:          run.Key,
		Stage:           run.Stage,
		Gen:             run.Gen,
		Repo:            run.Repo,
		Title:           run.Title,
		PlanEpicID:      run.PlanEpicID,
		Summary:         s.runCheckpointSummary(run),
		SummaryMaxBytes: RunCheckpointSummaryMaxBytes,
		DashboardURL:    runCheckpointDashboardURL(run.Key),
		DetailURL:       runCheckpointDashboardURL(run.Key),
		Staleness:       RunCheckpointStaleness{Fence: "lease_gen", Gen: run.Gen},
		Approvers:       RunCheckpointApproverRule{Role: "owner", VerifiedOwnerRequired: true},
	}
	payload.Decisions = []RunCheckpointDecision{
		{Action: runCheckpointDecisionApprove, Label: "Approve checkpoint", Method: http.MethodPost, URL: "/api/runs/" + url.PathEscape(run.Key) + "/checkpoint"},
		{Action: runCheckpointDecisionReject, Label: "Reject checkpoint", Method: http.MethodPost, URL: "/api/runs/" + url.PathEscape(run.Key) + "/checkpoint"},
	}
	payload.Approval = s.runCheckpointApprovalAttrs(run.Key, run.Stage, run.Gen)
	return payload, nil
}

// RunCheckpointNotificationPayload is the shared entry point for push/chat
// surfaces. The surface name is audit context for callers; the payload remains
// identical no matter which surface requested it.
func (s *Server) RunCheckpointNotificationPayload(key, _ string) (RunCheckpointPayload, error) {
	return s.RunCheckpointPayload(key)
}

func (s *Server) runCheckpointApprovalAttrs(runKey, stage string, gen uint64) map[string]string {
	if s == nil {
		return nil
	}
	for _, ev := range s.LifecycleTimeline().ByIssue(runKey) {
		if ev.Kind != timeline.KindStageApproval || ev.Attrs == nil {
			continue
		}
		if ev.Attrs[stageAttrStage] == stage && gen > 0 {
			evGen, _ := strconv.ParseUint(ev.Attrs[stageAttrGen], 10, 64)
			if evGen != 0 && evGen != gen {
				continue
			}
		}
		if ev.Attrs[stageAttrStage] != stage && ev.Attrs[runCheckpointStageAttr(stage, runCheckpointActorKey)] == "" {
			continue
		}
		if gen > 0 {
			stageGen, _ := strconv.ParseUint(ev.Attrs[runCheckpointStageAttr(stage, stageAttrGen)], 10, 64)
			if stageGen != 0 && stageGen != gen {
				continue
			}
		}
		out := map[string]string{}
		for _, key := range []string{runCheckpointActorKey, runCheckpointReasonKey, runCheckpointConfigSourceKey, runCheckpointEpicKey} {
			if v := strings.TrimSpace(firstRunNonEmpty(ev.Attrs[runCheckpointStageAttr(stage, key)], ev.Attrs[key])); v != "" {
				out[key] = v
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func (s *Server) runCheckpointRun(key string) (Run, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Run{}, errors.New("run key required")
	}
	runs, err := s.activeRuns(true)
	if err != nil {
		return Run{}, err
	}
	for _, run := range runs {
		if run.Key != key && run.LeaseKey != key {
			continue
		}
		if run.WaitingOn != RunWaitingOnHuman {
			return Run{}, errRunCheckpointNotHeld
		}
		if run.PlanEpicID == "" {
			if _, held := s.heldSpecCheckpointLease(run.Key, time.Now()); !held {
				return Run{}, errRunCheckpointNotHeld
			}
		}
		return run, nil
	}
	return Run{}, errors.New("run not found")
}

func (s *Server) runCheckpointSummary(run Run) string {
	if run.Stage == StageSpec {
		return s.runCheckpointSpecSummary(run)
	}
	lines := []string{run.Title}
	if run.WaitingReason != "" {
		lines = append(lines, "Waiting: "+run.WaitingReason)
	}
	if store, _ := s.findEpicStore(run.PlanEpicID); store != nil {
		if tree, err := planning.GetPlanTree(store, run.PlanEpicID); err == nil && tree != nil {
			if tree.EpicTitle != "" && tree.EpicTitle != run.Title {
				lines = append(lines, tree.EpicTitle)
			}
			for _, child := range tree.Children {
				text := strings.TrimSpace(child.Title)
				if child.PlanRef != "" {
					text = child.PlanRef + ": " + text
				}
				if child.Repo != "" {
					text += " [" + child.Repo + "]"
				}
				if text != "" {
					lines = append(lines, "- "+text)
				}
			}
		}
	}
	return capUTF8Bytes(strings.Join(lines, "\n"), RunCheckpointSummaryMaxBytes)
}

func (s *Server) runCheckpointSpecSummary(run Run) string {
	lines := []string{firstRunNonEmpty(run.Title, run.Key)}
	if run.WaitingReason != "" {
		lines = append(lines, "Waiting: "+run.WaitingReason)
	}
	lines = append(lines, "Spec detail: "+runCheckpointDashboardURL(run.Key))
	if detail, err := s.buildRunDetail(nil, run.Key); err == nil {
		for _, st := range detail.Stages {
			if st.Name != StageSpec || len(st.Documents) == 0 {
				continue
			}
			doc := st.Documents[0]
			title, sections := markdownTitleAndSections(firstRunNonEmpty(doc.Markdown, doc.Content))
			if title != "" {
				lines = append(lines, "Spec: "+title)
			}
			for _, section := range sections {
				lines = append(lines, "- "+section)
			}
			break
		}
	}
	return capUTF8Bytes(strings.Join(lines, "\n"), RunCheckpointSummaryMaxBytes)
}

func markdownTitleAndSections(markdown string) (string, []string) {
	lines := strings.Split(markdown, "\n")
	title := ""
	sections := []string{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") {
			continue
		}
		text := strings.TrimSpace(strings.TrimLeft(line, "#"))
		if text == "" {
			continue
		}
		if title == "" {
			title = text
			continue
		}
		sections = append(sections, text)
		if len(sections) >= 8 {
			break
		}
	}
	return title, sections
}

func capUTF8Bytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut]
}

func runCheckpointDashboardURL(key string) string {
	return "/?view=runs&run=" + url.QueryEscape(key) + "#checkpoint"
}

func pathRunKey(r *http.Request) string {
	key := strings.TrimSpace(r.PathValue("key"))
	if unescaped, err := url.PathUnescape(key); err == nil {
		key = strings.TrimSpace(unescaped)
	}
	return key
}

func runCheckpointStatus(err error) int {
	switch {
	case errors.Is(err, errRunCheckpointNotHeld):
		return http.StatusConflict
	case strings.Contains(err.Error(), "not found"):
		return http.StatusNotFound
	default:
		return http.StatusServiceUnavailable
	}
}
