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
	"net/http"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/linearagent"
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

// linearAgentService bundles the linearagent components the handlers share.
type linearAgentService struct {
	store     *linearagent.Store
	client    *linearagent.Client
	tracker   *linearagent.Tracker
	responder *linearagent.Responder
	receiver  *linearagent.WebhookReceiver
	states    *linearagent.StateStore
	creds     linearagent.Credentials
	// tokenURL / graphqlURL default to production; tests point them at fakes
	// before first use via newLinearAgentServiceForTest.
	tokenURL   string
	graphqlURL string
	// storeErr is a store-open failure (corrupt token file). Kept rather than
	// swallowed so status can surface it; install/webhook fail cleanly.
	storeErr error
}

// linearAgent lazily builds the service. sync.Once so the store is read and
// the responder wired exactly once per server.
func (s *Server) linearAgent() *linearAgentService {
	s.linearAgentOnce.Do(func() {
		if s.linearAgentSvc == nil {
			s.linearAgentSvc = s.newLinearAgentService("", "")
		}
	})
	return s.linearAgentSvc
}

// newLinearAgentService constructs the service. Empty tokenURL/graphqlURL mean
// production Linear.
func (s *Server) newLinearAgentService(tokenURL, graphqlURL string) *linearAgentService {
	svc := &linearAgentService{
		creds:      linearagent.CredentialsFromEnv(),
		states:     linearagent.NewStateStore(linearagent.StateTTL),
		tracker:    linearagent.NewTracker(),
		tokenURL:   tokenURL,
		graphqlURL: graphqlURL,
	}
	store, err := linearagent.NewStore(linearagent.DefaultStorePath())
	if err != nil {
		s.logger.Error("linear agent: install store unreadable", "error", err)
		svc.storeErr = err
		return svc
	}
	svc.store = store
	svc.client = linearagent.NewClient(store, svc.creds, nil, tokenURL, graphqlURL, s.logger)

	kick := func(agentName, message string) error {
		if s.deps == nil || s.deps.AgentMgr == nil {
			return errNoAgentManager
		}
		err := s.deps.AgentMgr.SendKick(agentName, message)
		if err == nil && s.deps.Governor != nil {
			s.deps.Governor.RecordKick(agentName)
		}
		return err
	}
	svc.responder = linearagent.NewResponder(svc.client, kick, s.resolveLinearSessionAgent, svc.tracker, s.logger)
	svc.receiver = linearagent.NewWebhookReceiver(svc.responder.HandleSessionEvent, s.logger)

	// Component D: run completion → response activity. The observer no-ops
	// for agents with no active Linear session, so installing it
	// unconditionally costs nothing.
	if s.deps != nil && s.deps.AgentMgr != nil {
		s.deps.AgentMgr.SetKickObserver(svc.responder.HandleAgentEvent)
	}
	return svc
}

// errNoAgentManager is the kick failure when the server has no agent manager
// (misconfigured test harness — never a production shape).
var errNoAgentManager = &noAgentManagerError{}

type noAgentManagerError struct{}

func (*noAgentManagerError) Error() string { return "agent manager unavailable" }

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
	return "", errSessionAgentUnset
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
// own origin — configured dashboard_url first, then the forwarded/request
// host. Never client-supplied (same rationale as openRouterCallbackURL).
func (s *Server) linearAgentCallbackURL(r *http.Request) string {
	base := ""
	if s.deps != nil && s.deps.Config != nil {
		base = strings.TrimSpace(s.deps.Config.Hub.DashboardURL)
	}
	if base == "" {
		scheme := "https"
		if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
			scheme = "http"
		}
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		}
		host := r.Header.Get("X-Forwarded-Host")
		if host == "" {
			host = r.Host
		}
		base = scheme + "://" + host
	}
	return strings.TrimRight(base, "/") + linearAgentCallbackPath
}

// handleLinearAgentInstall (owner) starts the actor=app authorize flow.
func (s *Server) handleLinearAgentInstall(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc := s.linearAgent()
	if svc.storeErr != nil {
		jsonError(w, "linear install store unreadable; see server log", http.StatusServiceUnavailable)
		return
	}
	if !svc.creds.Configured() {
		jsonError(w, "LINEAR_CLIENT_ID / LINEAR_CLIENT_SECRET are not set", http.StatusPreconditionFailed)
		return
	}
	state, err := svc.states.Create()
	if err != nil {
		jsonError(w, "failed to start flow", http.StatusInternalServerError)
		return
	}
	authorizeURL := linearagent.BuildAuthorizeURL(svc.creds.ClientID, s.linearAgentCallbackURL(r), state)
	s.auditFromRequest(r, "linear_agent_install_start", "", "")
	jsonResponse(w, map[string]interface{}{"authorize_url": authorizeURL})
}

// handleLinearAgentCallback is the PUBLIC OAuth return. The single-use state
// is verified before anything else; the code is then exchanged and the app's
// per-workspace identity captured beside the token.
func (s *Server) handleLinearAgentCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := strings.TrimSpace(q.Get("code"))
	state := strings.TrimSpace(q.Get("state"))
	svc := s.linearAgent()
	if code == "" || state == "" || svc.storeErr != nil || !svc.creds.Configured() {
		s.redirectLinearAgent(w, r, linearAgentErrorFlag)
		return
	}
	if !svc.states.Consume(state) {
		// Unknown / expired / replayed state — reject.
		s.redirectLinearAgent(w, r, linearAgentErrorFlag)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), linearAgentExchangeTimeout)
	defer cancel()
	tokenURL := svc.tokenURL
	if tokenURL == "" {
		tokenURL = linearagent.TokenURL
	}
	tok, err := linearagent.ExchangeCode(ctx, nil, tokenURL, svc.creds, code, s.linearAgentCallbackURL(r))
	if err != nil {
		s.logger.Warn("linear agent: code exchange failed", "error", err.Error())
		s.redirectLinearAgent(w, r, linearAgentErrorFlag)
		return
	}
	ident, err := linearagent.FetchIdentity(ctx, nil, svc.graphqlURL, tok.AccessToken)
	if err != nil {
		s.logger.Warn("linear agent: identity query failed", "error", err.Error())
		s.redirectLinearAgent(w, r, linearAgentErrorFlag)
		return
	}
	inst := linearagent.Install{
		ViewerID:           ident.ViewerID,
		OrganizationID:     ident.OrganizationID,
		OrganizationName:   ident.OrganizationName,
		OrganizationURLKey: ident.OrganizationURLKey,
		Token:              tok,
		ConnectedAt:        time.Now(),
	}
	if err := svc.store.Set(inst); err != nil {
		s.logger.Error("linear agent: failed to persist install", "error", err.Error())
		s.redirectLinearAgent(w, r, linearAgentErrorFlag)
		return
	}
	s.auditFromRequest(r, "linear_agent_connected", auditDetail("workspace", ident.OrganizationName), "")
	s.redirectLinearAgent(w, r, linearAgentConnectedFlag)
}

// handleLinearAgentWebhook is the PUBLIC AgentSessionEvent receiver. All
// verification (HMAC over raw body, replay window) lives in the receiver.
func (s *Server) handleLinearAgentWebhook(w http.ResponseWriter, r *http.Request) {
	svc := s.linearAgent()
	if svc.receiver == nil {
		http.Error(w, "linear agent unavailable", http.StatusServiceUnavailable)
		return
	}
	svc.receiver.ServeHTTP(w, r)
}

// handleLinearAgentStatus (owner) reports install state and tracked sessions.
func (s *Server) handleLinearAgentStatus(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	svc := s.linearAgent()
	resp := map[string]interface{}{
		"configured":    svc.creds.Configured(),
		"connected":     false,
		"webhook_path":  linearAgentWebhookPath,
		"callback_path": linearAgentCallbackPath,
	}
	if svc.storeErr != nil {
		resp["store_error"] = "install store unreadable; see server log"
	}
	if name, err := s.resolveLinearSessionAgent(); err == nil {
		resp["session_agent"] = name
	} else {
		resp["session_agent_error"] = err.Error()
	}
	if svc.store != nil {
		if inst, ok := svc.store.Get(); ok {
			resp["connected"] = true
			resp["viewer_id"] = inst.ViewerID
			resp["workspace"] = map[string]string{
				"id":      inst.OrganizationID,
				"name":    inst.OrganizationName,
				"url_key": inst.OrganizationURLKey,
			}
			resp["connected_at"] = inst.ConnectedAt
		}
	}
	if svc.tracker != nil {
		resp["sessions"] = svc.tracker.Snapshot()
	}
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
	if svc.store == nil {
		jsonError(w, "linear install store unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := svc.store.Clear(); err != nil {
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
