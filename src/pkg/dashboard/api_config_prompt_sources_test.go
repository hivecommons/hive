package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	promptSourcesPath         = "/api/config/prompt-sources"
	promptSourcesOwnerRepoPut = "/api/config/prompt-sources/owner-repos"
)

func doPromptSourcesAs(s *Server, method, path, role string, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&b).Encode(body)
	}
	req := httptest.NewRequest(method, path, &b)
	req.Header.Set("Content-Type", "application/json")
	if role == config.RoleOwner {
		markOwnerRequest(req)
	} else if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	req.Header.Set("X-Hive-User", "alice")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodePromptSources(t *testing.T, rec *httptest.ResponseRecorder) promptSourcesResponse {
	t.Helper()
	var out promptSourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	return out
}

func TestPromptSourcesNonOwnersForbidden(t *testing.T) {
	s := covApiServer(t)
	body := map[string]any{"repo": "myorg/repo1", "allowed": true, "acknowledge_trust": true}
	for _, role := range []string{config.RoleRead, config.RoleReadWrite, config.RoleMerger, ""} {
		if rec := doPromptSourcesAs(s, http.MethodGet, promptSourcesPath, role, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("GET as %q: status %d, want 403", role, rec.Code)
		}
		if rec := doPromptSourcesAs(s, http.MethodPut, promptSourcesOwnerRepoPut, role, body); rec.Code != http.StatusForbidden {
			t.Fatalf("PUT as %q: status %d, want 403", role, rec.Code)
		}
	}
	// An unverified owner header is not enough.
	req := httptest.NewRequest(http.MethodPut, promptSourcesOwnerRepoPut, strings.NewReader(`{"repo":"myorg/repo1","allowed":true,"acknowledge_trust":true}`))
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unverified owner: status %d, want 403", rec.Code)
	}
	if s.deps.Config.GitHubPromptAllowed("myorg/repo1") {
		t.Fatal("a refused request must not approve the repo")
	}
}

func TestPromptSourcesOwnerApproveAndRevoke(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	skipped := 0
	s.deps.SkipReloadFunc = func() { skipped++ }

	rec := doPromptSourcesAs(s, http.MethodGet, promptSourcesPath, config.RoleOwner, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	got := decodePromptSources(t, rec)
	if len(got.Repos) != 1 || got.Repos[0].Repo != "myorg/repo1" || !got.Repos[0].Managed || got.Repos[0].Allowed {
		t.Fatalf("initial repos = %+v", got.Repos)
	}
	if got.TrustNotice == "" || got.SeedOnlyNote == "" || got.SeedAllowlist == nil {
		t.Fatalf("response missing notices: %+v", got)
	}

	// Approval without the acknowledgement is refused.
	rec = doPromptSourcesAs(s, http.MethodPut, promptSourcesOwnerRepoPut, config.RoleOwner, map[string]any{"repo": "myorg/repo1", "allowed": true})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "acknowledge_trust") {
		t.Fatalf("no ack: %d %s", rec.Code, rec.Body.String())
	}
	if cfg.GitHubPromptAllowed("myorg/repo1") {
		t.Fatal("unacknowledged approval must not apply")
	}

	rec = doPromptSourcesAs(s, http.MethodPut, promptSourcesOwnerRepoPut, config.RoleOwner, map[string]any{"repo": "MyOrg/Repo1", "allowed": true, "acknowledge_trust": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	got = decodePromptSources(t, rec)
	if row := got.Repos[0]; !row.Allowed || !row.Owner || row.ApprovedBy != "alice" || row.ApprovedAt == "" {
		t.Fatalf("approved row = %+v", row)
	}
	if !cfg.GitHubPromptAllowed("myorg/repo1") || !cfg.GitHubDefinitionAllowed("myorg/repo1") {
		t.Fatal("approval must open both prompt and definition gates")
	}
	if cfg.Variables.Security.AllowGitHubPrompt {
		t.Fatal("approval must not flip the seed-wide flag")
	}
	if skipped == 0 {
		t.Fatal("approval must skip the next watcher reload")
	}
	entries := s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), promptSourceAuditAction)
	if len(entries) != 1 || !strings.Contains(entries[0].Detail, "myorg/repo1") || !strings.Contains(entries[0].Detail, "true") {
		t.Fatalf("approval audit = %+v", entries)
	}

	// Approving again changes nothing and is not re-audited.
	if rec := doPromptSourcesAs(s, http.MethodPut, promptSourcesOwnerRepoPut, config.RoleOwner, map[string]any{"repo": "myorg/repo1", "allowed": true, "acknowledge_trust": true}); rec.Code != http.StatusOK {
		t.Fatalf("re-approve: %d", rec.Code)
	}
	if n := len(s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), promptSourceAuditAction)); n != 1 {
		t.Fatalf("re-approve audited: %d entries", n)
	}

	// Revoke.
	rec = doPromptSourcesAs(s, http.MethodPut, promptSourcesOwnerRepoPut, config.RoleOwner, map[string]any{"repo": "myorg/repo1", "allowed": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if cfg.GitHubPromptAllowed("myorg/repo1") {
		t.Fatal("revoke must close the gate")
	}
	if n := len(s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), promptSourceAuditAction)); n != 2 {
		t.Fatalf("revoke not audited: %d entries", n)
	}
	// Revoking a repo that is not approved is a quiet no-op.
	if rec := doPromptSourcesAs(s, http.MethodPut, promptSourcesOwnerRepoPut, config.RoleOwner, map[string]any{"repo": "myorg/repo1", "allowed": false}); rec.Code != http.StatusOK {
		t.Fatalf("second revoke: %d", rec.Code)
	}
	if n := len(s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), promptSourceAuditAction)); n != 2 {
		t.Fatalf("no-op revoke audited: %d entries", n)
	}
}

func TestPromptSourcesRefusesNonManagedAndBadBodies(t *testing.T) {
	s := covApiServer(t)
	rec := doPromptSourcesAs(s, http.MethodPut, promptSourcesOwnerRepoPut, config.RoleOwner, map[string]any{"repo": "evil/prompts", "allowed": true, "acknowledge_trust": true})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "config volume") {
		t.Fatalf("non-managed: %d %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.GitHubPromptAllowed("evil/prompts") || len(s.deps.Config.OwnerPromptRepos()) != 0 {
		t.Fatal("non-managed repo must not be approved")
	}
	for name, raw := range map[string]string{
		"malformed":   "{nope",
		"no repo":     `{"allowed":true,"acknowledge_trust":true}`,
		"no allowed":  `{"repo":"myorg/repo1","acknowledge_trust":true}`,
		"bad slug":    `{"repo":"repo1","allowed":true,"acknowledge_trust":true}`,
		"blank repo":  `{"repo":"  ","allowed":false}`,
		"nested slug": `{"repo":"a/b/c","allowed":true,"acknowledge_trust":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, promptSourcesOwnerRepoPut, strings.NewReader(raw))
			markOwnerRequest(req)
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
	if n := len(s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), promptSourceAuditAction)); n != 0 {
		t.Fatalf("refused requests audited: %d", n)
	}
}

func TestPromptSourcesSectionShowsSeedAndInertApprovals(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1", "MyOrg/repo1", "repo2"}}}
	cfg.Variables.Security.AllowGitHubPrompt = true
	cfg.Variables.Security.GitHubPromptAllowlist = []string{"myorg/repo2"}
	cfg.Variables.Security.OwnerGitHubPromptRepos = []config.OwnerPromptRepo{
		{Repo: "myorg/repo1", ApprovedBy: "alice", ApprovedAt: "2026-11-16T00:00:00Z"},
		{Repo: "gone/repo", ApprovedBy: "bob"},
	}
	got := promptSourcesSection(cfg)
	if len(got.Repos) != 3 {
		t.Fatalf("rows = %+v", got.Repos)
	}
	if r := got.Repos[0]; r.Repo != "myorg/repo1" || !r.Owner || !r.Allowed || r.ApprovedBy != "alice" {
		t.Fatalf("owner row = %+v", r)
	}
	if r := got.Repos[1]; r.Repo != "myorg/repo2" || !r.Seed || !r.Allowed || r.Owner {
		t.Fatalf("seed row = %+v", r)
	}
	if r := got.Repos[2]; r.Repo != "gone/repo" || r.Managed || !r.Owner || r.Allowed {
		t.Fatalf("inert row = %+v", r)
	}
	if !got.SeedEnabled || len(got.SeedAllowlist) != 1 {
		t.Fatalf("seed fields = %+v", got)
	}
}

func TestPromptSourcesConfigUnavailable(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config = nil
	if rec := doPromptSourcesAs(s, http.MethodGet, promptSourcesPath, config.RoleOwner, nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET: %d", rec.Code)
	}
	if rec := doPromptSourcesAs(s, http.MethodPut, promptSourcesOwnerRepoPut, config.RoleOwner, map[string]any{"repo": "a/b", "allowed": true}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT: %d", rec.Code)
	}
}
