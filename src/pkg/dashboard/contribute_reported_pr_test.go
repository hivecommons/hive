package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestReconcileReportedPRPreservesAttributionAndIssue(t *testing.T) {
	for _, tt := range []struct {
		kind, source string
	}{
		{kind: "issue"},
		{kind: "issue", source: "github"},
		{kind: "issue", source: "external"},
		{kind: "pr"},
		{kind: "fix-pr"},
	} {
		t.Run(tt.kind+"/"+tt.source, func(t *testing.T) {
			var mu sync.Mutex
			body := "Contributor description"
			links := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/graphql") {
					// Report an existing sidebar link: the hub must still perform this lookup.
					links++
					_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{"state":"OPEN","closingIssuesReferences":{"nodes":[{"number":7,"repository":{"nameWithOwner":"myorg/repo1"}}]}}}}}`)
					return
				}
				if r.URL.Path != "/repos/myorg/repo1/pulls/42" {
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				switch r.Method {
				case http.MethodPatch:
					var patch struct {
						Body string `json:"body"`
					}
					if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
						t.Error(err)
					}
					body = patch.Body
				case http.MethodGet:
				default:
					t.Errorf("unexpected method %s", r.Method)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "open", "body": body, "user": map[string]any{"login": "alice"}, "base": map[string]any{"repo": map[string]any{"full_name": "myorg/repo1"}}})
			}))
			defer ts.Close()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			s := NewServer(0, logger)
			deps := testDeps(t)
			deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, logger)
			s.RegisterAPI(deps)
			t.Cleanup(s.CloseContributeHub)
			hub := NewContributeWSHub(logger, s)
			t.Cleanup(hub.Close)
			hub.reconcileReportedPR(&WSTaskAssign{Kind: tt.kind, SourceType: tt.source, Repo: "myorg/repo1", Number: 7}, "https://github.com/myorg/repo1/pull/42", &ContributorConnection{cliBackend: "codex", profile: &ContributorProfile{GitHubUsername: "alice"}})
			mu.Lock()
			defer mu.Unlock()
			for _, want := range []string{"Contributor description", "— hive: backend=codex"} {
				if !strings.Contains(body, want) {
					t.Errorf("body %q lost %q", body, want)
				}
			}
			if got := strings.Contains(body, "Fixes #7"); got != (tt.kind == "issue" && tt.source != "external") {
				t.Errorf("kind %s: issue reference present=%v", tt.kind, got)
			}
			wantLinks := 0
			if tt.kind == "issue" && tt.source != "external" {
				wantLinks = 1
			}
			if links != wantLinks {
				t.Errorf("sidebar lookups=%d, want %d", links, wantLinks)
			}
		})
	}
}
