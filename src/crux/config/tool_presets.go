package config

// ToolPresets maps preset names to their default deny rules.
// These match the existing Mode-based hardcoded behavior in agent/manager.go.
var ToolPresets = map[string][]ToolRule{
	// The deny sets below cover the FULL GitHub MCP write surface so an agent
	// cannot author issues/PRs/comments as the login USER via the MCP; the same
	// write-tool set is denied at Copilot launch (copilotGitHubWriteDenyFlags in
	// agent/manager.go). All GitHub writes must route through the App-gated gh
	// wrapper / hive-open-pr so they are authored by the App bot.
	"advisory": {
		{Pattern: "mcp__github__create_pull_request", Action: "deny"},
		{Pattern: "mcp__github__create_pull_request_with_copilot", Action: "deny"},
		{Pattern: "mcp__github__create_issue", Action: "deny"},
		{Pattern: "mcp__github__update_issue", Action: "deny"},
		{Pattern: "mcp__github__add_issue_comment", Action: "deny"},
		{Pattern: "mcp__github__merge_pull_request", Action: "deny"},
	},
	"issues-only": {
		{Pattern: "mcp__github__create_pull_request", Action: "deny"},
		{Pattern: "mcp__github__create_pull_request_with_copilot", Action: "deny"},
		{Pattern: "mcp__github__merge_pull_request", Action: "deny"},
	},
	"issues-prs": {},
	"full":       {},
}

// PresetToMode maps a tool preset name to the equivalent AgentMode string.
var PresetToMode = map[string]string{
	"advisory":    "ADVISORY",
	"issues-only": "ISSUES_ONLY",
	"issues-prs":  "ISSUES_AND_PRS",
	"full":        "ISSUES_PRS_MERGE",
}

// EffectiveRules expands the preset and applies per-rule overrides.
// An explicit "allow" rule overrides a preset "deny" for the same pattern.
func (t *ToolsConfig) EffectiveRules() []ToolRule {
	if t == nil {
		return nil
	}

	rules := make([]ToolRule, 0)
	if t.Preset != "" {
		if presetRules, ok := ToolPresets[t.Preset]; ok {
			rules = append(rules, presetRules...)
		}
	}

	for _, override := range t.Rules {
		found := false
		for i, existing := range rules {
			if WildcardMatch(existing.Pattern, override.Pattern) || existing.Pattern == override.Pattern {
				rules[i] = override
				found = true
				break
			}
		}
		if !found {
			rules = append(rules, override)
		}
	}

	return rules
}

// DenyPatterns returns just the denied tool patterns from the effective rules.
func (t *ToolsConfig) DenyPatterns() []string {
	var patterns []string
	for _, r := range t.EffectiveRules() {
		if r.Action == "deny" {
			patterns = append(patterns, r.Pattern)
		}
	}
	return patterns
}

// EffectiveMode derives the most appropriate AgentMode string from the tools config.
// Used for the proxy's defense-in-depth layer which still reads Mode.
func (t *ToolsConfig) EffectiveMode() string {
	if t == nil {
		return ""
	}
	if t.Preset != "" {
		if mode, ok := PresetToMode[t.Preset]; ok {
			return mode
		}
	}
	denies := t.DenyPatterns()
	if len(denies) == 0 {
		return "ISSUES_PRS_MERGE"
	}
	hasPRDeny := false
	hasIssueDeny := false
	for _, p := range denies {
		if WildcardMatch("mcp__github__create_pull_request", p) {
			hasPRDeny = true
		}
		if WildcardMatch("mcp__github__create_issue", p) {
			hasIssueDeny = true
		}
	}
	if hasIssueDeny {
		return "ADVISORY"
	}
	if hasPRDeny {
		return "ISSUES_ONLY"
	}
	return "ISSUES_AND_PRS"
}
