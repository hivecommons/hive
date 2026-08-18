package config

import (
	"testing"
)

// ── ToolPresets map integrity ─────────────────────────────────────────────────

func TestToolPresetsAdvisoryDeniesAllWrites(t *testing.T) {
	rules, ok := ToolPresets["advisory"]
	if !ok {
		t.Fatal("advisory preset missing from ToolPresets")
	}
	required := map[string]bool{
		"mcp__github__create_pull_request":             false,
		"mcp__github__create_pull_request_with_copilot": false,
		"mcp__github__create_issue":                    false,
		"mcp__github__update_issue":                    false,
		"mcp__github__add_issue_comment":               false,
		"mcp__github__merge_pull_request":              false,
	}
	for _, r := range rules {
		if r.Action != "deny" {
			t.Errorf("advisory rule %q has action %q, want deny", r.Pattern, r.Action)
		}
		if _, need := required[r.Pattern]; need {
			required[r.Pattern] = true
		}
	}
	for pat, found := range required {
		if !found {
			t.Errorf("advisory preset missing required deny for %q", pat)
		}
	}
}

func TestToolPresetsIssuesOnlyDeniesOnlyPRAndMerge(t *testing.T) {
	rules, ok := ToolPresets["issues-only"]
	if !ok {
		t.Fatal("issues-only preset missing from ToolPresets")
	}
	required := map[string]bool{
		"mcp__github__create_pull_request":             false,
		"mcp__github__create_pull_request_with_copilot": false,
		"mcp__github__merge_pull_request":              false,
	}
	for _, r := range rules {
		if r.Action != "deny" {
			t.Errorf("issues-only rule %q has action %q, want deny", r.Pattern, r.Action)
		}
		required[r.Pattern] = true
	}
	for pat, found := range required {
		if !found {
			t.Errorf("issues-only preset missing required deny for %q", pat)
		}
	}
	// issues-only must NOT deny issue creation
	for _, r := range rules {
		if r.Pattern == "mcp__github__create_issue" {
			t.Error("issues-only must not deny create_issue")
		}
	}
}

func TestToolPresetsIssuesPRsAndFullHaveNoDenies(t *testing.T) {
	for _, preset := range []string{"issues-prs", "full"} {
		rules, ok := ToolPresets[preset]
		if !ok {
			t.Fatalf("%s preset missing from ToolPresets", preset)
		}
		if len(rules) != 0 {
			t.Errorf("%s preset should have zero rules, got %d", preset, len(rules))
		}
	}
}

func TestPresetToModeCoversAllPresets(t *testing.T) {
	for name := range ToolPresets {
		if _, ok := PresetToMode[name]; !ok {
			t.Errorf("ToolPresets key %q has no entry in PresetToMode", name)
		}
	}
}

// ── EffectiveRules ────────────────────────────────────────────────────────────

func TestEffectiveRulesNilConfig(t *testing.T) {
	var tc *ToolsConfig
	if rules := tc.EffectiveRules(); rules != nil {
		t.Errorf("nil ToolsConfig.EffectiveRules() should return nil, got %v", rules)
	}
}

func TestEffectiveRulesPresetOnly(t *testing.T) {
	tc := &ToolsConfig{Preset: "advisory"}
	rules := tc.EffectiveRules()
	if len(rules) != len(ToolPresets["advisory"]) {
		t.Errorf("expected %d rules, got %d", len(ToolPresets["advisory"]), len(rules))
	}
}

func TestEffectiveRulesOverrideAllowUnblocksDeny(t *testing.T) {
	tc := &ToolsConfig{
		Preset: "advisory",
		Rules: []ToolRule{
			{Pattern: "mcp__github__create_issue", Action: "allow"},
		},
	}
	rules := tc.EffectiveRules()
	for _, r := range rules {
		if r.Pattern == "mcp__github__create_issue" {
			if r.Action != "allow" {
				t.Errorf("override should flip create_issue to allow, got %q", r.Action)
			}
			return
		}
	}
	t.Error("create_issue rule not found in effective rules after override")
}

func TestEffectiveRulesAddsNewRule(t *testing.T) {
	tc := &ToolsConfig{
		Preset: "full",
		Rules: []ToolRule{
			{Pattern: "custom_tool_xyz", Action: "deny"},
		},
	}
	rules := tc.EffectiveRules()
	found := false
	for _, r := range rules {
		if r.Pattern == "custom_tool_xyz" && r.Action == "deny" {
			found = true
		}
	}
	if !found {
		t.Error("custom rule not appended to effective rules")
	}
}

func TestEffectiveRulesUnknownPresetIgnored(t *testing.T) {
	tc := &ToolsConfig{
		Preset: "nonexistent",
		Rules: []ToolRule{
			{Pattern: "some_tool", Action: "deny"},
		},
	}
	rules := tc.EffectiveRules()
	if len(rules) != 1 {
		t.Errorf("unknown preset should be skipped, expected 1 rule, got %d", len(rules))
	}
}

// ── DenyPatterns ──────────────────────────────────────────────────────────────

func TestDenyPatternsAdvisory(t *testing.T) {
	tc := &ToolsConfig{Preset: "advisory"}
	denies := tc.DenyPatterns()
	if len(denies) != len(ToolPresets["advisory"]) {
		t.Errorf("expected %d deny patterns, got %d", len(ToolPresets["advisory"]), len(denies))
	}
}

func TestDenyPatternsFullIsEmpty(t *testing.T) {
	tc := &ToolsConfig{Preset: "full"}
	denies := tc.DenyPatterns()
	if len(denies) != 0 {
		t.Errorf("full preset should have 0 deny patterns, got %d", len(denies))
	}
}

func TestDenyPatternsNilConfig(t *testing.T) {
	var tc *ToolsConfig
	denies := tc.DenyPatterns()
	if len(denies) != 0 {
		t.Errorf("nil config should produce 0 deny patterns, got %d", len(denies))
	}
}

// ── EffectiveMode ─────────────────────────────────────────────────────────────

func TestEffectiveModeFromPreset(t *testing.T) {
	tests := []struct {
		preset string
		want   string
	}{
		{"advisory", "ADVISORY"},
		{"issues-only", "ISSUES_ONLY"},
		{"issues-prs", "ISSUES_AND_PRS"},
		{"full", "ISSUES_PRS_MERGE"},
	}
	for _, tt := range tests {
		tc := &ToolsConfig{Preset: tt.preset}
		got := tc.EffectiveMode()
		if got != tt.want {
			t.Errorf("EffectiveMode(%q) = %q, want %q", tt.preset, got, tt.want)
		}
	}
}

func TestEffectiveModeNilConfig(t *testing.T) {
	var tc *ToolsConfig
	if got := tc.EffectiveMode(); got != "" {
		t.Errorf("nil config EffectiveMode() = %q, want empty", got)
	}
}

func TestEffectiveModeInferredFromDenyRules(t *testing.T) {
	// No preset, but deny PR creation -> ISSUES_ONLY
	tc := &ToolsConfig{
		Rules: []ToolRule{
			{Pattern: "mcp__github__create_pull_request", Action: "deny"},
		},
	}
	if got := tc.EffectiveMode(); got != "ISSUES_ONLY" {
		t.Errorf("deny PR = %q, want ISSUES_ONLY", got)
	}

	// Deny issue creation -> ADVISORY
	tc2 := &ToolsConfig{
		Rules: []ToolRule{
			{Pattern: "mcp__github__create_issue", Action: "deny"},
		},
	}
	if got := tc2.EffectiveMode(); got != "ADVISORY" {
		t.Errorf("deny issue = %q, want ADVISORY", got)
	}

	// No denies at all -> ISSUES_PRS_MERGE
	tc3 := &ToolsConfig{
		Rules: []ToolRule{
			{Pattern: "some_tool", Action: "allow"},
		},
	}
	if got := tc3.EffectiveMode(); got != "ISSUES_PRS_MERGE" {
		t.Errorf("no denies = %q, want ISSUES_PRS_MERGE", got)
	}
}
