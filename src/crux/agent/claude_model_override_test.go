package agent

import (
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// A claude agent left holding the Copilot spelling of a Claude model
// (claude-sonnet-5.5) must launch with the Claude CLI's id
// (claude-sonnet-5-5): the Claude CLI refuses the dotted spelling on every
// turn. Aliases and unknown ids still pass through verbatim.
func TestNormalizeModelNameForBackend_ClaudeRespellsCopilotIDs(t *testing.T) {
	cases := []struct{ model, want string }{
		{"claude-sonnet-5.5", "claude-sonnet-5-5"},
		{"claude-opus-5.5", "claude-opus-5-5"},
		{"claude-haiku-4.5", "claude-haiku-4-5-20251001"},
		{"claude-sonnet-5-5", "claude-sonnet-5-5"},
		{"opus", "opus"},
		{"claude-sonnet-9.9", "claude-sonnet-9.9"},
	}
	for _, c := range cases {
		if got := normalizeModelNameForBackend(c.model, config.ClaudeBackend, false); got != c.want {
			t.Errorf("normalizeModelNameForBackend(%q, claude) = %q, want %q", c.model, got, c.want)
		}
	}
	// Inference backends launch the claude CLI too, but their model is a
	// gateway id and must stay verbatim.
	if got := normalizeModelNameForBackend("claude-sonnet-5.5", "litellm", true); got != "claude-sonnet-5.5" {
		t.Errorf("inference model rewritten: %q", got)
	}
}

// The model override is persisted to hive-state and replayed at boot, so on
// the claude backend it must be stored in the Claude CLI's spelling.
func TestSetModelOverride_ClaudeStoresClaudeSpelling(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})
	if err := m.SetModelOverride("a", "claude-sonnet-5.5"); err != nil {
		t.Fatal(err)
	}
	if got := m.agents["a"].ModelOverride; got != "claude-sonnet-5-5" {
		t.Errorf("ModelOverride = %q, want claude-sonnet-5-5", got)
	}
}

// A copilot→claude switch must not keep the Copilot spelling: the stored
// override is re-spelled for the new backend, a pin follows it, and a model
// the Claude CLI cannot run at all is dropped.
func TestSetBackendOverride_RespellsModelOverrideForClaude(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "copilot", Model: "claude-sonnet-4.6"},
		"b": {Backend: "copilot", Model: "gpt-5.5"},
	}, discardLogger(), ProjectContext{})

	// On copilot the dashed input is stored in the Copilot spelling.
	if err := m.PinModel("a", "claude-sonnet-5.5"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetModelOverride("a", "claude-sonnet-5-5"); err != nil {
		t.Fatal(err)
	}
	if got := m.agents["a"].ModelOverride; got != "claude-sonnet-5.5" {
		t.Fatalf("precondition: copilot ModelOverride = %q, want claude-sonnet-5.5", got)
	}
	if err := m.SetBackendOverride("a", "claude"); err != nil {
		t.Fatal(err)
	}
	a := m.agents["a"]
	if a.ModelOverride != "claude-sonnet-5-5" {
		t.Errorf("after switch to claude: ModelOverride = %q, want claude-sonnet-5-5", a.ModelOverride)
	}
	if a.PinnedModel != "claude-sonnet-5-5" {
		t.Errorf("after switch to claude: PinnedModel = %q, want the pin to follow the override", a.PinnedModel)
	}

	// No Claude equivalent: dropped, so the agent falls back to its config.
	if err := m.SetModelOverride("b", "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetBackendOverride("b", "claude"); err != nil {
		t.Fatal(err)
	}
	if got := m.agents["b"].ModelOverride; got != "" {
		t.Errorf("gpt-5.5 kept as the override on claude: %q", got)
	}

	// And back: claude→copilot takes the Copilot spelling.
	if err := m.SetBackendOverride("a", "copilot"); err != nil {
		t.Fatal(err)
	}
	if got := m.agents["a"].ModelOverride; got != "claude-sonnet-5.5" {
		t.Errorf("after switch back to copilot: ModelOverride = %q, want claude-sonnet-5.5", got)
	}
}
