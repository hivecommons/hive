package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	ghpkg "github.com/kubestellar/hive/pkg/github"
)

func TestHandleVersionIncludesCommitsBehindAndCaches(t *testing.T) {
	origHash, origShort := versionHash, versionShort
	versionHash, versionShort = "old11112222", "old1111"
	t.Cleanup(func() { versionHash, versionShort = origHash, origShort })
	ghcrCacheMu.Lock()
	ghcrCacheResult["abc1234"] = true
	ghcrCacheExpiry["abc1234"] = time.Now().Add(time.Hour)
	ghcrCacheMu.Unlock()
	t.Cleanup(func() {
		ghcrCacheMu.Lock()
		delete(ghcrCacheResult, "abc1234")
		delete(ghcrCacheExpiry, "abc1234")
		ghcrCacheMu.Unlock()
	})

	compareCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/kubestellar/hive/git/ref/heads/v4", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ref":    "refs/heads/v4",
			"object": map[string]any{"sha": "abc1234567890abcdef1234567890abcdef123456"},
		})
	})
	mux.HandleFunc("/repos/kubestellar/hive/commits/abc1234567890abcdef1234567890abcdef123456", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"message": "tip\nbody"}})
	})
	mux.HandleFunc("/repos/kubestellar/hive/compare/old1111...abc1234", func(w http.ResponseWriter, r *http.Request) {
		compareCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{"ahead_by": 5})
	})
	ghSrv := httptest.NewServer(mux)
	defer ghSrv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ghSrv.URL, "myorg", []string{"repo1"}, logger)
	s.RegisterAPI(deps)

	for i := 0; i < 2; i++ {
		rec := doGet(s, "/api/version")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if body["commitsBehind"] != float64(5) {
			t.Fatalf("commitsBehind = %v, want 5", body["commitsBehind"])
		}
	}
	if compareCalls != 1 {
		t.Fatalf("compare calls = %d, want 1 cached call", compareCalls)
	}
}
