package dashboard

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// These tests cover the App-present half of handleGovernorRepoCheckAccess —
// the DiscoverInstallationID probe the endpoint exists for. The App-less,
// cross-forge, and no-org guards are covered in repo_add_form_test.go; here a
// real *ghpkg.AppAuth (generated RSA key) talks to a stub GitHub API so both
// probe outcomes run deterministically and offline:
//   - installed  → 200 {ok:true, status:"ok", org}
//   - not installed → 409 {ok:false, needsInstall:true, org, installUrl} and
//     the pending-install banner mechanism is armed.

// newDiscoveryAppAuth builds an AppAuth whose JWT generation succeeds and whose
// API calls hit the given handler. Discovery results are memoised process-wide
// (discoveryCache keyed by apiURL|appID|org), so callers must use a unique org
// per test; the cache is also reset on cleanup to keep other tests pristine.
func newDiscoveryAppAuth(t *testing.T, handler http.HandlerFunc) *ghpkg.AppAuth {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	keyFile := filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(keyFile, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	t.Cleanup(ghpkg.ResetInstallationDiscoveryCache)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth, err := ghpkg.NewAppAuthWithCache(1, 2, keyFile,
		filepath.Join(t.TempDir(), "token.cache"), logger, srv.URL)
	if err != nil {
		t.Fatalf("NewAppAuthWithCache: %v", err)
	}
	return auth
}

// installedOrgHandler answers GET /orgs/{org}/installation affirmatively for
// exactly one org and 404s everything else (including the listing fallback).
func installedOrgHandler(org string, id int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orgs/"+org+"/installation" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      id,
				"account": map[string]any{"login": org},
			})
			return
		}
		if r.URL.Path == "/app/installations" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}
}

func checkAccessRequest(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/config/governor/repos/check-access", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleGovernorRepoCheckAccess(w, req)
	return w
}

// App installed on the repo's org: the probe reports ok and names the org it
// verified, and does NOT arm the pending-install banner.
func TestGovernorRepoCheckAccess_AppInstalledOK(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.GHAppAuth = newDiscoveryAppAuth(t, installedOrgHandler("probe-ok-org", 4242))

	w := checkAccessRequest(t, srv, `{"repo":"probe-ok-org/widget"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
	if resp["org"] != "probe-ok-org" {
		t.Errorf("org = %v, want probe-ok-org", resp["org"])
	}
	if srv.IsPendingGitHubAppInstall() {
		t.Error("pending-install armed on a successful probe")
	}
}

// A full same-forge URL paste resolves its org from the URL path (the host
// segment is stripped), not from the hive's configured org.
func TestGovernorRepoCheckAccess_FullURLResolvesOrg(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.GHAppAuth = newDiscoveryAppAuth(t, installedOrgHandler("probe-url-org", 777))

	w := checkAccessRequest(t, srv, `{"repo":"https://github.com/probe-url-org/widget"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "probe-url-org") {
		t.Errorf("expected org derived from URL, got: %s", w.Body.String())
	}
}

// App NOT installed on the org: 409 with needsInstall, the resolved org, the
// per-forge install URL, and the pending-install banner mechanism armed so the
// existing recheck plumbing drives the flow to completion.
func TestGovernorRepoCheckAccess_NotInstalled409(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.GHAppAuth = newDiscoveryAppAuth(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})

	w := checkAccessRequest(t, srv, `{"repo":"probe-missing-org/widget"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK           bool   `json:"ok"`
		NeedsInstall bool   `json:"needsInstall"`
		Org          string `json:"org"`
		InstallURL   string `json:"installUrl"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.OK {
		t.Error("ok = true, want false")
	}
	if !resp.NeedsInstall {
		t.Error("needsInstall = false, want true")
	}
	if resp.Org != "probe-missing-org" {
		t.Errorf("org = %q, want probe-missing-org", resp.Org)
	}
	if want := srv.deps.Config.GitHub.AppInstallURL(); resp.InstallURL != want {
		t.Errorf("installUrl = %q, want %q", resp.InstallURL, want)
	}
	if !strings.Contains(resp.Error, "probe-missing-org") {
		t.Errorf("error message should name the org, got: %q", resp.Error)
	}
	if !srv.IsPendingGitHubAppInstall() {
		t.Error("pending-install was not armed on a failed probe")
	}
}

// Malformed body and blank repo are rejected before any probe runs.
func TestGovernorRepoCheckAccess_BadInput(t *testing.T) {
	srv := newFullServer(t)

	if w := checkAccessRequest(t, srv, `{not json`); w.Code != http.StatusBadRequest {
		t.Errorf("malformed body: code = %d, want 400", w.Code)
	}
	if w := checkAccessRequest(t, srv, `{"repo":"   "}`); w.Code != http.StatusBadRequest {
		t.Errorf("blank repo: code = %d, want 400", w.Code)
	}
}
