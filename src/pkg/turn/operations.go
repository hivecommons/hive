package turn

import (
	"context"
	"errors"

	"github.com/hivecommons/hive/crux/toolapprove"
)

// LLMResponse is the output returned from an LLM model invocation.
type LLMResponse struct {
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	StopReason string     `json:"stop_reason,omitempty"`
}

// ErrInferenceCapacity is matched (errors.Is) by an LLMClient error meaning
// "no inference capacity right now" -- every backend is rate limited, cooling
// down, or out of its daily budget -- as opposed to a failed request. The
// Runner parks the session as StatusPaused instead of failing it, so a
// stateless coordinator can resume the same envelope on a later pass.
var ErrInferenceCapacity = errors.New("turn: no inference capacity available")

// LLMClient abstracts the language model completion backend.
type LLMClient interface {
	Generate(ctx context.Context, messages []Message, tools []toolapprove.ToolRequest) (LLMResponse, error)
}

// ToolExecutor executes an approved tool call in the target environment.
type ToolExecutor interface {
	Execute(ctx context.Context, call ToolCall) (ToolResult, error)
}

// HookHandler defines event callbacks for state transitions and lifecycle milestones.
type HookHandler interface {
	OnTurnStart(ctx context.Context, env SessionEnvelope)
	OnTurnComplete(ctx context.Context, env SessionEnvelope, out TurnOutput)
	OnStatusChange(ctx context.Context, env SessionEnvelope, from, to SessionStatus)
}

// NoopHookHandler provides an empty default implementation of HookHandler.
type NoopHookHandler struct{}

func (n *NoopHookHandler) OnTurnStart(ctx context.Context, env SessionEnvelope)                    {}
func (n *NoopHookHandler) OnTurnComplete(ctx context.Context, env SessionEnvelope, out TurnOutput) {}
func (n *NoopHookHandler) OnStatusChange(ctx context.Context, env SessionEnvelope, from, to SessionStatus) {
}

// Compactor manages conversation context compaction and windowing to fit model limits.
type Compactor interface {
	Compact(ctx context.Context, messages []Message, maxTokens int) ([]Message, error)
}

// DefaultCompactor provides a sliding-window message retention strategy.
type DefaultCompactor struct {
	PreserveHead int // Number of initial system/instruction messages to retain
	MaxRecent    int // Number of trailing messages to retain
}

// Compact shrinks the message history by retaining head system messages and trailing messages.
func (c *DefaultCompactor) Compact(ctx context.Context, messages []Message, maxTokens int) ([]Message, error) {
	if c.MaxRecent <= 0 || len(messages) <= c.PreserveHead+c.MaxRecent {
		return messages, nil
	}

	out := make([]Message, 0, c.PreserveHead+c.MaxRecent)
	if c.PreserveHead > 0 && len(messages) >= c.PreserveHead {
		out = append(out, messages[:c.PreserveHead]...)
	}

	recentStart := len(messages) - c.MaxRecent
	if recentStart < c.PreserveHead {
		recentStart = c.PreserveHead
	}
	out = append(out, messages[recentStart:]...)
	return out, nil
}
