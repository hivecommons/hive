package toolapprove

import (
	"strings"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
)

// Decision represents the tool approval outcome.
type Decision string

const (
	// DecisionAutoApprove indicates the tool request is approved to run immediately.
	DecisionAutoApprove Decision = "auto-approve"
	// DecisionSecurityScan indicates the tool request requires a pre-execution security scan.
	DecisionSecurityScan Decision = "security-scan"
	// DecisionOperatorApprove indicates the tool request requires human/operator approval.
	DecisionOperatorApprove Decision = "operator-approve"
	// DecisionDeny indicates the tool request is blocked by policy or security violation.
	DecisionDeny Decision = "deny"
)

// String returns the string representation of the decision.
func (d Decision) String() string {
	return string(d)
}

// ToolRequest encapsulates a requested tool invocation during an agent turn.
type ToolRequest struct {
	Tool         string         `json:"tool"`
	Arguments    map[string]any `json:"arguments,omitempty"`
	RawArguments string         `json:"raw_arguments,omitempty"`
	Target       string         `json:"target,omitempty"`
	Content      string         `json:"content,omitempty"`
	Command      string         `json:"command,omitempty"`
}

// GetCommand returns the shell/bash command string associated with this request.
func (r ToolRequest) GetCommand() string {
	if r.Command != "" {
		return r.Command
	}
	if r.Arguments != nil {
		for _, k := range []string{"command", "cmd", "script"} {
			if v, ok := r.Arguments[k]; ok {
				if s, isStr := v.(string); isStr {
					return s
				}
			}
		}
	}
	return ""
}

// GetFilePath returns the file path target associated with this request.
func (r ToolRequest) GetFilePath() string {
	if r.Target != "" {
		return r.Target
	}
	if r.Arguments != nil {
		for _, k := range []string{"file_path", "filePath", "path", "target", "filename", "file"} {
			if v, ok := r.Arguments[k]; ok {
				if s, isStr := v.(string); isStr {
					return s
				}
			}
		}
	}
	return ""
}

// GetContent returns the text content or body payload associated with this request.
func (r ToolRequest) GetContent() string {
	if r.Content != "" {
		return r.Content
	}
	if r.Arguments != nil {
		for _, k := range []string{"content", "text", "body", "new_string", "patch", "data"} {
			if v, ok := r.Arguments[k]; ok {
				if s, isStr := v.(string); isStr {
					return s
				}
			}
		}
	}
	return ""
}

// GetRepo returns any repository reference specified in the request arguments.
func (r ToolRequest) GetRepo() string {
	if r.Arguments != nil {
		for _, k := range []string{"repo", "repository", "owner/repo"} {
			if v, ok := r.Arguments[k]; ok {
				if s, isStr := v.(string); isStr {
					return s
				}
			}
		}
	}
	return ""
}

// AgentIdentity represents the caller's agent identity and authorization profile.
type AgentIdentity struct {
	Name         string              `json:"name"`
	Role         string              `json:"role,omitempty"`
	Mode         agent.AgentMode     `json:"mode,omitempty"`
	ToolsConfig  *config.ToolsConfig `json:"tools_config,omitempty"`
	AllowedRepos map[string]bool     `json:"allowed_repos,omitempty"`
}

// ScanResult holds the outcome and details of a pre-execution security scan.
type ScanResult struct {
	Passed     bool     `json:"passed"`
	Violations []string `json:"violations,omitempty"`
	Score      float64  `json:"score,omitempty"`
	Details    string   `json:"details,omitempty"`
}

// Verdict is the structured result of the tool approval decision point.
type Verdict struct {
	Decision   Decision    `json:"decision"`
	Rationale  string      `json:"rationale"`
	Tool       string      `json:"tool"`
	ACMMLevel  int         `json:"acmm_level"`
	Agent      string      `json:"agent"`
	ScanResult *ScanResult `json:"scan_result,omitempty"`

	// Kind is the request lane this verdict resolved (see the desk's Kind*
	// constants). Empty for verdicts produced by the pre-desk Decide/Resolve
	// entry points, which only ever handled agent tool calls.
	Kind string `json:"kind,omitempty"`
	// Rule names the operator rule that resolved this request, when one fired.
	// The dashboard shows this per row so an operator can see WHICH rule would
	// resolve an item, and audit records carry it as the policy provenance.
	Rule string `json:"rule,omitempty"`
	// RuleAction is what that rule asked for. It may differ from Decision when
	// the ACMM ceiling clamped the request — keeping both is what makes a
	// clamp visible in the audit trail rather than silent.
	RuleAction RuleAction `json:"rule_action,omitempty"`
	// CeilingApplied reports that the hive's ACMM ceiling reduced this verdict
	// below what base policy or a rule asked for.
	CeilingApplied bool `json:"ceiling_applied,omitempty"`
}

// AutoApproved reports whether the tool call is permitted without further gates.
func (v Verdict) AutoApproved() bool {
	return v.Decision == DecisionAutoApprove
}

// Denied reports whether the tool call is blocked.
func (v Verdict) Denied() bool {
	return v.Decision == DecisionDeny
}

// RequiresOperatorApproval reports whether human/operator approval is required.
func (v Verdict) RequiresOperatorApproval() bool {
	return v.Decision == DecisionOperatorApprove
}

// RequiresSecurityScan reports whether a pre-execution security scan is required.
func (v Verdict) RequiresSecurityScan() bool {
	return v.Decision == DecisionSecurityScan
}

// AuditFields returns structured key-value pairs formatted for AuditSink.Record.
func (v Verdict) AuditFields() map[string]any {
	fields := map[string]any{
		"decision":   string(v.Decision),
		"tool":       v.Tool,
		"acmm_level": v.ACMMLevel,
		"rationale":  v.Rationale,
	}
	if v.ScanResult != nil {
		fields["scan_passed"] = v.ScanResult.Passed
		if len(v.ScanResult.Violations) > 0 {
			fields["violations"] = strings.Join(v.ScanResult.Violations, "; ")
		}
	}
	// Policy provenance. RFC #4000 asks the verdict record to carry enough
	// metadata for the UI to render the relevant knob inline, and a clamp must
	// be visible in the audit trail rather than silent.
	if v.Kind != "" {
		fields["kind"] = v.Kind
	}
	if v.Rule != "" {
		fields["rule"] = v.Rule
		fields["rule_action"] = string(v.RuleAction)
	}
	if v.CeilingApplied {
		fields["ceiling_applied"] = true
	}
	return fields
}
