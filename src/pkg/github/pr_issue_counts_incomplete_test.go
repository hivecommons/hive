package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComputePRIssueCountsIncompleteAppFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("q"), "author:app/") {
			_, _ = io.WriteString(w, `{"total_count":0,"incomplete_results":true,"items":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"total_count":42,"incomplete_results":false,"items":[]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "org", []string{"repo1"})
	if counts, err := c.ComputePRIssueCounts(context.Background(), "repo1", "hive-app"); err == nil || counts != nil {
		t.Fatalf("partial author search accepted: counts=%+v, error=%v", counts, err)
	}
}

func TestComputePRIssueCountsCompleteZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"total_count":0,"incomplete_results":false,"items":[]}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "org", []string{"repo1"})
	counts, err := c.ComputePRIssueCounts(context.Background(), "repo1", "hive-bot[bot]")
	if err != nil || counts == nil {
		t.Fatalf("complete zero rejected: counts=%+v, error=%v", counts, err)
	}
	if counts.MergedPRs != 0 || counts.ClosedIssues != 0 {
		t.Fatalf("counts = %+v, want genuine zeros", counts)
	}
}
