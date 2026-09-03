package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// --- contributeSurface ---

func TestContributeSurface_HubMode(t *testing.T) {
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	deps.Config.Dashboard.HubProxied = false
	s.RegisterAPI(deps)

	if got := s.contributeSurface(); got != surfaceHub {
		t.Fatalf("contributeSurface() = %q, want %q", got, surfaceHub)
	}
}

func TestContributeSurface_SpokeMode(t *testing.T) {
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	deps.Config.Dashboard.HubProxied = true
	s.RegisterAPI(deps)

	if got := s.contributeSurface(); got != surfaceSpoke {
		t.Fatalf("contributeSurface() = %q, want %q", got, surfaceSpoke)
	}
}

func TestContributeSurface_NilDeps(t *testing.T) {
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// deps is nil: contributeSurface nil-guards both s.deps and s.deps.Config.
	if got := s.contributeSurface(); got != surfaceHub {
		t.Fatalf("contributeSurface() with nil deps = %q, want %q (fallback)", got, surfaceHub)
	}
}

// --- handleContributeLimits ---

func TestContributeLimits_AnonymousReturnsOnlyTiers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	deps.Config.Hub.TierLimits = map[string]config.TierRate{
		"newcomer":    {MaxPerHour: 3, MaxPerDay: 10, MaxConcurrent: 1},
		"contributor": {MaxPerHour: 10, MaxPerDay: 50, MaxConcurrent: 2},
	}
	s.RegisterAPI(deps)

	rec := doGet(s, "/api/contribute/limits")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/contribute/limits: %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tiers, ok := resp["tiers"].([]interface{})
	if !ok || len(tiers) == 0 {
		t.Fatalf("expected non-empty tiers array, got %v", resp["tiers"])
	}
	if _, hasYou := resp["you"]; hasYou {
		t.Error("anonymous request should NOT have a 'you' block")
	}
}

func TestContributeLimits_AuthenticatedIncludesYou(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	// Seed a profile for the authenticated user.
	p := &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "newcomer"}
	data, _ := json.MarshalIndent(p, "", "  ")
	os.WriteFile(filepath.Join(dir, "alice.json"), data, 0o644)

	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	deps.Config.Hub.TierLimits = map[string]config.TierRate{
		"newcomer": {MaxPerHour: 3, MaxPerDay: 10, MaxConcurrent: 1},
	}
	s.RegisterAPI(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/limits", nil)
	req.Header.Set("X-Hive-User", "alice")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/contribute/limits (authenticated): %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	you, ok := resp["you"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected 'you' block, got %v", resp["you"])
	}
	if you["username"] != "alice" {
		t.Errorf("you.username = %v, want alice", you["username"])
	}
	if you["tier"] != "newcomer" {
		t.Errorf("you.tier = %v, want newcomer", you["tier"])
	}
}

func TestContributeLimits_NilTierLimitsReturnsEmpty(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	deps.Config.Hub.TierLimits = nil
	s.RegisterAPI(deps)

	rec := doGet(s, "/api/contribute/limits")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	tiers := resp["tiers"]
	if tiers != nil {
		if arr, ok := tiers.([]interface{}); ok && len(arr) != 0 {
			t.Errorf("expected nil/empty tiers with nil config, got %v", tiers)
		}
	}
}

// --- handleContributeOpportunistic ---

func TestContributeOpportunistic_NilHub(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	s.RegisterAPI(deps)

	rec := doGet(s, "/api/contribute/opportunistic")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	opp, ok := resp["opportunistic"].([]interface{})
	if !ok {
		t.Fatalf("expected opportunistic array, got %v", resp["opportunistic"])
	}
	if len(opp) != 0 {
		t.Errorf("nil hub should yield empty opportunistic, got %d items", len(opp))
	}
}

// --- handleContributeQueue ---

func TestContributeQueue_NilHub(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	s.RegisterAPI(deps)

	rec := doGet(s, "/api/contribute/queue")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	queue, ok := resp["queue"].([]interface{})
	if !ok {
		t.Fatalf("expected queue array, got %v", resp["queue"])
	}
	if len(queue) != 0 {
		t.Errorf("nil hub should yield empty queue, got %d items", len(queue))
	}
}

// --- requireContributorWrite ---

// TestRequireContributorWrite_EmptyRoleForbidden pins v4's C5 fail-closed fix.
// This test previously asserted the PRE-fix behavior (an absent X-Hive-Role header
// defaulted to "owner" and PASSED), which is exactly the CWE-862 hole v4 closed:
// an unauthenticated caller reaching the pod with no role header could
// promote/revoke/delete contributors. Reconciled in the v2→v4 sync to require the
// hardened, fail-closed behavior — a missing role must be REJECTED. (v4's
// dedicated end-to-end coverage is TestContributorWrite_MissingRoleForbidden.)
func TestRequireContributorWrite_EmptyRoleForbidden(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	deps.Config.Dashboard.HubProxied = false
	s.RegisterAPI(deps)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	if s.requireContributorWrite(rec, req) {
		t.Fatalf("empty role must be rejected (fail closed), got true")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for missing role, got %d", rec.Code)
	}
}

func TestRequireContributorWrite_EmptyRoleForbiddenOnHubProxied(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	deps.Config.Dashboard.HubProxied = true
	s.RegisterAPI(deps)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	if s.requireContributorWrite(rec, req) {
		t.Fatalf("hub-proxied empty role should fail closed, got true")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestRequireContributorWrite_ReadRoleForbidden(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	s.RegisterAPI(deps)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Hive-Role", "read")
	rec := httptest.NewRecorder()
	if s.requireContributorWrite(rec, req) {
		t.Fatalf("read role should be rejected, got true")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestRequireContributorWrite_ReadWriteRolePasses(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	s.RegisterAPI(deps)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Hive-Role", "read-write")
	rec := httptest.NewRecorder()
	if !s.requireContributorWrite(rec, req) {
		t.Fatalf("read-write role should pass, got false")
	}
}

// --- handleContributeInterests unauthorized/forbidden paths ---

func TestContributeInterests_Unauthorized(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	s.RegisterAPI(deps)

	rec := doGet(s, "/api/contribute/interests")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /api/contribute/interests: want 401, got %d", rec.Code)
	}
}

func TestContributeInterests_NoProfile(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := testDeps(t)
	s.RegisterAPI(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/interests", nil)
	req.Header.Set("X-Hive-User", "ghost")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/contribute/interests with no profile: want 403, got %d", rec.Code)
	}
}
