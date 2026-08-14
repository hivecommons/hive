package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Package-level tests for handlePublicHiveStatus (the anonymous, read-only
// JSON endpoint for is_public:true hives), handlePublicHivePreview (the
// browser-facing HTML page that fetches it client-side), and a regression
// test confirming handleHiveStatus (the full, owner-only record) is
// unchanged by this fix.

func newPublicStatusHub() *HubServer {
	return &HubServer{
		logger:         slog.Default(),
		hubSecret:      testHubSecret,
		keyGenerations: legacyGenerationSet(testHubSecret),
	}
}

// TestHandlePublicHiveStatusAnonymousOnPublicHive verifies an anonymous
// caller gets 200 with ONLY the safe allowlist fields for an is_public:true
// hive, and that no sensitive field ever appears in the response body.
func TestHandlePublicHiveStatusAnonymousOnPublicHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{
		ID:          "hosted-available-oke-01-placeholder-bb95",
		Owner:       "some-owner-username",
		ProjectName: "My Public Hive",
		Org:         "kubestellar",
		PrimaryRepo: "kubestellar/hive",
		ACMMLevel:   3,
		Status:      "claimed",
		IsPublic:    true,
		GitHubHost:  "github.ibm.com", // sensitive: must NOT appear
		ClusterID:   "cluster-internal-id-42",
		Subdomain:   "internal-subdomain-secret",
		PendingAppConfig: &PendingAppIdentity{
			AppID:          123456,
			AppSlug:        "some-app-slug",
			InstallationID: 987654,
			APIURL:         "https://api.github.ibm.com",
			BaseURL:        "https://github.ibm.com",
		},
		Error:               "internal kubectl exec failed: connection refused to 10.0.0.5:6443",
		OCIFileSystemID:     "ocid1.filesystem.internal",
		RequestedGitHubHost: "github.internal.example.com",
		PendingRequests: []PendingAccessRequest{
			{Username: "some-requester", Status: "pending"},
		},
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}

	s := newPublicStatusHub()
	s.registry.Hives = []RegistryEntry{
		{
			ID:               h.ID,
			Version:          "v4.2.1",
			GitBranch:        "v4",
			Online:           true,
			AgentCount:       2,
			ActionableIssues: 5,
			ActionablePRs:    2,
			ClusterID:        "cluster-internal-id-42",             // sensitive: must NOT appear
			Namespace:        "hive-hosted-secret-namespace",       // sensitive: must NOT appear
			Health:           map[string]any{"internal": "detail"}, // sensitive: must NOT appear
			Agents: []AgentSummary{
				{Name: "scanner", State: "running", Paused: false, StartedAt: "2026-08-01T00:00:00Z"},
				{Name: "reviewer", State: "paused", Paused: true, NeedsLogin: true},
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/saas/hives/"+h.ID+"/public-status", nil)
	req = setPathValue(req, "id", h.ID)
	rec := httptest.NewRecorder()

	s.handlePublicHiveStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()

	// Sensitive substrings must be absent from the raw JSON, regardless of
	// field naming — this catches both "wrong field name" and "embedded the
	// whole struct" regressions.
	sensitive := []string{
		"github.ibm.com",
		"cluster-internal-id-42",
		"internal-subdomain-secret",
		"some-app-slug",
		"987654",
		"123456",
		"api.github.ibm.com",
		"connection refused",
		"ocid1.filesystem.internal",
		"github.internal.example.com",
		"some-requester",
		"hive-hosted-secret-namespace",
		"\"internal\":\"detail\"",
		"some-owner-username",
		"needsLogin",
		"startedAt",
	}
	for _, s := range sensitive {
		if strings.Contains(body, s) {
			t.Errorf("response body leaked sensitive content %q; body=%s", s, body)
		}
	}

	var out PublicHiveStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if out.ID != h.ID {
		t.Errorf("ID = %q, want %q", out.ID, h.ID)
	}
	if out.ProjectName != "My Public Hive" {
		t.Errorf("ProjectName = %q", out.ProjectName)
	}
	if out.ACMMLevel != 3 {
		t.Errorf("ACMMLevel = %d, want 3", out.ACMMLevel)
	}
	if !out.IsPublic {
		t.Errorf("IsPublic = false, want true")
	}
	if out.Version != "v4.2.1" {
		t.Errorf("Version = %q, want v4.2.1", out.Version)
	}
	if !out.Online {
		t.Errorf("Online = false, want true")
	}
	if out.AgentCount != 2 {
		t.Errorf("AgentCount = %d, want 2", out.AgentCount)
	}
	if len(out.Agents) != 2 {
		t.Fatalf("Agents len = %d, want 2", len(out.Agents))
	}
	if out.Agents[0].Name != "scanner" || out.Agents[0].State != "running" {
		t.Errorf("Agents[0] = %+v", out.Agents[0])
	}
	if out.Agents[1].Name != "reviewer" || !out.Agents[1].Paused {
		t.Errorf("Agents[1] = %+v", out.Agents[1])
	}
}

// TestHandlePublicHiveStatusAnonymousOnPrivateHive verifies an anonymous
// caller (and a non-owner authenticated caller) gets 404 on a hive that is
// NOT is_public, and that the response cannot be distinguished from a
// genuinely missing hive.
func TestHandlePublicHiveStatusAnonymousOnPrivateHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{
		ID:          "hosted-private-hive-1",
		Owner:       "the-owner",
		ProjectName: "Private Hive",
		IsPublic:    false,
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}

	s := newPublicStatusHub()

	cases := []struct {
		name     string
		username string
	}{
		{name: "anonymous", username: ""},
		{name: "unrelated authenticated user", username: "some-other-user"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.username == "" {
				req = httptest.NewRequest(http.MethodGet, "/api/saas/hives/"+h.ID+"/public-status", nil)
			} else {
				req = reqWithUser(http.MethodGet, "/api/saas/hives/"+h.ID+"/public-status", "", tc.username)
			}
			req = setPathValue(req, "id", h.ID)
			rec := httptest.NewRecorder()

			s.handlePublicHiveStatus(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body=%q)", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "the-owner") || strings.Contains(rec.Body.String(), "Private Hive") {
				t.Errorf("404 body leaked private hive data: %s", rec.Body.String())
			}
		})
	}
}

// TestHandlePublicHiveStatusOwnerOnPrivateHive verifies the hive's owner CAN
// still reach the safe-subset view of their own private hive through this
// endpoint (falls through the is_public gate via userIsHiveOwner), so an
// owner previewing a not-yet-public hive isn't blocked.
func TestHandlePublicHiveStatusOwnerOnPrivateHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{
		ID:          "hosted-private-hive-2",
		Owner:       "the-owner",
		ProjectName: "Private Hive 2",
		IsPublic:    false,
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "the-owner", Hives: map[string]string{}}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}

	s := newPublicStatusHub()

	req := reqWithUser(http.MethodGet, "/api/saas/hives/"+h.ID+"/public-status", "", "the-owner")
	req = setPathValue(req, "id", h.ID)
	rec := httptest.NewRecorder()

	s.handlePublicHiveStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestHandlePublicHiveStatusNotFound verifies a genuinely nonexistent hive
// ID 404s, same as a private hive — no information disclosure via status
// code alone.
func TestHandlePublicHiveStatusNotFound(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := newPublicStatusHub()
	req := httptest.NewRequest(http.MethodGet, "/api/saas/hives/does-not-exist/public-status", nil)
	req = setPathValue(req, "id", "does-not-exist")
	rec := httptest.NewRecorder()

	s.handlePublicHiveStatus(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestHandleHiveStatusStillOwnerGated is a regression test confirming the
// FULL, owner-only status route (handleHiveStatus, the raw SaaSHive record)
// is completely unchanged by this fix: it still 403s for a non-owner and
// still requires auth, exactly as #3692 left it.
func TestHandleHiveStatusStillOwnerGated(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{
		ID:          "hosted-owner-gated-hive",
		Owner:       "the-owner",
		ProjectName: "Owner Gated Hive",
		IsPublic:    true, // even a PUBLIC hive's full record stays owner-only
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "the-owner", Hives: map[string]string{}}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "not-the-owner", Hives: map[string]string{}}); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}

	s := newPublicStatusHub()

	t.Run("anonymous is forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/saas/hives/"+h.ID+"/status", nil)
		req = setPathValue(req, "id", h.ID)
		rec := httptest.NewRecorder()
		s.handleHiveStatus(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("non-owner is forbidden", func(t *testing.T) {
		req := reqWithUser(http.MethodGet, "/api/saas/hives/"+h.ID+"/status", "", "not-the-owner")
		req = setPathValue(req, "id", h.ID)
		rec := httptest.NewRecorder()
		s.handleHiveStatus(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body=%q)", rec.Code, rec.Body.String())
		}
	})

	t.Run("owner gets the full record", func(t *testing.T) {
		req := reqWithUser(http.MethodGet, "/api/saas/hives/"+h.ID+"/status", "", "the-owner")
		req = setPathValue(req, "id", h.ID)
		rec := httptest.NewRecorder()
		s.handleHiveStatus(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
		}
	})
}

// TestHandlePublicHivePreviewAnonymous verifies the preview PAGE — the
// actual browser-facing surface a shared public-hive link lands on — is
// reachable anonymously, serves HTML, and carries NO hive data inline (it
// fetches /public-status client-side, so the page itself cannot regress the
// allowlist no matter what a hive's fields contain).
func TestHandlePublicHivePreviewAnonymous(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := newPublicStatusHub()

	req := httptest.NewRequest(http.MethodGet, "/api/saas/hives/some-hive-id/preview", nil)
	req = setPathValue(req, "id", "some-hive-id")
	rec := httptest.NewRecorder()

	s.handlePublicHivePreview(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/public-status") {
		t.Errorf("preview page does not reference /public-status: %s", body)
	}
	// The page is a static shell — no hive ID, name, or any other
	// server-interpolated value should appear in the HTML itself.
	if strings.Contains(body, "some-hive-id") {
		t.Errorf("preview page leaked the hive id server-side instead of resolving it client-side: %s", body)
	}
}

// TestPublicHiveRoutesRegisteredWithoutAuth is a route-table regression test:
// it asserts /public-status and /preview are registered WITHOUT
// requireAuth (same pattern as /open and /access-status), by exercising them
// through the real mux with no auth cookie/header at all and confirming
// neither is redirected to login or 401s outright (a 404 for an unknown
// hive is fine and expected).
func TestPublicHiveRoutesRegisteredWithoutAuth(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{
		logger:         slog.Default(),
		hubSecret:      testHubSecret,
		keyGenerations: legacyGenerationSet(testHubSecret),
		mux:            http.NewServeMux(),
		hubBanners:     make(map[string]*HubBannerEntry),
	}
	s.registerSaaSRoutes()

	for _, path := range []string{
		"/api/saas/hives/unknown-hive/public-status",
		"/api/saas/hives/unknown-hive/preview",
	} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("%s is auth-gated (401 with no credentials); it must be reachable anonymously", path)
			}
			if loc := rec.Header().Get("Location"); strings.Contains(loc, "/login") {
				t.Fatalf("%s redirected to login (%q); it must be reachable anonymously", path, loc)
			}
		})
	}
}
