package config

// ToolApprovalConfig configures the approval desk (RFC #4000).
//
// The desk consolidates four independently-grown gates onto one decision point.
// Because a silent behavior change on upgrade day is exactly what the RFC's
// throughput contract forbids, the whole block is opt-in: with Enabled=false
// (the default) no producer consults the desk and every legacy gate stays
// authoritative. Turning it on does NOT change what a level permits — the desk
// reproduces current behavior 1:1 (see pkg/toolapprove/parity.go) — it makes
// the decision inspectable and gives operators a place to hang rules.
type ToolApprovalConfig struct {
	// Enabled turns the desk on for wired producers. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Rules are operator-authored CEL approval rules, evaluated in priority
	// order. A rule can steer a request into a lane but can NEVER approve above
	// the hive's ACMM level: the ceiling is applied after rule evaluation,
	// unconditionally. Malformed rules are rejected at config load.
	Rules []ToolApprovalRule `yaml:"rules,omitempty" json:"rules,omitempty"`
	// InboxPath overrides where pending operator approvals persist. Empty uses
	// the package default (/data/approvals/inbox.json) on the durable PVC.
	InboxPath string `yaml:"inbox_path,omitempty" json:"inbox_path,omitempty"`
}

// ToolApprovalRule is one declarative approval rule. Mirrors TriggerRule's
// shape so operators writing `triggers:` already know how to write these.
type ToolApprovalRule struct {
	// Name identifies the rule in verdicts, audit records, and the dashboard's
	// filter chips. Required and unique.
	Name string `yaml:"name" json:"name"`
	// Expr is a CEL expression over the `request` activation. Must return bool.
	// Example: request.kind == "self-merge" && request.checks_green &&
	//          request.author == "dependabot[bot]"
	Expr string `yaml:"expr" json:"expr"`
	// Action is one of auto-approve, security-scan, operator-approve.
	Action string `yaml:"action" json:"action"`
	// Priority orders competing rules; higher wins. Ties keep file order.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`
	// MinACMMLevel optionally scopes the rule to hives at or above a level.
	// This is scoping convenience, not the safety mechanism — the ACMM ceiling
	// is what actually bounds a rule's reach.
	MinACMMLevel int `yaml:"min_acmm_level,omitempty" json:"min_acmm_level,omitempty"`
}
