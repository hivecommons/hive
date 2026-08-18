package dashboard

import (
	"net/http"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

func TestGovernorSecurityEndpointUpdatesOverlayFields(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents["reviewer"] = config.AgentConfig{Enabled: true, Role: "reviewer"}

	rec := doPut(s, "/api/config/governor/security", map[string]any{
		"ioscanEnabled":            false,
		"ioscanFailMode":           "closed",
		"ioscanCanaries":           true,
		"intentEnforce":            true,
		"intentAlignmentModel":     "gpt-4o",
		"reviewRequireApproval":    true,
		"reviewFanOut":             true,
		"reviewMaxParallelReviews": 3,
		"reviewReviewerAgents":     []string{"reviewer"},
		"reviewFixerAgent":         "scanner",
		"agentSandboxEnabled":      true,
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
	if !deps.Config.Intent.Enforce || !deps.Config.Review.RequireApproval || !deps.Config.Review.FanOut || !deps.Config.AgentSandbox.Enabled {
		t.Fatalf("security settings not updated: intent=%+v review=%+v sandbox=%+v",
			deps.Config.Intent, deps.Config.Review, deps.Config.AgentSandbox)
	}
	if deps.Config.Intent.AlignmentModel != "gpt-4o" || deps.Config.Review.MaxParallelReviews != 3 ||
		deps.Config.Review.FixerAgent != "scanner" {
		t.Fatalf("advanced security settings not updated: intent=%+v review=%+v",
			deps.Config.Intent, deps.Config.Review)
	}

	body := decodeJSON(t, doGet(s, "/api/config/governor"))
	sec, ok := body["security"].(map[string]any)
	if !ok {
		t.Fatalf("security section missing: %#v", body["security"])
	}
	if sec["ioscanFailMode"] != "closed" || sec["reviewCapableAgents"].(float64) != 1 ||
		sec["intentAlignmentModel"] != "gpt-4o" {
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

func TestGovernorSecurityRejectsInvalidAdvancedValues(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "bad review max", body: map[string]any{"reviewMaxParallelReviews": 65}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := apiServer(t)
			rec := doPut(s, "/api/config/governor/security", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
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
