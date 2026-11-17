package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#18525: the "Run in sandbox" toggle used to write
// enabled: true alone, which wedged an agent when agent_sandbox had no image.

func TestSandboxToggleRefusedWithoutDefaults(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxEnabled": true})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"no sandbox image", "copy_from", "no agent has one yet"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal %q does not mention %q", body, want)
		}
	}
	if got := deps.Config.Agents["scanner"].Sandbox; got != nil {
		t.Fatalf("a refused toggle must not write a block: %#v", got)
	}
}

func TestSandboxToggleRefusalListsCopySources(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents["spyre"] = config.AgentConfig{Backend: "claude", Sandbox: &config.AgentSandboxOverride{Image: "spyre-img"}}
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxEnabled": true})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "candidates: spyre") {
		t.Fatalf("PUT = %d %s, want 400 naming spyre", rec.Code, rec.Body.String())
	}
}

func TestSandboxTogglePrefillsFromDefaults(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.AgentSandbox = config.AgentSandboxConfig{Enabled: true, Image: "registry.example/sandbox:1", Runtime: "job", Job: &config.SandboxJobConfig{WorkspaceClaim: "hive-data"}}
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxEnabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	got := deps.Config.Agents["scanner"].Sandbox
	if got == nil || got.Enabled == nil || !*got.Enabled {
		t.Fatalf("not enabled: %#v", got)
	}
	if got.Image != "registry.example/sandbox:1" || got.Runtime != "job" {
		t.Fatalf("block not prefilled from agent_sandbox: %#v", got)
	}
}

func TestSandboxToggleCopiesFromAnotherAgent(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents["spyre"] = config.AgentConfig{Backend: "claude", Sandbox: &config.AgentSandboxOverride{
		Image:   "spyre-img",
		Runtime: "job",
		Job:     &config.SandboxJobConfig{WorkspaceClaim: "hive-data", NodeSelector: map[string]string{"accel": "spyre"}},
		Publish: &config.SandboxPublishConfig{Verify: []string{"pytest tests/spyre"}},
	}}
	sc := deps.Config.Agents["scanner"]
	sc.Sandbox = &config.AgentSandboxOverride{Publish: &config.SandboxPublishConfig{Verify: []string{"make lint"}}}
	deps.Config.Agents["scanner"] = sc

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxEnabled": true, "copy_from": "spyre"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	got := deps.Config.Agents["scanner"].Sandbox
	if got == nil || got.Image != "spyre-img" || got.Runtime != "job" || got.Job == nil || got.Job.NodeSelector["accel"] != "spyre" {
		t.Fatalf("block not copied: %#v", got)
	}
	if got.Publish == nil || len(got.Publish.Verify) != 1 || got.Publish.Verify[0] != "make lint" {
		t.Fatalf("publish.verify must stay the receiver's own: %#v", got.Publish)
	}
	if got.Enabled == nil || !*got.Enabled {
		t.Fatal("copied block not enabled")
	}
}

func TestSandboxToggleCopyFromRefusals(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents["bare"] = config.AgentConfig{Backend: "claude"}
	deps.Config.Agents["partial"] = config.AgentConfig{Backend: "claude", Sandbox: &config.AgentSandboxOverride{Runtime: "job", Image: "img"}}
	for _, tt := range []struct{ from, want string }{
		{"ghost", "not found"},
		{"scanner", "into itself"},
		{"bare", "no sandbox block"},
		{"partial", "workspace_claim"},
	} {
		rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxEnabled": true, "sandboxCopyFrom": tt.from})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tt.want) {
			t.Errorf("copy from %s: %d %s, want 400 mentioning %q", tt.from, rec.Code, rec.Body.String(), tt.want)
		}
	}
}

func TestSandboxToggleOffKeepsBlock(t *testing.T) {
	s, deps := apiServer(t)
	on := true
	sc := deps.Config.Agents["scanner"]
	sc.Sandbox = &config.AgentSandboxOverride{Enabled: &on, Image: "img", Runtime: "podman"}
	deps.Config.Agents["scanner"] = sc
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxEnabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	got := deps.Config.Agents["scanner"].Sandbox
	if got == nil || got.Enabled == nil || *got.Enabled || got.Image != "img" || got.Runtime != "podman" {
		t.Fatalf("toggle off must keep the block with enabled: false: %#v", got)
	}

	// Off on an agent with no block writes a minimal disabled one.
	deps.Config.Agents["fresh"] = config.AgentConfig{Backend: "claude"}
	rec = doPut(s, "/api/config/agent/fresh/general", map[string]any{"sandboxEnabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT fresh = %d: %s", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Agents["fresh"].Sandbox; got == nil || got.Enabled == nil || *got.Enabled {
		t.Fatalf("fresh block = %#v", got)
	}
}

func TestAgentConfigGetReportsSandboxResolution(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents["spyre"] = config.AgentConfig{Backend: "claude", Sandbox: &config.AgentSandboxOverride{Image: "spyre-img"}}
	body := decodeJSON(t, doGet(s, "/api/config/agent/scanner"))
	general, _ := body["general"].(map[string]any)
	if p, _ := general["sandboxDefaultsProblem"].(string); !strings.Contains(p, "no sandbox image") {
		t.Fatalf("sandboxDefaultsProblem = %v", general["sandboxDefaultsProblem"])
	}
	sources, _ := general["sandboxCopySources"].([]any)
	if len(sources) != 1 || sources[0] != "spyre" {
		t.Fatalf("sandboxCopySources = %v, want [spyre]", general["sandboxCopySources"])
	}
}

func TestSandboxCopyFromBody(t *testing.T) {
	if got := sandboxCopyFromBody(map[string]interface{}{"copy_from": " spyre "}); got != "spyre" {
		t.Fatalf("copy_from = %q", got)
	}
	if got := sandboxCopyFromBody(map[string]interface{}{"sandboxCopyFrom": 3}); got != "" {
		t.Fatalf("non-string = %q", got)
	}
}
