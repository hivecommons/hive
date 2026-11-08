package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// A successful dashboard Claude sign-in must resume the claude agents the
// login detector paused, and nothing else. On hive-wild-mole two sign-ins
// (14:31Z, 14:50Z) wrote a valid shared credential and every login-paused
// agent stayed paused; the owner had to find and resume each one by hand.
// Operator pauses, fleet-breaker, hook and relay pauses are someone's
// decision and must survive the sign-in; a Copilot agent's login prompt is
// not fixed by a Claude login.
func TestClaudeSignIn_ResumesOnlyLoginDetectorPausedClaudeAgents(t *testing.T) {
	stubClaudeSeams(t)
	s := claudeDeepServer(t)
	rt := cruxtest.NewRecorder()
	s.deps.AgentMgr = rt
	s.deps.Ctx = context.Background()

	paused := func(backend, trigger string) *crux.Process {
		return &crux.Process{
			Config:        config.AgentConfig{Backend: backend},
			State:         crux.StatePaused,
			Paused:        true,
			PausedTrigger: trigger,
			PausedReason:  "test",
		}
	}
	rt.Procs["login-claude"] = paused("claude", "login-detector")
	rt.Procs["operator-claude"] = paused("claude", "dashboard-api")
	rt.Procs["breaker-claude"] = paused("claude", "fleet-breaker")
	rt.Procs["hook-claude"] = paused("claude", "hook")
	rt.Procs["relay-claude"] = paused("claude", "contributor-relay")
	rt.Procs["login-copilot"] = paused("copilot", "login-detector")
	override := paused("copilot", "login-detector")
	override.BackendOverride = "claude"
	rt.Procs["login-override-claude"] = override
	rt.Procs["running-claude"] = &crux.Process{Config: config.AgentConfig{Backend: "claude"}, State: crux.StateRunning}
	for name, p := range rt.Procs {
		p.Name = name
	}

	s.claudeOAuthFlow.codeVerifier = "verifier"
	s.claudeOAuthFlow.expiresAt = time.Now().Add(10 * time.Minute)
	claudeExchange = func(code, verifier, redirect string) (*claude.OAuthTokens, error) {
		return &claude.OAuthTokens{AccessToken: "fake-access", RefreshToken: "fake-refresh", ExpiresAt: time.Now().Add(8 * time.Hour).UnixMilli()}, nil
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange", strings.NewReader(`{"code":"good-code"}`))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var resumed []string
	reloadAt, firstResumeAt := -1, -1
	for i, c := range rt.Calls() {
		switch c.Method {
		case "ReloadClaudeToken":
			if reloadAt < 0 {
				reloadAt = i
			}
		case "Resume":
			if firstResumeAt < 0 {
				firstResumeAt = i
			}
			resumed = append(resumed, c.Args[0].(string))
			if c.Args[1] != "claude-sign-in" {
				t.Errorf("resume of %v recorded trigger %v, want claude-sign-in", c.Args[0], c.Args[1])
			}
		}
	}
	want := []string{"login-claude", "login-override-claude"}
	if strings.Join(resumed, ",") != strings.Join(want, ",") {
		t.Fatalf("sign-in resumed %v, want exactly %v", resumed, want)
	}
	if reloadAt < 0 || firstResumeAt < reloadAt {
		t.Errorf("agents must be resumed after the token reload (which re-bridges credentials): reload at %d, first resume at %d", reloadAt, firstResumeAt)
	}
	for _, name := range []string{"operator-claude", "breaker-claude", "hook-claude", "relay-claude", "login-copilot"} {
		if !rt.Procs[name].Paused {
			t.Errorf("%s (trigger %q) was resumed by the sign-in; only login-detector pauses may be", name, rt.Procs[name].PausedTrigger)
		}
	}
	m := decodeJSONMap(t, rec.Body)
	if got, _ := m["resumed"].([]interface{}); len(got) != 2 {
		t.Errorf("response resumed = %v, want the two resumed agents", m["resumed"])
	}
}

// TestBuildAgents_CredentialBridgeBroken: the status payload carries the
// bridge-broken reason so the card can name it instead of a generic
// needs-login.
func TestBuildAgents_CredentialBridgeBroken(t *testing.T) {
	broken := newBlockerProc("architect", config.AgentConfig{Backend: "claude", Enabled: true}, nil)
	broken.NeedsLogin = true
	broken.CredentialBridgeBroken = true
	healthy := newBlockerProc("coder", config.AgentConfig{Backend: "claude", Enabled: true}, nil)
	statuses := map[string]*crux.Process{"architect": broken, "coder": healthy}
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{
		"architect": {Backend: "claude", Enabled: true},
		"coder":     {Backend: "claude", Enabled: true},
	}}}
	got := map[string]FrontendAgent{}
	for _, a := range buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle}) {
		got[a.Name] = a
	}
	if !got["architect"].CredentialBridgeBroken {
		t.Error("architect: credentialBridgeBroken not carried into the status payload")
	}
	if got["coder"].CredentialBridgeBroken {
		t.Error("coder: healthy agent reported a broken credential bridge")
	}
}
