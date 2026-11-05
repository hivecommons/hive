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
