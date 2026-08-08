package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newLiteTestHub(t *testing.T) (*HubServer, func()) {
	t.Helper()
	oldVerify := verifyLiteRepoAccess
	verifyLiteRepoAccess = func(context.Context, string, string, string, string) (bool, error) {
		return true, nil
	}
	t.Cleanup(func() { verifyLiteRepoAccess = oldVerify })
	cleanup := helperSetupTempDirs(t)
	dir := t.TempDir()
	s := &HubServer{
		logger:       slog.Default(),
		hubSecret:    testHubSecret,
		saveCh:       make(chan struct{}, 1),
		registryPath: filepath.Join(dir, "registry.json"),
		clusters:     map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID}},
	}
	mkUser(t, "lite-user")
	return s, cleanup
}

func liteReqWithUser(method, target, body, username string) *http.Request {
	req := reqWithUser(method, target, body, username)
	req.Header.Set("Authorization", "Bearer ghp-test")
	return req
}

func TestHandleLiteEnrollTable(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		check      func(t *testing.T, s *HubServer, rec *httptest.ResponseRecorder)
	}{
		{
			name:       "happy path caps to L2 and persists lite marker",
			body:       `{"owner":"kubestellar","repo":"hive","installation_id":123,"acmm_level":5}`,
			wantStatus: http.StatusOK,
			check: func(t *testing.T, s *HubServer, rec *httptest.ResponseRecorder) {
				var resp LiteEnrollmentResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatal(err)
				}
				if resp.ACMMLevel != 2 || resp.InstallationID != 123 || resp.Mode != "lite" {
					t.Fatalf("response = %#v", resp)
				}
				if len(s.registry.Hives) != 1 || !s.registry.Hives[0].Lite || s.registry.Hives[0].HiveType != "lite" || s.registry.Hives[0].IsPublic {
					t.Fatalf("registry = %#v", s.registry.Hives)
				}
			},
		},
		{
			name:       "invalid repo rejected",
			body:       `{"owner":"bad/owner","repo":"hive","installation_id":123}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "installation required without hub app key",
			body:       `{"owner":"kubestellar","repo":"hive"}`,
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, cleanup := newLiteTestHub(t)
			defer cleanup()
			rec := httptest.NewRecorder()
			req := liteReqWithUser(http.MethodPost, "/api/saas/lite/enroll", tt.body, "lite-user")
			s.handleLiteEnroll(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), tt.wantStatus)
			}
			if tt.check != nil {
				tt.check(t, s, rec)
			}
		})
	}
}

func TestHandleLiteEnrollDuplicateIdempotent(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	body := `{"owner":"kubestellar","repo":"hive","installation_id":123,"acmm_level":2}`
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := liteReqWithUser(http.MethodPost, "/api/saas/lite/enroll", body, "lite-user")
		s.handleLiteEnroll(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	if len(s.registry.Hives) != 1 {
		t.Fatalf("duplicate enrollment created %d entries", len(s.registry.Hives))
	}
}

func TestHandleLiteEnrollDuplicateRejectsDifferentOwner(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	mkUser(t, "other-user")
	s.registry.Hives = []RegistryEntry{{
		ID:          liteHiveID(publicGitHubHost, "kubestellar", "hive"),
		Org:         "kubestellar",
		PrimaryRepo: "hive",
		Repos:       []string{"hive"},
		Owner:       "other-user",
		HiveType:    "lite",
		Lite:        true,
	}}
	rec := httptest.NewRecorder()
	req := liteReqWithUser(http.MethodPost, "/api/saas/lite/enroll", `{"owner":"kubestellar","repo":"hive","installation_id":123}`, "lite-user")
	s.handleLiteEnroll(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if s.registry.Hives[0].Owner != "other-user" {
		t.Fatalf("owner changed to %q", s.registry.Hives[0].Owner)
	}
}

func TestLiteHiveIDIncludesNonPublicHost(t *testing.T) {
	publicID := liteHiveID(publicGitHubHost, "org", "repo")
	gheID := liteHiveID("github.example.com", "org", "repo")
	if publicID == gheID {
		t.Fatalf("public and GHE IDs collided: %q", publicID)
	}
	if !strings.HasPrefix(publicID, "lite-org-repo-") {
		t.Fatalf("public ID = %q", publicID)
	}
	if !strings.HasPrefix(gheID, "lite-github-example-com-org-repo") {
		t.Fatalf("GHE ID = %q", gheID)
	}
	if mixed := liteHiveID("GitHub.Example.Com", "Org", "Repo"); mixed != gheID {
		t.Fatalf("case variant ID = %q, want %q", mixed, gheID)
	}
}

func TestWebhookMatcherSkipsLiteEntries(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	s.registry.Hives = []RegistryEntry{
		{ID: "lite", Org: "org", Repos: []string{"repo"}, PrimaryRepo: "repo", Lite: true, HiveType: "lite"},
		{ID: "spoke", Org: "org", Repos: []string{"repo"}, PrimaryRepo: "repo", HiveType: "hosted"},
	}
	got := s.findHiveByOrgRepos("org", []string{"repo"})
	if got == nil || got.ID != "spoke" {
		t.Fatalf("matched %#v, want spoke", got)
	}
}

func TestLiteEnrollRouteRequiresAuth(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/lite/enroll", s.requireAuth(s.handleLiteEnroll))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/saas/lite/enroll", strings.NewReader(`{"owner":"o","repo":"r","installation_id":1}`))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s, want 401", rec.Code, rec.Body.String())
	}
}
