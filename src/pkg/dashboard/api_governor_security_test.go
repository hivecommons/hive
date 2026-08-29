package dashboard

import (
	"net/http"
	"strings"
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

// TestSecuritySectionSurfacesInertSandboxGate is #4918's dashboard-facing
// half: flipping ONLY the global gate through the Security tab's own PUT
// endpoint must make the resulting GET response say so, not just the server
// log the operator flipping that toggle has no reason to be watching.
func TestSecuritySectionSurfacesInertSandboxGate(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents["quality"] = config.AgentConfig{Backend: "claude", Enabled: true}

	rec := doPut(s, "/api/config/governor/security", map[string]any{"agentSandboxEnabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT security = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	body := decodeJSON(t, doGet(s, "/api/config/governor"))
	sec, ok := body["security"].(map[string]any)
	if !ok {
		t.Fatalf("security section missing: %#v", body["security"])
	}
	warnings, ok := sec["sandboxWarnings"].([]any)
	if !ok {
		t.Fatalf("sandboxWarnings missing or wrong type: %#v", sec["sandboxWarnings"])
	}
	if len(warnings) != 1 {
		t.Fatalf("sandboxWarnings = %#v, want exactly one warning for an inert global-only gate", warnings)
	}
	msg, _ := warnings[0].(string)
	for _, want := range []string{"NO agent is opted in", "unconfined", "#4918"} {
		if !strings.Contains(msg, want) {
			t.Errorf("sandboxWarnings[0] = %q, want it to contain %q", msg, want)
		}
	}
}

// TestSecuritySectionSandboxWarningsEmptyWhenGateOff pins the non-noisy case:
// the documented default (sandbox off globally) must not populate warnings,
// and the field must be an empty array, not JSON null, so callers never need
// a nil-check on top of the falsy-array check.
func TestSecuritySectionSandboxWarningsEmptyWhenGateOff(t *testing.T) {
	s, _ := apiServer(t)
	body := decodeJSON(t, doGet(s, "/api/config/governor"))
	sec, ok := body["security"].(map[string]any)
	if !ok {
		t.Fatalf("security section missing: %#v", body["security"])
	}
	warnings, ok := sec["sandboxWarnings"].([]any)
	if !ok {
		t.Fatalf("sandboxWarnings missing or wrong type: %#v", sec["sandboxWarnings"])
	}
	if len(warnings) != 0 {
		t.Errorf("sandboxWarnings = %#v, want none when the sandbox is off globally (the default)", warnings)
	}
}

// TestSecuritySectionSandboxWarningsEmptyWhenFullyOptedIn: a correctly
// configured hive (global gate on, every agent opted in) must produce no
// warnings — the diagnostic must not become noise on a healthy config.
func TestSecuritySectionSandboxWarningsEmptyWhenFullyOptedIn(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.AgentSandbox = config.AgentSandboxConfig{Enabled: true, Image: "ghcr.io/example/agent:latest"}
	deps.Config.Agents["scanner"] = config.AgentConfig{
		Backend: "claude", Enabled: true,
		Sandbox: &config.AgentSandboxOverride{Enabled: boolPtrGovSecurityTest(true)},
	}

	body := decodeJSON(t, doGet(s, "/api/config/governor"))
	sec, ok := body["security"].(map[string]any)
	if !ok {
		t.Fatalf("security section missing: %#v", body["security"])
	}
	warnings, _ := sec["sandboxWarnings"].([]any)
	if len(warnings) != 0 {
		t.Errorf("sandboxWarnings = %#v, want none for a fully opted-in hive", warnings)
	}
}

func boolPtrGovSecurityTest(b bool) *bool { return &b }
