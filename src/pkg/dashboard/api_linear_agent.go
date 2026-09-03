package dashboard

// Linear agent integration wiring (RFC #4492, Part 2).
//
// This file connects pkg/linearagent to the spoke dashboard's HTTP surface:
//
//   - POST /api/linear/agent/install    (owner)  → returns the actor=app
//     authorize URL with a single-use state
//   - GET  /linear/callback             (PUBLIC) → OAuth return; the state
//     token is the credential, verified single-use server-side (same posture
//     as /openrouter/callback)
//   - POST /api/linear/webhook          (PUBLIC) → AgentSessionEvent receiver;
//     the HMAC signature is the credential, verified fail-closed
//   - GET  /api/linear/agent/status     (owner)  → install state + sessions
//   - POST /api/linear/agent/disconnect (owner)  → forget the install
//
// It also installs the agent-manager kick observer that maps run completion
// back onto Linear sessions (component D).

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	// linearAgentCallbackPath is the PUBLIC path Linear redirects the
	// installing admin's browser to. One const so route registration, the
	// isPublicPath allowlist, and the redirect_uri builder agree.
	linearAgentCallbackPath = "/linear/callback"

	// linearAgentWebhookPath is the PUBLIC path Linear posts
	// AgentSessionEvent webhooks to.
	linearAgentWebhookPath = "/api/linear/webhook"

	// linearAgentConnectedFlag / linearAgentErrorFlag are appended to the
	// post-callback dashboard redirect so the UI can show the outcome.
	linearAgentConnectedFlag = "?linear=connected"
	linearAgentErrorFlag     = "?linear=error"

	// linearAgentExchangeTimeout bounds the callback's code exchange plus
	// identity query.
	linearAgentExchangeTimeout = 30 * time.Second
)

// linearAgent lazily builds the service through the Dependencies factory
// (#5565 slice 3: the concrete pkg/linearagent bundle is constructed behind
// the LinearAgentGateway interface — in cmd/hive for production, in the test
// helper for tests). sync.Once so the store is read and the responder wired
// exactly once per server. Nil when no factory is wired.
func (s *Server) linearAgent() LinearAgentGateway {
	if s.linearAgentSvc == nil && (s.deps == nil || s.deps.NewLinearAgent == nil) {
		// No factory wired (bare test server): leave the Once unconsumed so a
		// factory wired later still constructs on the next call.
		return nil
	}
	s.linearAgentOnce.Do(func() {
		if s.linearAgentSvc != nil {
			return
		}
		svc := s.deps.NewLinearAgent(LinearAgentPorts{
			Kick:                s.linearAgentKick,
			ResolveSessionAgent: s.resolveLinearSessionAgent,
		})
		// Component D: run completion → response activity. The observer no-ops
		// for agents with no active Linear session, so installing it
		// unconditionally costs nothing.
		if svc != nil && s.deps.AgentMgr != nil {
			if obs := svc.AgentEventObserver(); obs != nil {
				s.deps.AgentMgr.SetKickObserver(obs)
			}
		}
		s.linearAgentSvc = svc
	})
	return s.linearAgentSvc
}

// linearAgentKick is the responder's kick port: send the message to the named
// agent and record the kick with the governor.
func (s *Server) linearAgentKick(agentName, message string) error {
	if s.deps == nil || s.deps.AgentMgr == nil {
		return errNoAgentManager
	}
	err := s.deps.AgentMgr.SendKick(agentName, message)
	if err == nil && s.deps.Governor != nil {
		s.deps.Governor.RecordKick(agentName)
	}
	return err
}

// errNoAgentManager is the kick failure when the server has no agent manager
// (misconfigured test harness — never a production shape).
var errNoAgentManager = &noAgentManagerError{}

type noAgentManagerError struct{}

func (*noAgentManagerError) Error() string { return "agent manager unavailable" }

// linearAgentTokenTimeout bounds LinearAgentAccessToken. The common path is a
// store read; only an expired token costs an HTTP refresh, and this runs on
// the agent LAUNCH path (Phase 2, after the manager's lock is released — but
// still blocking the launch), so it must not hang.
const linearAgentTokenTimeout = 5 * time.Second

// LinearAgentAccessToken returns the connected workspace's live OAuth access
// token, or "" when no workspace is connected (or the store is unreadable).
// It is the value pkg/agent injects into ISSUES_ONLY+ agents as
// LINEAR_ACCESS_TOKEN so their Linear writes are authored by the same "Hive"
// app identity that acknowledges sessions — the Linear analogue of the GitHub
// App token pushed as GITHUB_TOKEN. Never logged by callers.
func (s *Server) LinearAgentAccessToken() string {
	svc := s.linearAgent()
	if svc == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), linearAgentTokenTimeout)
	defer cancel()
	tok, err := svc.AccessToken(ctx)
	if err != nil {
		// "not installed" is the steady state of every hive without a Linear
		// workspace; log only at debug so GitHub-only hives stay quiet.
		s.logger.Debug("linear agent: no access token for agents", "error", err)
		return ""
	}
	return tok
}

// resolveLinearSessionAgent names the agent for Linear sessions:
// work_source.linear.session_agent when set (and configured), else the sole
// configured agent, else an error the responder reports into the session.
func (s *Server) resolveLinearSessionAgent() (string, error) {
	if s.deps == nil || s.deps.Config == nil {
		return "", errNoAgentManager
	}
	cfg := s.deps.Config
	if name := strings.TrimSpace(cfg.Governor.WorkSource.Linear.SessionAgent); name != "" {
		if _, ok := cfg.Agents[name]; !ok {
			return "", &unknownSessionAgentError{name: name}
		}
		return name, nil
	}
	if len(cfg.Agents) == 1 {
		for name := range cfg.Agents {
			return name, nil
		}
	}
	// ACMM-shaped fallback: when exactly one enabled agent may write to the
	// tracker (CanCreateIssues — ISSUES_ONLY and above), it is the only agent
	// that could do anything with a delegated issue beyond acknowledging it,
	// so it takes sessions. This is what makes the L3 pack (six agents, quality
	// the sole writer) work without an explicit session_agent. Two or more
	// writers is still ambiguous and still an error.
	level := 0
	if cfg.ACMMLevel != nil {
		level = *cfg.ACMMLevel
	}
	writer := ""
	for name, ac := range cfg.EnabledAgents() {
		mode, ok := agent.ParseAgentMode(ac.Mode)
		if !ok {
			mode = agent.DefaultAgentMode(name, level)
		}
		if !mode.CanCreateIssues() {
			continue
		}
		if writer != "" {
			return "", errSessionAgentUnset
		}
		writer = name
	}
	if writer != "" {
		return writer, nil
	}
	return "", errSessionAgentUnset
}

// LinearSessionHolder is the scheduler's in-flight lookup (see
// pkg/scheduler.InflightLookup): a Linear-sourced work item whose identifier
// has a working agent session is held by that session's agent.
func (s *Server) LinearSessionHolder(issue github.Issue) (string, bool) {
	if issue.SourceType != "linear" || issue.ExternalID == "" {
		return "", false
	}
	svc := s.linearAgent()
	if svc == nil {
		return "", false
	}
	agentName, sessionID, ok := svc.ActiveSessionForIssue(issue.ExternalID)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("agent %s via Linear session %s", agentName, sessionID), true
}

// LinearAgentPROpened is the pr-request watcher's PR-opened hook: it narrates
// the PR into the agent's active Linear session, if any.
func (s *Server) LinearAgentPROpened(agentName, repo string, number int, url string) {
	svc := s.linearAgent()
	if svc == nil {
		return
	}
	svc.HandlePROpened(agentName, repo, number, url)
}

// linearAgentCredentialKind reports which credential ISSUES_ONLY+ agents
// receive for api.linear.app: "oauth" (connected app), "api_key" (the
// work-source key), or "none". Status-only; values are never exposed.
func (s *Server) linearAgentCredentialKind(svc LinearAgentGateway) string {
	if svc != nil {
		if inst, ok := svc.Install(); ok && inst.HasAccessToken {
			return "oauth"
		}
	}
	if s.deps != nil && s.deps.Config != nil {
		ws := s.deps.Config.Governor.WorkSource
		if ws.Type == "linear" && strings.TrimSpace(ws.Linear.APIKey) != "" {
			return "api_key"
		}
	}
	return "none"
}

type unknownSessionAgentError struct{ name string }

func (e *unknownSessionAgentError) Error() string {
	return "work_source.linear.session_agent names unknown agent " + e.name
}

var errSessionAgentUnset = &sessionAgentUnsetError{}

type sessionAgentUnsetError struct{}

func (*sessionAgentUnsetError) Error() string {
	return "set work_source.linear.session_agent to the agent that should take Linear sessions"
}

// registerLinearAgentRoutes registers the Linear agent routes. The callback
// and webhook are public (added to isPublicPath); the rest ride normal auth.
func (s *Server) registerLinearAgentRoutes() {
	s.mux.HandleFunc("POST /api/linear/agent/install", s.handleLinearAgentInstall)
	s.mux.HandleFunc("GET /api/linear/agent/status", s.handleLinearAgentStatus)
	s.mux.HandleFunc("POST /api/linear/agent/disconnect", s.handleLinearAgentDisconnect)
	s.mux.HandleFunc("GET "+linearAgentCallbackPath, s.handleLinearAgentCallback)
	s.mux.HandleFunc("POST "+linearAgentWebhookPath, s.handleLinearAgentWebhook)
}

// linearAgentCallbackURL builds this hive's redirect_uri from an allowlisted
// own origin — see oauthPublicOrigin for the precedence (dashboard.public_url,
// hub.dashboard_url, then the forwarded/request host). Never client-supplied.
func (s *Server) linearAgentCallbackURL(r *http.Request) string {
	return s.oauthPublicOrigin(r) + linearAgentCallbackPath
}

// handleLinearAgentInstall (owner) starts the actor=app authorize flow.
func (s *Server) handleLinearAgentInstall(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc := s.linearAgent()
	if svc == nil {
		jsonError(w, "linear agent unavailable", http.StatusServiceUnavailable)
		return
	}
	if svc.StoreErr() != nil {
		jsonError(w, "linear install store unreadable; see server log", http.StatusServiceUnavailable)
		return
	}
	if !svc.Configured() {
		jsonError(w, "LINEAR_CLIENT_ID / LINEAR_CLIENT_SECRET are not set", http.StatusPreconditionFailed)
		return
	}
	state, err := svc.NewFlowState()
	if err != nil {
		jsonError(w, "failed to start flow", http.StatusInternalServerError)
		return
	}
	redirectURI := s.linearAgentCallbackURL(r)
	authorizeURL := svc.AuthorizeURL(redirectURI, state)
	s.auditFromRequest(r, "linear_agent_install_start", "", "")
	// redirect_uri is echoed so an operator can see the exact value the Linear
	// app's Callback URL must match without decoding authorize_url.
	jsonResponse(w, map[string]interface{}{"authorize_url": authorizeURL, "redirect_uri": redirectURI})
}

// handleLinearAgentCallback is the PUBLIC OAuth return. The single-use state
// is verified before anything else; the code is then exchanged and the app's
// per-workspace identity captured beside the token.
func (s *Server) handleLinearAgentCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := strings.TrimSpace(q.Get("code"))
	state := strings.TrimSpace(q.Get("state"))
	svc := s.linearAgent()
	if svc == nil || code == "" || state == "" || svc.StoreErr() != nil || !svc.Configured() {
		s.redirectLinearAgent(w, r, linearAgentErrorFlag)
		return
	}
	if !svc.ConsumeFlowState(state) {
		// Unknown / expired / replayed state — reject.
		s.redirectLinearAgent(w, r, linearAgentErrorFlag)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), linearAgentExchangeTimeout)
	defer cancel()
	// Exchange + identity + persist happen provider-side (CompleteInstall),
	// with the same step-level warn/error logging as before the interface cut.
	workspace, err := svc.CompleteInstall(ctx, code, s.linearAgentCallbackURL(r))
	if err != nil {
		s.redirectLinearAgent(w, r, linearAgentErrorFlag)
		return
	}
	s.auditFromRequest(r, "linear_agent_connected", auditDetail("workspace", workspace), "")
	s.redirectLinearAgent(w, r, linearAgentConnectedFlag)
}

// handleLinearAgentWebhook is the PUBLIC AgentSessionEvent receiver. All
// verification (HMAC over raw body, replay window) lives in the receiver.
func (s *Server) handleLinearAgentWebhook(w http.ResponseWriter, r *http.Request) {
	svc := s.linearAgent()
	if svc == nil {
		http.Error(w, "linear agent unavailable", http.StatusServiceUnavailable)
		return
	}
	h := svc.WebhookHandler()
	if h == nil {
		http.Error(w, "linear agent unavailable", http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, r)
}

// handleLinearAgentStatus (owner) reports install state and tracked sessions.
func (s *Server) handleLinearAgentStatus(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc := s.linearAgent()
	if svc == nil {
		jsonError(w, "linear agent unavailable", http.StatusServiceUnavailable)
		return
	}
	resp := map[string]interface{}{
		"configured":    svc.Configured(),
		"connected":     false,
		"webhook_path":  linearAgentWebhookPath,
		"callback_path": linearAgentCallbackPath,
	}
	if svc.StoreErr() != nil {
		resp["store_error"] = "install store unreadable; see server log"
	}
	if name, err := s.resolveLinearSessionAgent(); err == nil {
		resp["session_agent"] = name
	} else {
		resp["session_agent_error"] = err.Error()
	}
	if inst, ok := svc.Install(); ok {
		resp["connected"] = true
		resp["viewer_id"] = inst.ViewerID
		resp["workspace"] = map[string]string{
			"id":      inst.OrganizationID,
			"name":    inst.OrganizationName,
			"url_key": inst.OrganizationURLKey,
		}
		resp["connected_at"] = inst.ConnectedAt
	}
	if sessions, ok := svc.SessionsSnapshot(); ok {
		resp["sessions"] = sessions
	}
	resp["agent_credential"] = s.linearAgentCredentialKind(svc)
	jsonResponse(w, resp)
}

// handleLinearAgentDisconnect (owner) forgets the install. It does not revoke
// the grant on Linear's side — admins do that from Linear's app settings —
// but the hive stops holding the token.
func (s *Server) handleLinearAgentDisconnect(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc := s.linearAgent()
	if svc == nil || !svc.HasInstallStore() {
		jsonError(w, "linear install store unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := svc.ClearInstall(); err != nil {
		jsonError(w, "failed to clear install", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "linear_agent_disconnected", "", "")
	okResponse(w, map[string]string{"status": "disconnected"})
}

// redirectLinearAgent sends the browser back to the dashboard root with a
// status flag the UI reads.
func (s *Server) redirectLinearAgent(w http.ResponseWriter, r *http.Request, flag string) {
	http.Redirect(w, r, "/"+flag, http.StatusFound)
}
