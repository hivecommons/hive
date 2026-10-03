package github

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCollectReviewThreadsAllFiltered(t *testing.T) {
	mock := newGQLMock()
	mock.add(&gqlThread{ID: "low", Comments: []gqlComment{{Author: "Copilot", Body: "![P2 Badge](url)"}}, PRNumber: 1, RepoOwner: "o", RepoName: "r", HeadRef: "fix"})
	srv := httptest.NewServer(mock.handler(t))
	defer srv.Close()
	bots := testBots
	bots.MinPriority = "P1"
	c := reviewThreadTestClient(t, srv.URL, bots)
	report := c.CollectReviewThreads(context.Background(), []PullRequest{{Repo: "r", Number: 1, Author: "hive[bot]"}}, time.Now())
	if report.TotalThreads != 0 || len(report.PRs) != 1 || report.PRs[0].ExcludedByPriority != 1 || len(report.PRs[0].Threads) != 0 {
		t.Fatalf("all-filtered PR must remain visible: %+v", report)
	}
}

func TestFilterReviewThreadsPriority(t *testing.T) {
	bot := func(body string) gqlComment { return gqlComment{Author: "Copilot", Body: body} }
	low := bot("![P2 Badge](url)")
	threads := []rawReviewThread{
		rawThread("p0", false, false, bot("![P0 Badge](url)")),
		rawThread("p1", false, false, bot("![P1 Badge](url)")),
		rawThread("p2", false, false, low),
		rawThread("p3", false, false, bot("![P3 Badge](url)")),
		rawThread("unknown", false, false, bot("unknown"), low),
		rawThread("resolved", true, false, low),
		rawThread("outdated", false, true, low),
		rawThread("human", false, false, gqlComment{Author: "alice", Body: low.Body}),
		rawThread("attempted", false, false, low, gqlComment{Author: "hive[bot]"}),
		rawThread("late-badge", false, false, bot(strings.Repeat("x", reviewThreadBodyRunes+1)+low.Body)),
	}
	bots := testBots
	bots.MinPriority = "P1"
	got, excluded := filterReviewThreadsWithPriorityCount(threads, bots, isHiveTest)
	if len(got) != 3 || excluded != 3 {
		t.Fatalf("got %+v, excluded %d", got, excluded)
	}
	for i, id := range []string{"p0", "p1", "unknown"} {
		if got[i].ThreadID != id {
			t.Errorf("got %s want %s", got[i].ThreadID, id)
		}
	}
	// Filtering never mutates resolution state or consumes a reply attempt.
	if threads[2].IsResolved || len(threads[2].Comments.Nodes) != 1 {
		t.Fatal("filtered thread changed")
	}
	data, err := json.Marshal(ReviewThreadPR{Threads: got, ExcludedByPriority: excluded})
	if err != nil || !strings.Contains(string(data), `"excluded_by_priority":3`) {
		t.Fatalf("report: %s %v", data, err)
	}
	bots.MinPriority = ""
	got, excluded = filterReviewThreadsWithPriorityCount(threads, bots, isHiveTest)
	if len(got) != 6 || excluded != 0 {
		t.Fatalf("unset: %d threads, %d excluded", len(got), excluded)
	}
}
