package recommend

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

var refTime = time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)

func pr(n int, mut func(*github.PullRequest)) github.PullRequest {
	p := github.PullRequest{
		Repo:           "projectbluefin/common",
		Number:         n,
		Title:          "some change",
		Author:         "contributor",
		URL:            "https://github.com/projectbluefin/common/pull/" + itoa(n),
		CreatedAt:      refTime.Add(-10 * 24 * time.Hour),
		Mergeable:      github.MergeableYes,
		MergeableState: "clean",
		CIStatus:       "success",
		BaseRef:        "main",
	}
	if mut != nil {
		mut(&p)
	}
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func buildOne(t *testing.T, p github.PullRequest) Report {
	t.Helper()
	return Build("projectbluefin/common", []github.PullRequest{p}, Options{Now: refTime, HumanDecisionLabel: "3-human-queue"})
}

func bucketOf(t *testing.T, p github.PullRequest) Bucket {
	t.Helper()
	rep := buildOne(t, p)
	for b, items := range rep.Buckets {
		if len(items) > 0 {
			return b
		}
	}
	return ""
}

func TestBucketing(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*github.PullRequest)
		want Bucket
	}{
		{"clean and green is ready", nil, BucketReady},
		{"dirty needs a rebase", func(p *github.PullRequest) {
			p.MergeableState = "dirty"
			p.Mergeable = github.MergeableNo
		}, BucketConflicts},
		{"failing required check is red ci", func(p *github.PullRequest) {
			p.CIStatus = "failure"
			p.FailingChecks = []string{"build"}
		}, BucketRedCI},
		{"human decision label wins over everything", func(p *github.PullRequest) {
			p.Labels = []string{"3-human-queue"}
			p.MergeableState = "dirty"
		}, BucketDecision},
		{"old draft is stale", func(p *github.PullRequest) {
			p.Draft = true
			p.CreatedAt = refTime.Add(-60 * 24 * time.Hour)
		}, BucketStaleDraft},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bucketOf(t, pr(1, tt.mut)); got != tt.want {
				t.Fatalf("bucket = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFreshDraftIsCountedButNotListed(t *testing.T) {
	// A draft opened last week is someone's work in progress. Listing it
	// would be nagging; omitting it from the total would misstate the queue.
	rep := buildOne(t, pr(1, func(p *github.PullRequest) {
		p.Draft = true
		p.CreatedAt = refTime.Add(-3 * 24 * time.Hour)
	}))
	if rep.TotalOpen != 1 {
		t.Fatalf("TotalOpen = %d, want 1", rep.TotalOpen)
	}
	for b, items := range rep.Buckets {
		if len(items) > 0 {
			t.Fatalf("fresh draft should not be listed, found in %q", b)
		}
	}
}

func TestPendingCIIsNotCalledReady(t *testing.T) {
	// The digest's credibility rests on "ready" meaning ready. A PR whose
	// checks have not finished is not something to recommend merging.
	if got := bucketOf(t, pr(1, func(p *github.PullRequest) { p.CIStatus = "pending" })); got == BucketReady {
		t.Fatal("a PR with pending CI must not be listed as ready to merge")
	}
}

func TestUnstableIsNotCalledReady(t *testing.T) {
	// "unstable" passes the merge gate by policy (non-required checks are
	// ignored), but a human clicking "ready to merge" and finding red checks
	// stops trusting the digest.
	if got := bucketOf(t, pr(1, func(p *github.PullRequest) { p.MergeableState = "unstable" })); got == BucketReady {
		t.Fatal("an unstable PR must not be listed as ready to merge")
	}
}

func TestConflictBeatsRedCI(t *testing.T) {
	// Telling someone to fix CI on a branch that cannot merge wastes their
	// time; the rebase has to happen first.
	got := bucketOf(t, pr(1, func(p *github.PullRequest) {
		p.MergeableState = "dirty"
		p.Mergeable = github.MergeableNo
		p.CIStatus = "failure"
		p.FailingChecks = []string{"build"}
	}))
	if got != BucketConflicts {
		t.Fatalf("bucket = %q, want %q", got, BucketConflicts)
	}
}

func TestForkConflictSaysWhoCanFixIt(t *testing.T) {
	rep := buildOne(t, pr(1, func(p *github.PullRequest) {
		p.MergeableState = "dirty"
		p.Mergeable = github.MergeableNo
		p.FromFork = true
	}))
	note := rep.Buckets[BucketConflicts][0].Note
	if !strings.Contains(note, "only the author can rebase") {
		t.Fatalf("fork conflict note must say the hive cannot rebase it, got %q", note)
	}
}

func TestMarkdownNeverMentionsAnyone(t *testing.T) {
	// The body is rewritten every cycle. A single @handle would re-notify
	// that person on every refresh -- the exact behaviour that makes a
	// community mute an automation.
	rep := Build("projectbluefin/common", []github.PullRequest{
		pr(1, func(p *github.PullRequest) { p.Author = "castrojo" }),
		pr(2, func(p *github.PullRequest) { p.Author = "someone[bot]" }),
		pr(3, func(p *github.PullRequest) { p.Title = "ping @everyone about this" }),
	}, Options{Now: refTime})

	md := rep.Markdown()
	if strings.Contains(md, "@castrojo") {
		t.Fatal("digest must not @-mention a PR author")
	}
	if !strings.Contains(md, "`castrojo`") {
		t.Fatal("author should still be identified, just not as a mention")
	}
	if strings.Contains(md, "someone[bot]") {
		t.Fatal("bot authors add noise and should be omitted")
	}
}

func TestMarkdownLeadsWithTheAnswer(t *testing.T) {
	rep := Build("projectbluefin/common", []github.PullRequest{pr(1, nil), pr(2, nil)}, Options{Now: refTime})
	md := rep.Markdown()
	first := firstMeaningfulLine(md)
	if !strings.Contains(first, "ready to merge right now") {
		t.Fatalf("digest must open with the count of mergeable PRs, got %q", first)
	}
	if !strings.Contains(md, "**Start here:**") {
		t.Fatal("digest must name one concrete next action")
	}
}

func TestMarkdownCarriesTheMarker(t *testing.T) {
	// Without the marker the poster cannot find the issue again and would
	// open a new one every cycle.
	md := Build("projectbluefin/common", nil, Options{Now: refTime}).Markdown()
	if !strings.HasPrefix(md, Marker) {
		t.Fatalf("body must start with the lookup marker, got %q", firstMeaningfulLine(md))
	}
}

func TestEmptyQueueSaysSo(t *testing.T) {
	md := Build("projectbluefin/common", nil, Options{Now: refTime}).Markdown()
	if !strings.Contains(md, "The queue is empty") {
		t.Fatal("an empty queue should be stated plainly, not rendered as a blank digest")
	}
}

func TestBatchBlockListsEveryPRExplicitly(t *testing.T) {
	// A query-driven mass merge could act on something the reader never saw.
	// Naming each PR keeps the command bounded by what is on the page.
	rep := Build("projectbluefin/common", []github.PullRequest{pr(11, nil), pr(12, nil)}, Options{Now: refTime})
	md := rep.Markdown()
	for _, want := range []string{"gh pr merge 11 --repo projectbluefin/common", "gh pr merge 12 --repo projectbluefin/common"} {
		if !strings.Contains(md, want) {
			t.Fatalf("batch block missing explicit command %q", want)
		}
	}
	if !strings.Contains(md, "delete any line you do not want") {
		t.Fatal("batch block must tell the reader they can edit it first")
	}
}

func TestMarkdownStatesTheHiveDoesNotMerge(t *testing.T) {
	// This hive has no merge authority and the community has not asked for
	// it. A page full of merge commands has to say so explicitly.
	md := Build("projectbluefin/common", []github.PullRequest{pr(1, nil)}, Options{Now: refTime}).Markdown()
	if !strings.Contains(md, "does not merge, approve, or close anything") {
		t.Fatal("digest must state that the hive takes no action itself")
	}
}

func TestSectionsLinkAVerifiableQuery(t *testing.T) {
	rep := Build("projectbluefin/common", []github.PullRequest{pr(1, nil)}, Options{Now: refTime})
	md := rep.Markdown()
	if !strings.Contains(md, "https://github.com/projectbluefin/common/pulls?q=") {
		t.Fatal("each section must link the GitHub query that reproduces it")
	}
}

func TestTitleCannotBreakTheList(t *testing.T) {
	rep := buildOne(t, pr(1, func(p *github.PullRequest) {
		p.Title = "fix: thing\nwith a newline and [brackets]"
	}))
	md := rep.Markdown()
	if strings.Contains(md, "with a newline and [brackets]") {
		t.Fatal("author-supplied title must be sanitized before rendering")
	}
	if strings.Count(firstItemLine(md), "\n") > 0 {
		t.Fatal("a title newline must not split the bullet")
	}
}

func TestCapKeepsSectionsSkimmable(t *testing.T) {
	var prs []github.PullRequest
	for i := 1; i <= 25; i++ {
		prs = append(prs, pr(i, nil))
	}
	rep := Build("projectbluefin/common", prs, Options{Now: refTime, MaxPerBucket: 5})
	md := rep.Markdown()
	if !strings.Contains(md, "…and 20 more.") {
		t.Fatal("a capped section must say how many it did not show")
	}
	if rep.TotalOpen != 25 {
		t.Fatalf("TotalOpen = %d, want 25 — the digest must never understate the queue", rep.TotalOpen)
	}
}

func TestOldestFixRanksFirst(t *testing.T) {
	prs := []github.PullRequest{
		pr(1, func(p *github.PullRequest) {
			p.Title = "docs: tweak readme"
			p.ReviewClass = github.ReviewClassRefactorDocs
			p.CreatedAt = refTime.Add(-90 * 24 * time.Hour)
		}),
		pr(2, func(p *github.PullRequest) {
			p.Title = "fix: crash on startup"
			p.ReviewClass = github.ReviewClassFix
			p.CreatedAt = refTime.Add(-5 * 24 * time.Hour)
		}),
	}
	rep := Build("projectbluefin/common", prs, Options{Now: refTime})
	if got := rep.Buckets[BucketReady][0].Number; got != 2 {
		t.Fatalf("first ready item = #%d, want #2 (a fix outranks an older docs change)", got)
	}
}

func firstMeaningfulLine(md string) string {
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "<!--") {
			continue
		}
		return line
	}
	return ""
}

func firstItemLine(md string) string {
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "- [#") {
			return line
		}
	}
	return ""
}
