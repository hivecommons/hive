package recommend

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestBatchBlock(t *testing.T) {
	items := []Item{{Number: 1}, {Number: 2}, {Number: 3}}

	t.Run("empty repo yields nothing", func(t *testing.T) {
		if got := batchBlock("", BucketReady, items, 0); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("no items yields nothing", func(t *testing.T) {
		if got := batchBlock("o/r", BucketReady, nil, 0); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("unhandled bucket yields nothing", func(t *testing.T) {
		if got := batchBlock("o/r", BucketRedCI, items, 0); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("ready renders merge commands", func(t *testing.T) {
		got := batchBlock("o/r", BucketReady, items, 0)
		if !strings.Contains(got, "gh pr merge 1 --repo o/r --squash") ||
			!strings.Contains(got, "gh pr merge 3 --repo o/r --squash") {
			t.Fatalf("missing merge commands: %q", got)
		}
		if !strings.Contains(got, batchSummary[BucketReady]) || !strings.Contains(got, batchCaution[BucketReady]) {
			t.Fatalf("missing summary/caution: %q", got)
		}
	})
	t.Run("stale drafts render close commands", func(t *testing.T) {
		got := batchBlock("o/r", BucketStaleDraft, items, 0)
		if !strings.Contains(got, "gh pr close 2 --repo o/r --comment") ||
			!strings.Contains(got, staleDraftCloseNote) {
			t.Fatalf("missing close command: %q", got)
		}
	})
	t.Run("conflicts render read-only inspection", func(t *testing.T) {
		got := batchBlock("o/r", BucketConflicts, items, 0)
		if !strings.Contains(got, "gh pr view 1 --repo o/r --json") {
			t.Fatalf("missing view command: %q", got)
		}
		if strings.Contains(got, "merge") || strings.Contains(got, "close") {
			t.Fatalf("conflicts block must be read-only: %q", got)
		}
	})
	t.Run("cap trims the command list", func(t *testing.T) {
		got := batchBlock("o/r", BucketReady, items, 2)
		if strings.Contains(got, "gh pr merge 3") {
			t.Fatalf("cap 2 should drop the third item: %q", got)
		}
		if !strings.Contains(got, "gh pr merge 2") {
			t.Fatalf("cap 2 should keep the second item: %q", got)
		}
	})
}

func TestBucketQueryURL(t *testing.T) {
	rep := Report{Repo: "o/r", HumanDecisionLabel: "3-human-queue"}

	t.Run("empty repo yields nothing", func(t *testing.T) {
		if got := (Report{}).bucketQueryURL(BucketReady); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("unknown bucket yields nothing", func(t *testing.T) {
		if got := rep.bucketQueryURL(Bucket("nope")); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("decision without label yields nothing", func(t *testing.T) {
		r := Report{Repo: "o/r"}
		if got := r.bucketQueryURL(BucketDecision); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	wantContains := map[Bucket]string{
		BucketReady:      "status%3Asuccess",
		BucketConflicts:  "draft%3Afalse",
		BucketRedCI:      "status%3Afailure",
		BucketDecision:   "label",
		BucketStaleDraft: "draft%3Atrue",
	}
	for b, frag := range wantContains {
		got := rep.bucketQueryURL(b)
		if !strings.HasPrefix(got, "https://github.com/o/r/pulls?q=") {
			t.Fatalf("bucket %q: bad prefix %q", b, got)
		}
		if !strings.Contains(got, frag) {
			t.Fatalf("bucket %q: want fragment %q in %q", b, frag, got)
		}
	}
}

func TestOptionsDefaults(t *testing.T) {
	var o Options
	if got := o.staleDraftDays(); got != DefaultStaleDraftDays {
		t.Fatalf("staleDraftDays default = %d", got)
	}
	if got := (Options{StaleDraftDays: 7}).staleDraftDays(); got != 7 {
		t.Fatalf("staleDraftDays override = %d", got)
	}
	if got := o.maxPerBucket(); got != DefaultMaxPerBucket {
		t.Fatalf("maxPerBucket default = %d", got)
	}
	if o.now().IsZero() {
		t.Fatal("zero Options.Now must yield a real time")
	}
	if got := (Options{Now: refTime}).now(); !got.Equal(refTime) {
		t.Fatalf("now override = %v", got)
	}
}

func TestNewItemAgeClamping(t *testing.T) {
	future := pr(1, func(p *github.PullRequest) { p.CreatedAt = refTime.Add(24 * time.Hour) })
	if it := newItem(future, refTime); it.AgeDays != 0 {
		t.Fatalf("future CreatedAt should clamp to 0, got %d", it.AgeDays)
	}
	unset := pr(1, func(p *github.PullRequest) { p.CreatedAt = time.Time{} })
	if it := newItem(unset, refTime); it.AgeDays != 0 {
		t.Fatalf("zero CreatedAt should yield age 0, got %d", it.AgeDays)
	}
}

func TestIsReadyBranches(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*github.PullRequest)
		want bool
	}{
		{"clean green", nil, true},
		{"has_hooks counts as ready", func(p *github.PullRequest) { p.MergeableState = "has_hooks" }, true},
		{"draft is never ready", func(p *github.PullRequest) { p.Draft = true }, false},
		{"mergeable unknown is not ready", func(p *github.PullRequest) { p.Mergeable = github.MergeableUnknown }, false},
		{"pending ci is not ready", func(p *github.PullRequest) { p.CIStatus = "pending" }, false},
		{"failing ci is not ready", func(p *github.PullRequest) { p.CIStatus = "failure" }, false},
		{"blocked state is not ready", func(p *github.PullRequest) { p.MergeableState = "blocked" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReady(pr(1, tt.mut)); got != tt.want {
				t.Fatalf("isReady = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRebaseNote(t *testing.T) {
	p := pr(1, func(p *github.PullRequest) { p.BaseRef = "" })
	if got := rebaseNote(p); !strings.Contains(got, "the base branch") {
		t.Fatalf("empty base should name a placeholder: %q", got)
	}
	fork := pr(1, func(p *github.PullRequest) { p.FromFork = true })
	if got := rebaseNote(fork); !strings.Contains(got, "only the author can rebase") {
		t.Fatalf("fork note missing: %q", got)
	}
}

func TestFailingNote(t *testing.T) {
	if got := failingNote(pr(1, nil)); got != "a required check failed" {
		t.Fatalf("no checks: %q", got)
	}
	many := pr(1, func(p *github.PullRequest) {
		p.FailingChecks = []string{"d", "c", "b", "a", "e"}
	})
	got := failingNote(many)
	if !strings.Contains(got, "failing: a, b, c") || !strings.Contains(got, "(+2 more)") {
		t.Fatalf("overflow note: %q", got)
	}
}

func TestSortItemsTieBreakers(t *testing.T) {
	items := []Item{
		{Number: 4, Class: github.ReviewClassRefactorDocs, AgeDays: 90},
		{Number: 3, Class: github.ReviewClassFix, AgeDays: 2},
		{Number: 2, Class: github.ReviewClassFix, AgeDays: 5},
		{Number: 5, Class: github.ReviewClassTests, AgeDays: 5},
		{Number: 1, Class: github.ReviewClassFix, AgeDays: 5},
	}
	sortItems(items)
	want := []int{1, 2, 3, 5, 4}
	for i, n := range want {
		if items[i].Number != n {
			t.Fatalf("position %d = #%d, want #%d (%v)", i, items[i].Number, n, items)
		}
	}
}

func TestClassRank(t *testing.T) {
	order := []github.ReviewClass{github.ReviewClassFix, github.ReviewClassTests, github.ReviewClassRefactorDocs, github.ReviewClass("mystery")}
	for i := 1; i < len(order); i++ {
		if classRank(order[i-1]) >= classRank(order[i]) {
			t.Fatalf("rank(%q) should be < rank(%q)", order[i-1], order[i])
		}
	}
}

func TestHeadline(t *testing.T) {
	if got := (Report{}).headline(0); !strings.Contains(got, "queue is empty") {
		t.Fatalf("empty queue: %q", got)
	}
	if got := (Report{TotalOpen: 4}).headline(0); !strings.Contains(got, "Nothing is ready") {
		t.Fatalf("none ready: %q", got)
	}
	if got := (Report{TotalOpen: 4}).headline(1); !strings.Contains(got, "1 pull request is ready") {
		t.Fatalf("one ready: %q", got)
	}
	if got := (Report{TotalOpen: 4}).headline(3); !strings.Contains(got, "3 pull requests are ready") {
		t.Fatalf("many ready: %q", got)
	}
}

func TestSummaryTableUnclassified(t *testing.T) {
	rep := Report{Unclassified: 2, Buckets: map[Bucket][]Item{}}
	got := rep.summaryTable()
	if !strings.Contains(got, "Undetermined | 2") {
		t.Fatalf("unclassified row missing: %q", got)
	}
}

func TestRenderItems(t *testing.T) {
	items := []Item{
		{Number: 1, Title: "first", URL: "https://x/1", AgeDays: 3, Author: "alice", Note: "conflicts with main"},
		{Number: 2, Title: "second"},
		{Number: 3, Title: "third"},
	}
	t.Run("zero cap falls back to default", func(t *testing.T) {
		got := renderItems(items, 0)
		if !strings.Contains(got, "third") {
			t.Fatalf("default cap should show all three: %q", got)
		}
	})
	t.Run("overflow adds an and-more line", func(t *testing.T) {
		got := renderItems(items, 1)
		if !strings.Contains(got, "…and 2 more.") {
			t.Fatalf("overflow line missing: %q", got)
		}
	})
	t.Run("age author and note render", func(t *testing.T) {
		got := renderItems(items[:1], 5)
		for _, frag := range []string{"3 days old", "`alice`", "conflicts with main"} {
			if !strings.Contains(got, frag) {
				t.Fatalf("missing %q in %q", frag, got)
			}
		}
	})
}

func TestLink(t *testing.T) {
	if got := link(Item{Number: 7, Title: "t"}); got != "#7 t" {
		t.Fatalf("no URL: %q", got)
	}
	if got := link(Item{Number: 7, URL: "https://x/7"}); !strings.Contains(got, "[#7](https://x/7) #7") {
		t.Fatalf("empty title should fall back to number: %q", got)
	}
}

func TestSanitizeTitleTruncates(t *testing.T) {
	long := strings.Repeat("a", 200)
	got := sanitizeTitle(long)
	if !strings.HasSuffix(got, "…") || len(got) > 130 {
		t.Fatalf("long title not truncated: %d chars", len(got))
	}
}

func TestHumanDays(t *testing.T) {
	tests := []struct {
		days int
		want string
	}{
		{800, "2 years"},
		{400, "a year"},
		{90, "3 months"},
		{35, "a month"},
		{1, "1 day"},
		{5, "5 days"},
		{0, "0 days"},
	}
	for _, tt := range tests {
		if got := humanDays(tt.days); got != tt.want {
			t.Fatalf("humanDays(%d) = %q, want %q", tt.days, got, tt.want)
		}
	}
}

func TestStartHere(t *testing.T) {
	t.Run("empty report yields nothing", func(t *testing.T) {
		if got := (Report{Buckets: map[Bucket][]Item{}}).startHere(); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("decision outranks conflicts", func(t *testing.T) {
		rep := Report{Buckets: map[Bucket][]Item{
			BucketDecision:  {{Number: 9, Title: "d", AgeDays: 2}},
			BucketConflicts: {{Number: 8, Title: "c", Note: "conflicts with main"}},
		}}
		got := rep.startHere()
		if !strings.Contains(got, "waiting on your decision") || !strings.Contains(got, "open 2 days") {
			t.Fatalf("decision pick: %q", got)
		}
	})
	t.Run("conflicts pick carries the note", func(t *testing.T) {
		rep := Report{Buckets: map[Bucket][]Item{
			BucketConflicts: {{Number: 8, Title: "c", Note: "conflicts with main"}},
		}}
		if got := rep.startHere(); !strings.Contains(got, "conflicts with main") {
			t.Fatalf("conflicts pick: %q", got)
		}
	})
	t.Run("ready wins when present", func(t *testing.T) {
		rep := Report{Buckets: map[Bucket][]Item{
			BucketReady:    {{Number: 1, Title: "r"}},
			BucketDecision: {{Number: 9, Title: "d"}},
		}}
		if got := rep.startHere(); !strings.Contains(got, "ready to merge") {
			t.Fatalf("ready pick: %q", got)
		}
	})
}

func TestWaitedFor(t *testing.T) {
	if got := waitedFor(Item{AgeDays: 0}); got != "" {
		t.Fatalf("zero age: %q", got)
	}
	if got := waitedFor(Item{AgeDays: 40}); got != ", open a month" {
		t.Fatalf("40 days: %q", got)
	}
}
