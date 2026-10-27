package agent

import "testing"

// The kick prompt quotes the polling commands as prohibitions; pasting it
// into the pane must not count as the agent polling CI (#13648).
func TestCountCIPollCommandsIgnoresProhibitions(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want int
	}{
		{
			name: "kick prompt prohibition",
			pane: "After the push, STOP: do not watch, poll, or sleep on CI (no gh run watch/view loops).",
			want: 0,
		},
		{
			name: "policy text with backticks",
			pane: "sleep on CI** — no `gh run watch`, no `gh run view` loops, no `gh pr checks` loops",
			want: 0,
		},
		{
			name: "real command after prohibition text on another line",
			pane: "no `gh run watch` loops\n$ gh run watch 42",
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countCIPollCommands(tt.pane); got != tt.want {
				t.Errorf("countCIPollCommands(%q) = %d, want %d", tt.pane, got, tt.want)
			}
		})
	}
}
