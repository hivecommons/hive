package dashboard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func TestSubAgentModelsStatus(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"scanner": {Backend: "copilot", Enabled: true},
	}}
	statuses := map[string]*agent.AgentProcess{"scanner": {
		Name: "scanner", Config: cfg.Agents["scanner"],
		SubAgentModels: []agent.SubAgentModel{{AgentType: "General-purpose", Model: "claude-opus-5", OlderGeneration: true}},
	}}
	got := buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle})
	if len(got) != 1 || len(got[0].SubAgentModels) != 1 || !got[0].SubAgentModels[0].OlderGeneration {
		t.Fatalf("status lost model observation: %+v", got)
	}
	encoded, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"subAgentModels":[{"agentType":"General-purpose","model":"claude-opus-5","olderGeneration":true}]`) {
		t.Fatalf("model JSON missing: %s", encoded)
	}
	statuses["scanner"].SubAgentModels = nil
	encoded, err = json.Marshal(buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle})[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"subAgentModels"`) {
		t.Fatal("empty models must be omitted")
	}
}

func TestSubAgentModelsDetailWiring(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"if (!rows.length) return '';",
		"${esc(row.agentType)} → <code>${esc(row.model)}</code>",
		"row.olderGeneration ?",
		"⚠ older generation",
		"${subAgentModelsHtml(a)}",
		"JSON.stringify(a.subAgentModels || [])].join('|')",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("missing detail rendering/refresh guard: %s", snippet)
		}
	}
}
