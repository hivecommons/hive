package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
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

// TestHandleVersionUnbuiltTipReportsImageNotReady pins the #4804 contract:
// when the stable v4 tip has NO published GHCR image, the response must say so
// explicitly (stableV4ImageReady=false) and must not attempt the compare, so
// commitsBehind stays absent. Before the fix the frontend collapsed this known
// state with a genuine compare failure and rendered a contradictory
// "✓ ? behind" badge.
func TestHandleVersionUnbuiltTipReportsImageNotReady(t *testing.T) {
	origHash, origShort := versionHash, versionShort
	versionHash, versionShort = "old11112222", "old1111"
	t.Cleanup(func() { versionHash, versionShort = origHash, origShort })
	// Tip image does NOT exist (cached negative so no network call is made).
	ghcrCacheMu.Lock()
	ghcrCacheResult["abc1234"] = false
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

	rec := doGet(s, "/api/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["stableV4ImageReady"] != false {
		t.Fatalf("stableV4ImageReady = %v, want false when the tip has no GHCR image", body["stableV4ImageReady"])
	}
	if got, present := body["commitsBehind"]; present {
		t.Fatalf("commitsBehind = %v, want absent when the tip image is not built", got)
	}
	if body["behind"] != false {
		t.Fatalf("behind = %v, want false when the tip image is not built", body["behind"])
	}
	if compareCalls != 0 {
		t.Fatalf("compare calls = %d, want 0 (compare must not run for an unbuilt tip)", compareCalls)
	}
}

// TestHandleVersionBuiltTipReportsImageReady pins the complementary #4804
// contract: when the tip image exists, stableV4ImageReady is true and the
// compare runs, so a null commitsBehind on the frontend then genuinely means
// "the compare failed" and the "? behind" wording is accurate.
func TestHandleVersionBuiltTipReportsImageReady(t *testing.T) {
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
		_ = json.NewEncoder(w).Encode(map[string]any{"ahead_by": 5})
	})
	ghSrv := httptest.NewServer(mux)
	defer ghSrv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ghSrv.URL, "myorg", []string{"repo1"}, logger)
	s.RegisterAPI(deps)

	rec := doGet(s, "/api/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["stableV4ImageReady"] != true {
		t.Fatalf("stableV4ImageReady = %v, want true when the tip image exists", body["stableV4ImageReady"])
	}
	if body["commitsBehind"] != float64(5) {
		t.Fatalf("commitsBehind = %v, want 5", body["commitsBehind"])
	}
}
