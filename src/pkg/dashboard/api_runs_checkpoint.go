package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/planning"
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
	Decisions       []RunCheckpointDecision   `json:"decisions"`
	DashboardURL    string                    `json:"dashboard_url"`
	Staleness       RunCheckpointStaleness    `json:"staleness"`
	Approvers       RunCheckpointApproverRule `json:"approvers"`
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
	store, agentName := s.findEpicStore(payload.PlanEpicID)
	if store == nil {
		jsonError(w, "checkpoint plan not found", http.StatusNotFound)
		return
	}
	switch action {
	case runCheckpointDecisionApprove:
		if err := planning.ApprovePlan(store, payload.PlanEpicID); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.advanceApprovedPlanLease(payload.RunKey, payload.PlanEpicID, requestUser(r), time.Now()); err != nil {
			jsonError(w, err.Error(), http.StatusConflict)
			return
		}
		s.auditFromRequest(r, "plan_approve", auditDetail("epic", payload.PlanEpicID, "run", payload.RunKey, "surface", "run_checkpoint"), agentName)
	case runCheckpointDecisionReject:
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
		Staleness:       RunCheckpointStaleness{Fence: "lease_gen", Gen: run.Gen},
		Approvers:       RunCheckpointApproverRule{Role: "owner", VerifiedOwnerRequired: true},
	}
	payload.Decisions = []RunCheckpointDecision{
		{Action: runCheckpointDecisionApprove, Label: "Approve checkpoint", Method: http.MethodPost, URL: "/api/runs/" + url.PathEscape(run.Key) + "/checkpoint"},
		{Action: runCheckpointDecisionReject, Label: "Reject checkpoint", Method: http.MethodPost, URL: "/api/runs/" + url.PathEscape(run.Key) + "/checkpoint"},
	}
	return payload, nil
}

// RunCheckpointNotificationPayload is the shared entry point for push/chat
// surfaces. The surface name is audit context for callers; the payload remains
// identical no matter which surface requested it.
func (s *Server) RunCheckpointNotificationPayload(key, _ string) (RunCheckpointPayload, error) {
	return s.RunCheckpointPayload(key)
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
		if run.WaitingOn != RunWaitingOnHuman || run.PlanEpicID == "" {
			return Run{}, errRunCheckpointNotHeld
		}
		return run, nil
	}
	return Run{}, errors.New("run not found")
}

func (s *Server) runCheckpointSummary(run Run) string {
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
