package github

import (
	"testing"
	"time"
)

func TestReviewAddressed(t *testing.T) {
	reviewAt := time.Date(2026, 9, 30, 14, 15, 0, 0, time.UTC)
	after := reviewAt.Add(time.Hour)
	before := reviewAt.Add(-time.Hour)
	tests := []struct {
		name    string
		commits []PRCommit
		replies []PRComment
		want    bool
	}{
		{
			name:    "merge commit after review does not address",
			commits: []PRCommit{{SHA: "merge", AuthoredAt: after, ParentCount: 2}},
		},
		{
			name:    "non merge commit after review addresses",
			commits: []PRCommit{{SHA: "fix", AuthoredAt: after, ParentCount: 1}},
			want:    true,
		},
		{
			name:    "agent reply after review addresses",
			replies: []PRComment{{ID: "conversation:1", CreatedAt: after}},
			want:    true,
		},
		{
			name:    "older activity does not address",
			commits: []PRCommit{{SHA: "old", AuthoredAt: before, ParentCount: 1}},
			replies: []PRComment{{ID: "conversation:2", CreatedAt: before}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReviewAddressed(reviewAt, tc.commits, tc.replies); got != tc.want {
				t.Fatalf("ReviewAddressed() = %v, want %v", got, tc.want)
			}
		})
	}
}
