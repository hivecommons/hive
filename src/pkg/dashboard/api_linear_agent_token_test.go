package dashboard

// Tests for Server.LinearAgentAccessToken — the value pkg/agent injects into
// ISSUES_ONLY+ agents as LINEAR_ACCESS_TOKEN. Covers the connected, empty,
// unreadable-store, and refresh-on-expiry shapes, plus the sentinel error
// strings the responder surfaces into Linear sessions.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/linearagent"
)

// seedLinearInstall writes a connected install into the service's store.
func seedLinearInstall(t *testing.T, s *Server, tok linearagent.Token) {
	t.Helper()
	svc := s.linearAgent().(*testLinearService)
	if svc.store == nil {
		t.Fatal("test service has no store")
	}
	inst := linearagent.Install{
		ViewerID:         "viewer-1",
		OrganizationID:   "org-1",
		OrganizationName: "Acme",
		Token:            tok,
		ConnectedAt:      time.Now(),
	}
	if err := svc.store.Set(inst); err != nil {
		t.Fatalf("seed install: %v", err)
	}
}

func TestLinearAgentAccessToken_ConnectedWorkspace(t *testing.T) {
	s, _, _ := linearAgentTestServer(t)
	seedLinearInstall(t, s, linearagent.Token{
		AccessToken: "at-live",
		ExpiresAt:   time.Now().Add(time.Hour),
	})

	if got := s.LinearAgentAccessToken(); got != "at-live" {
		t.Errorf("LinearAgentAccessToken() = %q, want at-live", got)
	}
}

func TestLinearAgentAccessToken_NotConnectedIsEmpty(t *testing.T) {
	// The steady state of every GitHub-only hive: no install, no token, no
	// error escaping to the launch path.
	s, _, _ := linearAgentTestServer(t)

	if got := s.LinearAgentAccessToken(); got != "" {
		t.Errorf("LinearAgentAccessToken() = %q, want empty when not installed", got)
	}
}

func TestLinearAgentAccessToken_NilClientIsEmpty(t *testing.T) {
	// A corrupt store file leaves svc.client nil (newLinearAgentService
	// returns early); the token export must degrade to "" not panic.
	s, _, _ := linearAgentTestServer(t)
	s.linearAgent().(*testLinearService).client = nil

	if got := s.LinearAgentAccessToken(); got != "" {
		t.Errorf("LinearAgentAccessToken() = %q, want empty with nil client", got)
	}
}

func TestLinearAgentAccessToken_RefreshesExpiredToken(t *testing.T) {
	t.Setenv(linearagent.StoreEnvVar, filepath.Join(t.TempDir(), "linear-agent.json"))
	t.Setenv("LINEAR_CLIENT_ID", "client-id")
	t.Setenv("LINEAR_CLIENT_SECRET", "client-secret")

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "at-refreshed", "refresh_token": "rt-2", "expires_in": 86399,
		})
	}))
	t.Cleanup(fake.Close)

	s, deps := apiServer(t)
	deps.NewLinearAgent = newTestLinearAgentFactory(s.logger, fake.URL+"/oauth/token", fake.URL+"/graphql")
	seedLinearInstall(t, s, linearagent.Token{
		AccessToken:  "at-stale",
		RefreshToken: "rt-1",
		ExpiresAt:    time.Now().Add(-time.Minute),
	})

	if got := s.LinearAgentAccessToken(); got != "at-refreshed" {
		t.Errorf("LinearAgentAccessToken() = %q, want at-refreshed", got)
	}
}

func TestLinearAgentSentinelErrorStrings(t *testing.T) {
	// These strings surface in Linear session feeds and status responses;
	// pin them so a reword is a conscious choice.
	if got := errNoAgentManager.Error(); got != "agent manager unavailable" {
		t.Errorf("errNoAgentManager = %q", got)
	}
	if got := errSessionAgentUnset.Error(); !strings.Contains(got, "work_source.linear.session_agent") {
		t.Errorf("errSessionAgentUnset = %q, want it to name the config key", got)
	}
}
