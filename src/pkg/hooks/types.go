package hooks

import (
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

// Named, hookable state transitions across Hive.
const (
	// Agent lifecycle transitions
	OnAgentStarted = "on_agent_started"
	OnAgentStopped = "on_agent_stopped"
	OnAgentPaused  = "on_agent_paused"
	OnAgentResumed = "on_agent_resumed"
	OnAgentFailed  = "on_agent_failed"
	OnAgentKicked  = "on_agent_kicked"

	// Governor & sweep transitions
	OnACMMChange    = "on_acmm_change"
	OnSweepMerge    = "on_sweep_merge"
	OnStallDetected = "on_stall_detected"
	OnPROpened      = "on_pr_opened"
	OnIssueOpened   = "on_issue_opened"

	// Turn & tool transitions
	OnTurnStart            = "on_turn_start"
	OnTurnComplete         = "on_turn_complete"
	OnToolApprovalRequired = "on_tool_approval_required"
	OnToolApproved         = "on_tool_approved"
	OnToolDenied           = "on_tool_denied"
)

// Event carries structured metadata for a triggered state transition.
type Event struct {
	Transition string         `json:"transition"`
	Agent      string         `json:"agent,omitempty"`
	ACMMLevel  int            `json:"acmm_level,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Trigger    string         `json:"trigger,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Timestamp  time.Time      `json:"timestamp"`
}

// Result records the execution outcome of one hook action.
type Result struct {
	RuleName string        `json:"rule_name"`
	On       string        `json:"on"`
	Action   string        `json:"action"`
	Success  bool          `json:"success"`
	Output   string        `json:"output,omitempty"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration"`
}

// AuditFields returns structured key-value pairs formatted for AuditSink.Record.
func (r Result) AuditFields(event Event) map[string]any {
	fields := map[string]any{
		"hook_name":  r.RuleName,
		"transition": r.On,
		"action":     r.Action,
		"success":    r.Success,
		"duration":   r.Duration.String(),
	}
	if event.Agent != "" {
		fields["agent"] = event.Agent
	}
	if event.ACMMLevel > 0 {
		fields["acmm_level"] = event.ACMMLevel
	}
	if event.Reason != "" {
		fields["reason"] = event.Reason
	}
	if event.Trigger != "" {
		fields["trigger"] = event.Trigger
	}
	if r.Output != "" {
		fields["output"] = r.Output
	}
	if r.Error != "" {
		fields["error"] = r.Error
	}
	return fields
}

// ActionHandler is the interface for executing a specific hook action type.
type ActionHandler interface {
	Execute(event Event, rule config.HookRule) (Result, error)
}
