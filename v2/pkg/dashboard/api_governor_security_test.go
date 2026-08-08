package dashboard

import (
	"net/http"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
)

func TestGovernorSecurityEndpointUpdatesOverlayFields(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents["reviewer"] = config.AgentConfig{Enabled: true, Role: "reviewer"}

	rec := doPut(s, "/api/config/governor/security", map[string]any{
		"ioscanEnabled":         false,
		"ioscanFailMode":        "closed",
		"ioscanCanaries":        true,
		"intentEnforce":         true,
		"reviewRequireApproval": true,
		"reviewFanOut":          true,
		"retroEnabled":          true,
		"agentSandboxEnabled":   true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT security = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if deps.Config.Ioscan.IsEnabled() {
		t.Fatal("ioscan enabled was not persisted as explicit false")
	}
	if !deps.Config.Ioscan.FailClosed() || !deps.Config.Ioscan.Canaries {
		t.Fatalf("ioscan settings not updated: %+v", deps.Config.Ioscan)
	}
	if !deps.Config.Intent.Enforce || !deps.Config.Review.RequireApproval || !deps.Config.Review.FanOut || !deps.Config.Retro.Enabled || !deps.Config.AgentSandbox.Enabled {
		t.Fatalf("security settings not updated: intent=%+v review=%+v retro=%+v sandbox=%+v",
			deps.Config.Intent, deps.Config.Review, deps.Config.Retro, deps.Config.AgentSandbox)
	}

	body := decodeJSON(t, doGet(s, "/api/config/governor"))
	sec, ok := body["security"].(map[string]any)
	if !ok {
		t.Fatalf("security section missing: %#v", body["security"])
	}
	if sec["ioscanFailMode"] != "closed" || sec["reviewCapableAgents"].(float64) != 1 {
		t.Fatalf("security section wrong: %#v", sec)
	}
}

func TestGovernorSecurityRejectsInvalidFailMode(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/governor/security", map[string]any{"ioscanFailMode": "panic"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid fail mode status = %d, want 400", rec.Code)
	}
	if deps.Config.Ioscan.FailMode != "" {
		t.Fatalf("invalid fail mode mutated config: %q", deps.Config.Ioscan.FailMode)
	}
}

func TestAgentConfigGeneralPersistsSandboxOptIn(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxEnabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT agent general = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := deps.Config.Agents["scanner"].Sandbox
	if got == nil || got.Enabled == nil || !*got.Enabled {
		t.Fatalf("sandbox opt-in not persisted: %#v", got)
	}
}
