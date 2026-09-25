package agent

import (
	"strings"
	"testing"
)

func TestSanitizePaneTextStripsTUIChromeSamples(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "copilot footer only",
			in:   "← open sidebar · / commands · ? help · tab next tab",
			want: "",
		},
		{
			name: "claude working footer",
			in:   "◉ Working · esc to interrupt · ctrl+c quit\n```",
			want: "",
		},
		{
			name: "codex prompt placeholder",
			in:   "› Improve documentation in @filename\n› Explain this codebase",
			want: "",
		},
		{
			name: "box chrome around summary",
			in:   "╭────────────╮\nOpened PR #123: fix scanner\n╰────────────╯\n? help · / commands",
			want: "Opened PR #123: fix scanner",
		},
		{
			name: "keeps actionable lines",
			in:   "Reviewed issue #77\nNo changes needed\n← open sidebar · / commands · ? help · tab next tab",
			want: "Reviewed issue #77\nNo changes needed",
		},
		{
			name: "keeps prose mentioning shortcut",
			in:   "Fixed Ctrl+C handling in the terminal",
			want: "Fixed Ctrl+C handling in the terminal",
		},
		{
			name: "keeps prose mentioning slash commands",
			in:   "Updated / commands help text",
			want: "Updated / commands help text",
		},
		{
			name: "blank fenced chrome",
			in:   "```\n← open sidebar · / commands · ? help · tab next tab\n```",
			want: "",
		},
		{
			name: "bob placeholder and footer",
			in:   "Type your message or @path/to/file\nctrl+c to exit · enter to send",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizePaneText(tt.in, 20); got != tt.want {
				t.Fatalf("SanitizePaneText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizePaneTextDoesNotReturnBlankFences(t *testing.T) {
	got := SanitizePaneText("```\n\n```", 20)
	if strings.Contains(got, "```") || strings.TrimSpace(got) != "" {
		t.Fatalf("blank fences survived: %q", got)
	}
}
