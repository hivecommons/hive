package llmrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/turn"
)

// A tool call cut off by the output limit fails over rather than reaching
// the executor (live: gpt-oss task.complete truncated mid-summary).
func TestLengthTruncatedToolCallFailsOver(t *testing.T) {
	a := &fakeProvider{handler: func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"task__complete","arguments":"{\n  \"summary\": \"Root cause: pagination"}}]},"finish_reason":"length"}]}`)
	}}
	b := &fakeProvider{handler: okText("b")}
	sa, sb := a.server(t), b.server(t)
	var first Attempt
	r, _ := New([]Provider{prov("a", sa.URL), prov("b", sb.URL)}, WithEnv(allKeys()), WithAttemptHook(func(at Attempt) {
		if at.Provider == "a" {
			first = at
		}
	}))
	if got, err := r.Generate(context.Background(), msgs(), nil); err != nil || got.Content != "b" {
		t.Fatalf("Generate = %+v, %v", got, err)
	}
	if first.Outcome != OutcomeMalformed {
		t.Fatalf("outcome = %s", first.Outcome)
	}

	// Invalid arguments without a length stop still reach the executor,
	// which answers with an error the model can correct.
	// (A fresh router: the malformed answer above put "a" in cooldown.)
	a.handler = func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{oops"}}]},"finish_reason":"tool_calls"}]}`)
	}
	r, _ = New([]Provider{prov("a", sa.URL)}, WithEnv(allKeys()))
	got, err := r.Generate(context.Background(), msgs(), nil)
	if err != nil || len(got.ToolCalls) != 1 || got.ToolCalls[0].Arguments != "{oops" {
		t.Fatalf("Generate = %+v, %v", got, err)
	}
}

// Invalid arguments already in history are wrapped, so one bad turn cannot
// make every later request a 400.
func TestInvalidHistoryArgumentsAreWrapped(t *testing.T) {
	a := &fakeProvider{handler: okText("ok")}
	sa := a.server(t)
	p := prov("a", sa.URL)
	p.MaxTokens = 2048
	r, _ := New([]Provider{p}, WithEnv(allKeys()))
	history := append(msgs(),
		turn.Message{Role: turn.RoleAssistant, ToolCalls: []turn.ToolCall{
			{ID: "c1", Name: "task.complete", Arguments: "{\n  \"summary\": \"cut"},
			{ID: "c2", Name: "issue.read", Arguments: `{"number":1}`},
			{ID: "c3", Name: "task.abort", Arguments: "  "},
		}},
		turn.Message{Role: turn.RoleTool, ToolCallID: "c1", Content: "Error: arguments are not a JSON object"},
		turn.Message{Role: turn.RoleTool, ToolCallID: "c2", Content: "issue"},
		turn.Message{Role: turn.RoleTool, ToolCallID: "c3", Content: "ok"},
	)
	if _, err := r.Generate(context.Background(), history, nil); err != nil {
		t.Fatal(err)
	}
	if mt, _ := a.lastBody["max_tokens"].(float64); mt != 2048 {
		t.Fatalf("max_tokens = %v", a.lastBody["max_tokens"])
	}
	msgsOut := a.lastBody["messages"].([]any)
	calls := msgsOut[2].(map[string]any)["tool_calls"].([]any)
	args := func(i int) string {
		return calls[i].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	}
	var wrapped map[string]string
	if err := json.Unmarshal([]byte(args(0)), &wrapped); err != nil || wrapped["invalid_arguments"] != "{\n  \"summary\": \"cut" {
		t.Fatalf("wrapped = %q (%v)", args(0), err)
	}
	if args(1) != `{"number":1}` || args(2) != "{}" {
		t.Fatalf("valid/blank args changed: %q %q", args(1), args(2))
	}
	// No MaxTokens: the field is omitted.
	p.MaxTokens = 0
	r, _ = New([]Provider{p}, WithEnv(allKeys()))
	_, _ = r.Generate(context.Background(), msgs(), nil)
	if _, ok := a.lastBody["max_tokens"]; ok {
		t.Fatal("max_tokens sent without MaxTokens")
	}
}
