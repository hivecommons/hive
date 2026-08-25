package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

func TestAgentConfigGetWorksForConfiguredDisabledAgent(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Agents["guide"] = config.AgentConfig{
		Backend:      "claude",
		Model:        "sonnet",
		Enabled:      false,
		DisplayName:  "Guide",
		ModelOwner:   config.FieldOwnerPack,
		BackendOwner: config.FieldOwnerOperator,
	}

	rec := doGet(s, "/api/config/agent/guide")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for configured agent without a process: %s", rec.Code, rec.Body.String())
	}
	general := decodeJSON(t, rec)["general"].(map[string]any)
	if enabled, ok := general["enabled"].(bool); !ok || enabled {
		t.Fatalf("general.enabled = %#v, want false", general["enabled"])
	}
	if general["modelOwner"] != config.FieldOwnerPack || general["backendOwner"] != config.FieldOwnerOperator {
		t.Fatalf("ownership markers missing from config response: %#v", general)
	}
}

func TestConfiguredAgentsIncludesDisabledAgentsWithoutProcesses(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"running": {Enabled: true, SortOrder: 20},
		"guide":   {Enabled: false, SortOrder: 10, DisplayName: "Guide", ModelOwner: config.FieldOwnerPack},
	}}

	agents := buildConfiguredAgents(cfg)
	if len(agents) != 2 {
		t.Fatalf("configured agents = %d, want 2", len(agents))
	}
	if agents[0].Name != "guide" || agents[0].Enabled || agents[0].DisplayName != "Guide" {
		t.Fatalf("first configured agent = %#v, want disabled guide sorted first", agents[0])
	}
}

func TestDisabledAgentSidebarEnableWiring(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`window._configuredAgents = data.configuredAgents || [];`,
		`if (!configured.enabled && !runtimeNames.has(configured.name))`,
		`class="oc-nav-item disabled-agent"`,
		`data-tab="General" title="Configure and enable`,
		`id="cfg-agent-enabled"`,
		`data-key="enabled"`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing disabled-agent enable wiring %q", snippet)
		}
	}
}
