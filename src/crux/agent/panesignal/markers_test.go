package panesignal

import (
	"reflect"
	"testing"
)

func TestHasCLIMarker(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"bare bash prompt", "scanner@host:~$ ", false},
		{"claude prompt", "❯ ", true},
		{"pi context meter", "↑37k ↓20k 5.9%/1.0M (auto)", true},
		{"bob placeholder", BobInputPlaceholderDefault, true},
		{"codex banner", "OpenAI Codex (v0.144.1)", true},
		{"agy headless ready", AgyHeadlessReadyMarker, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasCLIMarker(tt.in); got != tt.want {
				t.Fatalf("HasCLIMarker(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestShowsInputPrompt(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"no prompt", "compiling...", false},
		{"claude caret", "❯", true},
		{"goose ready", "goose is ready", true},
		{"bare angle line", "output\n>\n", true},
		{"bob placeholder", BobInputPlaceholder, true},
		{"codex caret", "› Explain this codebase", true},
		{"pi prompt", "π > ", true},
		{"agy ready", AgyHeadlessReadyMarker, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShowsInputPrompt(tt.in); got != tt.want {
				t.Fatalf("ShowsInputPrompt(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestShowsCopilotQuestionForm(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"idle prompt", "❯\nenter send · esc cancel", false},
		{"needs information", "Copilot needs information.\nWhat would you like me to do?", true},
		{"asking user", "○ Asking user Waiting for response", true},
		{"form footer", "enter accept · tab next · ctrl+d decline · esc cancel", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShowsCopilotQuestionForm(tt.in); got != tt.want {
				t.Fatalf("ShowsCopilotQuestionForm(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestShowsBusyFooter(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"idle", "❯\n? help", false},
		{"esc to interrupt", "◉ Working · esc to interrupt", true},
		{"esc stop agents", "● Waiting · esc stop agents", true},
		{"waiting for background agents", CLIWaitingForBackgroundAgentsMarker, true},
		{"escape in prose", "escaped the loop", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShowsBusyFooter(tt.in); got != tt.want {
				t.Fatalf("ShowsBusyFooter(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestShowsAgentWorking_AgyHeadlessMarkers(t *testing.T) {
	running := AgyHeadlessReadyMarker + "\n" + AgyHeadlessRunningMarker
	if !ShowsAgentWorking(running) {
		t.Fatalf("running marker after ready marker must read as working")
	}
	readyAgain := AgyHeadlessRunningMarker + "\n" + AgyHeadlessReadyMarker
	if ShowsAgentWorking(readyAgain) {
		t.Fatalf("ready marker after running marker must read as idle")
	}
}

func TestIsVisualNoise(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", true},
		{"whitespace", "   \t", true},
		{"horizontal rule", "────────────", true},
		{"box corners", "╭──────╮", true},
		{"bare agent path", "/data/agents/scanner", true},
		{"agent path with text", "/data/agents/scanner [⎇ main]", false},
		{"prose", "Opened PR #1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsVisualNoise(tt.in); got != tt.want {
				t.Fatalf("IsVisualNoise(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsCLIChrome(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", true},
		{"commands prefix", "/ commands · ? help", true},
		{"help prefix", "? help", true},
		{"files prefix", "@ files", true},
		{"issues prefix", "# issues", true},
		{"code fence", "```go", true},
		{"esc cancel", "press esc cancel to abort", true},
		{"two key hints", "ctrl+c to quit, shift+tab to toggle", true},
		{"one hint with dot separator", "ctrl+c · quit", true},
		{"one hint with arrow prefix", "← ctrl+c quit", true},
		{"enter glyph", "↵ send", true},
		{"return glyph", "⏎ submit", true},
		{"spinner thinking", "⠋ Thinking about the next step", true},
		{"spinner working tokens", "◉ Working · 1.2k tokens", true},
		{"claude status bar", "Claude Opus · 45% context left", true},
		{"copilot token bar", "Copilot · 12k tokens", true},
		{"codex spinner bar", "○ codex is thinking", true},
		{"prose", "Fixed the bug in the parser", false},
		{"prose mentioning a model", "The Copilot backend returned an error while fetching the issue list", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCLIChrome(tt.in); got != tt.want {
				t.Fatalf("IsCLIChrome(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsBufferNoise(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"claude caret", "❯", true},
		{"codex caret", "›", true},
		{"angle prompt", ">", true},
		{"bob placeholder", BobInputPlaceholder, true},
		{"bob default placeholder", BobInputPlaceholderDefault, true},
		{"codex ghost text", "› Explain this codebase", true},
		{"codex custom input", "› list the open findings", false},
		{"trust yes option", "› Yes, I trust this folder", true},
		{"trust no option", "› No (Esc)", true},
		{"banner top", "╭─╮", true},
		{"banner shade", "▔▔▔▔▔▔", true},
		{"copilot version", "Copilot v1.0.88", true},
		{"mistakes disclaimer", "Check for mistakes in generated code", true},
		{"tip", "● Tip: run /help", true},
		{"tree tail", "└ 3 files changed", true},
		{"navigate hint", "↑/↓ to navigate", true},
		{"init hint", "Run /init to create copilot-instructions.md", true},
		{"trust dialog", "Do you trust the files in this folder?", true},
		{"folder trusted", "● Folder /data/agents/scanner is now trusted", true},
		{"model unavailable", "✗ Model gpt-5 is not available", true},
		{"real output", "Opened PR #123: fix scanner", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsBufferNoise(tt.in); got != tt.want {
				t.Fatalf("IsBufferNoise(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFilterOutput(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		n    int
		want []string
	}{
		{
			name: "content after last prompt wins",
			in:   []string{"old turn", "❯", "new output"},
			n:    0,
			want: []string{"new output"},
		},
		{
			name: "only chrome after prompt keeps earlier content",
			in:   []string{"old turn", "❯", "? help · / commands"},
			n:    0,
			want: []string{"old turn"},
		},
		{
			name: "trailing prompt is dropped as noise",
			in:   []string{"old turn", "›"},
			n:    0,
			want: []string{"old turn"},
		},
		{
			name: "no prompt strips noise only",
			in:   []string{"line one", "────", "line two"},
			n:    0,
			want: []string{"line one", "line two"},
		},
		{
			name: "n keeps the tail",
			in:   []string{"a", "b", "c", "d"},
			n:    2,
			want: []string{"c", "d"},
		},
		{
			name: "empty input",
			in:   nil,
			n:    5,
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterOutput(tt.in, tt.n)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("FilterOutput(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
		})
	}
}

func TestFilterOutputDoesNotAliasInput(t *testing.T) {
	in := []string{"a", "b", "c"}
	got := FilterOutput(in, 0)
	if len(got) == 0 {
		t.Fatalf("expected lines, got none")
	}
	got[0] = "mutated"
	if in[0] != "a" {
		t.Fatalf("FilterOutput returned a slice aliasing its input")
	}
}

func TestNormalizeLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"trailing whitespace", "done  \t", "done"},
		{"spinner frames collapse", "◉ Working", "○ Working"},
		{"half spinner frames collapse", "◐ Working", "○ Working"},
		{"credits counter normalised", "AI Credits: 12.5 remaining", "AI Credits: _ remaining"},
		{"plain line unchanged", "Opened PR #1", "Opened PR #1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeLine(tt.in); got != tt.want {
				t.Fatalf("NormalizeLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDeduplicateBlocksShortInputUnchanged(t *testing.T) {
	in := []string{"a", "b", "a"}
	got := DeduplicateBlocks(in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("DeduplicateBlocks(%q) = %q, want input unchanged", in, got)
	}
}
