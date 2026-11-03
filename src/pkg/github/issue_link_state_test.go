package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An issue closed while a PR that says "Fixes #N" is still open was closed
// by something else. The slot names only a MERGED closer: GitHub lists the
// open PR under closedByPullRequestsReferences too, and naming it would read
// as if the open PR had already delivered the issue.
func TestIssueLinkStateNamesOnlyMergedCloser(t *testing.T) {
	graphql := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql") {
			graphql++
			fmt.Fprint(w, `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[`+
				`{"number":101,"merged":false},`+
				`{"number":99,"url":"https://github.com/o/r/pull/99","merged":true,"mergedAt":"2026-10-29T16:38:39Z"}]}}}}}`)
			return
		}
		switch r.URL.Path {
		case "/repos/o/r/issues/7":
			fmt.Fprint(w, `{"number":7,"title":"Closed one","state":"closed","state_reason":"completed"}`)
		case "/repos/o/r/issues/8":
			fmt.Fprint(w, `{"number":8,"title":"Open one","state":"open"}`)
		case "/repos/o/r/issues/9":
			fmt.Fprint(w, `{"number":9,"title":"A PR","state":"open","pull_request":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c := NewClientForTest(ts.URL, "o", []string{"r"}, slog.Default())
	ctx := context.Background()

	got, err := c.IssueLinkState(ctx, "o", "r", 7, 101)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Closed one" || got.State != "closed" || got.StateReason != "completed" || got.ClosedBy != 99 || got.IsPR {
		t.Fatalf("closed issue = %+v, want closed by merged #99, never the open #101", got)
	}

	got, err = c.IssueLinkState(ctx, "o", "r", 8, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "open" || got.ClosedBy != 0 {
		t.Fatalf("open issue = %+v, want open with no closer", got)
	}
	if graphql != 1 {
		t.Fatalf("graphql reads = %d, want 1 (closed issues only)", graphql)
	}

	got, err = c.IssueLinkState(ctx, "o", "r", 9, 0)
	if err != nil || !got.IsPR || got.Title != "" {
		t.Fatalf("pull request = %+v, %v; want IsPR and nothing else", got, err)
	}
}

// A forge that cannot answer the closer query (older GHE, a token without
// the scope) still yields the title and state: the chip reads `closed`
// rather than the row losing its issue.
func TestIssueLinkStateCloserLookupBestEffort(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, `{"message":"nope"}`, http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{"number":7,"title":"Closed one","state":"closed","state_reason":"not_planned"}`)
	}))
	defer ts.Close()
	c := NewClientForTest(ts.URL, "o", []string{"r"}, slog.Default())
	got, err := c.IssueLinkState(context.Background(), "o", "r", 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Closed one" || got.State != "closed" || got.StateReason != "not_planned" || got.ClosedBy != 0 {
		t.Fatalf("got %+v, want title/state kept and ClosedBy 0", got)
	}
}

// closedByPullRequestsReferences can include a merged PR from another
// repository; its number must never be reported as a same-repo closer.
func TestIssueLinkStateIgnoresCrossRepoCloser(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql") {
			fmt.Fprint(w, `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[`+
				`{"number":55,"url":"https://github.com/other/repo/pull/55","merged":true,"mergedAt":"2026-10-29T16:38:39Z"},`+
				`{"number":99,"url":"https://github.com/O/R/pull/99","merged":true,"mergedAt":"2026-10-29T16:38:39Z"}]}}}}}`)
			return
		}
		if r.URL.Path == "/repos/o/r/issues/7" {
			fmt.Fprint(w, `{"number":7,"title":"Closed one","state":"closed","state_reason":"completed"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	c := NewClientForTest(ts.URL, "o", []string{"r"}, slog.Default())
	got, err := c.IssueLinkState(context.Background(), "o", "r", 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClosedBy != 99 {
		t.Fatalf("ClosedBy = %d, want 99 (cross-repo #55 ignored)", got.ClosedBy)
	}
}
