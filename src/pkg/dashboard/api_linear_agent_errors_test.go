package dashboard

// Error-path coverage for the Linear agent HTTP surface (api_linear_agent.go).
// The happy paths live in api_linear_agent_test.go; these tests pin the
// fail-closed behavior: a corrupt install store, OAuth exchange/identity/
// persist failures, and the kick closure's error legs. Every path here guards
// a live OAuth token or a public endpoint, so silently regressing one would
// turn a broken store into a silent "not installed" or a 200 on a dead
// webhook.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/linearagent"
)

// corruptStoreServer builds a server whose Linear install store file is
// unparseable JSON, so newLinearAgentService records storeErr and leaves
// store/client/receiver nil.
func corruptStoreServer(t *testing.T) *Server {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "linear-agent.json")
	if err := os.WriteFile(storePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(linearagent.StoreEnvVar, storePath)
	t.Setenv("LINEAR_CLIENT_ID", "client-id")
	t.Setenv("LINEAR_CLIENT_SECRET", "client-secret")
	t.Setenv("LINEAR_WEBHOOK_SECRET", "hook-secret")
	s, _ := apiServer(t)
	s.linearAgentSvc = s.newLinearAgentService("", "")
	if s.linearAgentSvc.storeErr == nil {
		t.Fatal("corrupt store file did not surface as storeErr")
	}
	return s
}

func TestLinearAgentCorruptStoreFailsClosed(t *testing.T) {
	s := corruptStoreServer(t)

	// Install: 503, not a broken authorize URL.
	if rec := doOwnerPost(s, "/api/linear/agent/install", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("install with corrupt store = %d, want 503", rec.Code)
	}

	// Callback: rejected before any code exchange, even with plausible params.
	rec := doGet(s, "/linear/callback?code=abc&state=xyz")
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "linear=error") {
		t.Errorf("callback with corrupt store: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// Webhook: 503 — a nil receiver must not read as "accepted".
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/linear/webhook", strings.NewReader("{}"))
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("webhook with corrupt store = %d, want 503", rec.Code)
	}

	// Disconnect: 503 (there is no store to clear).
	if rec := doOwnerPost(s, "/api/linear/agent/disconnect", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("disconnect with corrupt store = %d, want 503", rec.Code)
	}

	// Status: still 200, and it names the store problem instead of hiding it.
	rec = doOwnerGet(s, "/api/linear/agent/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var status struct {
		Connected  bool   `json:"connected"`
		StoreError string `json:"store_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Connected || status.StoreError == "" {
		t.Errorf("status hid the store failure: %+v", status)
	}
}

// failingLinearServer is linearAgentTestServer with the token and/or GraphQL
// endpoints replaced by failures.
func failingLinearServer(t *testing.T, tokenOK bool) *Server {
	t.Helper()
	t.Setenv(linearagent.StoreEnvVar, filepath.Join(t.TempDir(), "linear-agent.json"))
	t.Setenv("LINEAR_CLIENT_ID", "client-id")
	t.Setenv("LINEAR_CLIENT_SECRET", "client-secret")
	t.Setenv("LINEAR_WEBHOOK_SECRET", "hook-secret")
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" && tokenOK {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "at-1", "refresh_token": "rt-1", "expires_in": 86399,
			})
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(fake.Close)
	s, _ := apiServer(t)
	s.linearAgentSvc = s.newLinearAgentService(fake.URL+"/oauth/token", fake.URL+"/graphql")
	return s
}

func TestLinearAgentCallbackExchangeFailureRedirectsError(t *testing.T) {
	s := failingLinearServer(t, false)
	state, err := s.linearAgent().states.Create()
	if err != nil {
		t.Fatal(err)
	}
	rec := doGet(s, "/linear/callback?code=abc&state="+state)
	if !strings.Contains(rec.Header().Get("Location"), "linear=error") {
		t.Fatalf("failed exchange redirected to %q, want linear=error", rec.Header().Get("Location"))
	}
	// Nothing may have been persisted.
	if inst, ok := s.linearAgent().store.Get(); ok {
		t.Fatalf("failed exchange persisted an install: %+v", inst)
	}
}

func TestLinearAgentCallbackIdentityFailureRedirectsError(t *testing.T) {
	s := failingLinearServer(t, true) // token exchange succeeds, identity query 500s
	state, err := s.linearAgent().states.Create()
	if err != nil {
		t.Fatal(err)
	}
	rec := doGet(s, "/linear/callback?code=abc&state="+state)
	if !strings.Contains(rec.Header().Get("Location"), "linear=error") {
		t.Fatalf("failed identity redirected to %q, want linear=error", rec.Header().Get("Location"))
	}
	// A token without an identity must not be stored: the webhook's
	// organizationId check and the dashboard both need the workspace.
	if inst, ok := s.linearAgent().store.Get(); ok {
		t.Fatalf("failed identity persisted an install: %+v", inst)
	}
}

func TestLinearAgentCallbackPersistFailureRedirectsError(t *testing.T) {
	s, _, _ := linearAgentTestServer(t)
	// Make the store path un-writable by turning it into a directory: the
	// atomic rename in Store.Set then fails.
	storePath := linearagent.DefaultStorePath()
	if err := os.Mkdir(storePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storePath, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := s.linearAgent().states.Create()
	if err != nil {
		t.Fatal(err)
	}
	rec := doGet(s, "/linear/callback?code=abc&state="+state)
	if !strings.Contains(rec.Header().Get("Location"), "linear=error") {
		t.Fatalf("failed persist redirected to %q, want linear=error", rec.Header().Get("Location"))
	}
}

func TestLinearAgentDisconnectClearFailure(t *testing.T) {
	s, _, _ := linearAgentTestServer(t)
	// Same trick: the store opened fine (no file), but Clear's os.Remove hits
	// a non-empty directory and fails.
	storePath := linearagent.DefaultStorePath()
	if err := os.Mkdir(storePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storePath, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := doOwnerPost(s, "/api/linear/agent/disconnect", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("disconnect with failing Clear = %d, want 500", rec.Code)
	}
}

func TestLinearAgentStatusReportsSessionAgentError(t *testing.T) {
	s, deps, _ := linearAgentTestServer(t)
	// Two agents, no explicit session_agent → the status must carry the
	// resolver's error so the operator sees why sessions would fail.
	deps.Config.Agents["second"] = deps.Config.Agents["scanner"]
	rec := doOwnerGet(s, "/api/linear/agent/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var status struct {
		SessionAgent      string `json:"session_agent"`
		SessionAgentError string `json:"session_agent_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.SessionAgent != "" || status.SessionAgentError == "" {
		t.Errorf("ambiguous agent set not reported: %+v", status)
	}
}

func TestLinearAgentStatusCredentialKindAPIKey(t *testing.T) {
	s, deps, _ := linearAgentTestServer(t)
	deps.Config.Governor.WorkSource.Type = "linear"
	deps.Config.Governor.WorkSource.Linear.APIKey = "lin_api_test"
	rec := doOwnerGet(s, "/api/linear/agent/status")
	var status struct {
		AgentCredential string `json:"agent_credential"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.AgentCredential != "api_key" {
		t.Errorf("agent_credential = %q, want api_key (work-source key, no OAuth install)", status.AgentCredential)
	}
}

func TestResolveLinearSessionAgentNoDeps(t *testing.T) {
	// A server with no deps (or no config) must error, not panic.
	s := &Server{}
	if _, err := s.resolveLinearSessionAgent(); err == nil {
		t.Fatal("nil deps resolved a session agent")
	}
	s.deps = &Dependencies{}
	if _, err := s.resolveLinearSessionAgent(); err == nil {
		t.Fatal("nil config resolved a session agent")
	}
}

func TestResolveLinearSessionAgentHonorsACMMLevel(t *testing.T) {
	s, deps, _ := linearAgentTestServer(t)
	// Two agents without explicit modes: the configured ACMM level decides
	// their default modes and therefore the writer-fallback outcome.
	deps.Config.Agents["second"] = deps.Config.Agents["scanner"]
	level := 0
	deps.Config.ACMMLevel = &level
	if _, err := s.resolveLinearSessionAgent(); err == nil {
		t.Fatal("L0 (no writers) resolved a session agent")
	}
}

func TestLinearSessionHolderNilTracker(t *testing.T) {
	s, _ := apiServer(t)
	s.linearAgentSvc = &linearAgentService{} // storeErr shape: no tracker
	if _, ok := s.LinearSessionHolder(github.Issue{SourceType: "linear", ExternalID: "ENG-1"}); ok {
		t.Fatal("nil tracker reported a session holder")
	}
}

// connectLinearAgent runs the OAuth callback happy path so the store holds a
// live install (the responder's ack needs an access token).
func connectLinearAgent(t *testing.T, s *Server) {
	t.Helper()
	state, err := s.linearAgent().states.Create()
	if err != nil {
		t.Fatal(err)
	}
	rec := doGet(s, "/linear/callback?code=abc&state="+state)
	if !strings.Contains(rec.Header().Get("Location"), "linear=connected") {
		t.Fatalf("connect failed: %s", rec.Header().Get("Location"))
	}
}

func sessionEvent(id string) linearagent.SessionEvent {
	var ev linearagent.SessionEvent
	ev.Action = "created"
	ev.Type = "AgentSessionEvent"
	ev.AgentSession.ID = id
	ev.AgentSession.Issue.Identifier = "ENG-1"
	return ev
}

func trackedState(t *testing.T, s *Server, sessionID string) string {
	t.Helper()
	for _, sess := range s.linearAgent().tracker.Snapshot() {
		if sess.ID == sessionID {
			return sess.State
		}
	}
	t.Fatalf("session %s not tracked", sessionID)
	return ""
}

func TestLinearAgentKickFailureMarksSessionFailed(t *testing.T) {
	s, _, _ := linearAgentTestServer(t)
	connectLinearAgent(t, s)
	// The resolver names "scanner", but the real agent manager has no live
	// session to kick, so the kick errors and the session must land in
	// failed — never in working with no agent behind it.
	s.linearAgent().responder.HandleSessionEvent(sessionEvent("sess-kick-err"))
	if got := trackedState(t, s, "sess-kick-err"); got != linearagent.SessionStateFailed {
		t.Fatalf("session state after failed kick = %q, want %q", got, linearagent.SessionStateFailed)
	}
}

func TestLinearAgentKickWithoutAgentManagerFails(t *testing.T) {
	s, deps, _ := linearAgentTestServer(t)
	connectLinearAgent(t, s)
	// A service wired while AgentMgr is nil (misconfigured harness) must
	// return the sentinel error from its kick, not panic.
	deps.AgentMgr = nil
	s.linearAgentSvc = s.newLinearAgentService(s.linearAgent().tokenURL, s.linearAgent().graphqlURL)
	s.linearAgent().responder.HandleSessionEvent(sessionEvent("sess-no-mgr"))
	if got := trackedState(t, s, "sess-no-mgr"); got != linearagent.SessionStateFailed {
		t.Fatalf("session state without agent manager = %q, want %q", got, linearagent.SessionStateFailed)
	}
}
