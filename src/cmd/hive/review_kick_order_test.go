package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// hivecommons/hive#13100: the owner's `/hive decision` was recorded and the
// planner produced a fix kick for the author agent every cycle, but the
// cycle's governor kick to that same agent went out first, the agent was
// mid-turn by the time the fix kick's turn came, and the send timed out —
// cycle after cycle. A maintainer's instruction outranks routine queue work
// for the agent it addresses, so the fix kick must go first.
func TestOrderReviewFixKicksBeforeGovernorKicks(t *testing.T) {
	govScanner := scheduler.KickMessage{Agent: "scanner", Message: "issues", IssueRefs: []string{"acme/x#1"}}
	govReviewer := scheduler.KickMessage{Agent: "reviewer", Message: "issues"}
	reviewKick := scheduler.KickMessage{Agent: "reviewer", Message: "[review-perspective:style] …", IssueRefs: []string{"acme/x#9"}}
	fixKick := scheduler.KickMessage{Agent: "scanner", Message: "[review-fix:acme/x#9] …", IssueRefs: []string{"acme/x#9"}}
	byMessage := map[string]review.DispatchKick{
		reviewKickKey(reviewKick): {Kind: "review", Agent: "reviewer", PRRef: "acme/x#9"},
		reviewKickKey(fixKick):    {Kind: "fix", Agent: "scanner", PRRef: "acme/x#9"},
	}
	got := orderReviewFixKicksFirst([]scheduler.KickMessage{govScanner, govReviewer, reviewKick, fixKick}, byMessage)
	want := []scheduler.KickMessage{fixKick, govScanner, govReviewer, reviewKick}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Agent != want[i].Agent || got[i].Message != want[i].Message {
			t.Fatalf("position %d = %s %q, want %s %q", i, got[i].Agent, got[i].Message, want[i].Agent, want[i].Message)
		}
	}
}

// No fix kick, or a fix kick for an agent nothing else addresses: the order
// is untouched, so the review-kicks-after-governor-kicks contract holds.
func TestOrderReviewFixKicksFirstLeavesOtherOrdersAlone(t *testing.T) {
	gov := scheduler.KickMessage{Agent: "scanner", Message: "issues"}
	reviewKick := scheduler.KickMessage{Agent: "reviewer", Message: "[review-perspective:style]"}
	fixKick := scheduler.KickMessage{Agent: "ci-maintainer", Message: "[review-fix:acme/x#9]"}
	byMessage := map[string]review.DispatchKick{
		reviewKickKey(reviewKick): {Kind: "review", Agent: "reviewer"},
		reviewKickKey(fixKick):    {Kind: "fix", Agent: "ci-maintainer"},
	}
	in := []scheduler.KickMessage{gov, reviewKick, fixKick}
	got := orderReviewFixKicksFirst(in, byMessage)
	for i := range in {
		if got[i].Agent != in[i].Agent || got[i].Message != in[i].Message {
			t.Fatalf("order changed at %d: %+v", i, got)
		}
	}
	if got := orderReviewFixKicksFirst(nil, byMessage); len(got) != 0 {
		t.Fatalf("nil input = %+v", got)
	}
}

// A review-plan kick that failed to send must say which PR lost its kick:
// "failed to send kick agent=scanner" alone gave no way to connect the
// starved fix to hivecommons/hive#13100.
func TestLogUndeliveredReviewKickNamesPR(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	auditor := &recordingFixAuditor{}
	k := review.DispatchKick{Kind: "fix", Agent: "scanner", PRRef: "hivecommons/hive#13100", Repo: "hivecommons/hive", Number: 13100, HeadSHA: "2da603f"}
	logUndeliveredReviewKick(logger, auditor, k, errors.New("agent scanner CLI did not reach input prompt"))
	out := buf.String()
	for _, want := range []string{"review swarm kick not delivered", "kind=fix", "pr=hivecommons/hive#13100", "head=2da603f", "agent=scanner", "did not reach input prompt", "re-planned next cycle"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q: %s", want, out)
		}
	}
	if len(auditor.entries) != 1 {
		t.Fatalf("audit entries = %v, want one", auditor.entries)
	}
	for _, want := range []string{"governor|review_kick_undelivered|", "kind=fix", "pr=hivecommons/hive#13100", "head=2da603f", "agent=scanner", "error=agent scanner CLI did not reach input prompt"} {
		if !strings.Contains(auditor.entries[0], want) {
			t.Fatalf("audit entry lacks %q: %s", want, auditor.entries[0])
		}
	}
	// No dashboard: the log line still fires, nothing panics.
	logUndeliveredReviewKick(logger, nil, k, errors.New("x"))
}
