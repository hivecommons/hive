package agent

import (
	"strings"
	"testing"
)

// copilotFailurePane is a verbatim capture from the hosted console spoke's
// scanner agent after an intermittent TLS failure killed its turn. Both bugs
// this file guards are visible in it at once: the error carries no "API Error:"
// chrome, and "❯" is not the last non-empty line because a hint footer is drawn
// underneath the input box.
const copilotFailurePane = ` ● Execution failed: Error: Failed to get response from the AI model; retried 5 times (total retry wait time: 6.00 seconds) Last error: Failed native model HTTP request: error decoding response body: request or response body error: error reading a body from connection: cannot decrypt peer's message

 /data/agents/scanner [⎇ deps-lucide*+%]                                  Session: 67.7 AIC used
────────────────────────────────────────────────────────────────────────────────────────────────
❯
────────────────────────────────────────────────────────────────────────────────────────────────
 / commands · ? help · tab next tab                                               Claude Fable 5`

// TestPaneShowsTransientAPIError_CopilotShape is the detection half of the
// fix. The Copilot CLI never prints "API Error:", so gating on that string
// alone made this watchdog blind to every copilot-backend agent: the CLI
// exhausted its own retries, returned to idle, and then sat there until the
// next cadence kick instead of being nudged back to work.
func TestPaneShowsTransientAPIError_CopilotShape(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "copilot model failure with transport cause",
			line: " ● Execution failed: Error: Failed to get response from the AI model; retried 5 times Last error: Failed native model HTTP request: error decoding response body: cannot decrypt peer's message",
			want: true,
		},
		{
			name: "copilot model failure, body read aborted",
			line: "Failed to get response from the AI model; Last error: error reading a body from connection: broken pipe",
			want: true,
		},
		{
			name: "copilot model failure with no transport cause is not assumed retryable",
			line: "Execution failed: Error: Failed to get response from the AI model; the model refused the request",
			want: false,
		},
		{
			name: "claude chrome still detected (regression)",
			line: "API Error: connection error",
			want: true,
		},
		{
			name: "claude 503 still detected (regression)",
			line: "API Error: 503 Service Unavailable",
			want: true,
		},
		{
			name: "unrelated output is not an error",
			line: "Running tests for the AI model integration",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneShowsTransientAPIError([]string{tc.line}); got != tc.want {
				t.Errorf("paneShowsTransientAPIError(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestPaneShowsEmptyInputPrompt_CopilotFooter is the readiness half. Even once
// the error is detected, nudgeIfTransientAPIError refuses to act unless the
// pane reads as an idle empty prompt. The Copilot CLI draws a rule and a hint
// footer BELOW "❯", so the original "last non-empty line must be ❯" rule was
// never satisfiable there and the nudge could not fire.
func TestPaneShowsEmptyInputPrompt_CopilotFooter(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want bool
	}{
		{
			name: "real copilot idle pane with rule and hint footer",
			pane: copilotFailurePane,
			want: true,
		},
		{
			name: "copilot alternate footer",
			pane: "❯\n────────────\n @ files · # issues                    Claude Fable 5",
			want: true,
		},
		{
			name: "claude shape ending at the prompt (regression)",
			pane: "some output\n❯",
			want: true,
		},
		{
			name: "prompt with typed text is not an idle prompt",
			pane: "❯ go fix the build\n────────────\n / commands · ? help",
			want: false,
		},
		{
			name: "actively working pane is not an idle prompt",
			pane: "❯\n────────────\n ◉ Working · 19.0 KiB esc interrupt      Claude Fable 5",
			want: false,
		},
		{
			name: "no prompt at all",
			pane: "just some output\nand more output",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneShowsEmptyInputPrompt(tc.pane); got != tc.want {
				t.Errorf("paneShowsEmptyInputPrompt() = %v, want %v\npane:\n%s", got, tc.want, tc.pane)
			}
		})
	}
}

// TestCopilotFailurePane_EndToEndContract ties both halves to the decision the
// watchdog actually makes. Either bug alone is enough to block recovery, so
// asserting them together is what proves a copilot agent stalled by a transient
// model failure can now be nudged rather than left idle for a whole cadence.
func TestCopilotFailurePane_EndToEndContract(t *testing.T) {
	tail := paneTail(copilotFailurePane, transientAPIErrorTailLines)
	if tail == "" {
		t.Fatal("paneTail returned empty for the captured failure pane")
	}

	if paneShowsActiveWork(tail) {
		t.Error("captured failure pane reads as actively working; the nudge would be skipped")
	}
	if !paneShowsEmptyInputPrompt(tail) {
		t.Error("captured failure pane does not read as an idle prompt; the nudge would be skipped")
	}
	if !paneShowsTransientAPIError(strings.Split(tail, "\n")) {
		t.Error("captured failure pane is not recognized as a transient API error; no nudge would fire")
	}
}
