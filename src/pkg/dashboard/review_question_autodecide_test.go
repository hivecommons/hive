package dashboard

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

func TestReviewQuestionCandidatesPicksReviewerQuestionsAt085(t *testing.T) {
	ask := &StageTrackerDecision{Question: "@clubanderson should decide whether this merges ahead of the workflow edit.", AskedBy: decisionAskedByReviewer}
	items := []StageTrackerItem{
		{Kind: "pr", Repo: "docs", Number: 618, Head: "abc", Stage: trackerNumberNeedsDecision, AuthorKind: authorKindHive, DecisionNeeded: ask},
		{Kind: "pr", Repo: "hivecommons/hive", Number: 2, Head: "def", Stage: trackerNumberNeedsDecision, AuthorKind: authorKindHive, DecisionNeeded: ask, ciRed: true},
		{Kind: "pr", Repo: "hivecommons/hive", Number: 3, Head: "ghi", Stage: trackerNumberNeedsDecision, AuthorKind: authorKindHive, DecisionNeeded: ask, ciChecking: true},
		{Kind: "pr", Repo: "hivecommons/hive", Number: 4, Head: "jkl", Stage: trackerNumberNeedsDecision, AuthorKind: authorKindHive, DecisionNeeded: ask, OnHold: true},
		{Kind: "pr", Repo: "hivecommons/hive", Number: 5, Head: "mno", Stage: trackerNumberNeedsDecision, AuthorKind: authorKindHive, DecisionNeeded: &StageTrackerDecision{Question: "sentinel flagged", AskedBy: "sentinel"}},
		{Kind: "pr", Repo: "hivecommons/hive", Number: 6, Head: "", Stage: trackerNumberNeedsDecision, AuthorKind: authorKindHive, DecisionNeeded: ask},
		{Kind: "pr", Repo: "hivecommons/hive", Number: 7, Head: "pqr", Stage: 70, AuthorKind: authorKindHive, DecisionNeeded: ask},
		{Kind: "issue", Repo: "hivecommons/hive", Number: 8, Head: "stu", Stage: trackerNumberNeedsDecision, DecisionNeeded: ask},
		{Kind: "pr", Repo: "hivecommons/hive", Number: 9, Head: "vwx", Stage: trackerNumberNeedsDecision, AuthorKind: "person", AdoptedBy: 10, DecisionNeeded: ask},
	}
	qualify := func(r string) string {
		if r == "docs" {
			return "hivecommons/docs"
		}
		return r
	}
	got := reviewQuestionCandidates(items, qualify)
	if len(got) != 2 {
		t.Fatalf("candidates = %+v, want docs#618 and hive#9", got)
	}
	if got[0].Repo != "hivecommons/docs" || got[0].Number != 618 || !got[0].HiveAuthored {
		t.Errorf("first = %+v, want qualified hive-authored docs#618", got[0])
	}
	if got[1].Number != 9 || !got[1].HiveAuthored {
		t.Errorf("second = %+v, want adopted hive#9 read as hive-authored", got[1])
	}
}

func TestReviewQuestionUnresolvedFindings(t *testing.T) {
	clean := review.Aggregate{DecisionQuestion: "should this merge ahead?", Perspectives: map[review.Perspective]review.Verdict{"security": review.VerdictApprove}}
	if reviewQuestionUnresolvedFindings(clean) {
		t.Errorf("a bare question with approving perspectives is not an unresolved finding")
	}
	backed := clean
	backed.DecisionFinding = "security: token in workflow"
	if !reviewQuestionUnresolvedFindings(backed) {
		t.Errorf("a backed decision finding is unresolved")
	}
	changes := review.Aggregate{Perspectives: map[review.Perspective]review.Verdict{"correctness": review.VerdictChangesRequested}}
	if !reviewQuestionUnresolvedFindings(changes) {
		t.Errorf("changes_requested is unresolved")
	}
	finding := review.Aggregate{Findings: []review.PerspectiveFinding{{Perspective: "correctness", Finding: outputschema.Finding{Title: "nil deref", Severity: outputschema.SeverityHigh}}}}
	if !reviewQuestionUnresolvedFindings(finding) {
		t.Errorf("an in-scope finding is unresolved")
	}
}

func TestReviewQuestionAutodecideOncePerHead(t *testing.T) {
	st := &reviewQuestionAutodecide{path: filepath.Join(t.TempDir(), "markers.json")}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	if st.seen("hivecommons/docs", 618, "abc") {
		t.Fatal("fresh ledger reports seen")
	}
	if err := st.mark("hivecommons/docs", 618, reviewQuestionAutodecideMarker{Head: "abc", At: now, Class: "scope-question", Reason: "r"}); err != nil {
		t.Fatal(err)
	}
	if !st.seen("hivecommons/docs", 618, "ABC") {
		t.Error("same head (case-insensitive) must read as seen")
	}
	if st.seen("hivecommons/docs", 618, "def") {
		t.Error("a new push must ask again")
	}
	reloaded := &reviewQuestionAutodecide{path: st.path}
	if !reloaded.seen("HiveCommons/Docs", 618, "abc") {
		t.Error("marker must survive a reload")
	}
	if got := reloaded.countSince(now.Add(30*time.Minute), time.Hour); got != 1 {
		t.Errorf("count within window = %d, want 1", got)
	}
	if got := reloaded.countSince(now.Add(2*time.Hour), time.Hour); got != 0 {
		t.Errorf("count past window = %d, want 0", got)
	}
}

func TestReviewQuestionFollowUpTitle(t *testing.T) {
	got := reviewQuestionFollowUpTitle("  should we\n also apply the opt-in?  ")
	if got != "follow-up: should we also apply the opt-in?" {
		t.Errorf("title = %q", got)
	}
	long := reviewQuestionFollowUpTitle(repeatRune('q', 300))
	if r := []rune(long); len(r) != reviewQuestionFollowUpTitleMax || r[len(r)-1] != '…' {
		t.Errorf("long title not capped: %d runes", len(r))
	}
}

func repeatRune(r rune, n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = r
	}
	return string(out)
}
