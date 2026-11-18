package config

import "testing"

// A claude agent switched over from copilot keeps the copilot (dotted)
// spelling of its model, which the Claude CLI rejects. The loader rewrites it
// and records the rewrite; other backends and valid ids are left alone.
func TestNormalizeClaudeModelIDs(t *testing.T) {
	c := &Config{Agents: map[string]AgentConfig{
		"architect": {Backend: ClaudeBackend, Model: "claude-sonnet-5.5"},
		"guide":     {Backend: ClaudeBackend, Model: "claude-opus-4.6"},
		"healthy":   {Backend: ClaudeBackend, Model: "claude-sonnet-5-5"},
		"alias":     {Backend: ClaudeBackend, Model: "sonnet"},
		"copilot":   {Backend: "copilot", Model: "claude-sonnet-5.5"},
	}}

	got := c.NormalizeClaudeModelIDs()

	want := []ModelIDNormalization{
		{Agent: "architect", Backend: ClaudeBackend, From: "claude-sonnet-5.5", To: "claude-sonnet-5-5"},
		{Agent: "guide", Backend: ClaudeBackend, From: "claude-opus-4.6", To: "claude-opus-4-6"},
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
		"architect": "claude-sonnet-5-5",
		"guide":     "claude-opus-4-6",
		"healthy":   "claude-sonnet-5-5",
		"alias":     "sonnet",
		"copilot":   "claude-sonnet-5.5",
	} {
		if c.Agents[name].Model != model {
			t.Errorf("agent %s model = %q, want %q", name, c.Agents[name].Model, model)
		}
	}

	if again := c.NormalizeClaudeModelIDs(); len(again) != 0 {
		t.Errorf("second pass rewrote again: %+v", again)
	}
}
