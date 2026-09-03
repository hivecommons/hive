package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func newAPIV1QueueServer(t *testing.T, role, tokenUser, prAuthor, headSHA string) (*Server, *bool, *bool) {
	t.Helper()
	var reviewCreated, labelAdded bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			if r.Header.Get("Authorization") != "Bearer valid-token" {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"login": tokenUser})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			json.NewEncoder(w).Encode(map[string]any{
				"number": 7,
				"user":   map[string]string{"login": prAuthor},
				"head":   map[string]string{"sha": headSHA},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/labels/"+ghpkg.AutoMergeQueuedLabel:
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/labels":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"name": ghpkg.AutoMergeQueuedLabel})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			var body struct {
				CommitID string `json:"commit_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode review: %v", err)
			}
			if body.CommitID != headSHA {
				t.Fatalf("review commit_id = %q, want current head %q", body.CommitID, headSHA)
			}
			reviewCreated = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "state": "APPROVED"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
			labelAdded = true
			json.NewEncoder(w).Encode([]map[string]any{{"name": ghpkg.AutoMergeQueuedLabel}})
		default:
			t.Fatalf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	cfg := &config.Config{
		Project:   config.ProjectConfig{Org: "acme", Repos: []string{"widget"}},
		GitHub:    config.GitHubConfig{OAuthAPIURLOverride: api.URL},
		Dashboard: config.DashboardConfig{AuthorizedUsers: []string{tokenUser + ":" + role}},
	}
	s := NewServer(0, slog.Default())
	s.authToken = "dashboard-token"
	s.deps = &Dependencies{
		Config:   cfg,
		GHClient: ghpkg.NewClient("app-token", "acme", []string{"widget"}, slog.Default(), api.URL),
		Logger:   slog.Default(),
	}
	s.RegisterAPI(s.deps)
	return s, &reviewCreated, &labelAdded
}

func queueAPIV1(t *testing.T, s *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	return queueAPIV1Request(t, s, "/api/v1/prs/acme/widget/7/queue-automerge", token, nil)
}

func queueAPIV1Request(t *testing.T, s *Server, path, token string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestAPIV1QueuePRAutoMergeRequiresValidGitHubBearer(t *testing.T) {
	s, reviewCreated, labelAdded := newAPIV1QueueServer(t, config.RoleMerger, "alice", "bob", "head7")
	w := queueAPIV1(t, s, "wrong-token")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s, want 401", w.Code, w.Body.String())
	}
	if *reviewCreated || *labelAdded {
		t.Fatal("invalid bearer token must not mutate the pull request")
	}
}

func TestAPIV1QueuePRAutoMergeRequiresMergerRole(t *testing.T) {
	s, reviewCreated, labelAdded := newAPIV1QueueServer(t, config.RoleReadWrite, "alice", "bob", "head7")
	w := queueAPIV1(t, s, "valid-token")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", w.Code, w.Body.String())
	}
	if *reviewCreated || *labelAdded {
		t.Fatal("insufficient role must not mutate the pull request")
	}
}

func TestAPIV1QueuePRAutoMergeIgnoresForgedIdentityHeaders(t *testing.T) {
	s, reviewCreated, labelAdded := newAPIV1QueueServer(t, config.RoleReadWrite, "alice", "bob", "head7")
	w := queueAPIV1Request(t, s, "/api/v1/prs/acme/widget/7/queue-automerge", "valid-token", map[string]string{
		"X-Hive-User":           "mallory",
		"X-Hive-Role":           config.RoleOwner,
		ownerRoleVerifiedHeader: "true",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", w.Code, w.Body.String())
	}
	if *reviewCreated || *labelAdded {
		t.Fatal("forged identity headers must not mutate the pull request")
	}
}

func TestAPIV1QueuePRAutoMergeRejectsQueryToken(t *testing.T) {
	s, reviewCreated, labelAdded := newAPIV1QueueServer(t, config.RoleMerger, "alice", "bob", "head7")
	w := queueAPIV1Request(t, s, "/api/v1/prs/acme/widget/7/queue-automerge?token=valid-token", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s, want 401", w.Code, w.Body.String())
	}
	if *reviewCreated || *labelAdded {
		t.Fatal("query token must not authorize a mutation")
	}
}

func TestAPIV1QueuePRAutoMergeRejectsRepoOutsideHive(t *testing.T) {
	s, reviewCreated, labelAdded := newAPIV1QueueServer(t, config.RoleMerger, "alice", "bob", "head7")
	w := queueAPIV1Request(t, s, "/api/v1/prs/other/secret/7/queue-automerge", "valid-token", nil)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not managed") {
		t.Fatalf("status = %d body=%s, want managed-repository 403", w.Code, w.Body.String())
	}
	if *reviewCreated || *labelAdded {
		t.Fatal("repository scope rejection must not mutate the pull request")
	}
}

func TestAPIV1QueuePRAutoMergeRejectsOwnPR(t *testing.T) {
	s, reviewCreated, labelAdded := newAPIV1QueueServer(t, config.RoleMerger, "alice", "ALICE", "head7")
	w := queueAPIV1(t, s, "valid-token")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "own pull requests") {
		t.Fatalf("status = %d body=%s, want self-merge 403", w.Code, w.Body.String())
	}
	if *reviewCreated || *labelAdded {
		t.Fatal("self-merge rejection must not mutate the pull request")
	}
}

func TestAPIV1QueuePRAutoMergeUsesExistingExactHeadGuard(t *testing.T) {
	s, reviewCreated, labelAdded := newAPIV1QueueServer(t, config.RoleMerger, "alice", "bob", "")
	w := queueAPIV1(t, s, "valid-token")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "head SHA") {
		t.Fatalf("status = %d body=%s, want missing-head rejection", w.Code, w.Body.String())
	}
	if *reviewCreated || *labelAdded {
		t.Fatal("missing head SHA must not mutate the pull request")
	}
}

func TestAPIV1QueuePRAutoMergeBindsValidatedActor(t *testing.T) {
	s, reviewCreated, labelAdded := newAPIV1QueueServer(t, config.RoleMerger, "alice", "bob", "head7")
	w := queueAPIV1(t, s, "valid-token")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
	}
	if !*reviewCreated || !*labelAdded {
		t.Fatalf("reviewCreated=%v labelAdded=%v, want both true", *reviewCreated, *labelAdded)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got["status"] != "queued" || got["repo"] != "acme/widget" || got["number"] != float64(7) {
		t.Fatalf("response = %#v, want structured queue result", got)
	}
}

func TestAPIV1IsPublicOnlyWithinVersionedPrefix(t *testing.T) {
	for _, tt := range []struct {
		path string
		want bool
	}{
		{"/api/v1/status", true},
		{"/api/v1/prs/acme/widget/7/queue-automerge", true},
		{"/api/v10/status", false},
		{"/api/prs/acme/widget/7/queue-automerge", false},
	} {
		if got := isPublicPath(tt.path); got != tt.want {
			t.Errorf("isPublicPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// queue-automerge is a mutation: safe methods must never reach the handler.
func TestAPIV1QueuePRAutoMergeRejectsGET(t *testing.T) {
	s, reviewCreated, labelAdded := newAPIV1QueueServer(t, config.RoleMerger, "alice", "bob", "head7")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prs/acme/widget/7/queue-automerge", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d body=%s, want 405", w.Code, w.Body.String())
	}
	if *reviewCreated || *labelAdded {
		t.Fatal("GET must not mutate the pull request")
	}
}

// The legacy "token <pat>" scheme must also work for the mutation path.
func TestAPIV1QueuePRAutoMergeAcceptsLegacyTokenScheme(t *testing.T) {
	s, reviewCreated, labelAdded := newAPIV1QueueServer(t, config.RoleMerger, "alice", "bob", "head7")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/prs/acme/widget/7/queue-automerge", nil)
	req.Header.Set("Authorization", "token valid-token")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
	}
	if !*reviewCreated || !*labelAdded {
		t.Fatalf("reviewCreated=%v labelAdded=%v, want both true", *reviewCreated, *labelAdded)
	}
}
