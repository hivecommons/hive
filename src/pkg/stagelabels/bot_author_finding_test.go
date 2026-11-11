package stagelabels

import (
	"strings"
	"testing"
)

func TestBotAuthorFindingRouteAndComment(t *testing.T) {
	head := "e9e24fdabcdef0123456789"
	r := BotAuthorFindingRoute(head, " github-actions[bot] ", "stable", "  the build workflow drops the arm64 job ")
	if r.Stage != NeedsDecision || r.SelfAuthored || r.Head != head || r.ActionsHeading != "Your options:" {
		t.Fatalf("route = %+v", r)
	}
	if len(r.Reasons) != 2 || !strings.Contains(r.Reasons[0], "@"+ShortSHA(head)+" on a PR that github-actions[bot] opened and regenerates") || r.Reasons[1] != "the review found: the build workflow drops the arm64 job" {
		t.Fatalf("reasons = %q", r.Reasons)
	}
	if len(r.Actions) != 3 || !strings.HasPrefix(r.Actions[0], "**Fix it on `stable`**") || !strings.HasPrefix(r.Actions[1], "**Merge it as is**") || !strings.HasPrefix(r.Actions[2], "**Close it**") {
		t.Fatalf("actions = %q", r.Actions)
	}
	c := DecisionComment(r)
	for _, want := range []string{DecisionCommentMarker, "**Your options:**", "github-actions[bot]'s next run brings it into this PR"} {
		if !strings.Contains(c, want) {
			t.Fatalf("comment missing %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "How to release it") || strings.Contains(c, "/hive decision") {
		t.Fatalf("comment offers a fixer direction no agent can act on:\n%s", c)
	}

	// Base unknown, no summary: one reason, a generic base.
	bare := BotAuthorFindingRoute(head, "renovate[bot]", "", "")
	if len(bare.Reasons) != 1 || !strings.HasPrefix(bare.Actions[0], "**Fix it on the base branch**") {
		t.Fatalf("bare route = %+v", bare)
	}
}
