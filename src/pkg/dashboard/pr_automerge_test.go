package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestQueuePRAutoMergeRoleAndSelfGate(t *testing.T) {
	tests := []struct {
		name       string
		role       string
		user       string
		author     string
		wantStatus int
		users      []string
		wantError  string
	}{
		{"merger queues other pr", config.RoleMerger, "alice", "bob", http.StatusOK, []string{"alice:owner"}, ""},
		{"merger cannot queue own pr", config.RoleMerger, "alice", "ALICE", http.StatusForbidden, []string{"alice:owner"}, ""},
		{"read write forbidden", config.RoleReadWrite, "alice", "bob", http.StatusForbidden, []string{"alice:owner"}, ""},
		{"read forbidden", config.RoleRead, "alice", "bob", http.StatusForbidden, []string{"alice:owner"}, ""},
		{"owner queues other pr", config.RoleOwner, "alice", "bob", http.StatusOK, []string{"alice:owner"}, ""},
		{"owner cannot queue own pr", config.RoleOwner, "alice", "ALICE", http.StatusForbidden, []string{"alice:owner"}, ""},
		{"owner needs authenticated user", config.RoleOwner, "", "bob", http.StatusForbidden, []string{"alice:owner"}, ""},
		{"empty allowlist", config.RoleOwner, "alice", "bob", http.StatusForbidden, nil, "dashboard.authorized_users is empty"},
		{"unlisted internal", config.RoleOwner, "internal", "bob", http.StatusForbidden, []string{"alice:merger"}, "@internal cannot merge"},
		{"listed internal", config.RoleOwner, "internal", "bob", http.StatusOK, []string{"internal:merger"}, ""},
		{"listed merger", config.RoleMerger, "ALICE", "bob", http.StatusOK, []string{"alice:merger"}, ""},
		{"allowlist read cannot use owner request role", config.RoleOwner, "alice", "bob", http.StatusForbidden, []string{"alice:read"}, "@alice cannot merge"},
		{"unlisted human", config.RoleMerger, "alice", "bob", http.StatusForbidden, []string{"carol:owner"}, "@alice cannot merge"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Subtests reuse acme/widget#7 with different authors; bypass the shared PR-detail cache.
			t.Setenv(config.GitHubPRDetailTTLEnv, "0")
			var labelAdded, reviewCreated bool
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
					json.NewEncoder(w).Encode(map[string]any{
						"number": 7,
						"user":   map[string]string{"login": tt.author},
						"head":   map[string]string{"sha": "head7"},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/labels/"+ghpkg.AutoMergeQueuedLabel:
					http.NotFound(w, r)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/labels":
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(map[string]any{"name": ghpkg.AutoMergeQueuedLabel})
				case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
					labelAdded = true
					json.NewEncoder(w).Encode([]map[string]any{{"name": ghpkg.AutoMergeQueuedLabel}})
				case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
					reviewCreated = true
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(map[string]any{"id": 1, "state": "APPROVED"})
				case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/statuses/head7":
					// Prow's commit-bound lgtm needs a prow/lgtm status beside the label (hivecommons/hive#13616).
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(map[string]any{"id": 1})
				default:
					t.Fatalf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()

			s := NewServer(0, slog.Default())
			s.deps = &Dependencies{
				GHClient: ghpkg.NewClient("token", "acme", []string{"widget"}, slog.Default(), api.URL),
				Config:   &config.Config{Project: config.ProjectConfig{Org: "acme", Repos: []string{"widget"}}, Dashboard: config.DashboardConfig{AuthorizedUsers: tt.users}},
			}
			req := httptest.NewRequest(http.MethodPost, "/api/prs/acme/widget/7/queue-automerge", nil)
			req.Header.Set("X-Hive-Role", tt.role)
			if tt.role == config.RoleOwner {
				req.Header.Set(ownerRoleVerifiedHeader, "true")
			}
			req.Header.Set("X-Hive-User", tt.user)
			req.SetPathValue("owner", "acme")
			req.SetPathValue("repo", "widget")
			req.SetPathValue("number", "7")
			rr := httptest.NewRecorder()
			s.handleQueuePRAutoMerge(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status = %d body %s, want %d", rr.Code, rr.Body.String(), tt.wantStatus)
			}
			if tt.wantError != "" && !strings.Contains(rr.Body.String(), tt.wantError) {
				t.Fatalf("body = %s, want %q", rr.Body.String(), tt.wantError)
			}
			if tt.wantStatus != http.StatusOK && (labelAdded || reviewCreated) {
				t.Fatalf("refused queue mutated GitHub: labelAdded=%v reviewCreated=%v", labelAdded, reviewCreated)
			}
			if tt.wantStatus == http.StatusOK {
				if !labelAdded || !reviewCreated {
					t.Fatalf("labelAdded=%v reviewCreated=%v, want both true", labelAdded, reviewCreated)
				}
				entries := s.audit.Recent(1)
				if len(entries) != 1 || entries[0].Action != "queue-pr-automerge" || !strings.Contains(entries[0].Detail, "pr="+strconv.Itoa(7)) {
					t.Fatalf("audit entry = %#v, want queue-pr-automerge with pr detail", entries)
				}
			}
		})
	}
}

func TestQueuePRAutoMergeRejectsRepoOutsideHive(t *testing.T) {
	called := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		t.Fatalf("unexpected GitHub API call for unauthorized repo: %s %s", r.Method, r.URL.Path)
	}))
	defer api.Close()

	s := NewServer(0, slog.Default())
	s.deps = &Dependencies{
		GHClient: ghpkg.NewClient("token", "acme", []string{"widget"}, slog.Default(), api.URL),
		Config:   &config.Config{Project: config.ProjectConfig{Org: "acme", Repos: []string{"widget"}}},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/prs/other/secret/7/queue-automerge", nil)
	req.Header.Set("X-Hive-Role", config.RoleMerger)
	req.Header.Set("X-Hive-User", "alice")
	req.SetPathValue("owner", "other")
	req.SetPathValue("repo", "secret")
	req.SetPathValue("number", "7")
	rr := httptest.NewRecorder()
	s.handleQueuePRAutoMerge(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d body %s, want 403", rr.Code, rr.Body.String())
	}
	if called {
		t.Fatal("GitHub API should not be called for unauthorized repo")
	}
}

// /api/role is served before GitHub credentials exist, so the label lookup it
// performs must survive a nil deps/client rather than panicking the dashboard.
func TestAutoMergeLabelNilDeps(t *testing.T) {
	s := &Server{}
	if got := s.autoMergeLabel(); got != ghpkg.AutoMergeQueuedLabel {
		t.Fatalf("nil-deps label = %q, want %q", got, ghpkg.AutoMergeQueuedLabel)
	}
}
