package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// projectbluefin/utah#1074: every check that ran was green, and the
// ruleset-required `contract` never reported. The digest names it.
func TestSummarizeCommitChecksRequiredNamesMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/o/r/commits/abc/status":
			_, _ = w.Write([]byte(`{"state":"success","statuses":[]}`))
		case "/repos/o/r/commits/abc/check-runs":
			_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[{"name":"prow / prow","status":"completed","conclusion":"success","app":{"id":1}},{"name":"lint","status":"completed","conclusion":"success","app":{"id":1}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := gh.NewClient(nil)
	base, _ := url.Parse(srv.URL + "/")
	client.BaseURL = base

	got, err := SummarizeCommitChecksRequired(context.Background(), client, "o", "r", "abc", map[string]bool{"contract": true, "lint": true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Green != 2 || got.Total != 2 || !reflect.DeepEqual(got.MissingRequired, []string{"contract"}) {
		t.Fatalf("summary = %+v, want 2/2 green and contract missing", got)
	}
	got, err = SummarizeCommitChecksRequired(context.Background(), client, "o", "r", "abc", nil)
	if err != nil || got.MissingRequired != nil {
		t.Fatalf("no required set: %+v %v", got, err)
	}
}

// A required job skipped by its `if:` reports conclusion "skipped", which
// GitHub accepts as passing for a required check: it is reported, not
// missing. A required check still queued is reported too (pending), and only
// a context with no status and no check run at all is missing
// (projectbluefin/common#1619).
func TestSummarizeCommitChecksRequiredSkippedAndPendingAreReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/o/r/commits/abc/status":
			_, _ = w.Write([]byte(`{"state":"success","statuses":[]}`))
		case "/repos/o/r/commits/abc/check-runs":
			_, _ = w.Write([]byte(`{"total_count":3,"check_runs":[` +
				`{"name":"Canary","status":"completed","conclusion":"skipped","app":{"id":15368}},` +
				`{"name":"Unit tests","status":"queued","app":{"id":15368}},` +
				`{"name":"report","status":"completed","conclusion":"success","app":{"id":15368}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := gh.NewClient(nil)
	base, _ := url.Parse(srv.URL + "/")
	client.BaseURL = base

	required := map[string]bool{"Canary": true, "Unit tests": true, "Runtime contract": true}
	got, err := SummarizeCommitChecksRequired(context.Background(), client, "o", "r", "abc", required)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.MissingRequired, []string{"Runtime contract"}) {
		t.Fatalf("missing = %q, want only the never-reported Runtime contract", got.MissingRequired)
	}
	if got.Total != 3 || got.Green != 2 || got.Pending != 1 || len(got.Red) != 0 {
		t.Fatalf("summary = %+v, want 3 checks: skipped and success green, one pending", got)
	}
}
