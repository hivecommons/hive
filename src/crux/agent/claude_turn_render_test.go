package agent

// Renderer tests for `hive claude-turn` (claude_turn.go). The fake-claude
// tests in claude_turn_test.go drive only a Bash tool call and successful
// tool results; these pin the remaining tool-input summaries and the error
// and empty tool-result paths an operator reads in the pane.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSummarizeClaudeToolInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"command first", map[string]any{"command": "gh pr list\n  --limit 5", "description": "list"}, "gh pr list --limit 5"},
		{"file path", map[string]any{"file_path": "/repo/main.go", "limit": 20.0}, "/repo/main.go"},
		{"pattern", map[string]any{"pattern": "func  Test", "path": "src"}, "func Test"},
		{"url", map[string]any{"url": "https://example.com/a", "prompt": "read it"}, "https://example.com/a"},
		{"empty preferred key falls through", map[string]any{"command": "", "file_path": "/p"}, "/p"},
		{"non-string preferred key falls back", map[string]any{"command": 3.0}, "command=3"},
		{"fallback lists sorted params", map[string]any{"todos": 2.0, "a": "x y"}, `a="x y" todos=2`},
		{"no input", nil, ""},
	} {
		if got := summarizeClaudeToolInput(tc.input); got != tc.want {
			t.Errorf("%s: summarizeClaudeToolInput(%v) = %q, want %q", tc.name, tc.input, got, tc.want)
		}
	}
}

func TestClaudeTurnToolResultText(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"absent", "", ""},
		{"string", `"one\ntwo"`, "one\ntwo"},
		{"text blocks joined, non-text skipped", `[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":""},{"type":"text","text":"b"}]`, "a\nb"},
		{"unknown shape", `{"type":"text","text":"x"}`, ""},
	} {
		if got := claudeTurnToolResultText(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("%s: claudeTurnToolResultText(%s) = %q, want %q", tc.name, tc.raw, got, tc.want)
		}
	}
}

func TestClaudeTurnRenderer_ToolResults(t *testing.T) {
	var buf bytes.Buffer
	r := newClaudeTurnRenderer(&buf, time.Now())
	r.lastTool = "Bash"

	r.handleToolResults([]claudeTurnBlock{
		{Type: "text", Text: "not a tool result"},
		{Type: "tool_result", Content: json.RawMessage(`"  \n"`)},
		{Type: "tool_result", IsError: true, Content: json.RawMessage(`"exit status 1"`)},
	})

	out := buf.String()
	if strings.Contains(out, "not a tool result") {
		t.Errorf("non tool_result block rendered:\n%q", out)
	}
	if got := strings.Count(out, claudeHeadlessRunningMarker); got != 1 {
		t.Errorf("want exactly one emit (blank result skipped), got %d status lines:\n%q", got, out)
	}
	if !strings.Contains(out, "  error: exit status 1\n") {
		t.Errorf("an is_error tool result must render with an error: prefix:\n%q", out)
	}
	if strings.Contains(out, "· Bash") {
		t.Errorf("a tool result must clear the running tool from the status line:\n%q", out)
	}
}
