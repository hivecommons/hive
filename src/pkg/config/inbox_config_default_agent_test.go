package config

import "testing"

func TestInboxSpekAgentDefaultsToFirstEnabledAgent(t *testing.T) {
	c := &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{
		"zed":    {Enabled: true},
		"alpha":  {Enabled: true, Paused: true},
		"bravo":  {Enabled: true},
		"charly": {Enabled: false},
	}}}
	if got := c.InboxSpekAgent(); got != "bravo" {
		t.Fatalf("default agent = %q, want bravo (first enabled, unpaused)", got)
	}
	c.Inbox.Spek.Agent = " custom "
	if got := c.InboxSpekAgent(); got != "custom" {
		t.Fatalf("explicit agent = %q, want custom", got)
	}
	if got := (&Config{}).InboxSpekAgent(); got != "" {
		t.Fatalf("no agents should give empty, got %q", got)
	}
}
