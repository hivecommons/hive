package dashboard

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
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
		// Wiring, not prose: the button must open the agent config dialog on the
		// General tab. The visible label deliberately leads with a gear so the
		// row reads as configurable rather than as a one-click state toggle.
		`data-tab="General" title="Configure `,
		`>⚙ Enable…</button>`,
		`id="cfg-agent-enabled"`,
		`data-key="enabled"`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing disabled-agent enable wiring %q", snippet)
		}
	}
}

// TestConfiguredAgentsCarryResolvedMode pins the field the sidebar needs to
// show what a disabled agent WOULD do once enabled. A disabled agent has no
// runtime entry, so if buildConfiguredAgents omits the mode the badge silently
// renders nothing — dead markup that looks fine in review.
//
// Both branches matter: an explicit per-agent Mode must win, and an agent that
// sets none must still report the ACMM level default rather than empty.
func TestConfiguredAgentsCarryResolvedMode(t *testing.T) {
	level := 3
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents: map[string]config.AgentConfig{
			"explicit": {Enabled: false, Mode: "ADVISORY"},
			"default":  {Enabled: false},
		},
	}

	got := map[string]FrontendConfiguredAgent{}
	for _, a := range buildConfiguredAgents(cfg) {
		got[a.Name] = a
	}

	if got["explicit"].Mode != "ADVISORY" {
		t.Errorf("explicit Mode override must win: got %q, want ADVISORY", got["explicit"].Mode)
	}
	if got["explicit"].ModeEmoji == "" {
		t.Error("explicit agent must carry a mode emoji for the sidebar badge")
	}
	if got["default"].Mode == "" {
		t.Error("agent with no Mode override must still report the ACMM level default, not empty")
	}
	if got["default"].ModeEmoji == "" {
		t.Error("level-default agent must carry a mode emoji too")
	}
}
