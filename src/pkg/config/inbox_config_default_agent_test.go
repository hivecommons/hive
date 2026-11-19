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

func TestInboxEffectiveSourcesAndValidate(t *testing.T) {
	if got := (InboxConfig{}).EffectiveSources(); len(got) != len(DefaultInboxSources) || got[0] != DefaultInboxSources[0] {
		t.Fatalf("default sources = %v", got)
	}
	if got := (InboxConfig{Sources: []string{"wisprflow"}}).EffectiveSources(); len(got) != 1 || got[0] != "wisprflow" {
		t.Fatalf("explicit sources = %v", got)
	}
	ok := InboxConfig{
		Spek:  InboxSpek{Repo: "o/r"},
		Dibs:  InboxDibs{URL: "https://dibs.example"},
		Rules: []InboxRule{{TitlePrefix: "RFC:", Destination: InboxDestinationSpekRFC, Repo: "o/r"}, {Tag: "idea", Destination: InboxDestinationDibs}},
	}
	if err := ValidateInbox(ok); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := map[string]InboxConfig{
		"spek repo":        {Spek: InboxSpek{Repo: "not-a-repo"}},
		"dibs url":         {Dibs: InboxDibs{URL: "dibs.example"}},
		"rule destination": {Rules: []InboxRule{{TitlePrefix: "x", Destination: "email"}}},
		"rule filter":      {Rules: []InboxRule{{Destination: InboxDestinationDismiss}}},
		"rule repo":        {Rules: []InboxRule{{TitlePrefix: "x", Destination: InboxDestinationSpekRFC, Repo: "nope"}}},
	}
	for name, c := range bad {
		if err := ValidateInbox(c); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
