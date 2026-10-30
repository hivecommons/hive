package github

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

// The titles hivecommons/hive filed on one night: every carry-over wrapper
// strips to the finding it restates, so one finding keys to one string
// whichever perspective or head reported it.
func TestReviewBacklogNormalisedTitleCollapsesRealTitles(t *testing.T) {
	cases := []struct {
		title     string
		want      string
		carryOver bool
	}{
		// #15627 / #15628 / #15629: one finding on PR #15582.
		{"Review backlog (style): New test re-covers prWarnOnce semantics rather than the dispatcher wiring", "new test re-covers prwarnonce semantics rather than the dispatcher wiring", false},
		{"Review backlog (security): Previous style finding still present (non-blocking)", "", true},
		{"Review backlog (intent-alignment): Previous style note still present (non-blocking): TestReviewPromotedWarnOncePerPR exercises prWarnOnce, not planReviewDispatchWith wiring", "testreviewpromotedwarnonceperpr exercises prwarnonce, not planreviewdispatchwith wiring", true},
		// #15630 / #15631 on PR #15621.
		{"Review backlog (style): Prior correctness note still present: head-commit fetch precedes the compare", "head-commit fetch precedes the compare", true},
		{"Review backlog (security): Previous correctness note still present: headCommitTime runs before the compare proves the diff empty", "headcommittime runs before the compare proves the diff empty", true},
		// #15659 / #15660: the same finding wrapped two ways.
		{"Review backlog (docs-currency): Prior finding still present: waiver will recur per squash", "waiver will recur per squash", true},
		{"Review backlog (security): Previous intent-alignment note (waiver will recur per squash) still structurally true", "waiver will recur per squash", true},
		// #15602, #15182: wrapper with the finding inside.
		{"Review backlog (security): Previous intent-alignment finding still present: mobile-shots retry tuning in refactor PR", "mobile-shots retry tuning in refactor pr", true},
		{"Review backlog (correctness): Previous security finding (merge/trust policy expansion) unchanged at new head", "merge/trust policy expansion", true},
		// #15576, #15540: per-pass qualifiers vary; the finding does not.
		{"Review backlog (correctness): Retained 'drum cell per digit' clause still contradicts the newer odometer fragment (still present, advisory)", "retained 'drum cell per digit' clause still contradicts the newer odometer fragment", false},
		{"Review backlog (docs-currency): Dashboard help marks still land on index stub page (advisory, unchanged)", "dashboard help marks still land on index stub page", false},
		{"[reviewer] Review backlog (docs-currency): Dashboard help marks still land on index stub page", "dashboard help marks still land on index stub page", false},
		// #15548: "Earlier …" describing the code, not a prior note.
		{"Review backlog (docs-currency): Earlier unreleased changelog fragment still advertises 18 px digits and a grown pill", "earlier unreleased changelog fragment still advertises 18 px digits and a grown pill", false},
		{"Previous finding leaks the token: foo", "previous finding leaks the token: foo", false},
	}
	for _, tc := range cases {
		if got := reviewBacklogNormalisedTitle(tc.title); got != tc.want {
			t.Errorf("normalised(%q)\n got %q\nwant %q", tc.title, got, tc.want)
		}
		if got := reviewBacklogCarryOver(outputschema.Finding{Title: tc.title}); got != tc.carryOver {
			t.Errorf("carryOver(%q) = %v, want %v", tc.title, got, tc.carryOver)
		}
	}
	if !reviewBacklogCarryOver(outputschema.Finding{Title: "Retry tuning", Summary: "See <!-- hive-refers-to: #15600 --> for the earlier note."}) {
		t.Error("a refers-to marker makes the finding a reference")
	}
	if got := reviewRefersTo(outputschema.Finding{Summary: "<!-- hive-refers-to: #15600 --> and <!-- hive-refers-to: o/r#7 -->"}); len(got) != 2 || got[0] != 15600 || got[1] != 7 {
		t.Errorf("refersTo = %v", got)
	}
}

func TestReviewBacklogTitlesEqualIgnoresRolePrefix(t *testing.T) {
	a := "Review backlog (style): Dashboard help marks still land on index stub page"
	if !reviewBacklogTitlesEqual("[reviewer] "+a, a) || !reviewBacklogTitlesEqual(a, "[reviewer] "+a) || !reviewBacklogTitlesEqual(a, a) {
		t.Error("the [reviewer] prefix must not defeat the title match")
	}
	if reviewBacklogTitlesEqual("[reviewer] Fix the thing", "Fix the thing") {
		t.Error("only backlog titles are prefix-insensitive")
	}
	if reviewBacklogTitlesEqual("Review backlog (style): a", "Review backlog (style): b") {
		t.Error("different findings never match")
	}
}

func perspectiveReport(t *testing.T, p review.Perspective, number int, head string, findings ...map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"lane": "review-swarm", "kind": "review", "perspective": string(p), "verdict": "approve",
		"repo": "o/r", "number": number, "head_sha": head, "summary": "review", "findings": findings,
		"prs_opened": []any{}, "beads_filed": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func oos(title, file string, line int, summary string) map[string]any {
	return map[string]any{"title": title, "severity": "low", "summary": summary, "file": file, "line": line, "review_scope": "out-of-scope"}
}

// Three perspectives across two heads report one finding of PR 5: one item
// is filed; the other perspectives' "previous … still present" reports are
// references and file nothing; a new head leaves one "still present" note,
// not one per perspective; a carry-over that names nothing tracked is
// dropped instead of filed.
func TestReviewBacklogOneItemPerFindingAcrossPerspectivesAndHeads(t *testing.T) {
	withReviewBacklogState(t)
	var pending []review.PendingReview
	for _, head := range []string{"abc", "bbb"} {
		for _, p := range []review.Perspective{review.PerspectiveStyle, review.PerspectiveSecurity, review.PerspectiveIntentAlignment, review.PerspectiveCorrectness} {
			pending = append(pending, review.PendingReview{Repo: "o/r", Number: 5, HeadSHA: head, Perspective: p, Agent: "reviewer"})
		}
	}
	withVerdictDispatchState(t, review.DispatchState{Pending: pending})
	comments := map[int][]string{}
	srv := prCommentServer(t, comments)
	c := reviewTestClient(t, srv.URL)
	c.SetReviewBacklog(func() (bool, int) { return true, 10 })
	sink := &fakeBacklogSink{dest: config.ReviewBacklogLinear}
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P1"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear, MaxPerPRPerDay: 10},
			Sink:     sink,
		}
	})
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 22, 0, 0, 0, time.UTC)
	file := "src/cmd/hive/warn_once_pr_test.go"
	file5 := func(p review.Perspective, head string, findings ...map[string]any) {
		t.Helper()
		if err := c.fileOutOfScopeReviewBacklog(ctx, ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: perspectiveReport(t, p, 5, head, findings...)}, now); err != nil {
			t.Fatalf("%s@%s: %v", p, head, err)
		}
	}

	// Head abc: style raises the finding; security and intent-alignment
	// restate it (same file, same line / shared symbol).
	file5(review.PerspectiveStyle, "abc", oos("New test re-covers prWarnOnce semantics rather than the dispatcher wiring", file, 27, "the test exercises prWarnOnce"))
	file5(review.PerspectiveSecurity, "abc", oos("Previous style finding still present (non-blocking)", file, 27, "unchanged"))
	file5(review.PerspectiveIntentAlignment, "abc", oos("Previous style note still present (non-blocking): TestReviewPromotedWarnOncePerPR exercises prWarnOnce, not planReviewDispatchWith wiring", file, 30, "unchanged"))
	if len(sink.created) != 1 || sink.created[0].Title != "Review backlog (style): New test re-covers prWarnOnce semantics rather than the dispatcher wiring" {
		t.Fatalf("created = %+v, want the one style item", sink.created)
	}
	if len(sink.appends) != 0 {
		t.Fatalf("appends at the filing head = %+v, want none", sink.appends)
	}

	// Head bbb: all three report it again. One "still present" note.
	file5(review.PerspectiveStyle, "bbb", oos("New test re-covers prWarnOnce semantics rather than the dispatcher wiring", file, 27, "still there"))
	file5(review.PerspectiveSecurity, "bbb", oos("Previous style finding still present (non-blocking)", file, 27, "unchanged"))
	file5(review.PerspectiveIntentAlignment, "bbb", oos("Prior style note unchanged at new head", file, 27, "unchanged"))
	if len(sink.created) != 1 {
		t.Fatalf("a later head re-filed: %+v", sink.created)
	}
	if len(sink.appends) != 1 || sink.appends[0].ref.ID != "id-1" {
		t.Fatalf("appends = %+v, want one still-present note on id-1", sink.appends)
	}
	if note := sink.appends[0].comment; !strings.Contains(note, "Still present at bbb") || !strings.Contains(note, "(style)") || !strings.Contains(note, reviewBacklogStillPresentMarker) {
		t.Fatalf("still-present note = %q", note)
	}

	// A carry-over citing nothing tracked for this PR is a reference to
	// nowhere: dropped, never filed.
	file5(review.PerspectiveCorrectness, "bbb", oos("Prior security note still present: token leaks in the log line", "pkg/other.go", 4, "unchanged"))
	if len(sink.created) != 1 {
		t.Fatalf("an unanchored carry-over was filed: %+v", sink.created)
	}
	// A genuinely new finding elsewhere still files.
	file5(review.PerspectiveCorrectness, "bbb", oos("Dispatcher wiring untested", "pkg/other.go", 9, "nothing covers planReviewDispatchWith"))
	if len(sink.created) != 2 || sink.created[1].Title != "Review backlog (correctness): Dispatcher wiring untested" {
		t.Fatalf("created = %+v, want the new correctness finding filed second", sink.created)
	}

	ledger, err := loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ledger.PRFindings[reviewBacklogPRKey("o/r", 5)]); got != 2 {
		t.Fatalf("PR findings tracked = %d, want 2 (one per item)", got)
	}
	// The PR's summary comment is once per PR; re-reports and the second
	// filing add nothing to it.
	if len(comments[5]) != 1 {
		t.Fatalf("PR 5 summary comments = %d, want 1", len(comments[5]))
	}
}
