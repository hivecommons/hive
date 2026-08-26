package hub

// Branch coverage for the lite-enrollment helpers that the handler-level tests
// leave untouched: ACMM capping, bearer-token extraction, dashboard URL
// derivation, spoke/profile matching, the SSRF host gate, and the registry
// projection of a lite spoke. These are the pieces the enrollment security
// posture rests on (which token is trusted, which hosts are reachable, what
// ACMM a lite spoke may claim), so each decision point gets a direct test.

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCappedLiteACMM(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{-3, liteDefaultACMMLevel},
		{0, liteDefaultACMMLevel},
		{1, 1},
		{liteMaxACMMLevel, liteMaxACMMLevel},
		{liteMaxACMMLevel + 1, liteMaxACMMLevel},
		{99, liteMaxACMMLevel},
	}
	for _, tt := range tests {
		if got := cappedLiteACMM(tt.in); got != tt.want {
			t.Errorf("cappedLiteACMM(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestBearerTokenFromRequest(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"no headers", nil, ""},
		{"hive header wins", map[string]string{"X-Hive-GitHub-Token": " tok-a ", "Authorization": "Bearer tok-b"}, "tok-a"},
		{"bearer", map[string]string{"Authorization": "Bearer tok-b "}, "tok-b"},
		{"non-bearer scheme ignored", map[string]string{"Authorization": "token tok-c"}, ""},
		{"blank hive header falls through", map[string]string{"X-Hive-GitHub-Token": "   ", "Authorization": "Bearer tok-d"}, "tok-d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			if got := bearerTokenFromRequest(req); got != tt.want {
				t.Errorf("bearerTokenFromRequest = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDashboardURLForRequest(t *testing.T) {
	s := &HubServer{}

	t.Run("forwarded headers win", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "hub.example.test")
		if got := s.dashboardURLForRequest(req); got != "https://hub.example.test/dashboard" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("plain http from request host", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "hub.local:8080"
		if got := s.dashboardURLForRequest(req); got != "http://hub.local:8080/dashboard" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("tls implies https", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "hub.local"
		req.TLS = &tls.ConnectionState{}
		if got := s.dashboardURLForRequest(req); got != "https://hub.local/dashboard" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("no host falls back to relative path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = ""
		if got := s.dashboardURLForRequest(req); got != "/dashboard" {
			t.Errorf("got %q", got)
		}
	})
}

func TestLiteSpokeProfileCompatible(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	s.registry.Hives = []RegistryEntry{{ID: "lite-spoke", Lite: true}}

	tests := []struct {
		name string
		h    *SaaSHive
		want bool
	}{
		{"nil hive", nil, false},
		{"registry-lite spoke always compatible", &SaaSHive{ID: "lite-spoke", ACMMLevel: 5}, true},
		{"unset acmm compatible", &SaaSHive{ID: "full-1"}, true},
		{"acmm at lite cap compatible", &SaaSHive{ID: "full-2", ACMMLevel: liteMaxACMMLevel}, true},
		{"full spoke above cap incompatible", &SaaSHive{ID: "full-3", ACMMLevel: liteMaxACMMLevel + 3}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.liteSpokeProfileCompatible(tt.h); got != tt.want {
				t.Errorf("liteSpokeProfileCompatible = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSpokeGitHubHostForLiteMatch(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()

	tests := []struct {
		name string
		h    *SaaSHive
		want string
	}{
		{"nil hive is public", nil, publicGitHubHost},
		{"empty host is public", &SaaSHive{ID: "h1"}, publicGitHubHost},
		{"explicit host normalized", &SaaSHive{ID: "h2", GitHubHost: "https://github.ibm.com/"}, "github.ibm.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.spokeGitHubHostForLiteMatch(tt.h); got != tt.want {
				t.Errorf("spokeGitHubHostForLiteMatch = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRegistrySpokeIsLite(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	s.registry.Hives = []RegistryEntry{
		{ID: "flag-lite", Lite: true},
		{ID: "type-lite", HiveType: "lite"},
		{ID: "full"},
	}
	for _, tt := range []struct {
		id   string
		want bool
	}{
		{"flag-lite", true},
		{"type-lite", true},
		{"full", false},
		{"missing", false},
	} {
		if got := s.registrySpokeIsLite(tt.id); got != tt.want {
			t.Errorf("registrySpokeIsLite(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestValidateLiteGitHubHost(t *testing.T) {
	ctx := context.Background()
	if err := validateLiteGitHubHost(ctx, ""); err != nil {
		t.Errorf("empty host: %v", err)
	}
	if err := validateLiteGitHubHost(ctx, publicGitHubHost); err != nil {
		t.Errorf("public host: %v", err)
	}
	for _, host := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.5", "169.254.169.254", "localhost"} {
		if err := validateLiteGitHubHost(ctx, host); err == nil {
			t.Errorf("private host %q accepted, want SSRF rejection", host)
		}
	}
}

// A hub with no GitHub App identity for the default cluster cannot discover an
// installation; enrollment must fail with the actionable "installation_id is
// required" message rather than a nil-pointer or a silent zero.
func TestDiscoverLiteInstallationRequiresAppIdentity(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	_, err := s.discoverLiteInstallation(context.Background(), "kubestellar")
	if err == nil || !strings.Contains(err.Error(), "installation_id is required") {
		t.Fatalf("err = %v, want installation_id-required error", err)
	}
}

func TestHandleLiteEnrollRejectsBadInput(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantMsg    string
	}{
		{"invalid json", `{`, http.StatusBadRequest, "invalid JSON"},
		{"missing owner", `{"repo":"hive"}`, http.StatusBadRequest, "owner and repo are required"},
		{"missing repo", `{"owner":"kubestellar"}`, http.StatusBadRequest, "owner and repo are required"},
		{"invalid owner name", `{"owner":"bad owner!","repo":"hive"}`, http.StatusBadRequest, "invalid repo"},
		{"invalid github_host label", `{"owner":"kubestellar","repo":"hive","github_host":"bad host!"}`, http.StatusBadRequest, "invalid github_host"},
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
			if !strings.Contains(rec.Body.String(), tt.wantMsg) {
				t.Fatalf("body %q missing %q", rec.Body.String(), tt.wantMsg)
			}
		})
	}
}

func TestHandleLiteEnrollRequiresBearerToken(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	rec := httptest.NewRecorder()
	// reqWithUser authenticates the session but carries no GitHub bearer token.
	req := reqWithUser(http.MethodPost, "/api/saas/lite/enroll", `{"owner":"kubestellar","repo":"hive"}`, "lite-user")
	s.handleLiteEnroll(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s, want 401", rec.Code, rec.Body.String())
	}
}

func TestHandleLiteEnrollTokenWithoutAdminAccess(t *testing.T) {
	tests := []struct {
		name   string
		verify func(context.Context, string, string, string, string) (bool, error)
	}{
		{"verifier says no access", func(context.Context, string, string, string, string) (bool, error) { return false, nil }},
		{"verifier errors", func(context.Context, string, string, string, string) (bool, error) {
			return false, context.DeadlineExceeded
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, cleanup := newLiteTestHub(t)
			defer cleanup()
			oldVerify := verifyLiteRepoAccess
			verifyLiteRepoAccess = tt.verify
			t.Cleanup(func() { verifyLiteRepoAccess = oldVerify })
			rec := httptest.NewRecorder()
			req := liteReqWithUser(http.MethodPost, "/api/saas/lite/enroll", `{"owner":"kubestellar","repo":"hive","installation_id":1}`, "lite-user")
			s.handleLiteEnroll(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d body=%s, want 403", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleLiteEnrollQuotaReached(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	u := ensureSaaSUser("lite-user")
	u.SaaSQuota = 1
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	if err := saveSaaSHive(&SaaSHive{
		ID: "spoke-existing", Owner: "lite-user", Org: "otherorg", Repos: []string{"r"}, PrimaryRepo: "r",
		ACMMLevel: 2, Status: statusAssigned,
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := liteReqWithUser(http.MethodPost, "/api/saas/lite/enroll", `{"owner":"kubestellar","repo":"hive","installation_id":1}`, "lite-user")
	s.handleLiteEnroll(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "quota reached") {
		t.Fatalf("status = %d body=%s, want 400 quota reached", rec.Code, rec.Body.String())
	}
}

// Enrollment without an installation_id on a hub that cannot auto-discover one
// must fail actionably at the discovery step, not provision a broken spoke.
func TestHandleLiteEnrollDiscoveryFailureSurfaced(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	u := ensureSaaSUser("lite-user")
	u.SaaSQuota = 1
	if err := saveSaaSUser(u); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := liteReqWithUser(http.MethodPost, "/api/saas/lite/enroll", `{"owner":"kubestellar","repo":"hive"}`, "lite-user")
	s.handleLiteEnroll(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "installation_id is required") {
		t.Fatalf("status = %d body=%s, want 400 installation_id-required", rec.Code, rec.Body.String())
	}
}

func TestUpdateRegistryForLiteSpoke(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()

	// nil is a no-op, not a panic.
	s.updateRegistryForLiteSpoke(nil)
	if len(s.registry.Hives) != 0 {
		t.Fatalf("nil spoke mutated registry: %#v", s.registry.Hives)
	}

	h := &SaaSHive{
		ID: "lite-1", Owner: "lite-user", ProjectName: "Hive lite: o/r",
		Org: "o", Repos: []string{"r"}, PrimaryRepo: "r",
		ACMMLevel: 5, CreatedAt: "2026-08-01T00:00:00Z", GitHubHost: "github.ibm.com",
	}
	s.updateRegistryForLiteSpoke(h)
	if len(s.registry.Hives) != 1 {
		t.Fatalf("registry entries = %d, want 1", len(s.registry.Hives))
	}
	got := s.registry.Hives[0]
	if !got.Lite || got.HiveType != "lite" || got.GovernorMode != "ADVISORY" {
		t.Errorf("lite projection wrong: %#v", got)
	}
	if got.ACMMLevel != liteMaxACMMLevel {
		t.Errorf("ACMMLevel = %d, want capped %d", got.ACMMLevel, liteMaxACMMLevel)
	}
	if got.RegisteredAt != h.CreatedAt || got.GitHubHost != "github.ibm.com" || got.IsPublic {
		t.Errorf("registry entry fields wrong: %#v", got)
	}
	if got.LiteConfig == nil || got.LiteConfig.ACMMLevel != liteMaxACMMLevel || !got.LiteConfig.Advisory || !got.LiteConfig.ZeroSecrets {
		t.Errorf("lite config wrong: %#v", got.LiteConfig)
	}

	// Re-projecting the same spoke updates in place and preserves liveness
	// fields the projection does not own.
	s.registry.Hives[0].LastHeartbeat = "2026-08-25T00:00:00Z"
	s.registry.Hives[0].DashboardURL = "https://lite-1.example.test"
	s.registry.Hives[0].Online = true
	h.Repos = append(h.Repos, "r2")
	s.updateRegistryForLiteSpoke(h)
	if len(s.registry.Hives) != 1 {
		t.Fatalf("update duplicated entry: %d entries", len(s.registry.Hives))
	}
	got = s.registry.Hives[0]
	if len(got.Repos) != 2 || got.Repos[1] != "r2" {
		t.Errorf("repos not updated: %#v", got.Repos)
	}
	if got.LastHeartbeat != "2026-08-25T00:00:00Z" || got.DashboardURL != "https://lite-1.example.test" || !got.Online {
		t.Errorf("liveness fields not preserved: %#v", got)
	}
}
