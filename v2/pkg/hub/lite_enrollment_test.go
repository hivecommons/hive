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
		seed       func(t *testing.T)
		wantStatus int
		check      func(t *testing.T, s *HubServer, rec *httptest.ResponseRecorder)
	}{
		{
			name: "existing spoke gets repo through config callback",
			body: `{"owner":"kubestellar","repo":"hive","installation_id":123,"acmm_level":5}`,
			seed: func(t *testing.T) {
				t.Helper()
				if err := saveSaaSHive(&SaaSHive{
					ID: "spoke-1", Owner: "lite-user", Org: "kubestellar", Repos: []string{"old"}, PrimaryRepo: "old",
					ACMMLevel: 2, RequestedACMMLevel: 2, Status: statusAssigned, ClaimDelivered: true, ACMMDelivered: true,
				}); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: http.StatusOK,
			check: func(t *testing.T, s *HubServer, rec *httptest.ResponseRecorder) {
				var resp LiteEnrollmentResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatal(err)
				}
				if resp.ACMMLevel != 2 || resp.Action != "spoke_config_update" || resp.SpokeID != "spoke-1" || !resp.Existing {
					t.Fatalf("response = %#v", resp)
				}
				h := loadSaaSHive("spoke-1")
				if h == nil || !containsFold(h.Repos, "hive") || h.ClaimDelivered || h.ACMMDelivered {
					t.Fatalf("spoke meta = %#v", h)
				}
			},
		},
		{
			name:       "invalid repo rejected",
			body:       `{"owner":"bad/owner","repo":"hive","installation_id":123}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "installation required without hub app key",
			body: `{"owner":"kubestellar","repo":"hive"}`,
			seed: func(t *testing.T) {
				t.Helper()
				u := ensureSaaSUser("lite-user")
				u.SaaSQuota = 1
				if err := saveSaaSUser(u); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "hosted spoke requires quota",
			body:       `{"owner":"kubestellar","repo":"hive","installation_id":123}`,
			wantStatus: http.StatusForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, cleanup := newLiteTestHub(t)
			defer cleanup()
			if tt.seed != nil {
				tt.seed(t)
			}
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
	if err := saveSaaSHive(&SaaSHive{
		ID: "spoke-1", Owner: "lite-user", Org: "kubestellar", Repos: []string{"hive"}, PrimaryRepo: "hive",
		ACMMLevel: 2, RequestedACMMLevel: 2, Status: statusAssigned,
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"owner":"kubestellar","repo":"hive","installation_id":123,"acmm_level":2}`
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := liteReqWithUser(http.MethodPost, "/api/saas/lite/enroll", body, "lite-user")
		s.handleLiteEnroll(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	h := loadSaaSHive("spoke-1")
	if h == nil || len(h.Repos) != 1 || h.Repos[0] != "hive" {
		t.Fatalf("duplicate enrollment changed repos: %#v", h)
	}
}

func TestHandleLiteEnrollDoesNotAttachToWrongForgeSpoke(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	if err := saveSaaSHive(&SaaSHive{
		ID: "spoke-1", Owner: "lite-user", Org: "kubestellar", Repos: []string{"old"}, PrimaryRepo: "old",
		GitHubHost: "github.ibm.com", ACMMLevel: 2, Status: statusAssigned,
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := liteReqWithUser(http.MethodPost, "/api/saas/lite/enroll", `{"owner":"kubestellar","repo":"hive","installation_id":123}`, "lite-user")
	s.handleLiteEnroll(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want quota failure instead of mutating wrong-forge spoke", rec.Code, rec.Body.String())
	}
	h := loadSaaSHive("spoke-1")
	if h == nil || containsFold(h.Repos, "hive") {
		t.Fatalf("wrong-forge spoke was mutated: %#v", h)
	}
}

func TestHandleLiteEnrollHostedSpokeRequestedWhenNoExistingSpoke(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	u := ensureSaaSUser("lite-user")
	u.SaaSQuota = 1
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	s.clusters[defaultClusterID] = ClusterConfig{ID: defaultClusterID, Domain: "example.test", InCluster: true, StorageType: storageTypeDynamic, IngressType: "nginx", GitHubAppID: 123}
	rec := httptest.NewRecorder()
	req := liteReqWithUser(http.MethodPost, "/api/saas/lite/enroll", `{"owner":"kubestellar","repo":"hive","installation_id":123}`, "lite-user")
	s.handleLiteEnroll(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var resp LiteEnrollmentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Action != "hosted_lite_spoke_requested" || resp.SpokeID == "" {
		t.Fatalf("response = %#v", resp)
	}
}

func TestWebhookMatcherIncludesLiteSpokes(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	s.registry.Hives = []RegistryEntry{
		{ID: "lite", Org: "org", Repos: []string{"repo"}, PrimaryRepo: "repo", Lite: true, HiveType: "lite"},
	}
	got := s.findHiveByOrgRepos("org", []string{"repo"})
	if got == nil || got.ID != "lite" {
		t.Fatalf("matched %#v, want lite spoke", got)
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

func TestLiteReportRouteGone(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	s.mux = http.NewServeMux()
	s.registerSaaSRoutes()
	rec := httptest.NewRecorder()
	req := reqWithUser(http.MethodGet, "/api/saas/lite/spoke-1/report", "", "lite-user")
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
