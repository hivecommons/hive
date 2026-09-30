package config

import (
	"strings"
	"testing"
)

func TestAgentMayWrite_UnconfiguredAllowsEverything(t *testing.T) {
	var nilCfg *Config
	if !nilCfg.AgentMayWrite("scanner", "open_pr") {
		t.Error("nil config must allow")
	}
	cfg := &Config{}
	for _, op := range KnownWriteOps {
		if !cfg.AgentMayWrite("scanner", op) {
			t.Errorf("empty allowlist refused %s - the default must be no behaviour change", op)
		}
	}
}

func TestAgentMayWrite_ListedLaneIsNarrowed(t *testing.T) {
	cfg := &Config{WriteSurface: WriteSurfaceConfig{Allowlist: map[string][]string{
		"scanner":  {"comment", " Create_Issue "},
		"muted":    {},
		"operator": {WriteSurfaceAllowAll},
	}}}
	cases := []struct {
		agent, op string
		want      bool
	}{
		{"scanner", "comment", true},
		{"scanner", "create_issue", true}, // case/space-insensitive entry
		{"scanner", " COMMENT ", true},    // case/space-insensitive op
		{"scanner", "open_pr", false},
		{"scanner", "merge_pr", false},
		{"muted", "comment", false}, // present but empty allows nothing
		{"operator", "merge_pr", true},
		{"reviewer", "merge_pr", true}, // no entry: unrestricted
		{"", "merge_pr", true},         // unnamed: fails open
	}
	for _, tc := range cases {
		if got := cfg.AgentMayWrite(tc.agent, tc.op); got != tc.want {
			t.Errorf("AgentMayWrite(%q, %q) = %v, want %v", tc.agent, tc.op, got, tc.want)
		}
	}
}

// A replica is the same lane run more than once: with no entry of its own it
// carries its base agent's allowlist, and one of its own wins.
func TestAgentMayWrite_ReplicaUsesBaseAllowlist(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"scanner":   {},
			"scanner-2": {ReplicaOf: "scanner"},
			"scanner-3": {ReplicaOf: "scanner"},
		},
		WriteSurface: WriteSurfaceConfig{Allowlist: map[string][]string{
			"scanner":   {"comment"},
			"scanner-3": {"open_pr"},
		}},
	}
	if cfg.AgentMayWrite("scanner-2", "open_pr") {
		t.Error("replica escaped its base agent's allowlist")
	}
	if !cfg.AgentMayWrite("scanner-2", "comment") {
		t.Error("replica was refused an operation its base allows")
	}
	if !cfg.AgentMayWrite("scanner-3", "open_pr") || cfg.AgentMayWrite("scanner-3", "comment") {
		t.Error("a replica's own allowlist entry must win over its base's")
	}
}

func TestWriteSurfaceWarnings(t *testing.T) {
	if w := WriteSurfaceWarnings(nil); w != nil {
		t.Errorf("nil config warned: %v", w)
	}
	if w := WriteSurfaceWarnings(&Config{}); w != nil {
		t.Errorf("empty allowlist warned: %v", w)
	}
	cfg := &Config{WriteSurface: WriteSurfaceConfig{Allowlist: map[string][]string{
		"scanner":  {"comment", "open-pr", "*"},
		"reviewer": {"review", "Resolve_Thread"},
	}}}
	w := WriteSurfaceWarnings(cfg)
	if len(w) != 1 {
		t.Fatalf("got %d warnings, want exactly 1 (the misspelt open-pr): %v", len(w), w)
	}
	if !strings.Contains(w[0], "scanner") || !strings.Contains(w[0], `"open-pr"`) || !strings.Contains(w[0], "open_pr") {
		t.Errorf("warning does not name the agent, the bad op and the known ops: %q", w[0])
	}
}

func TestWriteSurfaceEnforced_DefaultOff(t *testing.T) {
	var nilCfg *Config
	if nilCfg.WriteSurfaceEnforced("scanner") {
		t.Error("nil config must not enforce")
	}
	cfg := &Config{WriteSurface: WriteSurfaceConfig{Allowlist: map[string][]string{"scanner": {"comment"}}}}
	if cfg.WriteSurfaceEnforced("scanner") {
		t.Error("an allowlist entry alone must not enforce; enforcement is a separate opt-in")
	}
}

func TestWriteSurfaceEnforced_ListedLanesOnly(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"scanner":   {},
			"scanner-2": {ReplicaOf: "scanner"},
			"reviewer":  {},
		},
		WriteSurface: WriteSurfaceConfig{Enforce: []string{" Scanner "}},
	}
	cases := map[string]bool{
		"scanner":   true,
		"SCANNER":   true,
		"scanner-2": true, // replica follows its base lane
		"reviewer":  false,
		"":          false,
	}
	for agentName, want := range cases {
		if got := cfg.WriteSurfaceEnforced(agentName); got != want {
			t.Errorf("WriteSurfaceEnforced(%q) = %v, want %v", agentName, got, want)
		}
	}
}

func TestWriteSurfaceEnforced_AllLanes(t *testing.T) {
	cfg := &Config{WriteSurface: WriteSurfaceConfig{Enforce: []string{WriteSurfaceAllowAll}}}
	if !cfg.WriteSurfaceEnforced("anyone") {
		t.Error(`"*" must enforce every lane`)
	}
	if cfg.WriteSurfaceEnforced("") {
		t.Error("an unnamed agent is never enforced")
	}
}

// Replacing the allowlist from the dashboard must not drop the enforce list.
func TestSetWriteSurfaceAllowlist_KeepsEnforce(t *testing.T) {
	cfg := &Config{WriteSurface: WriteSurfaceConfig{Enforce: []string{"scanner"}}}
	cfg.SetWriteSurfaceAllowlist(map[string][]string{"scanner": {"push_branch"}})
	if !cfg.WriteSurfaceEnforced("scanner") {
		t.Error("allowlist edit dropped write_surface.enforce")
	}
}

func TestWriteSurfaceNeutralizesMentions_DefaultOff(t *testing.T) {
	var nilCfg *Config
	if nilCfg.WriteSurfaceNeutralizesMentions("scanner") {
		t.Error("nil config must not neutralize mentions")
	}
	cfg := &Config{WriteSurface: WriteSurfaceConfig{Enforce: []string{"scanner"}}}
	if cfg.WriteSurfaceNeutralizesMentions("scanner") {
		t.Error("an enforce entry alone must not neutralize mentions; it is a separate opt-in")
	}
}

func TestWriteSurfaceNeutralizesMentions_ListedLanesOnly(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"scanner":   {},
			"scanner-2": {ReplicaOf: "scanner"},
			"reviewer":  {},
		},
		WriteSurface: WriteSurfaceConfig{NeutralizeMentions: []string{" Scanner "}},
	}
	cases := map[string]bool{
		"scanner":   true,
		"SCANNER":   true,
		"scanner-2": true, // replica follows its base lane
		"reviewer":  false,
		"":          false,
	}
	for agentName, want := range cases {
		if got := cfg.WriteSurfaceNeutralizesMentions(agentName); got != want {
			t.Errorf("WriteSurfaceNeutralizesMentions(%q) = %v, want %v", agentName, got, want)
		}
	}
	if cfg.WriteSurfaceEnforced("scanner") {
		t.Error("a neutralize_mentions entry must not enforce")
	}
}

func TestWriteSurfaceNeutralizesMentions_AllLanes(t *testing.T) {
	cfg := &Config{WriteSurface: WriteSurfaceConfig{NeutralizeMentions: []string{WriteSurfaceAllowAll}}}
	if !cfg.WriteSurfaceNeutralizesMentions("anyone") {
		t.Error(`"*" must cover every lane`)
	}
	if cfg.WriteSurfaceNeutralizesMentions("") {
		t.Error("an unnamed agent is never covered")
	}
}

// Replacing the allowlist from the dashboard must not drop neutralize_mentions.
func TestSetWriteSurfaceAllowlist_KeepsNeutralizeMentions(t *testing.T) {
	cfg := &Config{WriteSurface: WriteSurfaceConfig{NeutralizeMentions: []string{"scanner"}}}
	cfg.SetWriteSurfaceAllowlist(map[string][]string{"scanner": {"comment"}})
	if !cfg.WriteSurfaceNeutralizesMentions("scanner") {
		t.Error("allowlist edit dropped write_surface.neutralize_mentions")
	}
}
