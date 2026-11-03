package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/trajectory"
	"github.com/hivecommons/hive/crux/watchdog"
)

// snapshotPaneLines bounds the pane capture a snapshot carries; the
// runtime's filter already prefers the current CLI session's lines.
const snapshotPaneLines = 10000

// watchdogSourcer is what a runtime implements to expose the watchdog seam
// (*agent.Manager does); a runtime without it answers CodeNotSupported on
// the watchdog routes rather than inventing observations.
type watchdogSourcer interface {
	WatchdogSource() watchdog.Source
}

// registerFamilies adds the stage-3 routes (every crux.Runtime family not
// covered by the CLI-shaped routes) to mux.
func (s *Server) registerFamilies(mux *http.ServeMux) {
	for _, r := range []struct {
		method, path string
		h            http.HandlerFunc
	}{
		// Lifecycle
		{"POST", api.PathAgents + "/{name}/start", s.handleStart},
		{"POST", api.PathAgents + "/{name}/restart", s.handleRestart},
		{"POST", api.PathAgents + "/{name}/pause", s.handlePause},
		{"POST", api.PathAgents + "/{name}/resume", s.handleResume},
		{"DELETE", api.PathAgents + "/{name}/restart-count", s.handleResetRestartCount},
		// Observer
		{"GET", api.PathAgents + "/{name}/snapshot", s.handleSnapshot},
		{"GET", api.PathSnapshots, s.handleSnapshots},
		// Kicker
		{"POST", api.PathAgents + "/{name}/kick/validate", s.handleKickValidate},
		{"GET", api.PathAgents + "/{name}/kick", s.handleKickDispatch},
		{"GET", api.PathAgents + "/{name}/kicklogs", s.handleKickLogs},
		{"GET", api.PathAgents + "/{name}/kicklogs/{id}", s.handleKickLog},
		// Definitions
		{"POST", api.PathDefinitions, s.handleAddDefinition},
		{"DELETE", api.PathAgents + "/{name}", s.handleRemoveAgent},
		{"PUT", api.PathAgents + "/{name}/definition", s.handleUpdateDefinition},
		{"POST", api.PathReconcile, s.handleReconcile},
		{"PUT", api.PathAgents + "/{name}/overrides", s.handleOverrides},
		{"PUT", api.PathAgents + "/{name}/pins/{kind}", s.handlePin},
		{"DELETE", api.PathAgents + "/{name}/pins/{kind}", s.handleUnpin},
		{"POST", api.PathModesClear, s.handleModesClear},
		{"POST", api.PathModesSync, s.handleModesSync},
		{"POST", api.PathInference, s.handleInference},
		// Fleet
		{"GET", api.PathFleet, s.handleFleet},
		{"GET", api.PathBreaker, s.handleBreakerState},
		{"PUT", api.PathBreaker, s.handleRestoreBreaker},
		{"POST", api.PathBreakerOn, s.handleBreakerEngage},
		{"POST", api.PathBreakerOff, s.handleBreakerRelease},
		{"PUT", api.PathACMM, s.handleACMM},
		{"POST", api.PathRelaunchBob, s.handleRelaunchBob},
		{"POST", api.PathStartupQueued, s.handleStartupQueued},
		{"POST", api.PathRestartCrashed, s.handleRestartCrashed},
		{"POST", api.PathTokensRefresh, s.handleTokensRefresh},
		{"POST", api.PathKickLogsArchive, s.handleKickLogsArchive},
		// Credentials
		{"POST", api.PathAgents + "/{name}/login-code", s.handleLoginCode},
		{"GET", api.PathCopilotCred, s.handleCopilotGet},
		{"PUT", api.PathCopilotCred, s.handleCopilotPut},
		{"POST", api.PathClaudeReload, s.handleClaudeReload},
		{"GET", api.PathClaudeCred, s.handleClaudeGet},
		{"GET", api.PathOmpHomes, s.handleOmpHomes},
		// Resumer
		{"GET", api.PathAgents + "/{name}/session", s.handleSession},
		{"POST", api.PathAgents + "/{name}/session/kick", s.handleResumeKick},
		// Supervisor
		{"GET", api.PathAgents + "/{name}/supervision", s.handleSupervision},
		{"POST", api.PathAgents + "/{name}/tokens/refresh", s.handleAgentTokenRefresh},
		{"GET", api.PathBackends + "/{backend}/auth", s.handleBackendAuth},
		{"POST", api.PathAgents + "/{name}/nudge", s.handleNudge},
		{"POST", api.PathAudit, s.handleAudit},
		{"GET", api.PathTrajectory, s.handleTrajectory},
		// Restorer
		{"POST", api.PathAgents + "/{name}/seed", s.handleSeed},
		// Authorizer
		{"POST", api.PathAuthorize, s.handleAuthorize},
		// Watchdog seam
		{"GET", api.PathWatchdogAgents, s.handleWatchdogAgents},
		{"GET", api.PathAgents + "/{name}/watchdog", s.handleWatchdogObserve},
		{"PUT", api.PathAgents + "/{name}/watchdog/conditions", s.handleWatchdogConditions},
	} {
		mux.HandleFunc(r.method+" "+r.path, r.h)
	}
}

// resolve resolves the {name} path value and reports whether the runtime
// holds that agent; when it does not, a 404 has been written.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := s.Runtime.ResolveAgent(r.PathValue("name"))
	if p, err := s.Runtime.GetStatusFast(name); err != nil || p == nil {
		s.writeError(w, http.StatusNotFound, api.CodeNotFound, fmt.Sprintf("agent %q not found", name))
		return name, false
	}
	return name, true
}

// readOptionalJSON is readJSON for routes whose body may be empty.
func (s *Server) readOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	return s.readJSON(w, r, v)
}

func (s *Server) noContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// ---- Lifecycle ----

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if err := s.Runtime.Start(s.LaunchContext, name); err != nil {
		s.writeRuntimeError(w, name, "start", err)
		return
	}
	s.publish(api.Event{Type: api.EventStarted, Agent: name})
	s.writeJSON(w, http.StatusOK, s.agentResponse(name))
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var req api.RestartRequest
	if !s.readOptionalJSON(w, r, &req) {
		return
	}
	var err error
	if req.Bootstrap != "" {
		err = s.Runtime.RestartWithBootstrap(s.LaunchContext, name, req.Bootstrap)
	} else {
		err = s.Runtime.Restart(s.LaunchContext, name)
	}
	if err != nil {
		s.writeRuntimeError(w, name, "restart", err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.agentResponse(name))
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var req api.PauseRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Trigger) == "" {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "trigger is required")
		return
	}
	var err error
	switch {
	case req.Cause != nil:
		err = s.Runtime.PauseByCause(name, req.Trigger, req.Reason, req.By, crux.PauseCausation{
			Depth: req.Cause.Depth, HookName: req.Cause.HookName, OriginTransition: req.Cause.OriginTransition,
		})
	case req.By != "":
		err = s.Runtime.PauseBy(name, req.Trigger, req.Reason, req.By)
	default:
		err = s.Runtime.Pause(name, req.Trigger, req.Reason)
	}
	if err != nil {
		s.writeRuntimeError(w, name, "pause", err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.agentResponse(name))
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var req api.ResumeRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Trigger) == "" {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "trigger is required")
		return
	}
	if err := s.Runtime.Resume(s.LaunchContext, name, req.Trigger, req.Reason); err != nil {
		s.writeRuntimeError(w, name, "resume", err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.agentResponse(name))
}

func (s *Server) handleResetRestartCount(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if err := s.Runtime.ResetRestartCount(name); err != nil {
		s.writeRuntimeError(w, name, "reset restart count", err)
		return
	}
	s.noContent(w)
}

// ---- Observer ----

// snapshotOf projects a runtime snapshot onto the wire, without the output
// buffer (a ring buffer with a mutex; the logs route serves its content).
func snapshotOf(name string, p *crux.Process) (api.Snapshot, error) {
	c := *p
	c.OutputBuffer = nil
	raw, err := json.Marshal(&c)
	if err != nil {
		return api.Snapshot{}, err
	}
	return api.Snapshot{Agent: name, Process: raw, Pane: p.PaneLines(snapshotPaneLines)}, nil
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	name := s.Runtime.ResolveAgent(r.PathValue("name"))
	var (
		p   *crux.Process
		err error
	)
	if isTrue(r.URL.Query().Get(api.QueryFast)) {
		p, err = s.Runtime.GetStatusFast(name)
	} else {
		p, err = s.Runtime.GetStatus(name)
	}
	if err != nil || p == nil {
		s.writeError(w, http.StatusNotFound, api.CodeNotFound, fmt.Sprintf("agent %q not found", name))
		return
	}
	snap, err := snapshotOf(name, p)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, "snapshot "+name+": "+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleSnapshots(w http.ResponseWriter, _ *http.Request) {
	statuses := s.Runtime.AllStatuses()
	out := api.SnapshotList{Snapshots: make([]api.Snapshot, 0, len(statuses))}
	for name, p := range statuses {
		if p == nil {
			continue
		}
		snap, err := snapshotOf(name, p)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, api.CodeRuntime, "snapshot "+name+": "+err.Error())
			return
		}
		out.Snapshots = append(out.Snapshots, snap)
	}
	sort.Slice(out.Snapshots, func(i, j int) bool { return out.Snapshots[i].Agent < out.Snapshots[j].Agent })
	s.writeJSON(w, http.StatusOK, out)
}

// ---- Kicker ----

func (s *Server) handleKickValidate(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if err := s.Runtime.ValidateKick(name); err != nil {
		s.writeError(w, http.StatusConflict, api.CodeKickRefused, err.Error())
		return
	}
	s.noContent(w)
}

func (s *Server) handleKickDispatch(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	out := api.KickDispatchResponse{Agent: name}
	if d, ok := s.Runtime.KickDispatchState(name); ok {
		out.Dispatch = &api.KickDispatch{Agent: d.Agent, Phase: d.Phase, Error: d.Error, QueuedAt: d.QueuedAt, SettledAt: d.SettledAt}
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleKickLogs(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	infos, err := s.Runtime.ListKickLogs(name)
	if err != nil {
		s.writeRuntimeError(w, name, "kick logs", err)
		return
	}
	out := api.KickLogList{Agent: name, Logs: make([]api.KickLogInfo, 0, len(infos))}
	for _, in := range infos {
		out.Logs = append(out.Logs, api.KickLogInfo{ID: in.ID, Timestamp: in.Timestamp, SizeBytes: in.SizeBytes, Reason: in.Reason, Source: in.Source})
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleKickLog(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	content, err := s.Runtime.ReadKickLog(name, id)
	if err != nil {
		s.writeRuntimeError(w, name, "kick log "+id, err)
		return
	}
	s.writeJSON(w, http.StatusOK, api.KickLog{Agent: name, ID: id, Content: content})
}

// ---- Definitions ----

// decodeVerbatim parses a host-supplied config.AgentConfig without filling
// defaults: the host owns the complete definition.
func decodeVerbatim(raw json.RawMessage) (config.AgentConfig, error) {
	var cfg config.AgentConfig
	if len(raw) == 0 || string(raw) == "null" {
		return cfg, errors.New("definition is required")
	}
	err := json.Unmarshal(raw, &cfg)
	return cfg, err
}

func (s *Server) handleAddDefinition(w http.ResponseWriter, r *http.Request) {
	var req api.DefinitionRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "name is required")
		return
	}
	cfg, err := decodeVerbatim(req.Definition)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "definition: "+err.Error())
		return
	}
	status := http.StatusCreated
	if _, err := s.Runtime.GetStatusFast(req.Name); err == nil {
		status = http.StatusOK
	}
	s.Runtime.AddAgent(req.Name, cfg)
	p, err := s.Runtime.GetStatusFast(req.Name)
	if err != nil || p == nil {
		s.writeError(w, http.StatusUnprocessableEntity, api.CodeBadRequest,
			fmt.Sprintf("agent %q was not accepted by the runtime", req.Name))
		return
	}
	s.writeJSON(w, status, toWire(p))
}

func (s *Server) handleRemoveAgent(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	s.Runtime.RemoveAgent(name)
	s.noContent(w)
}

func (s *Server) handleUpdateDefinition(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var req api.DefinitionRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	cfg, err := decodeVerbatim(req.Definition)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "definition: "+err.Error())
		return
	}
	if err := s.Runtime.UpdateConfig(name, cfg); err != nil {
		s.writeRuntimeError(w, name, "update definition", err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.agentResponse(name))
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	var req api.ReconcileRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	configs := make(map[string]config.AgentConfig, len(req.Definitions))
	for name, raw := range req.Definitions {
		cfg, err := decodeVerbatim(raw)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, fmt.Sprintf("definition %q: %v", name, err))
			return
		}
		configs[name] = cfg
	}
	s.writeJSON(w, http.StatusOK, api.Names{Names: names(s.Runtime.ReconcileAgents(configs))})
}

func names(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func (s *Server) handleOverrides(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var req api.OverridesRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.Model == nil && req.Backend == nil {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "model or backend is required")
		return
	}
	if req.Model != nil {
		if err := s.Runtime.SetModelOverride(name, *req.Model); err != nil {
			s.writeRuntimeError(w, name, "model override", err)
			return
		}
	}
	if req.Backend != nil {
		if err := s.Runtime.SetBackendOverride(name, *req.Backend); err != nil {
			s.writeRuntimeError(w, name, "backend override", err)
			return
		}
	}
	s.writeJSON(w, http.StatusOK, s.agentResponse(name))
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var req api.PinRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Value) == "" {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "value is required")
		return
	}
	var err error
	switch kind := r.PathValue("kind"); kind {
	case api.PinCLI:
		err = s.Runtime.PinCLI(name, req.Value)
	case api.PinModel:
		err = s.Runtime.PinModel(name, req.Value)
	default:
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, fmt.Sprintf("pin kind must be %s or %s", api.PinCLI, api.PinModel))
		return
	}
	if err != nil {
		s.writeRuntimeError(w, name, "pin", err)
		return
	}
	s.noContent(w)
}

func (s *Server) handleUnpin(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var err error
	switch kind := r.PathValue("kind"); kind {
	case api.PinCLI:
		err = s.Runtime.UnpinCLI(name)
	case api.PinModel:
		err = s.Runtime.UnpinModel(name)
	default:
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, fmt.Sprintf("pin kind must be %s or %s", api.PinCLI, api.PinModel))
		return
	}
	if err != nil {
		s.writeRuntimeError(w, name, "unpin", err)
		return
	}
	s.noContent(w)
}

func (s *Server) handleModesClear(w http.ResponseWriter, r *http.Request) {
	var req api.Names
	if !s.readJSON(w, r, &req) {
		return
	}
	s.Runtime.ClearModeOverrides(req.Names)
	s.noContent(w)
}

func (s *Server) handleModesSync(w http.ResponseWriter, r *http.Request) {
	var req api.Level
	if !s.readJSON(w, r, &req) {
		return
	}
	s.Runtime.SyncModeFiles(req.Level)
	s.noContent(w)
}

func (s *Server) handleInference(w http.ResponseWriter, r *http.Request) {
	var req api.Backend
	if !s.readJSON(w, r, &req) {
		return
	}
	s.Runtime.RefreshInferenceRoutes(req.Backend)
	s.noContent(w)
}

// ---- Fleet ----

func (s *Server) handleFleet(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, api.FleetInfo{
		ACMMLevel:       s.Runtime.GetACMMLevel(),
		SharedDevUID:    s.Runtime.SharedDevUID(),
		AgentsWithModel: s.Runtime.CountAgentsWithModel(),
		ConsentWedged:   names(s.Runtime.ConsentWedgedAgents()),
	})
}

func (s *Server) handleBreakerState(w http.ResponseWriter, _ *http.Request) {
	engaged, paused := s.Runtime.BreakerState()
	s.writeJSON(w, http.StatusOK, api.Breaker{Engaged: engaged, Paused: names(paused)})
}

func (s *Server) handleRestoreBreaker(w http.ResponseWriter, r *http.Request) {
	var req api.Breaker
	if !s.readJSON(w, r, &req) {
		return
	}
	s.Runtime.RestoreBreaker(req.Engaged, req.Paused)
	s.noContent(w)
}

func (s *Server) handleBreakerEngage(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, api.Names{Names: names(s.Runtime.EngageBreaker())})
}

func (s *Server) handleBreakerRelease(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, api.Names{Names: names(s.Runtime.ReleaseBreaker(s.LaunchContext))})
}

func (s *Server) handleACMM(w http.ResponseWriter, r *http.Request) {
	var req api.Level
	if !s.readJSON(w, r, &req) {
		return
	}
	s.Runtime.SetACMMLevel(req.Level)
	s.noContent(w)
}

func (s *Server) handleRelaunchBob(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, api.Names{Names: names(s.Runtime.RelaunchBobAgentsAwaitingKey(s.LaunchContext))})
}

func (s *Server) handleStartupQueued(w http.ResponseWriter, r *http.Request) {
	var req api.Names
	if !s.readJSON(w, r, &req) {
		return
	}
	s.Runtime.MarkStartupLaunchQueued(req.Names)
	s.noContent(w)
}

func (s *Server) handleRestartCrashed(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, api.Names{Names: names(s.Runtime.CheckAndRestartCrashedAgents(s.LaunchContext))})
}

func (s *Server) handleTokensRefresh(w http.ResponseWriter, _ *http.Request) {
	s.Runtime.RefreshAgentTokens(s.LaunchContext)
	s.noContent(w)
}

func (s *Server) handleKickLogsArchive(w http.ResponseWriter, r *http.Request) {
	var req api.Reason
	if !s.readJSON(w, r, &req) {
		return
	}
	s.Runtime.ArchiveAllKickLogs(req.Reason)
	s.noContent(w)
}

// ---- Credentials ----

func (s *Server) handleLoginCode(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var req api.LoginCodeRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "code is required")
		return
	}
	if err := s.Runtime.SubmitLoginCode(name, req.Code); err != nil {
		s.writeRuntimeError(w, name, "login code", err)
		return
	}
	s.noContent(w)
}

func (s *Server) handleCopilotGet(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, api.CopilotCredential{Token: s.Runtime.CopilotToken(), Source: s.Runtime.CopilotTokenSource()})
}

func (s *Server) handleCopilotPut(w http.ResponseWriter, r *http.Request) {
	var req api.CopilotCredential
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.Activate {
		if err := s.Runtime.ActivateCopilotToken(req.Token); err != nil {
			s.writeError(w, http.StatusUnprocessableEntity, api.CodeRuntime, "activate copilot token: "+err.Error())
			return
		}
	} else {
		s.Runtime.SetCopilotToken(req.Token)
	}
	s.noContent(w)
}

func (s *Server) handleClaudeReload(w http.ResponseWriter, _ *http.Request) {
	s.Runtime.ReloadClaudeToken()
	s.noContent(w)
}

func (s *Server) handleClaudeGet(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, api.ClaudeCredential{CandidatePaths: names(s.Runtime.ClaudeCredentialCandidatePaths())})
}

func (s *Server) handleOmpHomes(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, api.OmpHomes{Homes: names(s.Runtime.OmpAgentHomes())})
}

// ---- Resumer ----

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	out := api.Session{Agent: name, Missing: s.Runtime.SessionMissing(name)}
	out.SessionID, out.HasSession = s.Runtime.SessionID(name)
	if h, ok := s.Runtime.ResumeHandle(name); ok {
		out.Handle = &api.ResumeHandle{Backend: h.Backend, SessionID: h.SessionID, Transcript: h.Transcript, Command: h.Command, LastActive: h.LastActive, CapturedAt: h.CapturedAt}
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleResumeKick maps the Resumer sentinels onto statuses: busy is a 409
// (retry later), a gone session a 410, an agent that cannot resume a 422.
func (s *Server) handleResumeKick(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var req api.ResumeKickRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Message) == "" || strings.TrimSpace(req.SessionID) == "" {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "message and session_id are required")
		return
	}
	err := s.Runtime.SendResumeKick(name, req.Message, req.SessionID)
	switch {
	case err == nil:
		s.publish(api.Event{Type: api.EventKicked, Agent: name, Detail: "resume"})
		s.writeJSON(w, http.StatusOK, s.agentResponse(name))
	case errors.Is(err, crux.ErrResumeBusy):
		s.writeError(w, http.StatusConflict, api.CodeResumeBusy, err.Error())
	case errors.Is(err, crux.ErrResumeSessionGone):
		s.writeError(w, http.StatusGone, api.CodeResumeSessionGone, err.Error())
	case errors.Is(err, crux.ErrResumeUnsupported):
		s.writeError(w, http.StatusUnprocessableEntity, api.CodeResumeUnsupported, err.Error())
	default:
		s.writeRuntimeError(w, name, "resume kick", err)
	}
}

// ---- Supervisor ----

func (s *Server) handleSupervision(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	rt := s.Runtime
	out := api.Supervision{
		Agent:              name,
		MayStart:           rt.GovernorMayStart(name),
		HasValidCredential: rt.AgentHasValidCredential(name),
		CrashOOMSuspected:  rt.CrashOOMSuspected(name),
	}
	out.CredentialRefreshable, out.CredentialKnown = rt.AgentCredentialRefreshable(name)
	out.AuthAvailable, out.AuthKnown = rt.AgentAuthAvailable(name)
	if issue, pr, merge, ok := rt.AgentCapabilities(name); ok {
		out.Capabilities = &api.Capabilities{CanOpenIssue: issue, CanOpenPR: pr, CanMerge: merge}
	}
	if b, ok := rt.EffectiveBackend(name); ok {
		out.Backend = &b
	}
	if b, m, e, ok := rt.InvocationMetadata(name); ok {
		out.Invocation = &api.Invocation{Backend: b, Model: m, Effort: e}
	}
	if total, last24h, at, reason, ok := rt.RestartTelemetry(name); ok {
		out.Restarts = &api.RestartTelemetry{Total: total, Last24h: last24h, LastRestartAt: at, LastReason: reason}
	}
	if f, ok := rt.StartFailureState(name); ok {
		out.StartFailure = &api.StartFailureSnapshot{Reason: f.Reason, Count: f.Count, LastAt: f.LastAt, LastExitCode: f.LastExitCode, LastSignal: f.LastSignal, Blocked: f.Blocked}
	}
	if d, provider, reason, ok := rt.ProviderErrorBackoffRemaining(name); ok {
		out.ProviderBackoff = &api.ProviderBackoff{RemainingMS: d.Milliseconds(), Provider: provider, Reason: reason}
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAgentTokenRefresh(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	if err := s.Runtime.RefreshAgentTokenFor(s.LaunchContext, name); err != nil {
		s.writeRuntimeError(w, name, "refresh token", err)
		return
	}
	s.noContent(w)
}

func (s *Server) handleBackendAuth(w http.ResponseWriter, r *http.Request) {
	backend := r.PathValue("backend")
	available, known := s.Runtime.BackendAuthAvailable(backend)
	s.writeJSON(w, http.StatusOK, api.BackendAuth{Backend: backend, Available: available, Known: known})
}

func (s *Server) handleNudge(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var req api.NudgeRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "message is required")
		return
	}
	s.Runtime.SendGitHubAPIBudgetNudge(name, req.Message)
	s.noContent(w)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	var req api.AuditRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Action) == "" {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "action is required")
		return
	}
	s.Runtime.RecordAudit(req.Action, req.Agent, req.Fields)
	s.noContent(w)
}

func (s *Server) handleTrajectory(w http.ResponseWriter, _ *http.Request) {
	views := s.Runtime.TrajectoryAgents()
	out := api.TrajectoryList{Agents: make([]api.TrajectoryAgent, 0, len(views))}
	for _, v := range views {
		out.Agents = append(out.Agents, trajectoryWire(v))
	}
	s.writeJSON(w, http.StatusOK, out)
}

func trajectoryWire(v trajectory.AgentView) api.TrajectoryAgent {
	return api.TrajectoryAgent{Name: v.Name, Running: v.Running, Intent: v.Intent, Transcript: v.Transcript, Idle: v.Idle, LastOutputAt: v.LastOutputAt}
}

// ---- Restorer ----

func (s *Server) handleSeed(w http.ResponseWriter, r *http.Request) {
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var req api.SeedRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	var loss crux.TurnLoss
	if len(req.TurnLoss) > 0 {
		if err := json.Unmarshal(req.TurnLoss, &loss); err != nil {
			s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "turn_loss: "+err.Error())
			return
		}
	}
	if req.Pause != nil {
		s.Runtime.SeedPauseState(name, req.Pause.PausedAt, req.Pause.Trigger, req.Pause.Reason, req.Pause.By)
	}
	if req.Restarts != nil {
		events := make([]crux.RestartEvent, 0, len(req.Restarts.Events))
		for _, e := range req.Restarts.Events {
			events = append(events, crux.RestartEvent{At: e.At, Reason: e.Reason})
		}
		s.Runtime.SeedRestartTelemetry(name, req.Restarts.Count, events, req.Restarts.LastReason)
	}
	if len(req.TurnLoss) > 0 {
		s.Runtime.SeedTurnLoss(name, loss)
	}
	if req.LastKick != nil {
		s.Runtime.SeedLastKick(name, *req.LastKick)
	}
	if req.KickHistory != nil {
		records := make([]crux.KickRecord, 0, len(*req.KickHistory))
		for _, k := range *req.KickHistory {
			records = append(records, crux.KickRecord{Timestamp: k.Timestamp, Agent: k.Agent, Snippet: k.Snippet})
		}
		s.Runtime.SeedKickHistory(name, records)
	}
	s.noContent(w)
}

// ---- Authorizer ----

// handleAuthorize answers 204 when the runtime permits the action and 403
// with the runtime's reason when it does not. An unknown action is a 400,
// which a client must treat as a denial.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	var req api.AuthorizeRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Agent) == "" {
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, "agent is required")
		return
	}
	var err error
	switch req.Action {
	case api.AuthorizePROpen:
		err = s.Runtime.AuthorizePROpen(req.Agent, req.FileUID)
	case api.AuthorizePushBranch:
		err = s.Runtime.AuthorizePushBranch(req.Agent, req.FileUID)
	case api.AuthorizeIssueOpen:
		err = s.Runtime.AuthorizeIssueOpen(req.Agent, req.FileUID, req.Kind)
	case api.AuthorizeMerge:
		err = s.Runtime.AuthorizeMerge(req.Agent, req.FileUID, req.Repo)
	case api.AuthorizeReviewRequest:
		err = s.Runtime.AuthorizeReviewRequest(req.Agent, req.FileUID)
	default:
		s.writeError(w, http.StatusBadRequest, api.CodeBadRequest, fmt.Sprintf("unknown action %q", req.Action))
		return
	}
	if err != nil {
		s.writeError(w, http.StatusForbidden, api.CodeForbidden, err.Error())
		return
	}
	s.noContent(w)
}

// ---- Watchdog seam ----

func (s *Server) watchdogSource(w http.ResponseWriter) (watchdog.Source, bool) {
	ws, ok := s.Runtime.(watchdogSourcer)
	if !ok {
		s.writeError(w, http.StatusNotImplemented, api.CodeNotSupported, "runtime does not expose a watchdog source")
		return nil, false
	}
	src := ws.WatchdogSource()
	if src == nil {
		s.writeError(w, http.StatusNotImplemented, api.CodeNotSupported, "runtime does not expose a watchdog source")
		return nil, false
	}
	return src, true
}

func (s *Server) handleWatchdogAgents(w http.ResponseWriter, _ *http.Request) {
	src, ok := s.watchdogSource(w)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, api.Names{Names: names(src.AgentNames())})
}

func (s *Server) handleWatchdogObserve(w http.ResponseWriter, r *http.Request) {
	src, ok := s.watchdogSource(w)
	if !ok {
		return
	}
	name := s.Runtime.ResolveAgent(r.PathValue("name"))
	o, err := src.Observe(name)
	if err != nil {
		s.writeRuntimeError(w, name, "observe", err)
		return
	}
	out := api.WatchdogObservation{
		Agent: name, Backend: o.Backend, SessionExists: o.SessionExists, Pane: o.Pane,
		HasCLIMarker: o.HasCLIMarker, ShowsLoginPrompt: o.ShowsLoginPrompt, LastChange: o.LastChange,
		StartedAt: o.StartedAt, AuthAvailable: o.AuthAvailable, AuthKnown: o.AuthKnown,
		CredentialProven: o.CredentialProven, ProviderErrorClass: o.ProviderErrorClass, ProviderErrorLine: o.ProviderErrorLine,
	}
	if t, ok := src.LastProduction(name); ok {
		out.LastProduction = &t
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleWatchdogConditions(w http.ResponseWriter, r *http.Request) {
	src, ok := s.watchdogSource(w)
	if !ok {
		return
	}
	name, ok := s.resolve(w, r)
	if !ok {
		return
	}
	var req api.Conditions
	if !s.readJSON(w, r, &req) {
		return
	}
	conds := make([]crux.Condition, 0, len(req.Conditions))
	for _, c := range req.Conditions {
		conds = append(conds, crux.Condition{
			Type: crux.ConditionType(c.Type), Status: crux.ConditionStatus(c.Status),
			Reason: c.Reason, Message: c.Message, LastTransitionTime: c.LastTransitionTime,
		})
	}
	src.SetConditions(name, conds)
	s.noContent(w)
}

// ---- Host observers → /v1/events ----

// backgroundLoops are the crux.Background methods, in the order Serve starts
// them and Health reports them.
var backgroundLoops = []string{"StartAgentTokenRefresh", "StartCredentialWatchdog", "StartCopilotSessionRefresh", "StartTranscriptActivityRefresh", "StartRepeatFailureMonitor"}

// publishData publishes an event whose Data is v's JSON.
func (s *Server) publishData(typ, agent string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		s.Logger.Warn("crux serve: encoding event", "type", typ, "err", err)
		return
	}
	s.publish(api.Event{Type: typ, Agent: agent, Data: raw})
}

// installHostObservers routes the crux.Host observer callbacks onto the
// event stream so a sidecar host sees what an in-process host would be
// called with. AuditSink and ConditionReporter are installed as tees:
// events always flow, and the Server's own sinks (when set) see the calls
// too. It returns the uninstaller.
func (s *Server) installHostObservers() func() {
	host, ok := s.Runtime.(crux.Host)
	if !ok {
		return func() {}
	}
	host.SetKickOutcomeObserver(func(agent string, o crux.KickOutcome) {
		s.publishData(api.EventKickOutcome, agent, api.KickOutcome{Kind: o.Kind, Reason: o.Reason, At: o.At, KickAt: o.KickAt})
	})
	host.SetPauseTransitionObserver(func(ev crux.PauseTransitionEvent) {
		s.publishData(api.EventPauseTransition, ev.Agent, api.PauseTransition{
			Paused: ev.Paused, Trigger: ev.Trigger, Reason: ev.Reason, By: ev.By, At: ev.At,
			Causation: api.PauseCausation{Depth: ev.Causation.Depth, HookName: ev.Causation.HookName, OriginTransition: ev.Causation.OriginTransition},
		})
	})
	host.SetRelaunchObserver(func(agent string) {
		s.publish(api.Event{Type: api.EventRelaunch, Agent: agent})
	})
	host.SetPersistPauseCallback(func(agent string, paused bool) {
		s.publishData(api.EventPausePersist, agent, api.PausePersist{Paused: paused})
	})
	host.SetRecordPromptCallback(func(agent, trigger, prompt string) {
		s.publishData(api.EventPrompt, agent, api.Prompt{Trigger: trigger, Prompt: prompt})
	})
	host.SetSandboxAuditCallback(func(agent, action, detail string) {
		s.publishData(api.EventSandboxAudit, agent, api.SandboxAudit{Action: action, Detail: detail})
	})
	host.SetAuditSink(auditTee{s})
	host.SetConditionReporter(conditionTee{s})
	return func() {
		host.SetKickOutcomeObserver(nil)
		host.SetPauseTransitionObserver(nil)
		host.SetRelaunchObserver(nil)
		host.SetPersistPauseCallback(nil)
		host.SetRecordPromptCallback(nil)
		host.SetSandboxAuditCallback(nil)
	}
}

type auditTee struct{ s *Server }

func (t auditTee) Record(actor, action, agentName string, fields map[string]any) {
	t.s.publishData(api.EventAudit, agentName, api.Audit{Actor: actor, Action: action, Fields: fields})
	if t.s.AuditSink != nil {
		t.s.AuditSink.Record(actor, action, agentName, fields)
	}
}

// startBackground runs the crux.Background loops under ctx when the Server
// is configured to, and returns the names Health reports.
func (s *Server) startBackground(ctx context.Context) {
	if !s.Background {
		return
	}
	rt := s.Runtime
	go rt.StartAgentTokenRefresh(ctx)
	go rt.StartCredentialWatchdog(ctx)
	go rt.StartCopilotSessionRefresh(ctx)
	go rt.StartTranscriptActivityRefresh(ctx)
	go rt.StartRepeatFailureMonitor(ctx)
	s.mu.Lock()
	s.background = append([]string(nil), backgroundLoops...)
	s.mu.Unlock()
}
