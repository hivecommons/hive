package config

import "testing"

// An agent switched between the claude and copilot backends keeps the other
// CLI's spelling of its Claude model, which its own CLI rejects. The loader
// rewrites it in both directions and records each rewrite; valid ids,
// non-Claude ids and other backends are left alone.
func TestNormalizeModelIDConventions(t *testing.T) {
	c := &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{
		"architect":  {Backend: ClaudeBackend, Model: "claude-sonnet-5.5"},
		"guide":      {Backend: ClaudeBackend, Model: "claude-opus-4.6"},
		"healthy":    {Backend: ClaudeBackend, Model: "claude-sonnet-5-5"},
		"alias":      {Backend: ClaudeBackend, Model: "sonnet"},
		"supervisor": {Backend: "copilot", Model: "claude-sonnet-4-6"},
		"brainstorm": {Backend: "copilot", Model: "claude-sonnet-5"},
		"copilotok":  {Backend: "copilot", Model: "claude-sonnet-5.5"},
		"gpt":        {Backend: "copilot", Model: "gpt-5-mini"},
		"gateway":    {Backend: "litellm", Model: "claude-sonnet-5-5"},
	}}}

	got := c.NormalizeModelIDConventions()

	want := []ModelIDNormalization{
		{Agent: "architect", Backend: ClaudeBackend, From: "claude-sonnet-5.5", To: "claude-sonnet-5-5"},
		{Agent: "guide", Backend: ClaudeBackend, From: "claude-opus-4.6", To: "claude-opus-4-6"},
		{Agent: "supervisor", Backend: "copilot", From: "claude-sonnet-4-6", To: "claude-sonnet-4.6"},
	}
	if len(got) != len(want) {
		t.Fatalf("normalizations = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("normalization[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(c.ModelIDNormalizations) != len(want) {
		t.Errorf("ModelIDNormalizations not recorded: %+v", c.ModelIDNormalizations)
	}
	for name, model := range map[string]string{
		"architect":  "claude-sonnet-5-5",
		"guide":      "claude-opus-4-6",
		"healthy":    "claude-sonnet-5-5",
		"alias":      "sonnet",
		"supervisor": "claude-sonnet-4.6",
		"brainstorm": "claude-sonnet-5",
		"copilotok":  "claude-sonnet-5.5",
		"gpt":        "gpt-5-mini",
		"gateway":    "claude-sonnet-5-5",
	} {
		if c.Agents[name].Model != model {
			t.Errorf("agent %s model = %q, want %q", name, c.Agents[name].Model, model)
		}
	}

	if again := c.NormalizeModelIDConventions(); len(again) != 0 {
		t.Errorf("second pass rewrote again: %+v", again)
	}
}
