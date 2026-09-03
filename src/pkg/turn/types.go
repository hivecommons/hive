package turn

import (
	"time"

	"github.com/hivecommons/hive/pkg/toolapprove"
)

// Role represents the sender role in a conversation message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	RoleSubagent  Role = "subagent"
)

// Message is an immutable entry in the conversation history.
type Message struct {
	Role       Role              `json:"role"`
	Content    string            `json:"content"`
	Name       string            `json:"name,omitempty"`
	ToolCalls  []ToolCall        `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	Timestamp  time.Time         `json:"timestamp"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// ToolCall represents a requested tool invocation from the model.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolResult represents the output or error resulting from a tool execution.
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Output     string `json:"output,omitempty"`
	Error      string `json:"error,omitempty"`
}

// SessionStatus tracks the lifecycle status of the conversation session.
type SessionStatus string

const (
	StatusActive          SessionStatus = "active"
	StatusWaitingApproval SessionStatus = "waiting_approval"
	StatusWaitingSubagent SessionStatus = "waiting_subagent"
	StatusCompleted       SessionStatus = "completed"
	StatusFailed          SessionStatus = "failed"
	StatusPaused          SessionStatus = "paused"
)

// SessionEnvelope is the self-contained, serializable state of an agent session.
// Because all execution state lives in the envelope, an agent session can be persisted
// to disk, checkpointed, or handed off across processes / spokes between turns.
type SessionEnvelope struct {
	SessionID        string                    `json:"session_id"`
	Agent            toolapprove.AgentIdentity `json:"agent"`
	ACMMLevel        int                       `json:"acmm_level"`
	TurnCount        int                       `json:"turn_count"`
	MaxTurns         int                       `json:"max_turns"`
	Status           SessionStatus             `json:"status"`
	Messages         []Message                 `json:"messages"`
	WorkingRepo      string                    `json:"working_repo,omitempty"`
	WorkingBranch    string                    `json:"working_branch,omitempty"`
	BeadID           string                    `json:"bead_id,omitempty"`
	Owner            string                    `json:"owner,omitempty"`
	Epoch            uint64                    `json:"epoch,omitempty"`
	LeaseExpiry      time.Time                 `json:"lease_expiry,omitempty"`
	Variables        map[string]string         `json:"variables,omitempty"`
	Subagents        map[string]string         `json:"subagents,omitempty"` // subagent sessionID -> status
	PendingApprovals []PendingApproval         `json:"pending_approvals,omitempty"`
	// Journal records every side-effectful operation attempted in this session
	// with its idempotency key and outcome (RFC #4002 stage 2). It travels
	// inside the envelope precisely so that re-entry — in a fresh process,
	// after a crash, or on another spoke — can determine "already done"
	// without re-performing the effect. See journal.go.
	Journal   Journal   `json:"journal,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PendingApproval holds state for a tool call paused awaiting operator approval.
type PendingApproval struct {
	ToolCall ToolCall            `json:"tool_call"`
	Verdict  toolapprove.Verdict `json:"verdict"`
}

// SubagentResult represents the output of a background subagent task.
type SubagentResult struct {
	SubagentID string `json:"subagent_id"`
	AgentName  string `json:"agent_name"`
	Output     string `json:"output"`
	Success    bool   `json:"success"`
}

// OperatorApprovalDecision represents an operator's manual approval or rejection.
type OperatorApprovalDecision struct {
	Approved  bool   `json:"approved"`
	Rationale string `json:"rationale,omitempty"`
	Operator  string `json:"operator,omitempty"`
}

// TurnInput is the input delivered to execute one turn.
type TurnInput struct {
	UserMessage      string                    `json:"user_message,omitempty"`
	SubagentResults  []SubagentResult          `json:"subagent_results,omitempty"`
	OperatorDecision *OperatorApprovalDecision `json:"operator_decision,omitempty"`
	Trigger          string                    `json:"trigger,omitempty"`
}

// TurnOutput is the structured output returned after executing one turn.
type TurnOutput struct {
	NewMessages []Message             `json:"new_messages"`
	ToolCalls   []ToolCall            `json:"tool_calls,omitempty"`
	ToolResults []ToolResult          `json:"tool_results,omitempty"`
	Verdicts    []toolapprove.Verdict `json:"verdicts,omitempty"`
	Status      SessionStatus         `json:"status"`
	Done        bool                  `json:"done"`
	Rationale   string                `json:"rationale,omitempty"`
}
