package agent

import "testing"

// TestCountCIPollCommandsDetectsPRViewStatusCheck covers a gap in the #9673
// harness guard: agents often check their own PR's CI status with
// `gh pr view --json statusCheckRollup` rather than `gh run watch/view` or
// `gh pr checks`, and that command bypassed countCIPollCommands entirely
// before ciPollCommandRe learned to recognize it. A plain `gh pr view` used
// to read PR metadata (title, author, files, ...) must still be ignored so
// the guard does not nudge agents for ordinary, non-CI PR lookups.
func TestCountCIPollCommandsDetectsPRViewStatusCheck(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want int
	}{
		{
			name: "pr view polling CI status is counted",
			pane: "gh pr view 123 --json statusCheckRollup",
			want: 1,
		},
		{
			name: "pr view for ordinary metadata is not counted",
			pane: "gh pr view 123 --json title,author,files",
			want: 0,
		},
		{
			name: "pr status is counted",
			pane: "gh pr status",
			want: 1,
		},
		{
			name: "mixed pane counts each poll command once",
			pane: "gh pr view 123 --json title\ngh run watch 6\ngh pr view 123 --json statusCheckRollup",
			want: 2,
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
