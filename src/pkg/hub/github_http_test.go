package hub

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testHubAppKeyFile(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestAuthGitHubRequestPrefersAppTokenThenPAT(t *testing.T) {
	var mintCalls atomic.Int32
	appAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/456/access_tokens" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		mintCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "app-installation-token",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer appAPI.Close()

	oldBase := githubAPIBase
	githubAPIBase = appAPI.URL
	t.Cleanup(func() {
		waitChannelTargetRefreshes(t)
		githubAPIBase = oldBase
		resetHubGitHubAppAuthForTest()
	})
	t.Setenv(hubGitHubAppIDEnv, "123")
	t.Setenv(hubGitHubInstallationIDEnv, "456")
	t.Setenv(hubGitHubAppKeyFileEnv, testHubAppKeyFile(t))
	t.Setenv(hubGitHubTokenEnv, "pat-token")
	resetHubGitHubAppAuthForTest()

	req := httptest.NewRequest(http.MethodGet, appAPI.URL+"/repos/hivecommons/hive", nil)
	authGitHubRequest(req)
	if got := req.Header.Get("Authorization"); got != "Bearer app-installation-token" {
		t.Fatalf("Authorization = %q, want app token", got)
	}
	if mintCalls.Load() != 1 {
		t.Fatalf("mint calls = %d, want 1", mintCalls.Load())
	}
}

func TestAuthGitHubRequestFallsBackToPATWhenAppUnavailable(t *testing.T) {
	appAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "mint failed", http.StatusInternalServerError)
	}))
	defer appAPI.Close()

	oldBase := githubAPIBase
	githubAPIBase = appAPI.URL
	t.Cleanup(func() {
		waitChannelTargetRefreshes(t)
		githubAPIBase = oldBase
		resetHubGitHubAppAuthForTest()
	})
	t.Setenv(hubGitHubAppIDEnv, "123")
	t.Setenv(hubGitHubInstallationIDEnv, "456")
	t.Setenv(hubGitHubAppKeyFileEnv, testHubAppKeyFile(t))
	t.Setenv(hubGitHubTokenEnv, "pat-token")
	resetHubGitHubAppAuthForTest()

	req := httptest.NewRequest(http.MethodGet, appAPI.URL+"/repos/hivecommons/hive", nil)
	authGitHubRequest(req)
	if got := req.Header.Get("Authorization"); got != "Bearer pat-token" {
		t.Fatalf("Authorization = %q, want PAT fallback", got)
	}
}

func TestAuthGitHubRequestUsesInjectedResolver(t *testing.T) {
	prev := hubGitHubAuthTokenForRequest
	hubGitHubAuthTokenForRequest = func(_ context.Context) string { return "injected-token" }
	t.Cleanup(func() { hubGitHubAuthTokenForRequest = prev })

	req := httptest.NewRequest(http.MethodGet, "https://api.github.com/repos/hivecommons/hive", nil)
	authGitHubRequest(req)
	if got := req.Header.Get("Authorization"); got != "Bearer injected-token" {
		t.Fatalf("Authorization = %q, want injected token", got)
	}
}

func TestHubGitHubHTTPClientRevalidatesWithETag(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"hub-etag"`)
			_, _ = io.WriteString(w, `{"ok":true}`)
		case 2:
			if got := r.Header.Get("If-None-Match"); got != `"hub-etag"` {
				t.Fatalf("If-None-Match = %q, want ETag", got)
			}
			w.WriteHeader(http.StatusNotModified)
		default:
			t.Fatalf("unexpected call %d", calls.Load())
		}
	}))
	defer srv.Close()

	client := hubGitHubHTTPClient()
	for i := 0; i < 2; i++ {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/resource", nil)
		if err != nil {
			t.Fatalf("NewRequest %d: %v", i, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Do %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != `{"ok":true}` {
			t.Fatalf("response %d = status %d body %q", i, resp.StatusCode, body)
		}
	}
}

func TestHubGHRateLimitsEndpointShape(t *testing.T) {
	t.Setenv(hubGitHubAppIDEnv, "")
	t.Setenv(hubGitHubInstallationIDEnv, "")
	t.Setenv(hubGitHubAppKeyFileEnv, "")
	t.Setenv(hubGitHubTokenEnv, "")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rate_limit" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"resources":{"core":{"limit":5000,"remaining":4999,"reset":1791493200},"search":{"limit":30,"remaining":29,"reset":1791493200},"graphql":{"limit":5000,"remaining":4000,"reset":1791493200}}}`)
	}))
	defer api.Close()

	oldBase := githubAPIBase
	githubAPIBase = api.URL
	t.Cleanup(func() { waitChannelTargetRefreshes(t); githubAPIBase = oldBase })

	srv := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	srv.handleGHRateLimits(rec, httptest.NewRequest(http.MethodGet, "/api/gh-rate-limits", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"core", "search", "graphql", "etag_cache", "top_consumers"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing %s in %v", key, got)
		}
	}
}
