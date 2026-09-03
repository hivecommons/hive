package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func covK2Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// covK2GHMock returns an httptest server that answers the go-github REST calls
// the metrics collector makes with minimal valid JSON.
func covK2GHMock(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/search/issues"):
			// SearchOutreachPRCount uses the search API.
			json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "items": []any{}})
		case strings.HasSuffix(p, "/contributors"):
			// GetContributorCount lists contributors.
			json.NewEncoder(w).Encode([]map[string]any{{"login": "a"}, {"login": "b"}})
		case strings.Contains(p, "/pulls"):
			// ComputeMTTR lists PRs.
			json.NewEncoder(w).Encode([]any{})
		case strings.Contains(p, "/contents/"):
			// GetFileContent (ADOPTERS / ACMM page).
			body := "# ADOPTERS\n| Org | Desc |\n| --- | --- |\n| Acme | prod |\n"
			json.NewEncoder(w).Encode(map[string]any{
				"type": "file", "encoding": "base64",
				"content": encodeBase64ForTest(body),
			})
		case strings.HasPrefix(p, "/repos/") && strings.Count(p, "/") == 3:
			// GetRepo: /repos/{owner}/{repo}
			json.NewEncoder(w).Encode(map[string]any{
				"stargazers_count": 42, "forks_count": 7,
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCovK2_MetricsCollect(t *testing.T) {
	logger := covK2Logger()
	srv := covK2GHMock(t)
	gh := ghpkg.NewClientForTest(srv.URL, "kubestellar", []string{"repo1"}, logger)

	// org == "kubestellar" enables the adopters/acmm/outreach-PR branches.
	mc := NewMetricsCollector(gh, "kubestellar", "repo1", "", "bot", "MyProject", logger)

	// Full collect exercises collectOutreach, collectCoverage, collectArchitect,
	// collectMTTR, saveToDisk (write to /data fails silently but is exercised).
	mc.collect(context.Background())

	// Direct calls for the individual collectors.
	_ = mc.collectOutreach(context.Background())
	mc.collectMTTR(context.Background())
	if n := mc.countAdopters(context.Background(), "kubestellar", "repo1"); n < 0 {
		t.Fatalf("countAdopters negative")
	}
	open, merged := mc.countOutreachPRs(context.Background())
	_ = open
	_ = merged

	// Get returns a populated copy.
	if got := mc.Get(); got == nil {
		t.Fatalf("Get returned nil")
	}
}

func TestCovK2_MetricsDiskFuncs(t *testing.T) {
	logger := covK2Logger()
	mc := &MetricsCollector{metrics: make(map[string]any), logger: logger}

	// No cache file present → early return.
	mc.loadFromDisk()
	mc.loadMTTRFromDisk()

	// save* write to fixed /data consts (unwritable in sandbox → silent) —
	// exercises the marshal + write-attempt path.
	mc.saveToDisk(map[string]any{"k": "v"})
	mc.saveMTTRToDisk(&ghpkg.MTTRResult{Count: 1, MedianMinutes: 5})
}

func TestCovK2_MetricsNilClientBranches(t *testing.T) {
	logger := covK2Logger()
	mc := &MetricsCollector{metrics: make(map[string]any), logger: logger}
	// nil client → early returns.
	mc.collectMTTR(context.Background())
	if _, m := mc.countOutreachPRs(context.Background()); m != 0 {
		t.Fatalf("countOutreachPRs with nil client should be 0")
	}
}

// --- litellm_key_store.go ---

func TestCovK2_LiteLLMKeyStore(t *testing.T) {
	logger := covK2Logger()
	s := NewServer(0, logger)
	s.RegisterAPI(testDeps(t))
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	// Redirect the PVC key file to a temp path so writeLiteLLMKeyFile succeeds.
	origFile := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = t.TempDir() + "/litellm_api_key"
	t.Cleanup(func() { writableLiteLLMKeyFile = origFile })

	if err := writeLiteLLMKeyFile("sk-litellm"); err != nil {
		t.Fatalf("writeLiteLLMKeyFile: %v", err)
	}
	if data, err := os.ReadFile(writableLiteLLMKeyFile); err != nil || string(data) != "sk-litellm" {
		t.Fatalf("key file not written correctly: %v %q", err, data)
	}

	// storeLiteLLMAPIKey: PVC write succeeds; patchKeyIntoHiveSecrets returns
	// errNotInCluster (no KUBERNETES_SERVICE_HOST) → returns the PVC path.
	path, err := s.storeLiteLLMAPIKey("sk-store")
	if err != nil {
		t.Fatalf("storeLiteLLMAPIKey: %v", err)
	}
	if path == "" {
		t.Fatalf("storeLiteLLMAPIKey returned empty path")
	}

	// actionableWriteError: permission error → actionable message; other → wrapped.
	permErr := actionableWriteError("/some/path", os.ErrPermission)
	if !strings.Contains(permErr.Error(), "read-only or misowned") {
		t.Fatalf("expected actionable permission message, got %v", permErr)
	}
	genErr := actionableWriteError("/some/path", errors.New("boom"))
	if !strings.Contains(genErr.Error(), "writing /some/path") {
		t.Fatalf("expected generic write message, got %v", genErr)
	}

	// patchKeyIntoHiveSecrets: not in cluster → errNotInCluster.
	if err := patchKeyIntoHiveSecrets("litellm_api_key", "v"); !errors.Is(err, errNotInCluster) {
		t.Fatalf("expected errNotInCluster, got %v", err)
	}
}

// --- claude_auth.go remaining exchange branches ---

func TestCovK2_ClaudeAuthExchangeBranches(t *testing.T) {
	logger := covK2Logger()
	s := NewServer(0, logger)
	s.RegisterAPI(testDeps(t))

	// Bad JSON body → 400.
	req := httptest.NewRequest(http.MethodPost, "/api/claude-auth/exchange", stringReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("exchange bad body: expected 400, got %d", rec.Code)
	}

	// callback_url carrying an error param → 400.
	if rec := doPost(s, "/api/claude-auth/exchange", map[string]any{
		"callback_url": "https://x/cb?error=access_denied&error_description=nope",
	}); rec.Code != http.StatusBadRequest {
		t.Fatalf("exchange error callback: expected 400, got %d", rec.Code)
	}

	// No code and no callback_url → 400 (no authorization code provided).
	if rec := doPost(s, "/api/claude-auth/exchange", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("exchange no code: expected 400, got %d", rec.Code)
	}

	// A code provided but no active PKCE flow (verifier empty) → 400 session expired.
	if rec := doPost(s, "/api/claude-auth/exchange", map[string]any{"code": "abc"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("exchange no-flow: expected 400, got %d", rec.Code)
	}

	// Start populates a flow; Status reports logged out with no creds file.
	if rec := doPost(s, "/api/claude-auth/start", nil); rec.Code != http.StatusOK {
		t.Fatalf("claude start: %d", rec.Code)
	}
	if rec := doGet(s, "/api/claude-auth/status"); rec.Code != http.StatusOK {
		t.Fatalf("claude status: %d", rec.Code)
	}
}
