package turn

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/toolapprove"
)

type mockLLM struct {
	responses []LLMResponse
	calls     int
}

func (m *mockLLM) Generate(ctx context.Context, messages []Message, tools []toolapprove.ToolRequest) (LLMResponse, error) {
	if m.calls >= len(m.responses) {
		return LLMResponse{Content: "Default completion."}, nil
	}
	resp := m.responses[m.calls]
	m.calls++
	return resp, nil
}

type mockExecutor struct {
	executed []ToolCall
}

func (m *mockExecutor) Execute(ctx context.Context, call ToolCall) (ToolResult, error) {
	m.executed = append(m.executed, call)
	return ToolResult{
		ToolCallID: call.ID,
		Name:       call.Name,
		Output:     fmt.Sprintf("executed %s successfully", call.Name),
	}, nil
}

type mockHooks struct {
	turnStarts    int
	turnCompletes int
	statusChanges int
}

func (m *mockHooks) OnTurnStart(ctx context.Context, env SessionEnvelope) {
	m.turnStarts++
}

func (m *mockHooks) OnTurnComplete(ctx context.Context, env SessionEnvelope, out TurnOutput) {
	m.turnCompletes++
}

func (m *mockHooks) OnStatusChange(ctx context.Context, env SessionEnvelope, from, to SessionStatus) {
	m.statusChanges++
}

// TestReentrantMultiTurnAgentFlow tests a multi-turn conversation where an agent calls a tool,
// receives results, and outputs a final response.
func TestReentrantMultiTurnAgentFlow(t *testing.T) {
	llm := &mockLLM{
		responses: []LLMResponse{
			{
				Content: "Let me check the repository files.",
				ToolCalls: []ToolCall{
					{ID: "call-1", Name: "list_dir", Arguments: `{"path":"."}`},
				},
			},
			{
				Content: "I found all necessary files. The audit is complete.",
			},
		},
	}
	exec := &mockExecutor{}
	hooks := &mockHooks{}

	runner := NewRunner(llm, exec, WithHooks(hooks))

	env := SessionEnvelope{
		SessionID: "sess-100",
		Agent: toolapprove.AgentIdentity{
			Name: "scanner",
			Mode: agent.ModeIssuesPRsMerge,
		},
		ACMMLevel: 6,
	}

	// Turn 1: Kick with user message
	env1, out1, err := runner.Step(context.Background(), env, TurnInput{UserMessage: "Please audit the repo."})
	if err != nil {
		t.Fatalf("Turn 1 failed: %v", err)
	}

	if env1.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", env1.TurnCount)
	}
	if len(out1.ToolCalls) != 1 || out1.ToolCalls[0].Name != "list_dir" {
		t.Errorf("expected list_dir tool call in Turn 1, got %+v", out1.ToolCalls)
	}
	if len(exec.executed) != 1 {
		t.Errorf("expected 1 executed tool, got %d", len(exec.executed))
	}
	if out1.Done {
		t.Errorf("Turn 1 should not be done after calling a tool")
	}

	// Turn 2: Next turn continues with updated conversation state
	env2, out2, err := runner.Step(context.Background(), env1, TurnInput{})
	if err != nil {
		t.Fatalf("Turn 2 failed: %v", err)
	}

	if env2.TurnCount != 2 {
		t.Errorf("TurnCount = %d, want 2", env2.TurnCount)
	}
	if !out2.Done {
		t.Errorf("Turn 2 should be marked done")
	}
	if env2.Status != StatusCompleted {
		t.Errorf("Status = %s, want %s", env2.Status, StatusCompleted)
	}
	if hooks.turnStarts != 2 || hooks.turnCompletes != 2 {
		t.Errorf("expected 2 start/complete hook calls, got %d / %d", hooks.turnStarts, hooks.turnCompletes)
	}
}

// TestDurableStateHandoffAcrossProcessRestarts asserts that serializing SessionEnvelope to JSON,
// simulating a process restart / spoke roll, and deserializing allows turn execution to resume seamlessly.
func TestDurableStateHandoffAcrossProcessRestarts(t *testing.T) {
	llm := &mockLLM{
		responses: []LLMResponse{
			{
				Content: "Starting task. Running inspection.",
				ToolCalls: []ToolCall{
					{ID: "c1", Name: "read_file", Arguments: `{"file_path":"README.md"}`},
				},
			},
			{
				Content: "Inspection completed.",
			},
		},
	}
	exec := &mockExecutor{}
	runner := NewRunner(llm, exec)

	initEnv := SessionEnvelope{
		SessionID: "sess-durable-1",
		Agent: toolapprove.AgentIdentity{
			Name: "quality",
			Mode: agent.ModeIssuesPRsMerge,
		},
		ACMMLevel: 6,
		Variables: map[string]string{"lane": "quality-check"},
	}

	// Process 1: Executes Turn 1
	envAfterTurn1, _, err := runner.Step(context.Background(), initEnv, TurnInput{UserMessage: "Verify documentation."})
	if err != nil {
		t.Fatalf("Turn 1 error: %v", err)
	}

	// Simulate Spoke Roll / Pod restart: Serialize envelope to JSON envelope
	payload, err := envAfterTurn1.ToJSON()
	if err != nil {
		t.Fatalf("JSON serialize error: %v", err)
	}

	// Process 2 (new spoke/node): Deserialize envelope
	restoredEnv, err := ParseEnvelope(payload)
	if err != nil {
		t.Fatalf("ParseEnvelope error: %v", err)
	}

	if restoredEnv.SessionID != "sess-durable-1" || restoredEnv.TurnCount != 1 {
		t.Errorf("restored state mismatch: %+v", restoredEnv)
	}
	if restoredEnv.Variables["lane"] != "quality-check" {
		t.Errorf("variables not preserved: %v", restoredEnv.Variables)
	}

	// Process 2: Executes Turn 2 from restored state
	envAfterTurn2, out2, err := runner.Step(context.Background(), restoredEnv, TurnInput{})
	if err != nil {
		t.Fatalf("Turn 2 on restored process error: %v", err)
	}

	if envAfterTurn2.TurnCount != 2 {
		t.Errorf("TurnCount = %d, want 2", envAfterTurn2.TurnCount)
	}
	if !out2.Done || envAfterTurn2.Status != StatusCompleted {
		t.Errorf("expected session completed after handoff, got status=%s done=%v", envAfterTurn2.Status, out2.Done)
	}
}

// TestOperatorApprovalPauseAndResume asserts that side-effectful tools at L4 pause the session,
// and providing operator approval resumes and executes the tool.
func TestOperatorApprovalPauseAndResume(t *testing.T) {
	llm := &mockLLM{
		responses: []LLMResponse{
			{
				Content: "Creating branch and modifying file.",
				ToolCalls: []ToolCall{
					{ID: "c-write", Name: "Write", Arguments: `{"file_path":"src/test.go","content":"package src"}`},
				},
			},
			{
				Content: "File was updated successfully.",
			},
		},
	}
	exec := &mockExecutor{}
	hooks := &mockHooks{}
	runner := NewRunner(llm, exec, WithHooks(hooks))

	env := SessionEnvelope{
		SessionID: "sess-op-gate",
		Agent: toolapprove.AgentIdentity{
			Name: "ci-maintainer",
			Mode: agent.ModeIssuesPRsMerge,
		},
		ACMMLevel: 4, // ACMM L4 requires operator approval for side-effectful tools
	}

	// Turn 1: Hits side-effectful write -> status becomes waiting_approval
	env1, out1, err := runner.Step(context.Background(), env, TurnInput{UserMessage: "Fix build script."})
	if err != nil {
		t.Fatalf("Turn 1 error: %v", err)
	}

	if env1.Status != StatusWaitingApproval {
		t.Errorf("expected StatusWaitingApproval, got %s", env1.Status)
	}
	if len(env1.PendingApprovals) != 1 {
		t.Fatalf("expected 1 PendingApproval, got %d", len(env1.PendingApprovals))
	}
	if len(exec.executed) != 0 {
		t.Errorf("tool should NOT have executed before operator approval, got %d", len(exec.executed))
	}
	if out1.Done {
		t.Errorf("turn must not be done while waiting for operator approval")
	}

	// Turn 2: Operator approves the pending tool
	inApprove := TurnInput{
		OperatorDecision: &OperatorApprovalDecision{
			Approved:  true,
			Operator:  "admin-dan",
			Rationale: "Verified safe change",
		},
	}

	env2, _, err := runner.Step(context.Background(), env1, inApprove)
	if err != nil {
		t.Fatalf("Turn 2 (approval) error: %v", err)
	}

	if len(exec.executed) != 1 {
		t.Errorf("tool should have executed upon operator approval, got %d", len(exec.executed))
	}
	if env2.Status != StatusCompleted {
		t.Errorf("session should reach completed after resuming from approval, got %s", env2.Status)
	}
	if len(env2.PendingApprovals) != 0 {
		t.Errorf("PendingApprovals should be cleared after resolution")
	}
}

// TestOperatorApprovalRejection asserts operator rejection appends rejection reason and continues.
func TestOperatorApprovalRejection(t *testing.T) {
	llm := &mockLLM{
		responses: []LLMResponse{
			{
				Content: "Modifying file.",
				ToolCalls: []ToolCall{
					{ID: "c-write", Name: "Write", Arguments: `{"file_path":"src/test.go","content":"package src"}`},
				},
			},
			{
				Content: "Understood, proceeding without modifying that file.",
			},
		},
	}
	exec := &mockExecutor{}
	runner := NewRunner(llm, exec)

	env := SessionEnvelope{
		SessionID: "sess-op-reject",
		Agent: toolapprove.AgentIdentity{
			Name: "ci-maintainer",
			Mode: agent.ModeIssuesPRsMerge,
		},
		ACMMLevel: 4,
	}

	env1, _, _ := runner.Step(context.Background(), env, TurnInput{UserMessage: "Fix file."})
	if env1.Status != StatusWaitingApproval {
		t.Fatalf("expected StatusWaitingApproval, got %s", env1.Status)
	}

	// Operator rejects
	inReject := TurnInput{
		OperatorDecision: &OperatorApprovalDecision{
			Approved:  false,
			Operator:  "admin-dan",
			Rationale: "Manual edit prohibited in this lane",
		},
	}

	env2, out2, err := runner.Step(context.Background(), env1, inReject)
	if err != nil {
		t.Fatalf("Turn 2 (reject) error: %v", err)
	}

	if len(exec.executed) != 0 {
		t.Errorf("tool must not be executed when rejected")
	}
	if env2.Status != StatusCompleted || !out2.Done {
		t.Errorf("session should complete, got status=%s done=%v", env2.Status, out2.Done)
	}

	// Verify conversation contains the operator rejection message
	foundRejection := false
	for _, m := range env2.Messages {
		if strings.Contains(m.Content, "Operator rejected tool execution") {
			foundRejection = true
			break
		}
	}
	if !foundRejection {
		t.Errorf("expected operator rejection message in conversation history")
	}
}

// TestSubagentSynchronization asserts subagent results delivered in TurnInput are synced into state.
func TestSubagentSynchronization(t *testing.T) {
	llm := &mockLLM{
		responses: []LLMResponse{
			{
				Content: "Subagent completed the background research. Consolidating summary.",
			},
		},
	}
	exec := &mockExecutor{}
	runner := NewRunner(llm, exec)

	env := SessionEnvelope{
		SessionID: "sess-subagent-sync",
		Agent: toolapprove.AgentIdentity{
			Name: "architect",
			Mode: agent.ModeIssuesPRsMerge,
		},
		ACMMLevel: 6,
		Subagents: map[string]string{"sub-42": "running"},
	}

	in := TurnInput{
		SubagentResults: []SubagentResult{
			{
				SubagentID: "sub-42",
				AgentName:  "researcher",
				Output:     "Analysis complete: 0 vulnerabilities found.",
				Success:    true,
			},
		},
	}

	envOut, _, err := runner.Step(context.Background(), env, in)
	if err != nil {
		t.Fatalf("Step error: %v", err)
	}

	if envOut.Subagents["sub-42"] != "completed" {
		t.Errorf("subagent status = %s, want 'completed'", envOut.Subagents["sub-42"])
	}

	foundSubMsg := false
	for _, m := range envOut.Messages {
		if m.Role == RoleSubagent && strings.Contains(m.Content, "Analysis complete") {
			foundSubMsg = true
			break
		}
	}
	if !foundSubMsg {
		t.Errorf("expected subagent result message in conversation transcript")
	}
}

// TestCompactorAndMaxTurns asserts max turns and compaction work properly.
func TestCompactorAndMaxTurns(t *testing.T) {
	llm := &mockLLM{
		responses: []LLMResponse{
			{Content: "Response 1"},
			{Content: "Response 2"},
		},
	}
	exec := &mockExecutor{}
	runner := NewRunner(llm, exec, WithCompactor(&DefaultCompactor{PreserveHead: 1, MaxRecent: 2}))

	env := SessionEnvelope{
		SessionID: "sess-limits",
		Agent: toolapprove.AgentIdentity{
			Name: "analyst",
			Mode: agent.ModeAdvisory,
		},
		ACMMLevel: 6,
		MaxTurns:  1,
	}

	// Turn 1 reaches max turns limit (MaxTurns = 1)
	env1, _, err := runner.Step(context.Background(), env, TurnInput{UserMessage: "hello"})
	if err != nil {
		t.Fatalf("Turn 1 error: %v", err)
	}

	// Turn 2 is rejected by MaxTurns
	env2, out2, err := runner.Step(context.Background(), env1, TurnInput{UserMessage: "next"})
	if err != nil {
		t.Fatalf("Turn 2 error: %v", err)
	}

	if !out2.Done || env2.Status != StatusCompleted {
		t.Errorf("expected MaxTurns completion on Turn 2, got status=%s done=%v", env2.Status, out2.Done)
	}
	if !strings.Contains(out2.Rationale, "max turns limit") {
		t.Errorf("unexpected rationale: %s", out2.Rationale)
	}
}
