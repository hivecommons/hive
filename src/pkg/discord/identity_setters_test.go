package discord

import "testing"

// snapshotIdentityState captures the package-level identity/alias maps so
// tests that mutate them can restore the originals afterwards.
func snapshotIdentityState(t *testing.T) {
	t.Helper()
	discordMu.Lock()
	savedIdentities := make(map[string]AgentIdentity, len(agentIdentities))
	for k, v := range agentIdentities {
		savedIdentities[k] = v
	}
	savedAliases := make(map[string]string, len(aliases))
	for k, v := range aliases {
		savedAliases[k] = v
	}
	discordMu.Unlock()
	t.Cleanup(func() {
		discordMu.Lock()
		agentIdentities = savedIdentities
		aliases = savedAliases
		discordMu.Unlock()
	})
}

func TestSetAgentIdentities_MergesOverDefaults(t *testing.T) {
	snapshotIdentityState(t)

	SetAgentIdentities(map[string]AgentIdentity{
		"scanner": {Emoji: "🔍", Color: 0x123456},
	})

	got := getIdentity("scanner")
	if got.Emoji != "🔍" || got.Color != 0x123456 {
		t.Fatalf("scanner identity = %+v, want emoji 🔍 color 0x123456", got)
	}
	// Built-in defaults must survive the replacement.
	if gov := getIdentity("governor"); gov.Emoji != "🚦" {
		t.Fatalf("governor default lost: %+v", gov)
	}
	if pipe := getIdentity("pipeline"); pipe.Emoji != "⚙️" {
		t.Fatalf("pipeline default lost: %+v", pipe)
	}
}

func TestSetAgentIdentities_CallerCanOverrideDefaults(t *testing.T) {
	snapshotIdentityState(t)

	SetAgentIdentities(map[string]AgentIdentity{
		"governor": {Emoji: "🛑", Color: 0x1},
	})

	if got := getIdentity("governor"); got.Emoji != "🛑" || got.Color != 0x1 {
		t.Fatalf("governor override not applied: %+v", got)
	}
}

func TestSetAgentIdentities_ReplacesPreviousCustomEntries(t *testing.T) {
	snapshotIdentityState(t)

	SetAgentIdentities(map[string]AgentIdentity{"old": {Emoji: "1️⃣", Color: 1}})
	SetAgentIdentities(map[string]AgentIdentity{"new": {Emoji: "2️⃣", Color: 2}})

	// Unknown names fall back to the pipeline identity.
	pipeline := getIdentity("pipeline")
	if got := getIdentity("old"); got != pipeline {
		t.Fatalf("stale identity survived a full replacement: %+v", got)
	}
	if got := getIdentity("new"); got.Emoji != "2️⃣" {
		t.Fatalf("new identity missing: %+v", got)
	}
}

func TestSetAgentAliases_AddsAndOverrides(t *testing.T) {
	snapshotIdentityState(t)

	SetAgentAliases(map[string]string{
		"zz": "sleeper",   // brand-new alias
		"sc": "sec-check", // overrides the built-in "sc" → "scanner"
	})

	if got := resolveAlias("zz"); got != "sleeper" {
		t.Fatalf("resolveAlias(zz) = %q, want sleeper", got)
	}
	if got := resolveAlias("sc"); got != "sec-check" {
		t.Fatalf("resolveAlias(sc) = %q, want sec-check (override)", got)
	}
	// Untouched built-ins keep working.
	if got := resolveAlias("qa"); got != "quality" {
		t.Fatalf("resolveAlias(qa) = %q, want quality", got)
	}
	// Non-aliases pass through unchanged.
	if got := resolveAlias("governor"); got != "governor" {
		t.Fatalf("resolveAlias(governor) = %q, want passthrough", got)
	}
}
