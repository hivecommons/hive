package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssueClaimBlockReason(t *testing.T) {
	for _, tc := range []struct {
		label, want string
	}{
		{"hold", "hold"},
		{"on-hold", "hold"},
		{"needs-human", "needs-human"},
		{" NEEDS-HUMAN ", "needs-human"},
		{"needs-human-followup", ""},
		{"bug", ""},
	} {
		t.Run(tc.label, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/repos/o/r/issues/42" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				fmt.Fprintf(w, `{"number":42,"labels":[{"name":%q}]}`, tc.label)
			}))
			defer srv.Close()
			got, err := issueTestClient(t, srv.URL).IssueClaimBlockReason(context.Background(), "o/r", 42)
			if err != nil || got != tc.want {
				t.Fatalf("reason = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestIssueRequestWatcher_ClaimRefusesHeldIssue(t *testing.T) {
	for _, label := range []string{"hold", "needs-human"} {
		t.Run(label, func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprintf(w, `{"number":42,"labels":[{"name":%q}]}`, label)
					return
				}
				writes++
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			dir := withIssueDir(t)
			path, err := WriteIssueRequest(dir, IssueRequest{Kind: "claim", Repo: "o/r", Number: 42, Agent: "scanner"})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())
			res := readIssueResultFile(t, path)
			if res.OK || !strings.Contains(res.Error, label) || writes != 0 {
				t.Fatalf("blocked claim: result %+v, writes %d", res, writes)
			}
		})
	}
}

// The shared enumerator supplies every kick and contributor work list. Human
// escalation stays excluded even when mixed with ordinary work labels.
func TestNeedsHumanExcludedFromActionableWork(t *testing.T) {
	for _, label := range []string{"needs-human", " NEEDS-HUMAN "} {
		t.Run(label, func(t *testing.T) {
			issues := []wireIssue{{Number: 42, Title: "human-only repair", User: wireUser{"alice"}, CreatedAt: hoursAgo(1), Labels: []wireLabel{{Name: "bug"}, {Name: label}}}}
			srv := httptest.NewServer(buildMux(t, "o", "r", issues, nil))
			defer srv.Close()
			result, err := newTestClient(t, srv, "o", []string{"r"}).EnumerateActionable(context.Background())
			if err != nil || result.Issues.Count != 0 || len(result.Issues.Items) != 0 {
				t.Fatalf("human work leaked into actionable list: %+v, %v", result.Issues, err)
			}
		})
	}
}

func TestIssueClaimBlockReasonLookupFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := issueTestClient(t, srv.URL).IssueClaimBlockReason(context.Background(), "o/r", 42); err == nil {
		t.Fatal("failed lookup treated as permission to claim")
	}
}

func TestIssueClaimBlockReasonNoClient(t *testing.T) {
	for _, c := range []*Client{nil, &Client{}} {
		if _, err := c.IssueClaimBlockReason(context.Background(), "o/r", 42); err == nil {
			t.Fatal("missing client treated as permission to claim")
		}
	}
}

// A pull request is never claimable as an issue: issue claims stage the item
// in the issue family (stage/020, stage/030) and a PR must never carry one.
// hivecommons/hive#12994 wore stage/030-pr-in-progress because its author's
// comment on it was taken as an issue start signal.
func TestIssueClaimBlockReasonPullRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/o/r/issues/12994" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"number":12994,"labels":[{"name":"bug"}],"pull_request":{"url":"https://api.github.example/repos/o/r/pulls/12994"}}`)
	}))
	defer srv.Close()
	got, err := issueTestClient(t, srv.URL).IssueClaimBlockReason(context.Background(), "o/r", 12994)
	if err != nil || got != ClaimBlockReasonPullRequest {
		t.Fatalf("reason = %q, %v; want %q", got, err, ClaimBlockReasonPullRequest)
	}
}

// The `claim` relay request on a pull request is refused before any write:
// no ownership label, no stage fact, no start mark.
func TestIssueRequestWatcher_ClaimRefusesPullRequest(t *testing.T) {
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"number":12994,"labels":[],"pull_request":{"url":"https://api.github.example/repos/o/r/pulls/12994"}}`)
			return
		}
		writes++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	calls := recordStarts(c)
	dir := withIssueDir(t)
	path, err := WriteIssueRequest(dir, IssueRequest{Kind: "claim", Repo: "o/r", Number: 12994, Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	res := readIssueResultFile(t, path)
	if res.OK || !strings.Contains(res.Error, ClaimBlockReasonPullRequest) || writes != 0 {
		t.Fatalf("claim on a pull request: result %+v, writes %d", res, writes)
	}
	if len(*calls) != 0 {
		t.Fatalf("refused claim on a pull request reported start signals: %+v", *calls)
	}
}
