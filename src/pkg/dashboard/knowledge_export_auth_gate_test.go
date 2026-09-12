package dashboard

// Tests for allowKnowledgeExport (api_knowledge.go), the authentication gate
// in front of every knowledge-export endpoint. Prior coverage exercised it
// only incidentally; these tests pin each branch of the gate directly:
//
//   1. an internal caller (X-Hive-Role already set by upstream auth) passes
//      without any contributor lookup;
//   2. a contributor with a valid registration token passes AND has the
//      X-Hive-User / X-Hive-Role headers stamped for downstream handlers;
//   3. a REVOKED contributor is refused 401 even with a valid token — the
//      revocation fence must hold on this surface;
//   4. no credential at all is refused 401.
//
// The contributor store is redirected to a temp dir via HIVE_CONTRIBUTORS_DIR
// in every subtest — including the ones that expect refusal — so the tests
// never read or create /data/contributors on a live host.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// writeContributorProfileFixture persists a minimal valid profile the way the
// store lays it out on disk, returning the PLAIN token a client would present.
// listContributorProfiles requires github_username and contributor_id to be
// non-empty, and matches on the sha256 hash of the registration token.
func writeContributorProfileFixture(t *testing.T, dir, username, tier string) string {
	t.Helper()
	plain := "tok-" + username + "-plaintext"
	p := ContributorProfile{
		GitHubUsername:    username,
		ContributorID:     "c-" + username,
		RegistrationToken: sha256Hex(plain),
		TrustTier:         tier,
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, username+".json"), data, 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	return plain
}

func TestAllowKnowledgeExportInternalRoleHeaderPasses(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	s := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/api/knowledge/export", nil)
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	rec := httptest.NewRecorder()

	if !s.allowKnowledgeExport(rec, req) {
		t.Fatal("a caller already carrying X-Hive-Role must pass the gate")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("gate wrote status %d on an allowed request", rec.Code)
	}
	// The pre-existing role must be left alone, not clobbered to read.
	if got := req.Header.Get("X-Hive-Role"); got != config.RoleOwner {
		t.Errorf("X-Hive-Role = %q, want the original %q", got, config.RoleOwner)
	}
}

func TestAllowKnowledgeExportValidContributorTokenStampsIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	plain := writeContributorProfileFixture(t, dir, "octocat", "trusted")
	s := &Server{}

	for _, scheme := range []string{"Bearer ", "token "} {
		t.Run(scheme, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/knowledge/export", nil)
			req.Header.Set("Authorization", scheme+plain)
			rec := httptest.NewRecorder()

			if !s.allowKnowledgeExport(rec, req) {
				t.Fatal("a registered, non-revoked contributor must pass the gate")
			}
			// Downstream handlers trust these two headers; the gate must stamp
			// the resolved identity and the READ role, nothing higher.
			if got := req.Header.Get("X-Hive-User"); got != "octocat" {
				t.Errorf("X-Hive-User = %q, want %q", got, "octocat")
			}
			if got := req.Header.Get("X-Hive-Role"); got != config.RoleRead {
				t.Errorf("X-Hive-Role = %q, want %q", got, config.RoleRead)
			}
		})
	}
}

func TestAllowKnowledgeExportRevokedContributorRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	plain := writeContributorProfileFixture(t, dir, "banished", "revoked")
	s := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/api/knowledge/export", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()

	if s.allowKnowledgeExport(rec, req) {
		t.Fatal("a revoked contributor must NOT pass the gate, even with a valid token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	// A refusal must not leak a partially-stamped identity to any downstream
	// handler that inspects the request afterwards.
	if got := req.Header.Get("X-Hive-Role"); got != "" {
		t.Errorf("refused request carries X-Hive-Role %q; want unset", got)
	}
}

func TestAllowKnowledgeExportAnonymousRefused(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	s := &Server{}

	cases := []struct {
		name  string
		authz string
	}{
		{"no authorization header", ""},
		{"unknown token", "Bearer no-such-token"},
		{"unsupported scheme", "Basic dXNlcjpwYXNz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/knowledge/export", nil)
			if tc.authz != "" {
				req.Header.Set("Authorization", tc.authz)
			}
			rec := httptest.NewRecorder()

			if s.allowKnowledgeExport(rec, req) {
				t.Fatal("an unauthenticated caller must not pass the gate")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
		})
	}
}
