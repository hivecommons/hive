package github

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

var reviewQueueTestNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func queueVerdict(repo string, number int, head string, score int, reasons ...string) review.Aggregate {
	return review.Aggregate{
		Repo: repo, Number: number, HeadSHA: head,
		Verdict:    review.VerdictApprove,
		Confidence: review.Confidence{Score: score, Reasons: reasons},
	}
}

func queueKeys(q []ReviewQueueEntry) []string {
	out := make([]string, 0, len(q))
	for _, e := range q {
		out = append(out, ReviewQueueKey(e.Repo, e.Number))
	}
	return out
}

func findEntry(t *testing.T, q []ReviewQueueEntry, repo string, number int) ReviewQueueEntry {
	t.Helper()
	for _, e := range q {
		if e.Repo == repo && e.Number == number {
			return e
		}
	}
	t.Fatalf("%s#%d not in queue %v", repo, number, queueKeys(q))
	return ReviewQueueEntry{}
}

func hasReason(e ReviewQueueEntry, substr string) bool {
	for _, r := range e.Reasons {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

// The acceptance test from #9590: an outside contributor's fix outranks a
// hive agent's coverage PR of the same age - even when the coverage PR is
// reviewed, safe and green and the fix is unreviewed and red. Class is the
// primary key; nothing below it can reorder across classes.
func TestBuildReviewQueue_T0ContributorFixOutranksT2AgentCoverage(t *testing.T) {
	opened := reviewQueueTestNow.Add(-6 * time.Hour)
	prs := []PullRequest{
		{
			Repo: "acme/app", Number: 10, Title: "[quality] test: cover the rescan error path",
			Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"agent/quality", "hold"},
			HeadSHA: "aaa", CIStatus: "success", CreatedAt: opened,
		},
		{
			Repo: "acme/app", Number: 11, Title: "fix: nil dereference when the proxy restarts",
			Author: "alice", HeadSHA: "bbb", CIStatus: "failure", CreatedAt: opened,
		},
	}
	q := BuildReviewQueue(prs, ReviewQueueOptions{
		Now:      reviewQueueTestNow,
		Verdicts: review.Artifact{Items: []review.Aggregate{queueVerdict("acme/app", 10, "aaa", review.ConfidenceMax)}},
	})
	if got := queueKeys(q); !reflect.DeepEqual(got, []string{"acme/app#11", "acme/app#10"}) {
		t.Fatalf("order = %v, want contributor fix first", got)
	}
	fix, cov := q[0], q[1]
	if fix.Class != ReviewClassFix || fix.Tier != "T0" || fix.Priority != ReviewPriorityHigh || fix.HiveAuthored {
		t.Fatalf("contributor fix entry = %+v", fix)
	}
	if cov.Class != ReviewClassTests || cov.Tier != "T2" || cov.Priority != ReviewPriorityLow || !cov.HiveAuthored {
		t.Fatalf("agent coverage entry = %+v", cov)
	}
	if fix.Position != 1 || cov.Position != 2 {
		t.Fatalf("positions = %d, %d", fix.Position, cov.Position)
	}
	if !hasReason(fix, "contributor PR") || hasReason(cov, "contributor PR") {
		t.Fatalf("contributor reason misattributed: fix=%v cov=%v", fix.Reasons, cov.Reasons)
	}
}

// Same inputs, any input order, same queue - and every key below class is
// honoured in turn.
func TestBuildReviewQueue_DeterministicOrdering(t *testing.T) {
	old := reviewQueueTestNow.Add(-72 * time.Hour)
	newer := reviewQueueTestNow.Add(-2 * time.Hour)
	prs := []PullRequest{
		// T1, safe, green, newer.
		{Repo: "acme/a", Number: 1, Title: "docs: explain the queue", HeadSHA: "s1", CIStatus: "success", CreatedAt: newer},
		// T1, safe, green, older -> first of the safe/green pair.
		{Repo: "acme/a", Number: 2, Title: "docs: explain the gate", HeadSHA: "s2", CIStatus: "success", CreatedAt: old},
		// T1, safe, red.
		{Repo: "acme/a", Number: 3, Title: "refactor: collapse builders", HeadSHA: "s3", CIStatus: "failure", CreatedAt: old},
		// T1, safe, pending.
		{Repo: "acme/a", Number: 4, Title: "refactor: drop dead code", HeadSHA: "s4", CIStatus: "pending", CreatedAt: old},
		// T1, needs attention (reviewed 2/5), green.
		{Repo: "acme/a", Number: 5, Title: "docs: rename a page", HeadSHA: "s5", CIStatus: "success", CreatedAt: old},
		// T1, unreviewed (needs attention), green, same age as #5 -> repo/number tiebreak.
		{Repo: "acme/a", Number: 6, Title: "docs: fix a typo in the guide", HeadSHA: "s6", CIStatus: "success", CreatedAt: old},
		// T1, do not merge, green.
		{Repo: "acme/a", Number: 7, Title: "refactor: risky rewrite", HeadSHA: "s7", CIStatus: "success", CreatedAt: old},
		// T0 unreviewed red: still above every T1.
		{Repo: "acme/b", Number: 8, Title: "fix: crash", HeadSHA: "s8", CIStatus: "failure", CreatedAt: newer},
		// T2 safe green old: below every T1.
		{Repo: "acme/b", Number: 9, Title: "test: coverage", HeadSHA: "s9", CIStatus: "success", CreatedAt: old},
	}
	verdicts := review.Artifact{Items: []review.Aggregate{
		queueVerdict("acme/a", 1, "s1", 5),
		queueVerdict("acme/a", 2, "s2", 4),
		queueVerdict("acme/a", 3, "s3", 5),
		queueVerdict("acme/a", 4, "s4", 5),
		queueVerdict("acme/a", 5, "s5", 2, "1 high finding"),
		queueVerdict("acme/a", 7, "s7", 0, "reject verdict"),
		queueVerdict("acme/b", 9, "s9", 5),
	}}
	opts := ReviewQueueOptions{Now: reviewQueueTestNow, Verdicts: verdicts}
	want := []string{
		"acme/b#8",
		"acme/a#2", "acme/a#1", "acme/a#4", "acme/a#3",
		"acme/a#5", "acme/a#6",
		"acme/a#7",
		"acme/b#9",
	}
	if got := queueKeys(BuildReviewQueue(prs, opts)); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v\nwant    %v", got, want)
	}
	// Reverse and rotate the input: the queue must not depend on it.
	reversed := make([]PullRequest, len(prs))
	for i := range prs {
		reversed[len(prs)-1-i] = prs[i]
	}
	rotated := append(append([]PullRequest{}, prs[4:]...), prs[:4]...)
	for name, in := range map[string][]PullRequest{"reversed": reversed, "rotated": rotated} {
		if got := queueKeys(BuildReviewQueue(in, opts)); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s input: order = %v, want %v", name, got, want)
		}
	}
	// Two runs over identical input produce identical entries, reasons included.
	a, b := BuildReviewQueue(prs, opts), BuildReviewQueue(prs, opts)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two runs over the same input differ")
	}
}

// An unreviewed PR has no score and must never be presented as safe: not
// when it has no verdict, not when its verdict is for an older head, and not
// when its head is unknown.
func TestBuildReviewQueue_UnreviewedIsNeverSafe(t *testing.T) {
	opened := reviewQueueTestNow.Add(-time.Hour)
	prs := []PullRequest{
		{Repo: "acme/a", Number: 1, Title: "docs: a", HeadSHA: "new", CIStatus: "success", CreatedAt: opened},
		{Repo: "acme/a", Number: 2, Title: "docs: b", HeadSHA: "", CIStatus: "success", CreatedAt: opened},
		{Repo: "acme/a", Number: 3, Title: "docs: c", HeadSHA: "c3", CIStatus: "success", CreatedAt: opened},
		{Repo: "acme/a", Number: 4, Title: "docs: d", HeadSHA: "d4", CIStatus: "success", CreatedAt: opened.Add(time.Minute)},
	}
	q := BuildReviewQueue(prs, ReviewQueueOptions{
		Now: reviewQueueTestNow,
		Verdicts: review.Artifact{Items: []review.Aggregate{
			queueVerdict("acme/a", 1, "old", review.ConfidenceMax), // stale head
			queueVerdict("acme/a", 2, "", review.ConfidenceMax),    // head unknown
			queueVerdict("acme/a", 4, "d4", review.ConfidenceMax),  // genuinely reviewed
		}},
	})
	for _, n := range []int{1, 2, 3} {
		e := findEntry(t, q, "acme/a", n)
		if e.Reviewed || e.ConfidenceScore != nil {
			t.Fatalf("#%d reads as reviewed: %+v", n, e)
		}
		if e.ConfidenceBand == ReviewQueueBandSafe || e.ConfidenceBand != ReviewQueueBandNeedsAttention {
			t.Fatalf("#%d band = %q, want %q", n, e.ConfidenceBand, ReviewQueueBandNeedsAttention)
		}
		if !hasReason(e, "not reviewed at this head") {
			t.Fatalf("#%d reasons = %v", n, e.Reasons)
		}
	}
	reviewed := findEntry(t, q, "acme/a", 4)
	if !reviewed.Reviewed || reviewed.ConfidenceScore == nil || *reviewed.ConfidenceScore != review.ConfidenceMax || reviewed.ConfidenceBand != ReviewQueueBandSafe {
		t.Fatalf("reviewed entry = %+v", reviewed)
	}
	// The safe PR outranks every unreviewed one even though it is newer.
	if q[0].Number != 4 {
		t.Fatalf("order = %v, want the reviewed-safe PR first", queueKeys(q))
	}
}

func TestBuildReviewQueue_ReasonsExplainEachKey(t *testing.T) {
	prs := []PullRequest{
		{
			Repo: "app", Number: 1, Title: "tidy the proxy", Author: "bob", HeadSHA: "h1",
			CIStatus: "failure", FailingChecks: []string{"build", "lint"}, MergeableState: "dirty",
			CreatedAt: reviewQueueTestNow.Add(-5 * 24 * time.Hour),
		},
		{
			Repo: "acme/app", Number: 2, Title: "handle nil forge", Author: "hive-bot",
			Labels: []string{"agent/scanner"}, HeadSHA: "h2", CIStatus: "failure",
			CreatedAt: reviewQueueTestNow.Add(-30 * time.Hour),
		},
		{Repo: "acme/app", Number: 3, Title: "test: x", Author: "carol", HeadSHA: "h3"},
	}
	q := BuildReviewQueue(prs, ReviewQueueOptions{
		Org: "acme", AIAuthor: "hive-bot", Now: reviewQueueTestNow,
		Held: map[string]bool{ReviewQueueKey("acme/app", 1): true},
		// Recorded under GitHub's full name; the PR list carries the bare repo.
		Verdicts: review.Artifact{Items: []review.Aggregate{queueVerdict("acme/app", 1, "h1", 3, "1 perspective requested changes")}},
	})

	unclassified := findEntry(t, q, "acme/app", 1)
	for _, want := range []string{
		"T1 unclassified (no title prefix, label or path signal",
		"confidence 3/5 (needs attention): 1 perspective requested changes",
		"CI red: build, lint",
		"open 5d",
		"contributor PR",
		"on hold",
		"has merge conflicts",
	} {
		if !hasReason(unclassified, want) {
			t.Fatalf("entry #1 missing reason %q: %v", want, unclassified.Reasons)
		}
	}
	if !unclassified.Held || unclassified.AgeHours != 5*24 {
		t.Fatalf("entry #1 = %+v", unclassified)
	}

	agent := findEntry(t, q, "acme/app", 2)
	if !agent.HiveAuthored || agent.Class != ReviewClassFix {
		t.Fatalf("agent entry = %+v", agent)
	}
	for _, want := range []string{"T0 fix (from label)", "CI red", "open 30h"} {
		if !hasReason(agent, want) {
			t.Fatalf("agent entry missing reason %q: %v", want, agent.Reasons)
		}
	}

	noAge := findEntry(t, q, "acme/app", 3)
	for _, want := range []string{"T2 tests (from title prefix)", "CI pending", "age unknown"} {
		if !hasReason(noAge, want) {
			t.Fatalf("entry #3 missing reason %q: %v", want, noAge.Reasons)
		}
	}
	if noAge.AgeHours != 0 || noAge.CIState != ReviewQueueCIPending {
		t.Fatalf("entry #3 = %+v", noAge)
	}
}

func TestBuildReviewQueue_SkipsInvalidAndDedupes(t *testing.T) {
	prs := []PullRequest{
		{Repo: "", Number: 1, Title: "fix: no repo"},
		{Repo: "acme/a", Number: 0, Title: "fix: no number"},
		{Repo: "acme/a", Number: 5, Title: "fix: first"},
		{Repo: "ACME/A", Number: 5, Title: "fix: duplicate"},
	}
	q := BuildReviewQueue(prs, ReviewQueueOptions{})
	if len(q) != 1 || q[0].Title != "fix: first" {
		t.Fatalf("queue = %+v, want the first occurrence only", q)
	}
	if prs[2].ReviewRank != 0 {
		t.Fatal("BuildReviewQueue mutated its input")
	}
}

func TestBuildReviewQueue_ContributorPathFallback(t *testing.T) {
	calls := 0
	paths := func(fullRepo string, number int, head string) []string {
		calls++
		if fullRepo != "acme/app" || head != "h" {
			t.Fatalf("paths asked for %s#%d@%s", fullRepo, number, head)
		}
		return map[int][]string{
			1: {"pkg/x/x_test.go", "web/src/x.test.ts"},
			2: {"README.md", "docs/guide.md"},
			3: {"pkg/x/x_test.go"},
		}[number]
	}
	prs := []PullRequest{
		{Repo: "app", Number: 1, Title: "more cases", HeadSHA: "h"},
		{Repo: "app", Number: 2, Title: "clarify setup", HeadSHA: "h"},
		// An agent PR on a custom lane keeps its (unknown) class: paths are
		// never consulted for PRs with an agent lane label.
		{Repo: "app", Number: 3, Title: "more cases", HeadSHA: "h", Labels: []string{"agent/custom"}},
	}
	q := BuildReviewQueue(prs, ReviewQueueOptions{Org: "acme", ChangedPaths: paths, Now: reviewQueueTestNow})
	if e := findEntry(t, q, "acme/app", 1); e.Class != ReviewClassTests || !hasReason(e, "T2 tests (from changed paths)") {
		t.Fatalf("test-only contributor PR = %+v", e)
	}
	if e := findEntry(t, q, "acme/app", 2); e.Class != ReviewClassRefactorDocs || !hasReason(e, "from changed paths") {
		t.Fatalf("docs-only contributor PR = %+v", e)
	}
	if e := findEntry(t, q, "acme/app", 3); e.Class != ReviewClassUnknown {
		t.Fatalf("agent PR class changed by paths: %+v", e)
	}
	if calls != 2 {
		t.Fatalf("ChangedPaths called %d times, want 2 (never for agent-lane PRs)", calls)
	}
}

func TestClassifyContributorReviewClass(t *testing.T) {
	tests := []string{"pkg/a/a_test.go"}
	cases := []struct {
		name   string
		title  string
		labels []string
		paths  []string
		want   ReviewClass
	}{
		{"title wins over paths", "fix: crash", nil, tests, ReviewClassFix},
		{"label wins over paths", "crash", []string{"kind/bug"}, tests, ReviewClassFix},
		{"paths when silent", "crash", nil, tests, ReviewClassTests},
		{"agent lane ignores paths", "crash", []string{"agent/custom"}, tests, ReviewClassUnknown},
		{"no signal at all", "crash", nil, nil, ReviewClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyContributorReviewClass(tc.title, tc.labels, tc.paths); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	// Agent PRs keep exactly the class ClassifyReviewClass gives them.
	for _, title := range []string{"[quality] test: x", "[scanner] handle nil", "refactor: y", "✨ feature: z"} {
		labels := []string{"agent/quality"}
		if got, want := ClassifyContributorReviewClass(title, labels, []string{"docs/a.md"}), ClassifyReviewClass(title, labels); got != want {
			t.Fatalf("%q: contributor path changed an agent PR's class: %q vs %q", title, got, want)
		}
	}
}

func TestClassifyReviewClassFromPaths(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		want  ReviewClass
	}{
		{"empty", nil, ReviewClassUnknown},
		{"blank only", []string{" ", ""}, ReviewClassUnknown},
		{"go tests", []string{"src/pkg/a/a_test.go", "src/pkg/b/b_test.go"}, ReviewClassTests},
		{"js spec and test infix", []string{"web/a.spec.ts", "web/b.test.js"}, ReviewClassTests},
		{"python prefix", []string{"tests_helpers/test_client.py"}, ReviewClassTests},
		{"test dirs", []string{"e2e/login.ts", "internal/testutil/eventually.go", "pkg/x/testdata/golden.json", "src/__tests__/a.js"}, ReviewClassTests},
		{"docs by extension", []string{"README.md", "CONTRIBUTING.rst", "guide.adoc", "page.mdx"}, ReviewClassRefactorDocs},
		{"docs dir", []string{"src/docs/diagram.png", "doc/logo.svg"}, ReviewClassRefactorDocs},
		{"mixed code and tests", []string{"pkg/a/a.go", "pkg/a/a_test.go"}, ReviewClassUnknown},
		{"mixed docs and tests", []string{"docs/a.md", "pkg/a/a_test.go"}, ReviewClassUnknown},
		{"code only is never a fix", []string{"pkg/a/a.go"}, ReviewClassUnknown},
		{"case-insensitive", []string{"PKG/A/A_TEST.GO"}, ReviewClassTests},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyReviewClassFromPaths(tc.paths); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReviewQueueHiveAuthored(t *testing.T) {
	cases := []struct {
		pr   PullRequest
		ai   string
		want bool
	}{
		{PullRequest{Author: "alice"}, "hive-bot", false},
		{PullRequest{Author: ""}, "hive-bot", false},
		{PullRequest{Author: "Hive-Bot"}, "hive-bot", true},
		{PullRequest{Author: "renovate[bot]"}, "", true},
		{PullRequest{Author: "alice", HiveAttributed: true}, "", true},
		{PullRequest{Author: "alice", AppAuthored: true}, "", true},
		{PullRequest{Author: "alice", Labels: []string{"Agent/Guide"}}, "", true},
		{PullRequest{Author: "alice"}, "", false},
	}
	for i, tc := range cases {
		if got := reviewQueueHiveAuthored(tc.pr, tc.ai); got != tc.want {
			t.Fatalf("case %d (%+v, ai=%q): got %v, want %v", i, tc.pr, tc.ai, got, tc.want)
		}
	}
}

// The mechanical label must match the rank: after applying the planned
// changes every PR carries exactly one review-priority/* label, it is the
// one for its priority, and priorities never rise going down the queue.
func TestPlanReviewPriorityLabels_LabelMatchesRank(t *testing.T) {
	opened := reviewQueueTestNow.Add(-time.Hour)
	prs := []PullRequest{
		{Repo: "acme/a", Number: 1, Title: "test: coverage", Labels: []string{"review-priority/high"}, CreatedAt: opened},
		{Repo: "acme/a", Number: 2, Title: "fix: crash", Labels: []string{"review-priority/high"}, CreatedAt: opened},
		{Repo: "acme/a", Number: 3, Title: "docs: page", Labels: []string{"review-priority/low", "Review-Priority/High", "hold"}, CreatedAt: opened},
		{Repo: "acme/a", Number: 4, Title: "✨ feature: card", CreatedAt: opened},
	}
	q := BuildReviewQueue(prs, ReviewQueueOptions{Now: reviewQueueTestNow})
	changes := PlanReviewPriorityLabels(q, 0)

	byKey := map[string]ReviewPriorityLabelChange{}
	for _, ch := range changes {
		byKey[ReviewQueueKey(ch.Repo, ch.Number)] = ch
	}
	if _, ok := byKey["acme/a#2"]; ok {
		t.Fatalf("a PR already carrying its label was planned: %+v", changes)
	}
	if ch := byKey["acme/a#1"]; ch.Add != "review-priority/low" || !reflect.DeepEqual(ch.Remove, []string{"review-priority/high"}) {
		t.Fatalf("#1 change = %+v", ch)
	}
	if ch := byKey["acme/a#3"]; ch.Add != "review-priority/normal" || len(ch.Remove) != 2 {
		t.Fatalf("#3 change = %+v", ch)
	}
	if ch := byKey["acme/a#4"]; ch.Add != "review-priority/normal" || len(ch.Remove) != 0 {
		t.Fatalf("#4 change = %+v", ch)
	}

	// Apply the plan and check the invariant on the result.
	priorityOrder := map[ReviewPriority]int{ReviewPriorityHigh: 0, ReviewPriorityNormal: 1, ReviewPriorityLow: 2}
	last := -1
	for _, e := range q {
		labels := map[string]bool{}
		for _, l := range e.Labels {
			labels[strings.ToLower(l)] = true
		}
		if ch, ok := byKey[ReviewQueueKey(e.Repo, e.Number)]; ok {
			for _, r := range ch.Remove {
				delete(labels, strings.ToLower(r))
			}
			if ch.Add != "" {
				labels[strings.ToLower(ch.Add)] = true
			}
		}
		var prio []string
		for l := range labels {
			if strings.HasPrefix(l, ReviewPriorityLabelPrefix) {
				prio = append(prio, l)
			}
		}
		if len(prio) != 1 || prio[0] != e.Priority.Label() {
			t.Fatalf("%s#%d ends with %v, want exactly %q", e.Repo, e.Number, prio, e.Priority.Label())
		}
		if priorityOrder[e.Priority] < last {
			t.Fatalf("priority rises going down the queue at %s#%d", e.Repo, e.Number)
		}
		last = priorityOrder[e.Priority]
	}
	// Idempotent: a queue already in step plans nothing.
	for i := range q {
		q[i].Labels = []string{q[i].Priority.Label()}
	}
	if again := PlanReviewPriorityLabels(q, 0); len(again) != 0 {
		t.Fatalf("second plan = %+v, want none", again)
	}
}

func TestPlanReviewPriorityLabels_CapKeepsTopOfQueue(t *testing.T) {
	var q []ReviewQueueEntry
	for i := 1; i <= DefaultReviewPriorityLabelMaxChanges+5; i++ {
		q = append(q, ReviewQueueEntry{Repo: "acme/a", Number: i, Priority: ReviewPriorityNormal})
	}
	if got := PlanReviewPriorityLabels(q, 0); len(got) != DefaultReviewPriorityLabelMaxChanges {
		t.Fatalf("default cap: %d changes", len(got))
	}
	got := PlanReviewPriorityLabels(q, 2)
	if len(got) != 2 || got[0].Number != 1 || got[1].Number != 2 {
		t.Fatalf("cap 2 = %+v, want the first two in queue order", got)
	}
}

func TestReviewPriorityMapping(t *testing.T) {
	cases := map[ReviewClass]ReviewPriority{
		ReviewClassFix:          ReviewPriorityHigh,
		ReviewClassRefactorDocs: ReviewPriorityNormal,
		ReviewClassUnknown:      ReviewPriorityNormal,
		ReviewClassTests:        ReviewPriorityLow,
	}
	for class, want := range cases {
		if got := reviewPriorityFor(class); got != want {
			t.Fatalf("%q -> %q, want %q", class, got, want)
		}
	}
	if ReviewPriorityHigh.Label() != "review-priority/high" || ReviewPriorityLow.Label() != "review-priority/low" {
		t.Fatal("label names changed")
	}
}

func TestStampReviewQueue(t *testing.T) {
	if StampReviewQueue(nil, ReviewQueueOptions{}) != nil {
		t.Fatal("nil actionable must stamp nothing")
	}
	if q := StampReviewQueue(&ActionableResult{}, ReviewQueueOptions{}); len(q) != 0 {
		t.Fatalf("empty actionable queue = %+v", q)
	}
	opened := reviewQueueTestNow.Add(-time.Hour)
	actionable := &ActionableResult{PRs: PRResult{
		Items: []PullRequest{
			{Repo: "a", Number: 1, Title: "test: x", CreatedAt: opened},
			{Repo: "a", Number: 2, Title: "docs: y", CreatedAt: opened},
		},
		Held: []PullRequest{{Repo: "a", Number: 3, Title: "fix: z", CreatedAt: opened, Labels: []string{"hold"}}},
	}}
	q := StampReviewQueue(actionable, ReviewQueueOptions{Org: "acme", Now: reviewQueueTestNow})
	if got := queueKeys(q); !reflect.DeepEqual(got, []string{"acme/a#3", "acme/a#2", "acme/a#1"}) {
		t.Fatalf("queue = %v", got)
	}
	if !q[0].Held || q[1].Held {
		t.Fatalf("held flags = %v, %v", q[0].Held, q[1].Held)
	}
	// List order is untouched; only the stamp is added.
	items := actionable.PRs.Items
	if items[0].Number != 1 || items[1].Number != 2 {
		t.Fatalf("Items reordered: %v, %v", items[0].Number, items[1].Number)
	}
	if items[0].ReviewRank != 3 || items[0].ReviewPriority != ReviewPriorityLow || len(items[0].ReviewRankReasons) == 0 {
		t.Fatalf("Items[0] stamp = %d %q %v", items[0].ReviewRank, items[0].ReviewPriority, items[0].ReviewRankReasons)
	}
	if items[1].ReviewRank != 2 || items[1].ReviewPriority != ReviewPriorityNormal {
		t.Fatalf("Items[1] stamp = %d %q", items[1].ReviewRank, items[1].ReviewPriority)
	}
	held := actionable.PRs.Held[0]
	if held.ReviewRank != 1 || held.ReviewPriority != ReviewPriorityHigh || !reflect.DeepEqual(held.ReviewRankReasons, q[0].Reasons) {
		t.Fatalf("Held stamp = %d %q %v", held.ReviewRank, held.ReviewPriority, held.ReviewRankReasons)
	}
}

func TestReviewQueueFromActionable_MergesCallerHeld(t *testing.T) {
	if ReviewQueueFromActionable(nil, ReviewQueueOptions{}) != nil {
		t.Fatal("nil actionable must yield nil")
	}
	actionable := &ActionableResult{PRs: PRResult{Items: []PullRequest{{Repo: "acme/a", Number: 1, Title: "fix: x"}}}}
	q := ReviewQueueFromActionable(actionable, ReviewQueueOptions{Held: map[string]bool{"acme/a#1": true}})
	if len(q) != 1 || !q[0].Held {
		t.Fatalf("queue = %+v, want the caller's held flag kept", q)
	}
}

func TestCachedPRChangedPaths(t *testing.T) {
	dupSweepCacheMu.Lock()
	saved := dupSweepCache
	dupSweepCache = map[string]prFingerprint{
		"acme/app#7@abc": {files: []string{"a_test.go", "b_test.go"}},
	}
	dupSweepCacheMu.Unlock()
	t.Cleanup(func() {
		dupSweepCacheMu.Lock()
		dupSweepCache = saved
		dupSweepCacheMu.Unlock()
	})

	got := CachedPRChangedPaths("acme/app", 7, "abc")
	if !reflect.DeepEqual(got, []string{"a_test.go", "b_test.go"}) {
		t.Fatalf("cached paths = %v", got)
	}
	got[0] = "mutated"
	if again := CachedPRChangedPaths("acme/app", 7, "abc"); again[0] != "a_test.go" {
		t.Fatal("caller could mutate the cache through the returned slice")
	}
	for _, tc := range []struct {
		repo string
		num  int
		sha  string
	}{
		{"acme/app", 7, "other"}, // new head: not fingerprinted yet
		{"acme/app", 7, ""},
		{"app", 7, "abc"},
	} {
		if got := CachedPRChangedPaths(tc.repo, tc.num, tc.sha); got != nil {
			t.Fatalf("%+v -> %v, want nil", tc, got)
		}
	}
}
