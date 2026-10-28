package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestReconcileContributorPRIssue(t *testing.T) {
	for _, tt := range []struct {
		name, body, state, author, baseRepo, assignedRepo string
		wantBody                                          string
		wantPatches, wantLinks                            int
		wantErr                                           bool
	}{
		{name: "missing reference on release branch", body: "Description\n\n— hive: backend=codex", wantBody: "Description\n\n— hive: backend=codex\n\nFixes #7", wantPatches: 1, wantLinks: 1},
		{name: "existing closing still ensures sidebar", body: "Resolves hivecommons/hive#7", wantLinks: 1},
		{name: "existing URL closing", body: "Fixes https://github.com/hivecommons/hive/issues/7", wantLinks: 1},
		{name: "unrelated closing", body: "Fixes other/repo#7", wantBody: "Fixes other/repo#7\n\nFixes #7", wantPatches: 1, wantLinks: 1},
		{name: "different issue", body: "Fixes #70", wantBody: "Fixes #70\n\nFixes #7", wantPatches: 1, wantLinks: 1},
		{name: "partial refs", body: "Refs #7 — follow-up remains"},
		{name: "partial wording", body: "Leave #7 open", wantBody: "Leave #7 open\n\nRefs #7", wantPatches: 1},
		{name: "closed", state: "closed"},
		{name: "wrong author", author: "mallory", wantErr: true},
		{name: "wrong base repo", baseRepo: "other/repo", wantErr: true},
		{name: "wrong assignment", assignedRepo: "other/repo", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			body := tt.body
			patches := 0
			fixture := &closingLinkFixture{base: "v6", prState: "OPEN", issueState: "OPEN"}
			graphql := fixture.handler(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/graphql") {
					graphql(w, r)
					return
				}
				if r.URL.Path != "/repos/hivecommons/hive/pulls/1" {
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPatch {
					var patch struct {
						Body string `json:"body"`
					}
					if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
						t.Error(err)
					}
					body = patch.Body
					patches++
				} else if r.Method != http.MethodGet {
					t.Errorf("unexpected method %s", r.Method)
				}
				state, author, repo := tt.state, tt.author, tt.baseRepo
				if state == "" {
					state = "open"
				}
				if author == "" {
					author = "alice"
				}
				if repo == "" {
					repo = "hivecommons/hive"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "body": body, "state": state, "user": map[string]any{"login": author}, "base": map[string]any{"ref": "v6", "repo": map[string]any{"full_name": repo}}})
			}))
			defer srv.Close()
			client := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
			assigned := tt.assignedRepo
			if assigned == "" {
				assigned = "hivecommons/hive"
			}
			// Repeated completion must not duplicate the body line or sidebar mutation.
			for i := 0; i < 2; i++ {
				err := client.ReconcileContributorPRIssue(context.Background(), assigned, 7, "https://github.com/hivecommons/hive/pull/1", "alice")
				if (err != nil) != tt.wantErr {
					t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			wantBody := tt.wantBody
			if wantBody == "" {
				wantBody = tt.body
			}
			if body != wantBody {
				t.Errorf("body = %q, want %q", body, wantBody)
			}
			if patches != tt.wantPatches || int(fixture.mutations.Load()) != tt.wantLinks {
				t.Errorf("patches=%d links=%d; want %d, %d", patches, fixture.mutations.Load(), tt.wantPatches, tt.wantLinks)
			}
		})
	}
}
