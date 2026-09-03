package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/linearagent"
)

// linearAgentTestServer wires an apiServer whose linearagent service points at
// fake token + GraphQL endpoints and an isolated install store.
func linearAgentTestServer(t *testing.T) (*Server, *Dependencies, *httptest.Server) {
	t.Helper()
	t.Setenv(linearagent.StoreEnvVar, filepath.Join(t.TempDir(), "linear-agent.json"))
	t.Setenv("LINEAR_CLIENT_ID", "client-id")
	t.Setenv("LINEAR_CLIENT_SECRET", "client-secret")
	t.Setenv("LINEAR_WEBHOOK_SECRET", "hook-secret")

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "at-1", "refresh_token": "rt-1", "expires_in": 86399,
				"scope": "read,write,app:assignable,app:mentionable",
			})
		case "/graphql":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"viewer":       map[string]string{"id": "viewer-1"},
					"organization": map[string]string{"id": "org-1", "name": "Acme", "urlKey": "acme"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.Close)

	s, deps := apiServer(t)
	s.linearAgentSvc = s.newLinearAgentService(fake.URL+"/oauth/token", fake.URL+"/graphql")
	return s, deps, fake
}

func TestLinearAgentRoutesOwnerGated(t *testing.T) {
	s, _, _ := linearAgentTestServer(t)
	for _, probe := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/linear/agent/install"},
		{http.MethodGet, "/api/linear/agent/status"},
		{http.MethodPost, "/api/linear/agent/disconnect"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(probe.method, probe.path, nil)
		s.mux.ServeHTTP(rec, req) // no owner headers
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without owner role = %d, want 403", probe.method, probe.path, rec.Code)
		}
	}
}

func TestLinearAgentPublicPaths(t *testing.T) {
	// The callback and webhook must bypass dashboard auth (Linear cannot
	// authenticate); nothing else Linear-related may.
	for path, want := range map[string]bool{
		"/linear/callback":         true,
		"/api/linear/webhook":      true,
		"/api/linear/agent/status": false,
	} {
		if got := isPublicPath(path); got != want {
			t.Errorf("isPublicPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestLinearAgentInstallReturnsAuthorizeURL(t *testing.T) {
	s, _, _ := linearAgentTestServer(t)
	rec := doOwnerPost(s, "/api/linear/agent/install", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("install = %d — %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"https://linear.app/oauth/authorize", "actor=app", "client_id=client-id", "state=", "app%3Aassignable", "app%3Amentionable", "%2Flinear%2Fcallback"} {
		if !strings.Contains(resp.AuthorizeURL, want) {
			t.Errorf("authorize_url missing %q: %s", want, resp.AuthorizeURL)
		}
	}
}

func TestLinearAgentInstallRequiresCredentials(t *testing.T) {
	t.Setenv(linearagent.StoreEnvVar, filepath.Join(t.TempDir(), "store.json"))
	t.Setenv("LINEAR_CLIENT_ID", "")
	t.Setenv("LINEAR_CLIENT_SECRET", "")
	s, _ := apiServer(t)
	if rec := doOwnerPost(s, "/api/linear/agent/install", nil); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("install without creds = %d", rec.Code)
	}
}

func TestLinearAgentCallbackFlow(t *testing.T) {
	s, _, _ := linearAgentTestServer(t)

	// Bad / unknown state is rejected before any exchange.
	rec := doGet(s, "/linear/callback?code=abc&state=bogus")
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "linear=error") {
		t.Fatalf("bogus state: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Missing code likewise.
	if rec := doGet(s, "/linear/callback?state=x"); !strings.Contains(rec.Header().Get("Location"), "linear=error") {
		t.Fatalf("missing code: %s", rec.Header().Get("Location"))
	}

	// Happy path with a real single-use state.
	state, err := s.linearAgent().states.Create()
	if err != nil {
		t.Fatal(err)
	}
	rec = doGet(s, "/linear/callback?code=abc&state="+state)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "linear=connected") {
		t.Fatalf("callback: %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// The state was consumed: replaying the same callback fails.
	rec = doGet(s, "/linear/callback?code=abc&state="+state)
	if !strings.Contains(rec.Header().Get("Location"), "linear=error") {
		t.Fatalf("state replay accepted: %s", rec.Header().Get("Location"))
	}

	// The install landed and status reflects it.
	rec = doOwnerGet(s, "/api/linear/agent/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var status struct {
		Configured   bool   `json:"configured"`
		Connected    bool   `json:"connected"`
		ViewerID     string `json:"viewer_id"`
		SessionAgent string `json:"session_agent"`
		WebhookPath  string `json:"webhook_path"`
		Workspace    struct {
			Name string `json:"name"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Configured || !status.Connected || status.ViewerID != "viewer-1" || status.Workspace.Name != "Acme" {
		t.Errorf("status = %+v", status)
	}
	if status.SessionAgent != "scanner" {
		t.Errorf("session_agent = %q (testDeps has exactly one agent)", status.SessionAgent)
	}
	if status.WebhookPath != "/api/linear/webhook" {
		t.Errorf("webhook_path = %q", status.WebhookPath)
	}

	// Disconnect forgets the install.
	if rec := doOwnerPost(s, "/api/linear/agent/disconnect", nil); rec.Code != http.StatusOK {
		t.Fatalf("disconnect = %d", rec.Code)
	}
	rec = doOwnerGet(s, "/api/linear/agent/status")
	var after struct {
		Connected bool `json:"connected"`
	}
	json.Unmarshal(rec.Body.Bytes(), &after)
	if after.Connected {
		t.Error("still connected after disconnect")
	}
}

func TestLinearAgentWebhookEndpoint(t *testing.T) {
	s, _, _ := linearAgentTestServer(t)

	body, _ := json.Marshal(map[string]interface{}{
		"type": "Issue", "action": "update",
		"webhookTimestamp": time.Now().UnixMilli(),
	})
	mac := hmac.New(sha256.New, []byte("hook-secret"))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	// Unsigned → 401 (the signature is the credential on this public path).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/linear/webhook", strings.NewReader(string(body)))
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook = %d, want 401", rec.Code)
	}

	// Signed → accepted.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/linear/webhook", strings.NewReader(string(body)))
	req.Header.Set("Linear-Signature", sig)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("signed webhook = %d — %s", rec.Code, rec.Body.String())
	}
}

func TestResolveLinearSessionAgent(t *testing.T) {
	s, deps, _ := linearAgentTestServer(t)

	// Exactly one agent → it is the implicit session agent.
	name, err := s.resolveLinearSessionAgent()
	if err != nil || name != "scanner" {
		t.Fatalf("sole agent: %q, %v", name, err)
	}

	// Explicit session_agent wins when it names a real agent.
	deps.Config.Governor.WorkSource.Linear.SessionAgent = "scanner"
	if name, err := s.resolveLinearSessionAgent(); err != nil || name != "scanner" {
		t.Fatalf("explicit agent: %q, %v", name, err)
	}

	// An unknown name is an error, not a silent fallback.
	deps.Config.Governor.WorkSource.Linear.SessionAgent = "ghost"
	if _, err := s.resolveLinearSessionAgent(); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("unknown agent err = %v", err)
	}

	// Multiple agents with no explicit choice is ambiguous → error.
	deps.Config.Governor.WorkSource.Linear.SessionAgent = ""
	deps.Config.Agents["second"] = deps.Config.Agents["scanner"]
	if _, err := s.resolveLinearSessionAgent(); err == nil {
		t.Fatal("ambiguous agent set did not error")
	}

	// ACMM fallback: with several agents but exactly one tracker writer
	// (CanCreateIssues), that writer takes sessions — the L3 shape.
	second := deps.Config.Agents["second"]
	second.Mode = "ISSUES_AND_PRS"
	deps.Config.Agents["second"] = second
	scanner := deps.Config.Agents["scanner"]
	scanner.Mode = "ADVISORY"
	deps.Config.Agents["scanner"] = scanner
	if name, err := s.resolveLinearSessionAgent(); err != nil || name != "second" {
		t.Fatalf("sole writer fallback: %q, %v", name, err)
	}

	// Two writers is ambiguous again.
	scanner.Mode = "ISSUES_ONLY"
	deps.Config.Agents["scanner"] = scanner
	if _, err := s.resolveLinearSessionAgent(); err == nil {
		t.Fatal("two writers did not error")
	}
}

func TestLinearSessionHolder(t *testing.T) {
	s, _, _ := linearAgentTestServer(t)
	tr := s.linearAgent().tracker
	var ev linearagent.SessionEvent
	ev.AgentSession.ID = "sess-9"
	ev.AgentSession.Issue.Identifier = "ENG-9"
	tr.Observe(ev)
	tr.SetAgent("sess-9", "scanner")

	if _, ok := s.LinearSessionHolder(github.Issue{SourceType: "github", Number: 9}); ok {
		t.Fatal("GitHub items are never session-held")
	}
	holder, ok := s.LinearSessionHolder(github.Issue{SourceType: "linear", ExternalID: "ENG-9"})
	if !ok || !strings.Contains(holder, "scanner") || !strings.Contains(holder, "sess-9") {
		t.Fatalf("holder = %q, %v", holder, ok)
	}
	if _, ok := s.LinearSessionHolder(github.Issue{SourceType: "linear", ExternalID: "ENG-10"}); ok {
		t.Fatal("unrelated identifier reported held")
	}
}

func TestLinearAgentCallbackURL(t *testing.T) {
	s, deps, _ := linearAgentTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "http://hive.example:8080/api/linear/agent/install", nil)
	if got := s.linearAgentCallbackURL(req); got != "http://hive.example:8080/linear/callback" {
		t.Errorf("request-host URL = %q", got)
	}

	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "hive.public.example")
	if got := s.linearAgentCallbackURL(req); got != "https://hive.public.example/linear/callback" {
		t.Errorf("forwarded URL = %q", got)
	}

	deps.Config.Hub.DashboardURL = "https://dash.example/"
	if got := s.linearAgentCallbackURL(req); got != "https://dash.example/linear/callback" {
		t.Errorf("configured URL = %q", got)
	}
}
