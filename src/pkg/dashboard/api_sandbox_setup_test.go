package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#19574: sandbox.setup is editable from the dashboard, per
// agent (sandboxSetup) and hive-wide (agentSandboxSetup), with the same
// validation as the preflight.

func TestAgentSandboxSetupEditable(t *testing.T) {
	s, deps := apiServer(t)
	writableConfig(t, deps.Config)
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{
		"sandboxSetup": map[string]any{
			"commands":       []any{"./scripts/install-ml-stack.sh", " "},
			"healthCommands": []any{"python -c 'import torch'"},
			"timeoutS":       600,
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	sb := deps.Config.Agents["scanner"].Sandbox
	if sb == nil || sb.Setup == nil {
		t.Fatalf("no setup block stored: %#v", sb)
	}
	if strings.Join(sb.Setup.Commands, ",") != "./scripts/install-ml-stack.sh" || strings.Join(sb.Setup.HealthCommands, ",") != "python -c 'import torch'" || sb.Setup.TimeoutS != 600 {
		t.Fatalf("setup = %#v", sb.Setup)
	}
	if err := config.ValidateSandboxSetup(sb.Setup); err != nil {
		t.Fatalf("stored setup fails config validation: %v", err)
	}

	fields := decodeJSON(t, doGet(s, "/api/config/agent/scanner"))["general"].(map[string]any)["sandboxFields"].(map[string]any)
	own, _ := fields["sandboxSetup"].(map[string]any)
	if cmds, _ := own["commands"].([]any); len(cmds) != 1 {
		t.Errorf("GET sandboxSetup = %v", fields["sandboxSetup"])
	}
	if d, _ := fields["sandboxSetupDefaults"].(map[string]any); d["timeoutS"] != float64(config.DefaultSandboxSetupTimeoutS) {
		t.Errorf("GET sandboxSetupDefaults = %v", fields["sandboxSetupDefaults"])
	}
	eff := fields["sandboxEffective"].(map[string]any)
	if eff["setupSource"] != sandboxSourceAgent {
		t.Errorf("effective setup source = %v", eff["setupSource"])
	}
	if es, _ := eff["setup"].(map[string]any); es["totalTimeoutS"] != float64(config.DefaultSandboxSetupTotalTimeoutS) {
		t.Errorf("effective setup = %v, want the default total timeout resolved", eff["setup"])
	}

	// null inherits the hive-wide block again.
	rec = doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxSetup": nil})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if sb := deps.Config.Agents["scanner"].Sandbox; sb.Setup != nil {
		t.Fatalf("not cleared: %#v", sb.Setup)
	}
}

func TestAgentSandboxSetupEmptyListsTurnItOff(t *testing.T) {
	s, deps := apiServer(t)
	writableConfig(t, deps.Config)
	deps.Config.AgentSandbox.Setup = &config.SandboxSetupConfig{Commands: []string{"./install.sh"}}
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxSetup": map[string]any{"commands": []any{}, "healthCommands": []any{}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	a := deps.Config.Agents["scanner"]
	if a.Sandbox == nil || a.Sandbox.Setup == nil {
		t.Fatalf("an empty override block must be kept: %#v", a.Sandbox)
	}
	if eff := a.SandboxSetup(deps.Config.AgentSandbox); eff.Enabled() {
		t.Fatalf("effective setup = %+v, want off", eff)
	}
}

func TestAgentSandboxSetupValidation(t *testing.T) {
	s, deps := apiServer(t)
	token := "ghp_" + strings.Repeat("a", 36)
	cases := map[string]map[string]any{
		"shape":          {"sandboxSetup": "./install.sh"},
		"commands shape": {"sandboxSetup": map[string]any{"commands": "./install.sh"}},
		"health shape":   {"sandboxSetup": map[string]any{"healthCommands": map[string]any{}}},
		"negative":       {"sandboxSetup": map[string]any{"commands": []any{"true"}, "timeoutS": -1}},
		"fraction":       {"sandboxSetup": map[string]any{"commands": []any{"true"}, "totalTimeoutS": 1.5}},
		"cred command":   {"sandboxSetup": map[string]any{"commands": []any{"pip install --index-url https://u:" + token + "@mirror/simple x"}}},
		"cred health":    {"sandboxSetup": map[string]any{"healthCommands": []any{"curl -H 'Authorization: token " + token + "' x"}}},
		"long":           {"sandboxSetup": map[string]any{"commands": []any{strings.Repeat("x", 5000)}}},
	}
	for name, body := range cases {
		rec := doPut(s, "/api/config/agent/scanner/general", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
	if deps.Config.Agents["scanner"].Sandbox != nil {
		t.Fatalf("a refused edit wrote a block: %#v", deps.Config.Agents["scanner"].Sandbox)
	}
}

func TestAgentSandboxEffectiveViewInheritsHiveWideSetup(t *testing.T) {
	global := config.AgentSandboxConfig{Setup: &config.SandboxSetupConfig{Commands: []string{"./install.sh"}, HealthCommands: []string{"smoke"}, TotalTimeoutS: 120}}
	view := agentSandboxEffectiveView(config.AgentConfig{}, global)
	if view["setupSource"] != sandboxSourceGlobal {
		t.Fatalf("setup source = %v, want %s", view["setupSource"], sandboxSourceGlobal)
	}
	setup := view["setup"].(map[string]interface{})
	if setup["totalTimeoutS"] != 120 || setup["timeoutS"] != config.DefaultSandboxSetupTimeoutS {
		t.Fatalf("effective setup = %v", setup)
	}
	if v := agentSandboxEffectiveView(config.AgentConfig{}, config.AgentSandboxConfig{}); v["setupSource"] != sandboxSourceUnset {
		t.Fatalf("unset setup source = %v", v["setupSource"])
	}
}

func TestSecuritySectionSetsSandboxSetup(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/governor/security", map[string]any{
		"agentSandboxSetup": map[string]any{"commands": []any{"./scripts/install-ml-stack.sh"}, "healthCommands": []any{"python -c 'import torch'"}, "totalTimeoutS": 1200},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	g := deps.Config.AgentSandbox.Setup
	if g == nil || strings.Join(g.Commands, ",") != "./scripts/install-ml-stack.sh" || len(g.HealthCommands) != 1 || g.TotalTimeoutS != 1200 {
		t.Fatalf("hive-wide setup = %#v", g)
	}
	resp := securitySectionResponse(deps.Config)
	if resp["agentSandboxSetup"] == nil || resp["agentSandboxSetupDefaults"] == nil {
		t.Errorf("response lacks the setup keys: %v / %v", resp["agentSandboxSetup"], resp["agentSandboxSetupDefaults"])
	}

	// Hive-wide, an empty block removes it.
	rec = doPut(s, "/api/config/governor/security", map[string]any{"agentSandboxSetup": map[string]any{"commands": []any{}}})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if deps.Config.AgentSandbox.Setup != nil {
		t.Fatalf("not cleared: %#v", deps.Config.AgentSandbox.Setup)
	}
}

func TestSecuritySectionSandboxSetupValidation(t *testing.T) {
	s, deps := apiServer(t)
	cases := map[string]map[string]any{
		"negative": {"agentSandboxSetup": map[string]any{"timeoutS": -1}},
		"cred":     {"agentSandboxSetup": map[string]any{"commands": []any{"curl -H 'Authorization: token ghp_" + strings.Repeat("b", 36) + "' x"}}},
		// A bad setup refuses the whole request, other fields included.
		"mixed": {"agentSandboxImage": "should-not-land", "agentSandboxSetup": map[string]any{"healthCommands": "not-a-list"}},
	}
	beforeSetup, beforeImage := deps.Config.AgentSandbox.Setup, deps.Config.AgentSandbox.Image
	for name, body := range cases {
		rec := doPut(s, "/api/config/governor/security", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
	if g := deps.Config.AgentSandbox; g.Setup != beforeSetup || g.Image != beforeImage {
		t.Fatalf("a refused edit changed agent_sandbox: %#v", g)
	}
}
