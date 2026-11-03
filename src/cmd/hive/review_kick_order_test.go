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
	got := orderReviewFixKicksFirst([]scheduler.KickMessage{govScanner, govReviewer, reviewKick, fixKick}, byMessage, nil)
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
	got := orderReviewFixKicksFirst(in, byMessage, nil)
	for i := range in {
		if got[i].Agent != in[i].Agent || got[i].Message != in[i].Message {
			t.Fatalf("order changed at %d: %+v", i, got)
		}
	}
	if got := orderReviewFixKicksFirst(nil, byMessage, nil); len(got) != 0 {
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

// hivecommons/hive#15166: a fix kick delivered first leaves the lane busy, so
// the governor kick behind it is deferred; an adopted PR under
// changes_requested is re-planned as a fix kick every cycle, so the scanner's
// backlog kick was deferred cycle after cycle (live: 0 backlog kicks and 6
// fix kicks in 71 minutes, 13 issues sitting at stage/010-actionable). After
// governorKickDisplacementLimit consecutive displacements the governor kick
// goes first for that agent; a delivered governor kick resets the count.
func TestKickLaneFairnessPutsGovernorFirstAfterConsecutiveDisplacement(t *testing.T) {
	gov := scheduler.KickMessage{Agent: "scanner", Message: "issues"}
	fix := scheduler.KickMessage{Agent: "scanner", Message: "[review-fix:acme/x#9]"}
	byMessage := map[string]review.DispatchKick{
		reviewKickKey(fix): {Kind: "fix", Agent: "scanner", PRRef: "acme/x#9"},
	}
	f := newKickLaneFairness()

	for cycle := 1; cycle <= governorKickDisplacementLimit; cycle++ {
		if f.GovernorFirst("scanner") {
			t.Fatalf("cycle %d: governor first before the limit was reached", cycle)
		}
		ordered := orderReviewFixKicksFirst([]scheduler.KickMessage{gov, fix}, byMessage, f.GovernorFirst)
		if ordered[0].Message != fix.Message {
			t.Fatalf("cycle %d: order = %+v, want fix kick first", cycle, ordered)
		}
		// The fix kick went out; the governor kick was deferred behind it.
		f.ObserveCycle(ordered, []scheduler.KickMessage{fix}, byMessage)
	}
	if !f.GovernorFirst("scanner") {
		t.Fatalf("displaced %d cycles, governor still not first", governorKickDisplacementLimit)
	}
	ordered := orderReviewFixKicksFirst([]scheduler.KickMessage{gov, fix}, byMessage, f.GovernorFirst)
	if ordered[0].Message != gov.Message || ordered[1].Message != fix.Message {
		t.Fatalf("after displacement limit order = %+v, want governor first", ordered)
	}
	f.ObserveCycle(ordered, []scheduler.KickMessage{gov}, byMessage)
	if f.GovernorFirst("scanner") || f.Displaced("scanner") != 0 {
		t.Fatalf("delivered governor kick did not reset: displaced=%d", f.Displaced("scanner"))
	}
}

// Another agent's displacement is its own; a cycle with no governor kick for
// the agent, or one where the governor kick failed outright rather than
// being deferred behind a delivered fix, does not count.
func TestKickLaneFairnessCountsOnlyDeferralBehindDeliveredFix(t *testing.T) {
	govScanner := scheduler.KickMessage{Agent: "scanner", Message: "issues"}
	fixScanner := scheduler.KickMessage{Agent: "scanner", Message: "[review-fix:acme/x#9]"}
	fixCI := scheduler.KickMessage{Agent: "ci-maintainer", Message: "[review-fix:acme/x#8]"}
	byMessage := map[string]review.DispatchKick{
		reviewKickKey(fixScanner): {Kind: "fix", Agent: "scanner"},
		reviewKickKey(fixCI):      {Kind: "fix", Agent: "ci-maintainer"},
	}
	f := newKickLaneFairness()

	// Fix kick only, no governor kick assembled for the scanner: nothing to starve.
	f.ObserveCycle([]scheduler.KickMessage{fixScanner}, []scheduler.KickMessage{fixScanner}, byMessage)
	if f.Displaced("scanner") != 0 {
		t.Fatalf("no governor kick assembled yet displaced=%d", f.Displaced("scanner"))
	}
	// Both assembled, neither delivered (send failed): a kick failure, not a displacement.
	f.ObserveCycle([]scheduler.KickMessage{fixScanner, govScanner}, nil, byMessage)
	if f.Displaced("scanner") != 0 {
		t.Fatalf("failed send counted as displacement: %d", f.Displaced("scanner"))
	}
	// A displacement on the scanner lane leaves ci-maintainer untouched.
	f.ObserveCycle([]scheduler.KickMessage{fixScanner, govScanner, fixCI}, []scheduler.KickMessage{fixScanner, fixCI}, byMessage)
	if f.Displaced("scanner") != 1 || f.Displaced("ci-maintainer") != 0 {
		t.Fatalf("displaced scanner=%d ci-maintainer=%d, want 1 / 0", f.Displaced("scanner"), f.Displaced("ci-maintainer"))
	}
	var nilFairness *kickLaneFairness
	if nilFairness.GovernorFirst("scanner") {
		t.Fatal("nil fairness must never put the governor first")
	}
}
