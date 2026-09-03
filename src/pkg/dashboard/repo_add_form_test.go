package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// These tests cover the Repos-tab add-form backend guards:
//   - single-forge rejection on add (requirement #2)
//   - save rejected when repos exist but no default is set (requirement #4)
//   - the App-access probe endpoint's single-host + org-resolution behaviour
//     (requirement #3)

// Requirement #2: a hive on github.com must reject a repo pasted as
// host/org/repo. The canonical target must be configured as org + bare repo.
func TestGovernorRepos_RejectsCrossForge(t *testing.T) {
	srv := newFullServer(t)
	body := `{"repos":["github.ibm.com/acme/widget"],"primaryRepo":"widget"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 for a cross-forge repo", w.Code)
	}
	if !strings.Contains(w.Body.String(), "one GitHub host") {
		t.Errorf("expected single-host message, got: %s", w.Body.String())
	}
	// The hive's forge must be left untouched.
	if srv.deps.Config.GitHub.BaseURL != "" {
		t.Errorf("hive forge was mutated to %q", srv.deps.Config.GitHub.BaseURL)
	}
}

// A GHE hive accepts same-forge full URLs for org migration, but still rejects
// repos pasted from another GitHub host.
func TestGovernorRepos_GHEHiveHostRules(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.GitHub.BaseURL = "https://github.ibm.com"
	srv.deps.Config.GitHub.APIURL = "https://github.ibm.com/api/v3"

	// Same-forge full URL paste is an explicit org/repo target.
	ok := `{"repos":["https://github.ibm.com/acme/widget"],"primaryRepo":"widget"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/repos", strings.NewReader(ok))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("same-forge URL add: code = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if srv.deps.Config.Project.Org != "acme" {
		t.Fatalf("same-forge URL add org = %q, want acme", srv.deps.Config.Project.Org)
	}

	// public github.com paste on a GHE hive: rejected as mixing.
	bad := `{"repos":["github.com/acme/other"],"primaryRepo":"other"}`
	req = httptest.NewRequest("PUT", "/api/config/governor/repos", strings.NewReader(bad))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("cross-forge add on GHE hive: code = %d, want 400", w.Code)
	}
}

// Requirement #4: a save that leaves repos with no default (primary not among
// them) is rejected, and the pre-mutation config is restored intact.
func TestGovernorRepos_RejectsSaveWithNoDefault(t *testing.T) {
	srv := newFullServer(t)
	prevRepos := append([]string(nil), srv.deps.Config.Project.Repos...)
	prevPrimary := srv.deps.Config.Project.PrimaryRepo

	// Replace the repo set with repos that do NOT include the current default,
	// and do not send a new primary — the default is now dangling.
	body := `{"repos":["alpha","beta"]}`
	req := httptest.NewRequest("PUT", "/api/config/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 when no default is set", w.Code)
	}
	if !strings.Contains(w.Body.String(), "default repo") {
		t.Errorf("expected 'default repo' message, got: %s", w.Body.String())
	}
	// The rejected save must not leave a half-applied config in memory.
	if got := srv.deps.Config.Project.Repos; len(got) != len(prevRepos) || (len(got) > 0 && got[0] != prevRepos[0]) {
		t.Errorf("repos mutated on reject: got %v, want %v", got, prevRepos)
	}
	if srv.deps.Config.Project.PrimaryRepo != prevPrimary {
		t.Errorf("primary mutated on reject: got %q, want %q", srv.deps.Config.Project.PrimaryRepo, prevPrimary)
	}
}

// Removing the current default (dropping it from the repos list) without naming
// a replacement is rejected too — the same no-default guard.
func TestGovernorRepos_RejectsRemovingDefault(t *testing.T) {
	srv := newFullServer(t)
	// Baseline: testrepo is the only repo and the default. Submit a set that
	// drops it entirely and names no new default.
	body := `{"repos":["another"]}`
	req := httptest.NewRequest("PUT", "/api/config/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 when the default is removed with no replacement", w.Code)
	}

	// Same set WITH a valid new default is accepted.
	body = `{"repos":["another"],"primaryRepo":"another"}`
	req = httptest.NewRequest("PUT", "/api/config/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 once a new default is named (body: %s)", w.Code, w.Body.String())
	}
	if srv.deps.Config.Project.PrimaryRepo != "another" {
		t.Errorf("primary = %q, want 'another'", srv.deps.Config.Project.PrimaryRepo)
	}
}

// Requirement #3: the access-probe endpoint rejects a cross-forge repo before it
// even attempts an installation lookup (same single-host message).
func TestGovernorRepoCheckAccess_RejectsCrossForge(t *testing.T) {
	srv := newFullServer(t)
	body := `{"repo":"github.ibm.com/acme/widget"}`
	req := httptest.NewRequest("POST", "/api/config/governor/repos/check-access", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleGovernorRepoCheckAccess(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 for a cross-forge repo", w.Code)
	}
	if !strings.Contains(w.Body.String(), "one GitHub host") {
		t.Errorf("expected single-host message, got: %s", w.Body.String())
	}
}

// An App-less hive (no App private key) needs no installation-access check: the
// probe reports "no-app" and the add proceeds. newFullServer has no GHAppAuth.
func TestGovernorRepoCheckAccess_AppLessHiveSkips(t *testing.T) {
	srv := newFullServer(t)
	body := `{"repo":"testorg/somerepo"}`
	req := httptest.NewRequest("POST", "/api/config/governor/repos/check-access", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleGovernorRepoCheckAccess(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (no-app skip); body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no-app") {
		t.Errorf("expected no-app status, got: %s", w.Body.String())
	}
}

// The probe requires a resolvable org: a bare repo name with no configured org
// cannot name one, so it is a 400 rather than a silent pass.
func TestGovernorRepoCheckAccess_RequiresOrg(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = ""
	body := `{"repo":"lonelyrepo"}`
	req := httptest.NewRequest("POST", "/api/config/governor/repos/check-access", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleGovernorRepoCheckAccess(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 when the org cannot be determined", w.Code)
	}
}

// repoRefHostLabel / sameForgeHost unit coverage — the shared single-host rule.
func TestRepoRefHostLabelAndSameForgeHost(t *testing.T) {
	cases := []struct {
		in   string
		host string
	}{
		{"repo", ""},
		{"owner/repo", ""},
		{"github.ibm.com/owner/repo", "github.ibm.com"},
		{"https://github.com/owner/repo", "github.com"},
		{"GitHub.IBM.com/o/r", "github.ibm.com"}, // lowercased
	}
	for _, c := range cases {
		if got := repoRefHostLabel(c.in); got != c.host {
			t.Errorf("repoRefHostLabel(%q) = %q, want %q", c.in, got, c.host)
		}
	}
	if !sameForgeHost("", "github.com") {
		t.Error(`"" and "github.com" should be the same forge`)
	}
	if !sameForgeHost("GITHUB.COM", "github.com") {
		t.Error("host comparison should be case-insensitive")
	}
	if sameForgeHost("github.ibm.com", "github.com") {
		t.Error("a GHE host must not equal public github.com")
	}
}

// hiveForgeHost mirrors config.HostLabel(): github.com by default, the GHE host
// when the hive carries one.
func TestHiveForgeHost(t *testing.T) {
	srv := newFullServer(t)
	if got := srv.hiveForgeHost(); got != "github.com" {
		t.Errorf("default forge = %q, want github.com", got)
	}
	srv.deps.Config.GitHub = config.GitHubConfig{BaseURL: "https://github.ibm.com"}
	if got := srv.hiveForgeHost(); got != "github.ibm.com" {
		t.Errorf("GHE forge = %q, want github.ibm.com", got)
	}
}
