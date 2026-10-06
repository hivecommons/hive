package dashboard

import (
	"strings"
	"testing"
)

func TestEnsureIssueClosingLine(t *testing.T) {
	tests := []struct {
		name                string
		body                string
		issue               int
		defaultBranchTarget bool
		want                string
	}{
		{
			name:                "missing keyword appends closes",
			body:                "## Summary\n\nImplement the fix.",
			issue:               10816,
			defaultBranchTarget: true,
			want:                "## Summary\n\nImplement the fix.\n\nCloses #10816",
		},
		{
			name:                "existing fixes unchanged",
			body:                "## Summary\n\nFixes #10816\n",
			issue:               10816,
			defaultBranchTarget: true,
			want:                "## Summary\n\nFixes #10816\n",
		},
		{
			name:                "refs only appends closes",
			body:                "Refs #10816 (reporter confirmation required).",
			issue:               10816,
			defaultBranchTarget: true,
			want:                "Refs #10816 (reporter confirmation required).\n\nCloses #10816",
		},
		{
			name:                "no issue unchanged",
			body:                "## Summary\n\nImplement the fix.",
			issue:               0,
			defaultBranchTarget: true,
			want:                "## Summary\n\nImplement the fix.",
		},
		{
			name:                "non default branch unchanged",
			body:                "Refs #10816",
			issue:               10816,
			defaultBranchTarget: false,
			want:                "Refs #10816",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ensureIssueClosingLine(tt.body, tt.issue, tt.defaultBranchTarget); got != tt.want {
				t.Fatalf("ensureIssueClosingLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestContributorPRClosingInstructionNamesClosingKeyword(t *testing.T) {
	got := contributorPRClosingInstruction("hivecommons/hive#10816")
	if !containsIssueClosingKeyword(got, "#10816") {
		t.Fatalf("instruction lost closing keyword: %q", got)
	}
	for _, want := range []string{"Fixes #10816", "Refs #10816", "default branch"} {
		if !strings.Contains(got, want) {
			t.Fatalf("instruction missing %q: %q", want, got)
		}
	}
}
