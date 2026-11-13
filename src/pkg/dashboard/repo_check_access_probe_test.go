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
	"strconv"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// App-present access checks use a real AppAuth and an offline GitHub stub.

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

// installedOrgHandler supplies an installation covering widget.
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
		if r.URL.Path == "/app/installations/"+strconv.FormatInt(id, 10)+"/access_tokens" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`))
			return
		}
		if r.URL.Path == "/installation/repositories" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"repositories": []any{map[string]any{"full_name": org + "/widget"}}})
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

func TestGovernorRepoCheckAccess_BareRepoUsesConfiguredOrg(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "bare-org"
	srv.deps.GHAppAuth = newDiscoveryAppAuth(t, installedOrgHandler("bare-org", 123))
	w := checkAccessRequest(t, srv, `{"repo":"widget"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body: %s", w.Code, w.Body.String())
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

func TestGovernorRepoCheckAccess_RepoSelection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		listStatus int
		listBody   string
		wantStatus int
	}{
		{"unselected", http.StatusOK, `{"repositories":[{"full_name":"selection-org/other"}]}`, http.StatusConflict},
		{"list failure", http.StatusInternalServerError, `{"message":"unavailable"}`, http.StatusServiceUnavailable},
		{"case insensitive", http.StatusOK, `{"repositories":[{"full_name":"SELECTION-ORG/WIDGET"}]}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFullServer(t)
			base := installedOrgHandler("selection-org", 4242)
			srv.deps.GHAppAuth = newDiscoveryAppAuth(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/installation/repositories" {
					if r.Header.Get("Authorization") != "Bearer installation-token" {
						t.Errorf("repository listing did not use installation token")
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.listStatus)
					_, _ = w.Write([]byte(tc.listBody))
					return
				}
				base(w, r)
			})
			w := checkAccessRequest(t, srv, `{"repo":"selection-org/widget"}`)
			if w.Code != tc.wantStatus {
				t.Fatalf("code = %d, want %d; body: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus == http.StatusConflict {
				var resp struct {
					NeedsInstall bool   `json:"needsInstall"`
					InstallURL   string `json:"installUrl"`
					Error        string `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatal(err)
				}
				if !resp.NeedsInstall || !strings.Contains(resp.InstallURL, "/organizations/selection-org/settings/installations/4242") || !strings.Contains(resp.Error, "widget") {
					t.Errorf("unexpected response: %+v", resp)
				}
			}
		})
	}
}

func TestGovernorRepoCheckAccess_DiscoveryFailure(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.GHAppAuth = newDiscoveryAppAuth(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	})
	w := checkAccessRequest(t, srv, `{"repo":"discovery-failure/widget"}`)
	if w.Code != http.StatusServiceUnavailable || srv.IsPendingGitHubAppInstall() {
		t.Fatalf("unexpected response %d %s; pending=%v", w.Code, w.Body.String(), srv.IsPendingGitHubAppInstall())
	}
}

func TestGovernorRepoCheckAccess_TruncatedCoverageUsesRepositoryInstallation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{"selected", http.StatusOK, `{"id":4242}`, http.StatusOK},
		{"not selected", http.StatusNotFound, `{"message":"Not Found"}`, http.StatusConflict},
		{"different installation", http.StatusOK, `{"id":2}`, http.StatusServiceUnavailable},
		{"missing installation id", http.StatusOK, `{}`, http.StatusServiceUnavailable},
		{"forbidden", http.StatusForbidden, `{"message":"rate limit"}`, http.StatusServiceUnavailable},
		{"rate limited", http.StatusTooManyRequests, `{"message":"rate limit"}`, http.StatusServiceUnavailable},
		{"server failure", http.StatusInternalServerError, `{"message":"unavailable"}`, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFullServer(t)
			base := installedOrgHandler("truncated-org", 4242)
			probed := false
			srv.deps.GHAppAuth = newDiscoveryAppAuth(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/installation/repositories":
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Link", `<http://example.invalid/installation/repositories?page=2>; rel="next"`)
					_, _ = w.Write([]byte(`{"repositories":[]}`))
				case "/repos/truncated-org/widget/installation":
					probed = true
					if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ") || auth == "Bearer installation-token" {
						t.Errorf("repository installation probe did not use App JWT: %q", auth)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				default:
					base(w, r)
				}
			})
			w := checkAccessRequest(t, srv, `{"repo":"truncated-org/widget"}`)
			if !probed || w.Code != tc.wantStatus {
				t.Fatalf("probed=%v code=%d, want %d; body: %s", probed, w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus == http.StatusConflict && !strings.Contains(w.Body.String(), "/organizations/truncated-org/settings/installations/4242") {
				t.Fatalf("missing installation settings link: %s", w.Body.String())
			}
		})
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
