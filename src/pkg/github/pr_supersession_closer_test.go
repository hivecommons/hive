package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClosedByPullRequestReferenceSelectsLatestMergedNode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{"nodes":[`+
			`{"number":40,"url":"https://github.com/o/r/pull/40","merged":true,"mergedAt":"2026-10-10T11:40:00Z"},`+
			`{"number":41,"url":"https://github.com/o/r/pull/41","merged":false,"mergedAt":null},`+
			`{"number":42,"url":"https://github.com/other/repo/pull/42","merged":true,"mergedAt":"2026-10-12T11:40:00Z"},`+
			`{"number":43,"url":"https://github.com/o/r/pull/43","merged":true,"mergedAt":"2026-10-12T11:40:00Z"}]}}}}}`)
	}))
	defer ts.Close()
	c := NewClientForTest(ts.URL, "o", []string{"r"}, slog.Default())

	got, ok := c.closedByPullRequestReference(context.Background(), "o", "r", 7, 41)
	wantMergedAt := time.Date(2026, 10, 12, 11, 40, 0, 0, time.UTC)
	if !ok || got.Repo != "o/r" || got.Number != 43 || got.URL != "https://github.com/o/r/pull/43" || !got.MergedAt.Equal(wantMergedAt) {
		t.Fatalf("closer = %+v, %v; want latest same-repo merged PR #43", got, ok)
	}
}
