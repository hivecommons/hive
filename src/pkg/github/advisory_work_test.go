package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAdvisoryWorkState(t *testing.T) {
	for _, tc := range []struct {
		name, issue, pr, kind, state string
		issueStatus, prStatus        int
		wantError                    bool
	}{
		{name: "open issue", issue: `{"state":"open"}`, kind: "issue", state: "OPEN"},
		{name: "closed issue", issue: `{"state":"closed"}`, kind: "issue", state: "CLOSED"},
		{name: "open PR", issue: `{"state":"open","pull_request":{}}`, pr: `{"state":"open","merged":false}`, kind: "pr", state: "OPEN"},
		{name: "merged PR", issue: `{"state":"closed","pull_request":{}}`, pr: `{"state":"closed","merged":true}`, kind: "pr", state: "MERGED"},
		{name: "closed PR", issue: `{"state":"closed","pull_request":{}}`, pr: `{"state":"closed","merged":false}`, kind: "pr", state: "CLOSED"},
		{name: "missing reference", issueStatus: 404, wantError: true},
		{name: "rate limited", issueStatus: 429, wantError: true},
		{name: "PR lookup failed", issue: `{"state":"closed","pull_request":{}}`, prStatus: 500, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(resetPRDetailCacheForTest(time.Now, prDetailCacheMaxEntries))
			issueCalls, prCalls := 0, 0
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/other/repo/issues/7", func(w http.ResponseWriter, r *http.Request) {
				issueCalls++
				if tc.issueStatus != 0 {
					w.WriteHeader(tc.issueStatus)
				}
				fmt.Fprint(w, tc.issue)
			})
			mux.HandleFunc("/repos/other/repo/pulls/7", func(w http.ResponseWriter, r *http.Request) {
				prCalls++
				if tc.prStatus != 0 {
					w.WriteHeader(tc.prStatus)
				}
				fmt.Fprint(w, tc.pr)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			client := newTestClient(t, srv, "default", []string{"primary"})
			work, err := client.AdvisoryWorkState(context.Background(), "other", "repo", 7)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, wantError=%v", err, tc.wantError)
			}
			if !tc.wantError && (work.Kind != tc.kind || work.State != tc.state || work.CheckedAt.IsZero()) {
				t.Fatalf("work = %+v, want %s %s with check time", work, tc.kind, tc.state)
			}
			if tc.wantError && work.State != "" {
				t.Fatal("failed lookup presented a known state")
			}
			if issueCalls != 1 || (tc.kind == "issue" && prCalls != 0) {
				t.Fatalf("unexpected lookups: issues=%d pulls=%d", issueCalls, prCalls)
			}
		})
	}
}

func TestAdvisoryWorkStateNoClient(t *testing.T) {
	var client *Client
	if _, err := client.AdvisoryWorkState(context.Background(), "org", "repo", 7); err == nil {
		t.Fatal("nil client should return an inconclusive lookup")
	}
}
