package dashboard

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
)

func (s *Server) handleKick(w http.ResponseWriter, r *http.Request) {
	// Owner-only: the kick prompt is typed verbatim into the agent's CLI
	// session, and agents execute shell commands with App-scoped credentials.
	// Without this gate any read-write contributor could inject arbitrary
	// prompts into any agent (#6557).
	if !requireOwnerRole(w, r) {
		return
	}
	name := s.resolveAgentParam(r.PathValue("agent"))
	var body struct {
		Prompt  string `json:"prompt"`
		Message string `json:"message"`
	}
	if err := decodeBody(r, &body); err != nil {
		s.deps.Logger.Debug("kick body decode failed, using auto-generated message", "agent", name, "error", err)
	}

	msg := body.Prompt
	if msg == "" {
		msg = body.Message
	}

	const maxKickPromptLen = 10000
	if len(msg) > maxKickPromptLen {
		jsonError(w, fmt.Sprintf("prompt too long (%d chars, max %d)", len(msg), maxKickPromptLen), http.StatusBadRequest)
		return
	}

	deliverKick := func(kickMsg string) (bool, error) {
		return s.deps.AgentMgr.SendKickAsync(name, kickMsg)
	}

	if msg == "" && s.deps.Scheduler != nil {
		if !s.deps.Scheduler.FirstScanDone() {
			if err := s.deps.AgentMgr.ValidateKick(name); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.deps.Scheduler.DeferKickUntilFirstScan(name, func(kickMsg string) {
				started, err := deliverKick(kickMsg)
				if err != nil {
					s.deps.Logger.Error("deferred manual kick failed", "agent", name, "error", err)
					return
				}
				if !started {
					s.deps.Logger.Info("deferred manual kick already in flight", "agent", name)
					return
				}
				s.deps.Governor.RecordKick(name)
				s.deps.Logger.Info("audit: deferred agent kick delivered", "agent", name, "trigger", "dashboard-api")
				s.refreshAfterMutation()
			})
			s.deps.Logger.Info("audit: agent kick deferred", "agent", name, "trigger", "dashboard-api", "reason", "waiting for first governor scan")
			s.auditFromRequest(r, "kick_deferred", "", name)
			jsonStatusResponse(w, http.StatusAccepted, map[string]interface{}{
				"agent": name, "ok": true, "status": kickStatusDeferred, "reason": "waiting for first governor scan",
			})
			return
		}
		msg = s.deps.Scheduler.BuildAgentMessageFromLastActionable(name)
	}

	// Queue the kick and answer immediately (#5325).
	//
	// The old code called the synchronous SendKick inline. Its slow leg waits
	// for the CLI's input prompt for up to inputPromptTimeout (120s), which
	// exceeds a typical ingress idle timeout (commonly 60s) — so a kick to an
	// agent whose CLI was merely slow to present its prompt was answered by the
	// proxy with 504 while the wait was still running. The wait then completed,
	// the prompt WAS typed, and the agent ran the session; the operator had
	// been told it failed, and the natural retry delivered the work twice.
	//
	// SendKickAsync keeps every fast, deterministic precondition synchronous —
	// unknown agent, paused/stopped, missing tmux session, sandbox rejection
	// still return 400 here — and moves only the prompt wait and the typing to
	// a background goroutine with an exactly-once in-flight guard. The outcome
	// is reported by GET /api/kick/{agent}/status, off the request path.
	started, err := deliverKick(msg)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !started {
		// A delivery for this agent is already in flight. Answering 202 with
		// status "in-flight" is what makes an operator's retry harmless: the
		// prompt is delivered exactly once regardless of how many times Kick
		// is clicked.
		jsonStatusResponse(w, http.StatusAccepted, map[string]interface{}{
			"ok": true, "status": kickStatusInFlight, "agent": name,
			"message": "a kick is already being delivered to " + name + "; not sending it twice",
		})
		return
	}

	s.deps.Governor.RecordKick(name)
	s.deps.Logger.Info("audit: agent kicked", "agent", name, "trigger", "dashboard-api")
	s.auditFromRequest(r, "kick", "", name)
	s.refreshAfterMutation()
	// 202, not 200: the message is queued, not yet proven delivered.
	jsonStatusResponse(w, http.StatusAccepted, map[string]interface{}{
		"ok": true, "status": kickStatusQueued, "agent": name,
	})
}

// Kick dispatch statuses on the wire. "queued"/"in-flight" are the POST's
// answers; the poll adds the terminal "delivered" and "failed".
const (
	kickStatusQueued    = "queued"
	kickStatusDeferred  = "deferred"
	kickStatusInFlight  = "in-flight"
	kickStatusUnknown   = "unknown"
	kickStatusDelivered = "delivered"
	kickStatusFailed    = "failed"
)

// handleKickStatus reports the outcome of the most recent asynchronous kick
// for an agent (#5325).
//
// This is where kick success or failure is now decided. The POST only promises
// the kick was queued; a client learns whether the prompt actually reached the
// CLI by polling here. While the phase is "queued"/"in-flight" the outcome is
// INDETERMINATE — pending is not failure, and a UI must not render it as one.
//
// Read-only, so any authenticated role may call it.
func (s *Server) handleKickStatus(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	if s.deps == nil || s.deps.AgentMgr == nil {
		jsonError(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}
	d, ok := s.deps.AgentMgr.KickDispatchState(name)
	if !ok {
		// No async kick has been dispatched for this agent in this process's
		// lifetime. That is not an error — it is simply "nothing to report".
		jsonResponse(w, map[string]interface{}{
			"ok": true, "agent": name, "status": kickStatusUnknown, "pending": false,
		})
		return
	}
	resp := map[string]interface{}{
		"ok":       true,
		"agent":    name,
		"status":   kickPhaseStatus(d.Phase),
		"pending":  d.Pending(),
		"queuedAt": d.QueuedAt.UTC().Format(time.RFC3339),
	}
	if d.Error != "" {
		resp["error"] = d.Error
	}
	if !d.SettledAt.IsZero() {
		resp["settledAt"] = d.SettledAt.UTC().Format(time.RFC3339)
	}
	jsonResponse(w, resp)
}

// kickPhaseStatus maps a manager dispatch phase onto the wire status. The
// pending phase is reported as "in-flight" so the poll's vocabulary matches the
// POST's, and so no client can mistake it for a settled outcome.
func kickPhaseStatus(phase string) string {
	switch phase {
	case agent.KickPhaseDelivered:
		return kickStatusDelivered
	case agent.KickPhaseFailed:
		return kickStatusFailed
	default:
		return kickStatusInFlight
	}
}

// claimAgentFieldOwnership writes an operator's model and/or backend choice
// into hive.yaml and the per-agent overlay, and marks those fields
// operator-owned. Empty arguments leave the corresponding field untouched.
//
// This is the durability half of the model/method revert fix. The in-memory
// ModelOverride/BackendOverride on the agent process is replayed from
// /data/hive-state.json on restart, but the saved config still carried the
// PACK's model — and ApplyPack re-reconciles from the pack on every restart.
// For managed agents the per-agent overlay replaces the hive.yaml entry on
// every config load, so both persistent layers must receive the operator's
// value and ownership marker for the choice to actually survive.
func (s *Server) claimAgentFieldOwnership(name, model, backend string) {
	if s.deps == nil || s.deps.Config == nil {
		return
	}
	ac, ok := s.deps.Config.Agents[name]
	if !ok {
		return
	}
	if model != "" {
		ac.Model = model
		ac.ModelOwner = config.FieldOwnerOperator
	}
	if backend != "" {
		ac.Backend = backend
		ac.BackendOwner = config.FieldOwnerOperator
	}
	s.deps.Config.Agents[name] = ac
	_ = s.deps.AgentMgr.UpdateConfig(name, ac)
	if err := s.saveConfig(); err != nil {
		s.deps.Logger.Error("failed to persist agent model/method choice", "agent", name, "error", err)
		s.AddSystemAlert("agent-field-save-failed", "error",
			"Could not save the model/method choice for "+name+" — it will revert on the next restart: "+err.Error())
		return
	}
	if agentsDir := s.deps.Config.Data.AgentsDir; agentsDir != "" {
		if err := config.SaveAgentFile(agentsDir, name, ac); err != nil {
			s.deps.Logger.Error("failed to persist agent overlay after model/method choice", "agent", name, "error", err)
			s.AddSystemAlert("agent-field-save-failed", "error",
				"Could not save the model/method choice for "+name+" to its agent overlay — it will revert on the next config load: "+err.Error())
			return
		}
	}
	s.ClearSystemAlert("agent-field-save-failed")
}

// claimAgentPauseOwnership marks name's pause/run state operator-owned and
// persists the marker to hive.yaml and the per-agent overlay — the same
// two-layer durability as claimAgentFieldOwnership above, and for the same
// reason: for managed agents the overlay replaces the hive.yaml entry on every
// config load, so a marker written to only one layer does not survive. An
// operator-owned pause state makes the agent immune to the ACMM pack
// visibility sweep's "agent not in pack level N" pause (#5706). A no-op when
// the claim is already recorded, so a routine resume does not rewrite config.
func (s *Server) claimAgentPauseOwnership(name string) {
	if s.deps == nil || s.deps.Config == nil {
		return
	}
	ac, ok := s.deps.Config.Agents[name]
	if !ok || ac.PauseIsOperatorOwned() {
		return
	}
	ac.PauseOwner = config.FieldOwnerOperator
	s.deps.Config.Agents[name] = ac
	_ = s.deps.AgentMgr.UpdateConfig(name, ac)
	if err := s.saveConfig(); err != nil {
		s.deps.Logger.Error("failed to persist pause-state ownership", "agent", name, "error", err)
		s.AddSystemAlert("agent-pause-owner-save-failed", "error",
			"Could not record that you resumed "+name+" — an ACMM pack apply may re-pause it on the next restart: "+err.Error())
		return
	}
	if ac.Managed {
		if agentsDir := s.deps.Config.Data.AgentsDir; agentsDir != "" {
			if err := config.SaveAgentFile(agentsDir, name, ac); err != nil {
				s.deps.Logger.Error("failed to persist pause-state ownership to agent overlay", "agent", name, "error", err)
				s.AddSystemAlert("agent-pause-owner-save-failed", "error",
					"Could not record that you resumed "+name+" in its agent overlay — an ACMM pack apply may re-pause it on the next config load: "+err.Error())
				return
			}
		}
	}
	s.ClearSystemAlert("agent-pause-owner-save-failed")
}

// validateModelForAgent rejects a model the agent's effective backend does not
// offer, so an unhonorable choice surfaces as a 400 the operator can see
// instead of silently degrading to a default at launch time.
//
// Backends whose model list cannot be enumerated (no reachable endpoint, or a
// backend that accepts free-form model ids) are allowed through: refusing a
// value we simply cannot verify would block legitimate configurations.
func (s *Server) validateModelForAgent(name, model string) error {
	if model == "" {
		return fmt.Errorf("model must not be empty")
	}
	proc, err := s.deps.AgentMgr.GetStatus(name)
	if err != nil || proc == nil {
		// Agent lookup failures are reported by the caller's SetModelOverride.
		return nil
	}
	backend := proc.Config.Backend
	if proc.BackendOverride != "" {
		backend = proc.BackendOverride
	}

	known := s.modelIDsForBackend(backend)
	if len(known) == 0 {
		return nil // cannot enumerate — do not block
	}
	// Copilot catalog/CLI nomenclature drifts between "." and "-" (#4262):
	// the served list is canonical, so compare the canonicalized candidate
	// rather than rejecting a dotted/dashed variant of an available model.
	if backend == "copilot" {
		model = agent.CanonicalizeCopilotModel(model)
	}
	for _, id := range known {
		if id == model {
			return nil
		}
	}
	return fmt.Errorf("model %q is not available for backend %q (available: %s)",
		model, backend, strings.Join(known, ", "))
}

func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	// Owner-only, matching handleEffortSet: switching backends persists to
	// hive.yaml, claims operator field-ownership, and restarts the agent (#6557).
	if !requireOwnerRole(w, r) {
		return
	}
	name := s.resolveAgentParam(r.PathValue("agent"))
	backend := sanitizeString(r.PathValue("backend"))

	if err := s.deps.AgentMgr.SetBackendOverride(name, backend); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Persist to hive.yaml and mark the field operator-owned so the next pack
	// apply (which runs on every restart) does not reconcile it away.
	s.claimAgentFieldOwnership(name, "", backend)

	s.deps.Logger.Info("audit: backend switched", "agent", name, "backend", backend, "trigger", "dashboard-api")
	s.auditFromRequest(r, "switch_backend", auditDetail("backend", backend), name)

	// Restart the agent session so the new backend takes effect immediately.
	if err := s.deps.AgentMgr.Restart(s.deps.Ctx, name); err != nil {
		s.deps.Logger.Warn("restart after backend switch failed", "agent", name, "error", err)
	}

	minStatusSeq := s.refreshAndPersistSeq()
	jsonResponse(w, map[string]any{"ok": true, "status": "switched", "agent": name, "backend": backend, "minStatusSeq": minStatusSeq})
}

func (s *Server) handleModelSet(w http.ResponseWriter, r *http.Request) {
	// Owner-only, matching handleEffortSet: the model is the same class of
	// operator-owned agent configuration as the reasoning effort (#6557).
	if !requireOwnerRole(w, r) {
		return
	}
	name := s.resolveAgentParam(r.PathValue("agent"))
	model := sanitizeString(r.PathValue("model"))

	// Reject a model the effective backend cannot serve BEFORE storing it.
	// Silently accepting an unusable value and then falling back at launch is
	// what made this class of bug invisible: the grid showed the choice, the
	// agent ran on something else, and the value appeared to "revert".
	if err := s.validateModelForAgent(name, model); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.deps.AgentMgr.SetModelOverride(name, model); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Persist to hive.yaml and mark the field operator-owned so the next pack
	// apply (which runs on every restart) does not reconcile it away.
	s.claimAgentFieldOwnership(name, model, "")

	s.deps.Logger.Info("audit: model set", "agent", name, "model", model, "trigger", "dashboard-api")
	s.auditFromRequest(r, "set_model", auditDetail("model", model), name)

	// Restart the agent session so the new model takes effect immediately.
	if err := s.deps.AgentMgr.Restart(s.deps.Ctx, name); err != nil {
		s.deps.Logger.Warn("restart after model switch failed", "agent", name, "error", err)
	}

	minStatusSeq := s.refreshAndPersistSeq()
	jsonResponse(w, map[string]any{"ok": true, "status": "model_set", "agent": name, "model": model, "minStatusSeq": minStatusSeq})
}

// handleEffortSet sets an agent's reasoning effort from the grid dropdown and
// restarts the session so it takes effect — the effort exists only on the
// launch command line (codex -c model_reasoning_effort, agy --effort), so a
// running CLI never picks it up in place. The {effort} path value "default"
// clears the field back to the backend's own default: a path segment cannot
// be empty, and empty IS the meaningful cleared value.
func (s *Server) handleEffortSet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	name := s.resolveAgentParam(r.PathValue("agent"))
	effort := sanitizeString(r.PathValue("effort"))
	if effort == "default" {
		effort = ""
	}

	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	// Validate against the backend the agent actually launches with —
	// including a runtime backend override — for the same reason
	// handleModelSet validates the model: silently accepting an unusable
	// value and falling back at launch makes the choice appear to "revert".
	backend, model := agentCfg.Backend, agentCfg.Model
	if proc, err := s.deps.AgentMgr.GetStatus(name); err == nil && proc != nil {
		if proc.BackendOverride != "" {
			backend = proc.BackendOverride
		}
		if proc.ModelOverride != "" {
			model = proc.ModelOverride
		}
	}
	if err := s.validateAgentReasoningEffort(backend, model, effort); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	agentCfg.ReasoningEffort = effort
	s.deps.Config.Agents[name] = agentCfg

	// Sync into the agent process and persist, mirroring handleAgentConfigModels.
	if err := s.deps.AgentMgr.UpdateConfig(name, agentCfg); err != nil {
		s.logger.Warn("failed to sync agent config to process", "agent", name, "error", err)
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after effort update", "agent", name, "error", err)
	}
	if agentsDir := s.deps.Config.Data.AgentsDir; agentsDir != "" {
		if err := config.SaveAgentFile(agentsDir, name, agentCfg); err != nil {
			s.logger.Error("failed to persist agent overlay after effort update", "agent", name, "error", err)
		}
	}

	s.deps.Logger.Info("audit: reasoning effort set", "agent", name, "effort", effort, "trigger", "dashboard-api")
	s.auditFromRequest(r, "set_reasoning_effort", auditDetail("reasoning_effort", effort), name)

	// Restart the agent session so the new effort takes effect immediately.
	if err := s.deps.AgentMgr.Restart(s.deps.Ctx, name); err != nil {
		s.deps.Logger.Warn("restart after effort switch failed", "agent", name, "error", err)
	}

	minStatusSeq := s.refreshAndPersistSeq()
	jsonResponse(w, map[string]any{"ok": true, "status": "effort_set", "agent": name, "reasoning_effort": effort, "minStatusSeq": minStatusSeq})
}

// pauseStateLabel names the authoritative pause-dimension state reported by
// the pause/resume/agent-state endpoints. It deliberately says nothing about
// process health — "running" here means "not operator-paused" (the governor
// will kick it), matching what the pause/resume toggle controls.
const (
	pauseStatePaused  = "paused"
	pauseStateRunning = "running"
)

func pauseStateLabel(paused bool) string {
	if paused {
		return pauseStatePaused
	}
	return pauseStateRunning
}

// pauseToggleResponse is the shared response shape for pause/resume. `changed`
// distinguishes a real transition from a no-op: pausing an already-paused
// agent used to return the same undifferentiated success as a real pause,
// which let a dashboard with a stale belief silently re-pause an agent the
// operator was trying to START (audit showed pause-pairs seconds apart while
// the agent stayed paused indefinitely). `state` is the authoritative
// post-request pause state the client must render from.
func pauseToggleResponse(w http.ResponseWriter, status, agent string, changed, paused bool, minStatusSeq uint64) {
	jsonResponse(w, map[string]interface{}{
		"ok":           true,
		"status":       status,
		"agent":        agent,
		"changed":      changed,
		"state":        pauseStateLabel(paused),
		"minStatusSeq": minStatusSeq,
	})
}

// requireOwnerRole returns true when authenticate established owner-level access.
// A client-supplied X-Hive-Role is not enough: authenticate strips inbound role
// headers before auth and sets ownerRoleVerifiedHeader only for trusted owner
// sessions, proof-verified hub proxy identities, or shared-token operators
// (X-Hive-Internal / bearer token — possession of the secret is the owner
// credential on those deployments). Owner-only mutations must require its
// server-only verification marker, so legacy/proofless proxy headers fail
// closed instead of trusting spoofable client input.
func requireOwnerRole(w http.ResponseWriter, r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if !isOwnerRole(role) || r.Header.Get(ownerRoleVerifiedHeader) != "true" {
		jsonError(w, "owner access required", http.StatusForbidden)
		return false
	}
	return true
}

func requireMergerOrOwnerRole(w http.ResponseWriter, r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if isOwnerRole(role) && r.Header.Get(ownerRoleVerifiedHeader) != "true" {
		jsonError(w, "merger or owner access required", http.StatusForbidden)
		return false
	}
	if !config.RoleAtLeast(role, config.RoleMerger) {
		jsonError(w, "merger or owner access required", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	name := s.resolveAgentParam(r.PathValue("agent"))

	// No-op guard: the agent is already paused. Do NOT call Pause again —
	// that would clobber the original PausedAt/reason/trigger — and tell the
	// client explicitly so it can correct its stale UI instead of believing
	// it just paused a running agent. An unknown agent falls through to
	// Pause, which returns the same 400 as before.
	if proc, err := s.deps.AgentMgr.GetStatus(name); err == nil && proc != nil && proc.Paused {
		s.auditFromRequest(r, "pause", auditDetail("result", "noop-already-paused"), name)
		pauseToggleResponse(w, "paused", name, false, true, s.currentStatusSeq())
		return
	}

	// Record the acting user on the pause itself (#4041): the audit log has
	// always known who clicked, but the agent state did not, so a deliberate
	// owner mass-pause was indistinguishable from a malfunction days later.
	if err := s.deps.AgentMgr.PauseBy(name, "dashboard-api", "manual pause", requestUser(r)); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.auditFromRequest(r, "pause", "", name)
	minStatusSeq := s.refreshAndPersistSeq()
	pauseToggleResponse(w, "paused", name, true, true, minStatusSeq)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	name := s.resolveAgentParam(r.PathValue("agent"))

	// No-op guard, mirror of handlePause: resuming an agent that is not
	// paused must report changed:false with the authoritative state.
	if proc, err := s.deps.AgentMgr.GetStatus(name); err == nil && proc != nil && !proc.Paused {
		s.auditFromRequest(r, "resume", auditDetail("result", "noop-not-paused"), name)
		pauseToggleResponse(w, "resumed", name, false, false, s.currentStatusSeq())
		return
	}

	if err := s.deps.AgentMgr.Resume(s.deps.Ctx, name, "dashboard-api", "manual resume"); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// An explicit operator resume claims ownership of the agent's pause state
	// (#5706). Without the claim, the ACMM pack visibility sweep that runs on
	// every restart re-paused any non-pack agent as "agent not in pack level
	// N" — so this resume silently lasted only until the next pod roll.
	s.claimAgentPauseOwnership(name)

	s.auditFromRequest(r, "resume", "", name)
	minStatusSeq := s.refreshAndPersistSeq()
	pauseToggleResponse(w, "resumed", name, true, false, minStatusSeq)
}

// handleAgentState is a lightweight authoritative pause-state probe. The
// dashboard's pause/resume toggle calls it immediately before acting so the
// action derives from the SERVER's persisted state rather than a stale DOM
// dataset — the belt to the response contract's suspenders above.
func (s *Server) handleAgentState(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	proc, err := s.deps.AgentMgr.GetStatus(name)
	if err != nil || proc == nil {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}
	jsonResponse(w, map[string]interface{}{
		"ok":        true,
		"agent":     name,
		"paused":    proc.Paused,
		"state":     pauseStateLabel(proc.Paused),
		"procState": string(proc.State),
	})
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	// Owner-only: pinning claims operator ownership of an agent config
	// dimension and persists to hive.yaml (#6557).
	if !requireOwnerRole(w, r) {
		return
	}
	name := s.resolveAgentParam(r.PathValue("agent"))
	dimension := r.PathValue("dimension")

	var body struct {
		Value string `json:"value"`
	}
	if err := decodeBody(r, &body); err != nil {
		s.deps.Logger.Debug("pin body decode failed, using current value", "agent", name, "error", err)
	}

	if body.Value == "" {
		proc, getErr := s.deps.AgentMgr.GetStatus(name)
		if getErr != nil || proc == nil {
			jsonError(w, "agent not found", http.StatusBadRequest)
			return
		}
		switch dimension {
		case "cli":
			body.Value = proc.Config.Backend
			if proc.BackendOverride != "" {
				body.Value = proc.BackendOverride
			}
		case "model":
			body.Value = proc.Config.Model
			if proc.ModelOverride != "" {
				body.Value = proc.ModelOverride
			}
		}
	}

	var err error
	switch dimension {
	case "cli":
		err = s.deps.AgentMgr.PinCLI(name, body.Value)
	case "model":
		err = s.deps.AgentMgr.PinModel(name, body.Value)
	default:
		jsonError(w, "dimension must be 'cli' or 'model'", http.StatusBadRequest)
		return
	}

	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// A pin is a stronger statement of intent than a plain switch, so it must
	// at least be as durable. Previously a pin lived only on the agent process
	// (replayed from /data/hive-state.json) while hive.yaml kept the pack's
	// value, so the pin did not protect the field from the pack re-apply that
	// runs on every restart — the pin icon was effectively decorative.
	switch dimension {
	case "model":
		s.claimAgentFieldOwnership(name, body.Value, "")
	case "cli":
		s.claimAgentFieldOwnership(name, "", body.Value)
	}

	s.deps.Logger.Info("audit: agent pinned", "agent", name, "dimension", dimension, "value", body.Value, "trigger", "dashboard-api")
	s.auditFromRequest(r, "pin", auditDetail("dimension", dimension, "value", body.Value), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "pinned", "agent": name, "dimension": dimension, "value": body.Value})
}

func (s *Server) handleUnpin(w http.ResponseWriter, r *http.Request) {
	// Owner-only, symmetric with handlePin (#6557).
	if !requireOwnerRole(w, r) {
		return
	}
	name := s.resolveAgentParam(r.PathValue("agent"))
	dimension := r.PathValue("dimension")

	var err error
	switch dimension {
	case "cli":
		err = s.deps.AgentMgr.UnpinCLI(name)
	case "model":
		err = s.deps.AgentMgr.UnpinModel(name)
	default:
		jsonError(w, "dimension must be 'cli' or 'model'", http.StatusBadRequest)
		return
	}

	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Logger.Info("audit: agent unpinned", "agent", name, "dimension", dimension, "trigger", "dashboard-api")
	s.auditFromRequest(r, "unpin", auditDetail("dimension", dimension), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "unpinned", "agent": name, "dimension": dimension})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	// Owner-only, matching handlePause/handleResume: restarting an agent is
	// the same class of lifecycle control as pausing it (#6557).
	if !requireOwnerRole(w, r) {
		return
	}
	name := s.resolveAgentParam(r.PathValue("agent"))

	// Serialize restart operations to prevent concurrent pause/resume cycles
	// from interfering through shared state (tmux server, config writes).
	s.restartMu.Lock()
	defer s.restartMu.Unlock()

	if err := s.deps.AgentMgr.Restart(s.deps.Ctx, name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Logger.Info("audit: agent restarted", "agent", name, "trigger", "dashboard-api")
	s.auditFromRequest(r, "restart", "", name)
	s.refreshAndPersist()
	// A restart cancels the agent's pending kick (#7363). Tell the operator
	// so: the old behaviour was to silently replay the interrupted prompt into
	// the relaunched CLI, and "restarted" alone would leave them expecting
	// exactly that.
	resp := map[string]any{"ok": true, "status": "restarted", "agent": name}
	if d, ok := s.deps.AgentMgr.KickDispatchState(name); ok && d.Phase == agent.KickPhaseFailed && strings.HasPrefix(d.Error, "cancelled:") {
		resp["kickCancelled"] = true
	}
	jsonResponse(w, resp)
}

func (s *Server) handleResetRestarts(w http.ResponseWriter, r *http.Request) {
	// Owner-only: clearing the restart counter defeats the crash-loop
	// breaker's escalation history (#6557).
	if !requireOwnerRole(w, r) {
		return
	}
	name := s.resolveAgentParam(r.PathValue("agent"))

	if err := s.deps.AgentMgr.ResetRestartCount(name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Logger.Info("audit: restart count reset", "agent", name, "trigger", "dashboard-api")
	floor := s.refreshAndPersistSeq()
	// minStatusSeq: status snapshots below this seq were built before the
	// reset — the dashboard drops them so the zeroed counter can't flicker
	// back to the stale value (#4348).
	jsonResponse(w, map[string]any{"ok": true, "status": "reset", "agent": name, "minStatusSeq": floor})
}
