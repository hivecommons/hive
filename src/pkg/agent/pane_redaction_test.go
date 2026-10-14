package agent

import (
	"strings"
	"testing"
)

func TestCompactPaneTailBranches(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		maxLines int
		want     string
	}{
		{name: "zero max lines", output: "a\nb", maxLines: 0, want: ""},
		{name: "negative max lines", output: "a\nb", maxLines: -3, want: ""},
		{name: "empty output", output: "", maxLines: 4, want: ""},
		{name: "whitespace only", output: " \n\t\n   \n", maxLines: 4, want: ""},
		{name: "fewer lines than max", output: "one\ntwo\n", maxLines: 5, want: "one | two"},
		{name: "exactly max lines", output: "one\ntwo\nthree", maxLines: 3, want: "one | two | three"},
		{name: "truncates to last lines", output: "one\ntwo\nthree\nfour", maxLines: 2, want: "three | four"},
		{name: "trims surrounding space", output: "  one  \n\ttwo\t", maxLines: 2, want: "one | two"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compactPaneTail(tt.output, tt.maxLines); got != tt.want {
				t.Fatalf("compactPaneTail(%q, %d) = %q, want %q", tt.output, tt.maxLines, got, tt.want)
			}
		})
	}
}

func TestCompactPaneTailRedactsPerLine(t *testing.T) {
	tests := []struct {
		name   string
		pane   string
		secret string
		keep   string
	}{
		{name: "github token", pane: "before\ntoken ghp_abcdefghijklmnop1234\nafter", secret: "ghp_abcdefghijklmnop1234", keep: "after"},
		{name: "sk api key", pane: "before\nkey sk-ant-abcdef123456\nafter", secret: "sk-ant-abcdef123456", keep: "before"},
		{name: "device code", pane: "before\nfirst copy your one-time code: WXYZ-9876\nafter", secret: "WXYZ-9876", keep: "after"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compactPaneTail(tt.pane, 10)
			if strings.Contains(got, tt.secret) {
				t.Fatalf("compactPaneTail leaked %q: %q", tt.secret, got)
			}
			if !strings.Contains(got, tt.keep) {
				t.Fatalf("compactPaneTail dropped %q: %q", tt.keep, got)
			}
		})
	}
}

func TestRedactPaneTokensWrapper(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		secret string
	}{
		{name: "github token", in: "token ghp_abcdefghijklmnop1234 end", secret: "ghp_abcdefghijklmnop1234"},
		{name: "sk api key", in: "key sk-ant-abcdef123456 end", secret: "sk-ant-abcdef123456"},
		{name: "device code", in: "first copy your one-time code: WXYZ-9876", secret: "WXYZ-9876"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactPaneTokens(tt.in); strings.Contains(got, tt.secret) {
				t.Fatalf("RedactPaneTokens leaked %q: %q", tt.secret, got)
			}
		})
	}
	if got := RedactPaneTokens("plain text"); got != "plain text" {
		t.Fatalf("RedactPaneTokens altered clean input: %q", got)
	}
	if got := RedactPaneTokens(""); got != "" {
		t.Fatalf("RedactPaneTokens(\"\") = %q", got)
	}
}
