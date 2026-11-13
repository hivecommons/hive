package codescanning

// The settings views (GET /api/config/code-scanning, the dashboard and the
// admin MCP) show a policy with its defaults resolved. These accessors hand
// out the values the sweep itself uses, so a view cannot drift from Judge.

// EffectiveMinLevel is the lowest level the policy files: MinLevel, or
// DefaultMinLevel when it is unset.
func (p Policy) EffectiveMinLevel() Level { return p.minLevel() }

// EffectiveMaxNewIssues is the per-pass cap on new issues: MaxNewIssues, or
// DefaultMaxNewIssues when it is unset.
func (p Policy) EffectiveMaxNewIssues() int { return p.maxNewIssues() }

// EffectiveExcludeRules is the rule-ID pattern list the policy leaves out:
// DefaultExcludeRules, minus the "!" entries, plus the policy's own patterns.
func (p Policy) EffectiveExcludeRules() []string { return p.excludeList() }
