package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ============================================================
// #4193 — follow-ups to the dibs SSO work in #4171/#4174.
//
//  1. Sessions minted BEFORE the cookie-domain widening never received the
//     Domain=.kubestellar.io cookie: it was only issued at the OAuth callback,
//     so already-signed-in users' browsers kept the old scope and
//     dibs.kubestellar.io never saw the session. handleAuthUser (the one call
//     the dashboard makes on every load) now re-issues the SAME verified
//     session value with the widened Domain.
//  2. GET /api/saas/dibs/repos — the public repo registry feed dibs polls
//     every ~5 minutes (it used to 404).
// ============================================================

// TestAuthUserRemintsWideCookie pins the re-mint: an authenticated
// /api/auth/user response must carry the live hive_hub_user cookie, with the
// SAME value the request presented, scoped Domain=.kubestellar.io, plus the
// legacy-scope expiry — exactly what the login callback emits.
func TestAuthUserRemintsWideCookie(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "octocat")

	presented := testAuthCookie("octocat")
	req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/api/auth/user", nil)
	req.AddCookie(presented)
	rec := httptest.NewRecorder()
	s.handleAuthUser(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated /api/auth/user = %d, want 200", rec.Code)
	}
	var live, legacyClear *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name != "hive_hub_user" {
			continue
		}
		if c.Value != "" {
			live = c
		} else {
			legacyClear = c
		}
	}
	if live == nil {
		t.Fatal("authenticated /api/auth/user re-minted no hive_hub_user cookie — pre-widening sessions never converge to the wide scope")
	}
	if live.Value != presented.Value {
		t.Errorf("re-mint changed the session value — it must re-issue the SAME verified value, not mint a new session")
	}
	if live.Domain != "kubestellar.io" {
		t.Errorf("re-minted cookie Domain = %q, want .kubestellar.io", live.Domain)
	}
	if !live.Secure || !live.HttpOnly || live.SameSite != http.SameSiteLaxMode {
		t.Errorf("re-minted cookie lost hardening: Secure=%v HttpOnly=%v SameSite=%v", live.Secure, live.HttpOnly, live.SameSite)
	}
	if legacyClear == nil {
		t.Fatal("re-mint did not expire the legacy .hive.kubestellar.io-scoped copy")
	}
	if legacyClear.Domain != "hive.kubestellar.io" || legacyClear.MaxAge >= 0 {
		t.Errorf("legacy clear cookie Domain=%q MaxAge=%d, want .hive.kubestellar.io with MaxAge<0", legacyClear.Domain, legacyClear.MaxAge)
	}
}

// TestAuthUserRemintHostOnlyForDev pins the dev fallback: on a host outside
// kubestellar.io the re-minted cookie must stay host-only (a browser rejects a
// non-covering Domain, so widening would break local sessions).
func TestAuthUserRemintHostOnlyForDev(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "octocat")

	req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/api/auth/user", nil)
	req.AddCookie(testAuthCookie("octocat"))
	rec := httptest.NewRecorder()
	s.handleAuthUser(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" && c.Domain != "" {
			t.Errorf("dev host re-mint carries Domain=%q, want host-only", c.Domain)
		}
	}
}

// TestAuthUserNoCookieForAnonymous pins that an unauthenticated request gets
// NO Set-Cookie: re-minting is strictly for requests that already carry a
// valid session.
func TestAuthUserNoCookieForAnonymous(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()

	for _, c := range []*http.Cookie{nil, {Name: "hive_hub_user", Value: "forged"}} {
		req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/api/auth/user", nil)
		if c != nil {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		s.handleAuthUser(rec, req)
		if got := rec.Result().Cookies(); len(got) != 0 {
			t.Errorf("anonymous /api/auth/user emitted %d Set-Cookie header(s), want none", len(got))
		}
	}
}

// mkDibsHive writes a hive record with the fields the dibs feed keys on.
func mkDibsHive(t *testing.T, h SaaSHive) {
	t.Helper()
	if err := saveSaaSHive(&h); err != nil {
		t.Fatalf("saveSaaSHive(%s): %v", h.ID, err)
	}
}

// TestDibsReposShapeAndFiltering pins both halves of the feed contract: the
// JSON field names dibs's registry client decodes (repoID/hiveID/owner/
// description), and the inclusion rules that keep the unauthenticated answer
// public-only (is_public, public github.com, real assigned identity).
func TestDibsReposShapeAndFiltering(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()

	// Included: public hive on public github.com with a real org/repo, plus a
	// secondary repo and a duplicate of the primary that must be deduped.
	mkDibsHive(t, SaaSHive{
		ID: "hosted-good-1", Owner: "octocat", Org: "hivecommons",
		PrimaryRepo: "hive", Repos: []string{"hive", "dibs"},
		ProjectName: "KubeStellar Hive", IsPublic: true,
	})
	// Included: primary_repo already carrying owner/repo is the repo ID as-is.
	mkDibsHive(t, SaaSHive{
		ID: "hosted-good-2", Owner: "hubber", Org: "some-org",
		PrimaryRepo: "other-owner/tool", IsPublic: true,
	})
	// Included: the EXPLICIT "github.com" github_host production meta.json
	// records actually store (spoke-heartbeat truth) means public GitHub,
	// same as "" — the empty-only check excluded every production hive
	// (#4233). Case-insensitive, and it outranks a cluster GHE default.
	mkDibsHive(t, SaaSHive{
		ID: "hosted-good-3", Owner: "prodder", Org: "prod-org",
		PrimaryRepo: "prod-repo", IsPublic: true, GitHubHost: "GitHub.com",
	})
	// Excluded: not public.
	mkDibsHive(t, SaaSHive{
		ID: "hosted-private", Owner: "octocat", Org: "secretcorp",
		PrimaryRepo: "skunkworks", IsPublic: false,
	})
	// Excluded: GHE host — an enterprise repo's name can itself be confidential.
	mkDibsHive(t, SaaSHive{
		ID: "hosted-ghe", Owner: "octocat", Org: "ibm-internal",
		PrimaryRepo: "z-tool", IsPublic: true, GitHubHost: "github.ibm.com",
	})
	// Excluded: GHE pinned via github_base_url rather than github_host.
	mkDibsHive(t, SaaSHive{
		ID: "hosted-ghe-base", Owner: "octocat", Org: "cisco-internal",
		PrimaryRepo: "net-tool", IsPublic: true, GitHubBaseURL: "https://github.cisco.com",
	})
	// Excluded: non-GitHub forge.
	mkDibsHive(t, SaaSHive{
		ID: "hosted-gitlab", Owner: "octocat", Org: "gl-org",
		PrimaryRepo: "gl-repo", IsPublic: true, Forge: "gitlab",
	})
	// Excluded: unclaimed placeholder (synthetic inventory org, no repo).
	mkDibsHive(t, SaaSHive{
		ID: "hosted-available-3", Owner: "", Org: placeholderOrgPrefix + "3",
		IsPublic: true, Status: statusAvailable,
	})

	req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/api/saas/dibs/repos", nil)
	rec := httptest.NewRecorder()
	// The non-is_public hive (hosted-private) reaches the public-repo verdict
	// path; point it at a fake GitHub API that 404s everything so the test
	// never touches the network and the repo stays excluded.
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	s.dibsPublic = newTestDibsChecker(notFound.URL)
	s.handleDibsRepos(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/api/saas/dibs/repos = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// Decode into the exact shape dibs's registry client uses, keyed on the
	// wire names — a renamed field here breaks the sibling silently.
	var got []struct {
		RepoID      string `json:"repoID"`
		HiveID      string `json:"hiveID"`
		Owner       string `json:"owner"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not the expected JSON array: %v (body %s)", err, rec.Body.String())
	}
	want := map[string]struct{ hive, owner string }{
		"hivecommons/hive":   {"hosted-good-1", "octocat"},
		"hivecommons/dibs":   {"hosted-good-1", "octocat"},
		"other-owner/tool":   {"hosted-good-2", "hubber"},
		"prod-org/prod-repo": {"hosted-good-3", "prodder"},
	}
	if len(got) != len(want) {
		t.Fatalf("feed lists %d repos %v, want exactly the %d public github.com repos", len(got), got, len(want))
	}
	for _, e := range got {
		w, ok := want[e.RepoID]
		if !ok {
			t.Errorf("feed leaked repo %q — only public repos of public hives may appear", e.RepoID)
			continue
		}
		if e.HiveID != w.hive || e.Owner != w.owner {
			t.Errorf("repo %s: hiveID=%q owner=%q, want %q/%q", e.RepoID, e.HiveID, e.Owner, w.hive, w.owner)
		}
	}
	for _, e := range got {
		if e.RepoID == "hivecommons/hive" && e.Description != "KubeStellar Hive" {
			t.Errorf("description = %q, want the hive's public project name", e.Description)
		}
	}
}

// TestDibsReposPublicNoSession pins that the endpoint answers server-to-server
// callers carrying no cookie at all (dibs has no hub session of its own), and
// that an EMPTY registry still returns a JSON array, never null — dibs decodes
// into a slice and merges, so null vs [] is the difference between a sync and
// a decode surprise.
func TestDibsReposPublicNoSession(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()

	req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/api/saas/dibs/repos", nil)
	rec := httptest.NewRecorder()
	s.handleDibsRepos(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("no-session /api/saas/dibs/repos = %d, want 200 — dibs calls with no cookie", rec.Code)
	}
	if body := rec.Body.String(); body != "[]\n" {
		t.Errorf("empty registry body = %q, want a JSON array literal []", body)
	}
}
