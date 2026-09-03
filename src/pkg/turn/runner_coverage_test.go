package turn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/toolapprove"
)

type recordingSink struct {
	events []string
}

func (r *recordingSink) Record(actor, action, target string, fields map[string]any) {
	r.events = append(r.events, fmt.Sprintf("%s/%s/%s", actor, action, target))
}

type permissiveScanner struct{}

func (permissiveScanner) Scan(ctx context.Context, req toolapprove.ToolRequest) (toolapprove.ScanResult, error) {
	return toolapprove.ScanResult{Passed: true}, nil
}

// TestRunnerOptionsWiring asserts each functional option takes effect on the
// runner's behavior: the configured tools reach the LLM, the configured
// scanner decides side-effectful calls, and approval verdicts land in the
// configured audit sink.
func TestRunnerOptionsWiring(t *testing.T) {
	var seenTools []toolapprove.ToolRequest
	llm := &toolCapturingLLM{capture: &seenTools, response: LLMResponse{
		Content:   "writing",
		ToolCalls: []ToolCall{{ID: "w1", Name: "write", Arguments: `{"file_path":"notes.txt"}`}},
	}}
	sink := &recordingSink{}
	runner := NewRunner(llm, &mockExecutor{},
		WithSecurityScanner(permissiveScanner{}),
		WithAvailableTools([]toolapprove.ToolRequest{{Tool: "write"}}),
		WithAuditSink(sink),
	)

	env := SessionEnvelope{
		SessionID: "sess-opts",
		Agent:     toolapprove.AgentIdentity{Name: "opt-agent"},
		ACMMLevel: 6,
	}
	out, turnOut, err := runner.Step(context.Background(), env, TurnInput{UserMessage: "write the notes"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(seenTools) != 1 || seenTools[0].Tool != "write" {
		t.Errorf("available tools not delivered to LLM: %+v", seenTools)
	}
	if len(turnOut.Verdicts) != 1 || !turnOut.Verdicts[0].AutoApproved() {
		t.Errorf("expected auto-approve via permissive scanner at L6, got %+v", turnOut.Verdicts)
	}
	if len(out.Operations) != 1 || out.Operations[0].Kind != OperationKindToolApproval || out.Operations[0].Status != OperationStatusApproved {
		t.Errorf("approval operation not recorded as approved: %+v", out.Operations)
	}
	if len(sink.events) != 1 || !strings.Contains(sink.events[0], "tool_approval") {
		t.Errorf("verdict not recorded in audit sink: %v", sink.events)
	}
	if out.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", out.TurnCount)
	}
}

type toolCapturingLLM struct {
	capture  *[]toolapprove.ToolRequest
	response LLMResponse
	fail     error
}

func (l *toolCapturingLLM) Generate(ctx context.Context, messages []Message, tools []toolapprove.ToolRequest) (LLMResponse, error) {
	*l.capture = append(*l.capture, tools...)
	if l.fail != nil {
		return LLMResponse{}, l.fail
	}
	return l.response, nil
}

// TestStepWithDefaultHooksAndNilLLM covers the NoopHookHandler default and the
// no-LLM error path.
func TestStepWithDefaultHooksAndNilLLM(t *testing.T) {
	// Default hooks (noop) exercised through a max-turns completion.
	runner := NewRunner(&mockLLM{}, &mockExecutor{})
	env := SessionEnvelope{SessionID: "s", MaxTurns: 1, TurnCount: 1}
	got, out, err := runner.Step(context.Background(), env, TurnInput{})
	if err != nil {
		t.Fatalf("max-turns Step: %v", err)
	}
	if !out.Done || got.Status != StatusCompleted {
		t.Errorf("expected completed at max turns, got %+v", out)
	}

	// Nil LLM is a configuration error.
	bad := &Runner{ToolExecutor: &mockExecutor{}}
	if _, _, err := bad.Step(context.Background(), SessionEnvelope{}, TurnInput{}); err == nil {
		t.Error("expected error for nil LLMClient")
	}
}

// TestStepLLMFailureMarksSessionFailed covers the inference-error path.
func TestStepLLMFailureMarksSessionFailed(t *testing.T) {
	var seen []toolapprove.ToolRequest
	llm := &toolCapturingLLM{capture: &seen, fail: errors.New("backend unavailable")}
	runner := NewRunner(llm, &mockExecutor{})
	env, out, err := runner.Step(context.Background(), SessionEnvelope{SessionID: "s-fail"}, TurnInput{UserMessage: "go"})
	if err == nil {
		t.Fatal("expected inference error to propagate")
	}
	if env.Status != StatusFailed || out.Status != StatusFailed || !out.Done {
		t.Errorf("expected failed session, got env=%s out=%+v", env.Status, out)
	}
}

// TestDefaultCompactorWindowing covers the compaction branch: head preserved,
// middle dropped, tail retained.
func TestDefaultCompactorWindowing(t *testing.T) {
	c := &DefaultCompactor{PreserveHead: 2, MaxRecent: 3}
	var msgs []Message
	for i := 0; i < 10; i++ {
		msgs = append(msgs, Message{Role: RoleUser, Content: fmt.Sprintf("m%d", i)})
	}
	out, err := c.Compact(context.Background(), msgs, 0)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5 (2 head + 3 recent)", len(out))
	}
	if out[0].Content != "m0" || out[1].Content != "m1" {
		t.Errorf("head not preserved: %v %v", out[0].Content, out[1].Content)
	}
	if out[4].Content != "m9" || out[2].Content != "m7" {
		t.Errorf("tail not retained: %+v", out)
	}

	// Overlap guard: recent window reaching into the preserved head must not duplicate.
	tight := &DefaultCompactor{PreserveHead: 4, MaxRecent: 5}
	small := msgs[:6]
	out2, err := tight.Compact(context.Background(), small, 0)
	if err != nil {
		t.Fatalf("Compact overlap: %v", err)
	}
	if len(out2) != len(small) {
		t.Errorf("under-threshold history must be unchanged, got %d", len(out2))
	}
}

// TestParseArguments covers valid, invalid, and empty raw argument payloads.
func TestParseArguments(t *testing.T) {
	if got := parseArguments(`{"a":"b"}`); got["a"] != "b" {
		t.Errorf("valid JSON not parsed: %v", got)
	}
	if got := parseArguments("not-json"); got != nil {
		t.Errorf("invalid JSON should yield nil, got %v", got)
	}
	if got := parseArguments("   "); got != nil {
		t.Errorf("blank input should yield nil, got %v", got)
	}
}

// TestNoopHookHandlerIsSafe pins the default hook implementation as callable.
func TestNoopHookHandlerIsSafe(t *testing.T) {
	n := &NoopHookHandler{}
	n.OnTurnStart(context.Background(), SessionEnvelope{})
	n.OnTurnComplete(context.Background(), SessionEnvelope{}, TurnOutput{})
	n.OnStatusChange(context.Background(), SessionEnvelope{}, StatusActive, StatusCompleted)
}
