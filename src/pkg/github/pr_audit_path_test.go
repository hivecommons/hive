package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

type prAuditRec struct{ action, detail, agent string }

// The hive-merge relay (MergePR via the merge-request watcher) records its
// merge as pr_merged with path=relay, so the dashboard can tell it apart from
// the auto-merge sweep's merges.
func TestMergeRequestWatcher_AuditsPRMergedWithRelayPath(t *testing.T) {
	merges := 0
	srv := newMergeMockServer(t, 0, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	var trail []prAuditRec
	c.SetAttributionAudit(func(action, detail, agent string) {
		trail = append(trail, prAuditRec{action, detail, agent})
	})

	dir := t.TempDir()
	mergeRequestDirForTest = dir
	defer func() { mergeRequestDirForTest = "" }()

	if _, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 42, Method: "squash", Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessMergeRequestsOnce(context.Background())

	if merges != 1 {
		t.Fatalf("expected 1 merge, got %d", merges)
	}
	var got *prAuditRec
	for i := range trail {
		if trail[i].action == AuditActionPRMerged {
			got = &trail[i]
		}
	}
	if got == nil {
		t.Fatalf("no %s entry on the trail; trail=%#v", AuditActionPRMerged, trail)
	}
	if got.agent != AttributionAgentGovernor {
		t.Errorf("agent = %q, want %q", got.agent, AttributionAgentGovernor)
	}
	for _, want := range []string{"repo=o/r", "number=42", "method=squash", "sha=deadbeef", "path=" + PRAuditPathRelay} {
		if !strings.Contains(got.detail, want) {
			t.Errorf("pr_merged detail missing %q: %q", want, got.detail)
		}
	}
}

func TestRecordPRMergedAuditCarriesPath(t *testing.T) {
	for _, path := range []string{PRAuditPathSweep, PRAuditPathQueue, PRAuditPathRelay} {
		t.Run(path, func(t *testing.T) {
			c := &Client{}
			var trail []prAuditRec
			c.SetAttributionAudit(func(action, detail, agent string) {
				trail = append(trail, prAuditRec{action, detail, agent})
			})
			c.RecordPRMergedAudit("o/r", 7, "squash", "abc", path)
			if len(trail) != 1 || trail[0].action != AuditActionPRMerged {
				t.Fatalf("trail = %#v, want one pr_merged", trail)
			}
			if !strings.Contains(trail[0].detail, "path="+path) {
				t.Errorf("detail %q missing path=%s", trail[0].detail, path)
			}
		})
	}
}

// A close request through the issue-request watcher records pr_closed
// (path=relay) when the number is a pull request, and keeps recording
// agent_issue_closed for a plain issue.
func TestIssueRequestWatcher_CloseAuditsPRClosedForPullRequests(t *testing.T) {
	tests := []struct {
		name       string
		pr         bool
		wantAction string
		wantPath   bool
	}{
		{name: "issue", pr: false, wantAction: AuditActionIssueClosed},
		{name: "pull request", pr: true, wantAction: AuditActionPRClosed, wantPath: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := &gh.Issue{
				Number: gh.Ptr(7),
				State:  gh.Ptr("open"),
				User:   &gh.User{Login: gh.Ptr("hive-app[bot]"), Type: gh.Ptr("Bot")},
			}
			if tt.pr {
				issue.PullRequestLinks = &gh.PullRequestLinks{URL: gh.Ptr("https://api.github.example/repos/o/r/pulls/7")}
			}
			closed := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
					_ = json.NewEncoder(w).Encode(issue)
				case r.Method == http.MethodPatch && r.URL.Path == "/repos/o/r/issues/7":
					closed = true
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			c := issueTestClient(t, srv.URL)
			var trail []prAuditRec
			c.SetAttributionAudit(func(action, detail, agent string) {
				trail = append(trail, prAuditRec{action, detail, agent})
			})
			dir := withIssueDir(t)
			reqPath, err := WriteIssueRequest(dir, IssueRequest{Kind: "close", Repo: "o/r", Number: 7, Agent: "scanner"})
			if err != nil {
				t.Fatal(err)
			}

			c.ProcessIssueRequestsOnce(context.Background())

			if !closed {
				t.Fatal("close request did not PATCH state=closed")
			}
			if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
				t.Error("close request should be consumed")
			}
			if len(trail) != 1 {
				t.Fatalf("trail = %#v, want exactly one entry", trail)
			}
			if trail[0].action != tt.wantAction {
				t.Errorf("action = %q, want %q", trail[0].action, tt.wantAction)
			}
			if hasPath := strings.Contains(trail[0].detail, "path="+PRAuditPathRelay); hasPath != tt.wantPath {
				t.Errorf("detail %q: has path=relay = %v, want %v", trail[0].detail, hasPath, tt.wantPath)
			}
		})
	}
}
