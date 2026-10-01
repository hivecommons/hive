package scheduler

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestIssuePriorityMarker_NamesInheritedAcknowledgement9840(t *testing.T) {
	cases := []struct {
		name  string
		issue github.Issue
		want  string
	}{
		{"human", github.Issue{AuthorIsHuman: true}, "[human]"},
		{"own ack", github.Issue{HumanAcknowledged: true}, "[hive-filed+ack]"},
		{"inherited ack", github.Issue{HumanAcknowledged: true, AckParent: 9802}, "[hive-filed+parent-ack #9802]"},
		{"nothing", github.Issue{}, "[hive-filed]"},
	}
	for _, tc := range cases {
		if got := issuePriorityMarker(tc.issue); got != tc.want {
			t.Errorf("%s: marker = %q, want %q", tc.name, got, tc.want)
		}
	}
}
