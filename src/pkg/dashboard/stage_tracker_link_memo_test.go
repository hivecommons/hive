package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestStageTrackerLinkMemoSharesLookupAndExcludesCarryingPR(t *testing.T) {
	var gets, closerReads atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql") {
			closerReads.Add(1)
			fmt.Fprint(w, `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[{"number":70,"url":"https://github.com/hivecommons/hive/pull/70","merged":true,"mergedAt":"2026-10-27T12:00:00Z"}]}}}}}`)
			return
		}
		gets.Add(1)
		fmt.Fprint(w, `{"number":76,"title":"Delivered issue","state":"closed","state_reason":"completed"}`)
	}))
	defer ts.Close()
	s := newTestServer()
	s.deps = &Dependencies{GHClient: github.NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, s.logger)}
	qualify := func(string) string { return "hivecommons/hive" }
	now := time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)

	// Merged PRs are not tracker rows today. Exercise both row orders so a
	// future inclusion cannot let a row's exclusion poison its sibling's memo.
	for _, order := range [][]int{{70, 90}, {90, 70}} {
		s.stageTrackerLinks = stageTrackerLinkMemo{}
		beforeGets, before := gets.Load(), closerReads.Load()
		for rebuild := 0; rebuild < 2; rebuild++ {
			var items []StageTrackerItem
			for _, number := range order {
				items = append(items, StageTrackerItem{Repo: "hive", Number: number, Kind: "pr", LinkedIssues: []StageTrackerLinkedIssue{{Number: 76}}})
			}
			s.stageTrackerEnrichSoftLinks(items, qualify, now.Add(time.Duration(rebuild)*time.Minute))
			for _, item := range items {
				want := 70
				if item.Number == 70 {
					want = 0
				}
				if got := item.LinkedIssues[0].ClosedBy; got != want {
					t.Fatalf("order %v rebuild %d PR #%d: closer = %d, want %d", order, rebuild, item.Number, got, want)
				}
			}
		}
		if got := gets.Load() - beforeGets; got != 1 {
			t.Fatalf("order %v: issues GETs = %d, want 1 shared across siblings and rebuilds", order, got)
		}
		if got := closerReads.Load() - before; got != 1 {
			t.Fatalf("order %v: closer reads = %d, want 1 shared across siblings and rebuilds", order, got)
		}
	}
}
