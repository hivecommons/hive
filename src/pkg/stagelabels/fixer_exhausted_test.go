package stagelabels

import (
	"strings"
	"testing"
)

// hivecommons/hive#15744: the 085 reason and route for a hive PR whose fix
// dispatcher refused another fix after the re-engagement cap.

func TestFixerExhaustedReasonRoundTrip(t *testing.T) {
	r := FixerExhaustedReason(6)
	if r != "fixer-exhausted:6" {
		t.Fatalf("reason = %q", r)
	}
	n, ok := ParseFixerExhausted(" " + r + " ")
	if !ok || n != 6 {
		t.Fatalf("parse = %d %v", n, ok)
	}
	for _, bad := range []string{"", "fixer-exhausted:", "fixer-exhausted:x", "fixer-exhausted:-1", "human-changes-requested", "fixer-exhausted"} {
		if n, ok := ParseFixerExhausted(bad); ok || n != 0 {
			t.Fatalf("ParseFixerExhausted(%q) = %d %v, want 0 false", bad, n, ok)
		}
	}
}

func TestFixerExhaustedRouteAndComment(t *testing.T) {
	head := "5b62304abcdef0123456789"
	r := FixerExhaustedRoute(head, 6, "  tests fail on arm64 ", 0)
	if r.Stage != NeedsDecision || !r.SelfAuthored || r.Head != head || r.ActionsHeading != "Your options:" {
		t.Fatalf("route = %+v", r)
	}
	if len(r.Reasons) != 2 || !strings.Contains(r.Reasons[0], "gave up after 6 attempts on `"+ShortSHA(head)+"`") || r.Reasons[1] != "last reviewer finding: tests fail on arm64" {
		t.Fatalf("reasons = %q", r.Reasons)
	}
	if len(r.Actions) != 3 || !strings.HasPrefix(r.Actions[0], "**Give direction**") || !strings.HasPrefix(r.Actions[1], "**Hand it to a person**") || !strings.HasPrefix(r.Actions[2], "**Close as not planned**") {
		t.Fatalf("actions = %q", r.Actions)
	}
	for _, a := range r.Actions {
		if strings.Contains(strings.ToLower(a), "re-author") {
			t.Fatalf("re-authoring offered as a human choice on a hive PR: %q", a)
		}
	}
	// Second exhaustion on the hive's own re-author: the route says so.
	again := FixerExhaustedRoute(head, 6, "", 16456)
	if len(again.Reasons) != 2 || !strings.Contains(again.Reasons[1], "re-author of #16456") {
		t.Fatalf("re-authored reasons = %q", again.Reasons)
	}

	c := DecisionComment(r)
	for _, want := range []string{DecisionCommentMarker, "Needs a decision, not a merge:\n- the hive's fixer gave up", "- last reviewer finding: tests fail on arm64", "**Your options:**", "/hive decision"} {
		if !strings.Contains(c, want) {
			t.Fatalf("comment missing %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "How to release it") || DecisionActions(r) != "" {
		t.Fatalf("exhausted route borrowed the hold/requires_human wording:\n%s", c)
	}

	// No summary: a single reason, rendered inline.
	one := FixerExhaustedRoute(head, 3, "", 0)
	if len(one.Reasons) != 1 {
		t.Fatalf("reasons = %q", one.Reasons)
	}
	if c := DecisionComment(one); !strings.Contains(c, "Needs a decision, not a merge: the hive's fixer gave up after 3 attempts") {
		t.Fatalf("single-reason comment:\n%s", c)
	}
}

func TestReauthoringReason(t *testing.T) {
	if got := ReauthoringReason(16265); got != "re-authoring:16265" {
		t.Fatalf("ReauthoringReason = %q", got)
	}
	if n, ok := ParseReauthoring("re-authoring:16265"); !ok || n != 16265 {
		t.Fatalf("ParseReauthoring = %d %v", n, ok)
	}
	for _, bad := range []string{"", "re-authoring:", "re-authoring:0", "re-authoring:x", "fixer-exhausted:6"} {
		if n, ok := ParseReauthoring(bad); ok || n != 0 {
			t.Fatalf("ParseReauthoring(%q) = %d %v, want 0 false", bad, n, ok)
		}
	}
}
