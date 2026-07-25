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
)

func covContribServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.RegisterAPI(testDeps(t))
	return s
}

// seedContributor writes a profile JSON into a temp contributors dir and points
// HIVE_CONTRIBUTORS_DIR at it for the duration of the test.
func covSeedContributor(t *testing.T, p *ContributorProfile) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, p.GitHubUsername+".json"), data, 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	return dir
}

// --- Contributor profile store helpers ---

func TestCovH_ContributorProfileStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)

	// save + load round trip.
	p := &ContributorProfile{GitHubUsername: "alice", ContributorID: "c-abc", TrustTier: "newcomer"}
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadContributorProfile("alice")
	if err != nil || got == nil || got.ContributorID != "c-abc" {
		t.Fatalf("load: %v %+v", err, got)
	}

	// path-traversal guards.
	if _, err := loadContributorProfile("../evil"); err == nil {
		t.Fatalf("expected traversal load to fail")
	}
	if err := saveContributorProfile(&ContributorProfile{GitHubUsername: "a/b"}); err == nil {
		t.Fatalf("expected traversal save to fail")
	}

	// listContributorProfiles returns the saved profile.
	profiles := listContributorProfiles()
	if len(profiles) != 1 || profiles[0].GitHubUsername != "alice" {
		t.Fatalf("list: %+v", profiles)
	}

	// findContributor by username (fast path) and by contributor_id (slow path).
	if findContributor("alice") == nil {
		t.Fatalf("findContributor by username failed")
	}
	if findContributor("c-abc") == nil {
		t.Fatalf("findContributor by id failed")
	}
	if findContributor("nope") != nil {
		t.Fatalf("expected nil for unknown contributor")
	}
	if findContributor("../x") != nil {
		t.Fatalf("expected nil for traversal id")
	}
}

// --- Contributor management handlers ---

func TestCovH_ContributorsListAndGet(t *testing.T) {
	covSeedContributor(t, &ContributorProfile{
		GitHubUsername: "bob", ContributorID: "c-bob", TrustTier: "contributor",
		RegistrationToken: "hash", TokenPlain: "secret",
	})
	s := covContribServer(t)

	rec := doGet(s, "/api/contributors")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	// Tokens must be scrubbed from the list response.
	if body := rec.Body.String(); containsStr(body, "secret") {
		t.Fatalf("token leaked in contributors list: %s", body)
	}

	rec = doGet(s, "/api/contributors/c-bob")
	if rec.Code != http.StatusOK {
		t.Fatalf("get by id: %d", rec.Code)
	}

	rec = doGet(s, "/api/contributors/unknown")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get unknown: expected 404, got %d", rec.Code)
	}
}

func TestCovH_ContributorTrust(t *testing.T) {
	covSeedContributor(t, &ContributorProfile{GitHubUsername: "carol", ContributorID: "c-carol", TrustTier: "newcomer"})
	s := covContribServer(t)

	// Unknown contributor → 404.
	rec := doPut(s, "/api/contributors/nope/trust", map[string]string{"tier": "trusted"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("trust unknown: expected 404, got %d", rec.Code)
	}

	// Invalid tier → 400.
	rec = doPut(s, "/api/contributors/c-carol/trust", map[string]string{"tier": "bogus"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("trust invalid tier: expected 400, got %d", rec.Code)
	}

	// Valid tier → 200.
	rec = doPut(s, "/api/contributors/c-carol/trust", map[string]string{"tier": "trusted"})
	if rec.Code != http.StatusOK {
		t.Fatalf("trust valid: expected 200, got %d", rec.Code)
	}
}

func TestCovH_ContributorRevokeAndDelete(t *testing.T) {
	covSeedContributor(t, &ContributorProfile{GitHubUsername: "dave", ContributorID: "c-dave", TrustTier: "contributor"})
	s := covContribServer(t)

	// Revoke unknown → 404, known → 200.
	if rec := doPost(s, "/api/contributors/nope/revoke", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown: expected 404, got %d", rec.Code)
	}
	if rec := doPost(s, "/api/contributors/c-dave/revoke", nil); rec.Code != http.StatusOK {
		t.Fatalf("revoke known: expected 200, got %d", rec.Code)
	}

	// Delete unknown → 404, known → 200.
	if rec := doDelete(s, "/api/contributors/nope", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: expected 404, got %d", rec.Code)
	}
	if rec := doDelete(s, "/api/contributors/c-dave", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete known: expected 200, got %d", rec.Code)
	}
}

func TestCovH_ContributeReissueToken(t *testing.T) {
	s := covContribServer(t)

	// No / malformed Authorization header → 401 (token validation fails offline).
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/reissue-token", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reissue no-auth: expected 401, got %d", rec.Code)
	}

	// A Bearer token still fails validation offline (no GitHub) → 401.
	req = httptest.NewRequest(http.MethodPost, "/api/contribute/reissue-token", nil)
	req.Header.Set("Authorization", "Bearer ghp_faketoken")
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reissue bad-token: expected 401, got %d", rec.Code)
	}
}

func TestCovH_ContributeStatusAndActivity(t *testing.T) {
	s := covContribServer(t)
	if rec := doGet(s, "/api/contribute/status"); rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	if rec := doGet(s, "/api/contribute/activity"); rec.Code != http.StatusOK {
		t.Fatalf("activity: %d", rec.Code)
	}
}

// --- Federation registry / hives register ---

func TestCovH_HivesRegisterValidation(t *testing.T) {
	t.Setenv("HIVE_FEDERATION_REGISTRY_PATH", filepath.Join(t.TempDir(), "fed.json"))
	s := covContribServer(t)

	// Bad body → 400.
	req := httptest.NewRequest(http.MethodPost, "/api/hives/register", io.NopCloser(stringReader("{bad")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}

	// Missing required fields → 400.
	if rec := doPost(s, "/api/hives/register", map[string]string{"project_name": "p"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fields: expected 400, got %d", rec.Code)
	}

	// Bad URL scheme → 400.
	if rec := doPost(s, "/api/hives/register", map[string]string{
		"project_name": "p", "org": "o", "hub_url": "ftp://x",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad scheme: expected 400, got %d", rec.Code)
	}

	// Private URL → 400 (localhost is private).
	if rec := doPost(s, "/api/hives/register", map[string]string{
		"project_name": "p", "org": "o", "hub_url": "http://127.0.0.1:9999",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("private url: expected 400, got %d", rec.Code)
	}
}

// --- Federation registry load/save helpers ---

func TestCovH_FederationRegistryLoadSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fed.json")
	t.Setenv("HIVE_FEDERATION_REGISTRY_PATH", path)

	// Load with no file → empty registry.
	reg := loadFederationRegistry()
	if reg == nil {
		t.Fatalf("expected non-nil registry")
	}
	reg.Hives = append(reg.Hives, FederationHive{ID: "hive-o-p", ProjectName: "p", Org: "o", HubURL: "https://x"})
	if err := saveFederationRegistry(reg); err != nil {
		t.Fatalf("save federation registry: %v", err)
	}
	// Reload sees the persisted hive.
	reg2 := loadFederationRegistry()
	if len(reg2.Hives) != 1 || reg2.Hives[0].ID != "hive-o-p" {
		t.Fatalf("reload mismatch: %+v", reg2)
	}
}

// --- small local helpers (uniquely named) ---

func containsStr(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

type covStringReader struct {
	s string
	i int
}

func stringReader(s string) *covStringReader { return &covStringReader{s: s} }

func (r *covStringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}
