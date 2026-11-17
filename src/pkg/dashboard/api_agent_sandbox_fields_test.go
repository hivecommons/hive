package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#18797: an agent's sandbox image, runtime and Job fields are
// editable through the General section, and a block can be copied without the
// toggle.

func completeBlock() *config.AgentSandboxOverride {
	return &config.AgentSandboxOverride{
		Image: "spyre-img", Runtime: "job",
		Job: &config.SandboxJobConfig{WorkspaceClaim: "hive-data", ServiceAccount: "sbx", EnvFromSecrets: []string{"gh-token"}, TTLSeconds: 300},
	}
}

func TestSandboxFieldsEditable(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{
		"sandboxImage": "registry.example/sbx:2", "sandboxRuntime": "job",
		"sandboxJob": map[string]any{"workspaceClaim": "hive-data", "serviceAccount": "sbx", "ttlSeconds": 600, "envFromSecrets": []string{"gh-token", " llm-key "}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	got := deps.Config.Agents["scanner"].Sandbox
	if got == nil || got.Image != "registry.example/sbx:2" || got.Runtime != "job" || got.Job == nil ||
		got.Job.WorkspaceClaim != "hive-data" || got.Job.ServiceAccount != "sbx" || got.Job.TTLSeconds != 600 ||
		strings.Join(got.Job.EnvFromSecrets, ",") != "gh-token,llm-key" {
		t.Fatalf("fields not stored: %#v", got)
	}
	if got.Enabled != nil && *got.Enabled {
		t.Fatal("editing fields must not enable the sandbox")
	}
	view := decodeJSON(t, doGet(s, "/api/config/agent/scanner"))
	fields, _ := view["general"].(map[string]any)["sandboxFields"].(map[string]any)
	if fields["sandboxImage"] != "registry.example/sbx:2" || fields["sandboxRuntimeEffective"] != "job" {
		t.Fatalf("GET sandboxFields = %v", fields)
	}
	// Blank values clear back to inherit, and an empty Job is dropped.
	rec = doPut(s, "/api/config/agent/scanner/general", map[string]any{
		"sandboxImage": "", "sandboxRuntime": "",
		"sandboxJob": map[string]any{"workspaceClaim": "", "serviceAccount": "", "ttlSeconds": 0, "envFromSecrets": []string{}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("clear PUT = %d: %s", rec.Code, rec.Body.String())
	}
	got = deps.Config.Agents["scanner"].Sandbox
	if got.Image != "" || got.Runtime != "" || got.Job != nil {
		t.Fatalf("not cleared: %#v", got)
	}
}

func TestSandboxFieldValidation(t *testing.T) {
	s, deps := apiServer(t)
	for name, body := range map[string]map[string]any{
		"runtime":    {"sandboxRuntime": "docker"},
		"image ws":   {"sandboxImage": "a b"},
		"ttl":        {"sandboxJob": map[string]any{"ttlSeconds": -1}},
		"secret val": {"sandboxJob": map[string]any{"envFromSecrets": []string{"TOKEN=abc123"}}},
		"job shape":  {"sandboxJob": "x"},
	} {
		rec := doPut(s, "/api/config/agent/scanner/general", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
	if deps.Config.Agents["scanner"].Sandbox != nil {
		t.Fatalf("a refused edit wrote a block: %#v", deps.Config.Agents["scanner"].Sandbox)
	}
}

func TestSandboxEnabledWithoutImageRefusedNamingField(t *testing.T) {
	s, deps := apiServer(t)
	on := true
	sc := deps.Config.Agents["scanner"]
	sc.Sandbox = &config.AgentSandboxOverride{Enabled: &on, Image: "img", Runtime: "podman"}
	deps.Config.Agents["scanner"] = sc
	// Clearing the only image of an enabled sandbox is refused.
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxImage": ""})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sandbox.image") {
		t.Fatalf("PUT = %d %s, want 400 naming sandbox.image", rec.Code, rec.Body.String())
	}
	if deps.Config.Agents["scanner"].Sandbox.Image != "img" {
		t.Fatal("refused edit changed the image")
	}
	// Enabling and supplying the image in one save works.
	deps.Config.Agents["fresh"] = config.AgentConfig{Backend: "claude"}
	rec = doPut(s, "/api/config/agent/fresh/general", map[string]any{"sandboxEnabled": true, "sandboxImage": "img2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("enable + image = %d: %s", rec.Code, rec.Body.String())
	}
	if b := deps.Config.Agents["fresh"].Sandbox; b == nil || b.Image != "img2" || b.Enabled == nil || !*b.Enabled {
		t.Fatalf("block = %#v", b)
	}
}

func TestSandboxCopyWithoutToggleClearsWarning(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents["deps"] = config.AgentConfig{Backend: "claude", Sandbox: completeBlock()}
	before := decodeJSON(t, doGet(s, "/api/config/agent/scanner"))["general"].(map[string]any)
	if p, _ := before["sandboxDefaultsProblem"].(string); !strings.Contains(p, "no sandbox image") {
		t.Fatalf("precondition: sandboxDefaultsProblem = %v", before["sandboxDefaultsProblem"])
	}
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxCopyFrom": "deps"})
	if rec.Code != http.StatusOK {
		t.Fatalf("copy PUT = %d: %s", rec.Code, rec.Body.String())
	}
	got := deps.Config.Agents["scanner"].Sandbox
	if got == nil || got.Image != "spyre-img" || got.Runtime != "job" || got.Job == nil || got.Job.ServiceAccount != "sbx" ||
		strings.Join(got.Job.EnvFromSecrets, ",") != "gh-token" || got.Enabled == nil || !*got.Enabled {
		t.Fatalf("block not copied and enabled: %#v", got)
	}
	// The copy is deep: editing the receiver must not touch the source.
	got.Job.EnvFromSecrets[0] = "changed"
	if deps.Config.Agents["deps"].Sandbox.Job.EnvFromSecrets[0] != "gh-token" {
		t.Fatal("copy aliases the source's secret list")
	}
	after := decodeJSON(t, doGet(s, "/api/config/agent/scanner"))["general"].(map[string]any)
	if p, _ := after["sandboxDefaultsProblem"].(string); p != "" {
		t.Fatalf("warning not cleared: %q", p)
	}
}

func TestSandboxCopySourcesOnlyListCompleteBlocks(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents["deps"] = config.AgentConfig{Backend: "claude", Sandbox: completeBlock()}
	deps.Config.Agents["noimage"] = config.AgentConfig{Backend: "claude", Sandbox: &config.AgentSandboxOverride{Runtime: "podman"}}
	deps.Config.Agents["nojobclaim"] = config.AgentConfig{Backend: "claude", Sandbox: &config.AgentSandboxOverride{Image: "i", Runtime: "job"}}
	general := decodeJSON(t, doGet(s, "/api/config/agent/scanner"))["general"].(map[string]any)
	sources, _ := general["sandboxCopySources"].([]any)
	if len(sources) != 1 || sources[0] != "deps" {
		t.Fatalf("sandboxCopySources = %v, want [deps]", sources)
	}
}

func TestSecuritySectionSetsDefaultSandboxImageAndJobSecrets(t *testing.T) {
	s, deps := apiServer(t)
	rec := doPut(s, "/api/config/governor/security", map[string]any{
		"agentSandboxImage": "registry.example/default:1", "agentSandboxRuntime": "job",
		"agentSandboxJobWorkspaceClaim": "hive-data", "agentSandboxJobEnvFromSecrets": []string{"gh-token"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	g := deps.Config.AgentSandbox
	if g.Image != "registry.example/default:1" || g.Job == nil || strings.Join(g.Job.EnvFromSecrets, ",") != "gh-token" {
		t.Fatalf("default not stored: %#v", g)
	}
	// With the default set, an agent's toggle no longer needs a copy source.
	if rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"sandboxEnabled": true}); rec.Code != http.StatusOK {
		t.Fatalf("toggle after default = %d: %s", rec.Code, rec.Body.String())
	}
	rec = doPut(s, "/api/config/governor/security", map[string]any{"agentSandboxJobEnvFromSecrets": []string{"KEY=value"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("secret value accepted: %d %s", rec.Code, rec.Body.String())
	}
}

func TestApplySandboxJobFieldsImagePullSecrets(t *testing.T) {
	var job config.SandboxJobConfig
	if err := applySandboxJobFields(&job, map[string]interface{}{"imagePullSecrets": []interface{}{"reg-a", " reg-b "}}); err != nil {
		t.Fatal(err)
	}
	if len(job.ImagePullSecrets) != 2 || job.ImagePullSecrets[1] != "reg-b" {
		t.Errorf("ImagePullSecrets = %v", job.ImagePullSecrets)
	}
	if err := applySandboxJobFields(&job, map[string]interface{}{"imagePullSecrets": []interface{}{"user:pass"}}); err == nil {
		t.Error("a value pasted as a name was accepted")
	}
}
