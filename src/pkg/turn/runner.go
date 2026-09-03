package turn

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/toolapprove"
)

// Runner manages the execution of re-entrant turns over a SessionEnvelope.
type Runner struct {
	LLM             LLMClient
	ToolExecutor    ToolExecutor
	SecurityScanner toolapprove.SecurityScanner
	Hooks           HookHandler
	Compactor       Compactor
	AvailableTools  []toolapprove.ToolRequest
	AuditSink       agent.AuditSink
}

// Option configures a Runner instance.
type Option func(*Runner)

// WithSecurityScanner sets a custom SecurityScanner.
func WithSecurityScanner(s toolapprove.SecurityScanner) Option {
	return func(r *Runner) { r.SecurityScanner = s }
}

// WithHooks sets a custom HookHandler.
func WithHooks(h HookHandler) Option {
	return func(r *Runner) { r.Hooks = h }
}

// WithCompactor sets a custom Compactor.
func WithCompactor(c Compactor) Option {
	return func(r *Runner) { r.Compactor = c }
}

// WithAvailableTools configures the tool definitions supplied to the LLM.
func WithAvailableTools(tools []toolapprove.ToolRequest) Option {
	return func(r *Runner) { r.AvailableTools = tools }
}

// WithAuditSink sets the audit sink for recording tool approval verdicts.
func WithAuditSink(sink agent.AuditSink) Option {
	return func(r *Runner) { r.AuditSink = sink }
}

// NewRunner creates a new Runner with the required LLM and ToolExecutor implementations.
func NewRunner(llm LLMClient, exec ToolExecutor, opts ...Option) *Runner {
	r := &Runner{
		LLM:             llm,
		ToolExecutor:    exec,
		SecurityScanner: toolapprove.NewDefaultSecurityScanner(),
		Hooks:           &NoopHookHandler{},
		Compactor:       &DefaultCompactor{PreserveHead: 2, MaxRecent: 20},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Step executes one re-entrant agent turn:
// (SessionEnvelope, TurnInput) -> (new SessionEnvelope, TurnOutput, error).
func (r *Runner) Step(ctx context.Context, env SessionEnvelope, in TurnInput) (SessionEnvelope, TurnOutput, error) {
	outEnv := env.Clone()
	if outEnv.CreatedAt.IsZero() {
		outEnv.CreatedAt = time.Now().UTC()
	}
	if outEnv.Variables == nil {
		outEnv.Variables = make(map[string]string)
	}
	if outEnv.Subagents == nil {
		outEnv.Subagents = make(map[string]string)
	}

	var output TurnOutput
	output.Status = StatusActive

	// 1. Assimilate Turn Input (Operator decisions, User messages, Subagent sync)
	if in.OperatorDecision != nil && len(outEnv.PendingApprovals) > 0 {
		for _, pending := range outEnv.PendingApprovals {
			if in.OperatorDecision.Approved {
				if r.ToolExecutor != nil {
					res, err := r.ToolExecutor.Execute(ctx, pending.ToolCall)
					errStr := ""
					if err != nil {
						errStr = err.Error()
					}
					msg := outEnv.AddToolResultMessage(pending.ToolCall.ID, pending.ToolCall.Name, res.Output, errStr)
					output.NewMessages = append(output.NewMessages, msg)
					output.ToolResults = append(output.ToolResults, res)
				}
			} else {
				rejectionMsg := fmt.Sprintf("Operator rejected tool execution: %s", in.OperatorDecision.Rationale)
				msg := outEnv.AddToolResultMessage(pending.ToolCall.ID, pending.ToolCall.Name, "", rejectionMsg)
				output.NewMessages = append(output.NewMessages, msg)
			}
		}
		outEnv.PendingApprovals = nil
		outEnv.Status = StatusActive
	}

	if in.UserMessage != "" {
		msg := outEnv.AddMessage(RoleUser, in.UserMessage)
		output.NewMessages = append(output.NewMessages, msg)
		outEnv.Status = StatusActive
	}

	if len(in.SubagentResults) > 0 {
		for _, sub := range in.SubagentResults {
			if sub.Success {
				outEnv.Subagents[sub.SubagentID] = "completed"
			} else {
				outEnv.Subagents[sub.SubagentID] = "failed"
			}
			content := fmt.Sprintf("[subagent:%s (%s)] %s", sub.AgentName, sub.SubagentID, sub.Output)
			msg := outEnv.AddMessage(RoleSubagent, content)
			output.NewMessages = append(output.NewMessages, msg)
		}
		outEnv.Status = StatusActive
	}

	// 2. Pre-turn hook
	if r.Hooks != nil {
		r.Hooks.OnTurnStart(ctx, outEnv)
	}

	// 3. Max-turns check
	if outEnv.MaxTurns > 0 && outEnv.TurnCount >= outEnv.MaxTurns {
		prev := outEnv.Status
		outEnv.Status = StatusCompleted
		if r.Hooks != nil && prev != StatusCompleted {
			r.Hooks.OnStatusChange(ctx, outEnv, prev, StatusCompleted)
		}
		output.Status = StatusCompleted
		output.Done = true
		output.Rationale = fmt.Sprintf("max turns limit (%d) reached", outEnv.MaxTurns)
		return outEnv, output, nil
	}

	// 4. Context Compaction
	if r.Compactor != nil {
		compacted, err := r.Compactor.Compact(ctx, outEnv.Messages, 0)
		if err == nil {
			outEnv.Messages = compacted
		}
	}

	// 5. Model Inference Call
	if r.LLM == nil {
		return outEnv, output, fmt.Errorf("no LLMClient configured")
	}

	outEnv.TurnCount++
	llmResp, err := r.LLM.Generate(ctx, outEnv.Messages, r.AvailableTools)
	if err != nil {
		outEnv.Status = StatusFailed
		output.Status = StatusFailed
		output.Done = true
		output.Rationale = fmt.Sprintf("LLM inference error: %v", err)
		return outEnv, output, err
	}

	// Append Assistant response message
	asstMsg := outEnv.AddToolCallMessage(llmResp.Content, llmResp.ToolCalls)
	output.NewMessages = append(output.NewMessages, asstMsg)
	output.ToolCalls = llmResp.ToolCalls

	// 6. Process Tool Calls through Tool Approval and Execution
	if len(llmResp.ToolCalls) > 0 {
		for _, call := range llmResp.ToolCalls {
			req := toolapprove.ToolRequest{
				Tool:         call.Name,
				RawArguments: call.Arguments,
				Arguments:    parseArguments(call.Arguments),
			}

			// Evaluate ACMM-gated tool approval
			verdict := toolapprove.EvaluateAndAudit(
				ctx,
				req,
				outEnv.ACMMLevel,
				outEnv.Agent,
				r.SecurityScanner,
				r.AuditSink,
				outEnv.Agent.Name,
			)
			output.Verdicts = append(output.Verdicts, verdict)

			switch {
			case verdict.RequiresOperatorApproval():
				outEnv.PendingApprovals = append(outEnv.PendingApprovals, PendingApproval{
					ToolCall: call,
					Verdict:  verdict,
				})
				prev := outEnv.Status
				outEnv.Status = StatusWaitingApproval
				if r.Hooks != nil && prev != StatusWaitingApproval {
					r.Hooks.OnStatusChange(ctx, outEnv, prev, StatusWaitingApproval)
				}

			case verdict.Denied():
				errDetail := fmt.Sprintf("Tool denied: %s", verdict.Rationale)
				msg := outEnv.AddToolResultMessage(call.ID, call.Name, "", errDetail)
				output.NewMessages = append(output.NewMessages, msg)
				output.ToolResults = append(output.ToolResults, ToolResult{
					ToolCallID: call.ID,
					Name:       call.Name,
					Error:      errDetail,
				})

			case verdict.AutoApproved():
				if toolapprove.IsSubagent(call.Name) {
					outEnv.Subagents[call.ID] = "running"
				}

				if r.ToolExecutor != nil {
					res, execErr := r.ToolExecutor.Execute(ctx, call)
					errStr := ""
					if execErr != nil {
						errStr = execErr.Error()
					}
					msg := outEnv.AddToolResultMessage(call.ID, call.Name, res.Output, errStr)
					output.NewMessages = append(output.NewMessages, msg)
					output.ToolResults = append(output.ToolResults, res)
				}
			}
		}
	} else {
		// Model completed turn without additional tool calls
		if outEnv.Status == StatusActive {
			outEnv.Status = StatusCompleted
			output.Done = true
		}
	}

	if len(outEnv.PendingApprovals) > 0 {
		outEnv.Status = StatusWaitingApproval
		output.Done = false
	}

	output.Status = outEnv.Status

	// 7. Post-turn hook
	if r.Hooks != nil {
		r.Hooks.OnTurnComplete(ctx, outEnv, output)
	}

	return outEnv, output, nil
}

func parseArguments(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err == nil {
		return args
	}
	return nil
}
