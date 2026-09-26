package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func seedSocialCardProfile(t *testing.T, username string) *Server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	resetSocialCardCacheForTest()
	storeGitHubUserCache(username, githubUserCacheEntry{ok: false, fetchedAt: time.Now()})
	p := &ContributorProfile{
		GitHubUsername: username,
		ContributorID:  "c-" + username,
		TrustTier:      "trusted",
		RegisteredAt:   "2026-01-02T03:04:05Z",
		TasksCompleted: 7,
		TasksWithPR:    7,
		RateLimits:     ContributorRateLimits{MaxConcurrent: 1, MaxPerHour: 3, MaxPerDay: 10},
	}
	if err := saveContributorProfile(p); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.registerContributeRoutes()
	return s
}

func resetSocialCardCacheForTest() {
	socialCardCache.Lock()
	defer socialCardCache.Unlock()
	socialCardCache.entries = map[string]socialCardCacheEntry{}
}

func TestAchievementTierMetalMapping(t *testing.T) {
	cases := map[string]string{
		achievementTierSolo:     "Copper",
		achievementTierDual:     "Silver",
		achievementTierFireteam: "Gold",
		achievementTierRaid:     "Platinum",
		"unknown":               "Bronze",
	}
	for tier, want := range cases {
		if got := achievementTierMetal(tier).Name; got != want {
			t.Fatalf("achievementTierMetal(%q) = %q, want %q", tier, got, want)
		}
	}
}

func TestSocialCardSVGHeadersCachingAndEscaping(t *testing.T) {
	s := seedSocialCardProfile(t, "alice")
	req := httptest.NewRequest(http.MethodGet, "/cards/achievement/alice/first-useful-change.svg", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("unsafe CSP: %q", csp)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script") {
		t.Fatalf("svg includes script: %s", body)
	}
	if !strings.Contains(body, "ACHIEVEMENT UNLOCKED") || !strings.Contains(body, "First Useful Change") {
		t.Fatalf("svg missing expected content: %s", body)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/cards/achievement/alice/first-useful-change.svg", nil)
	req2.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec2 := httptest.NewRecorder()
	s.mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match status = %d", rec2.Code)
	}
}

func TestSocialCardCacheDoesNotCaptureHostHeader(t *testing.T) {
	s := seedSocialCardProfile(t, "alice")
	req := httptest.NewRequest(http.MethodGet, "/cards/player/alice.svg", nil)
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("poisoning request status = %d", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	s.mux.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/cards/player/alice.svg", nil))
	body := rec2.Body.String()
	if strings.Contains(body, "evil.example") || !strings.Contains(body, `href="/share/player/alice"`) {
		t.Fatalf("cached SVG should use a relative landing href, got %s", body)
	}
}

func TestSocialCardValidationAndUnknownPlayer(t *testing.T) {
	s := seedSocialCardProfile(t, "alice")
	paths := []string{
		"/cards/player/ghost.svg",
		"/cards/player/alice!.svg",
		"/cards/achievement/alice/bad.id.svg",
		"/cards/leaderboard/not-a-board.svg",
	}
	for _, path := range paths {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, rec.Code)
		}
	}
}

func TestSharePageHasOpenGraphCard(t *testing.T) {
	s := seedSocialCardProfile(t, "alice")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/share/player/alice", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"property=\"og:image\"", "name=\"twitter:card\" content=\"summary_large_image\"", "/cards/player/alice.svg", "Open the public dossier"} {
		if !strings.Contains(body, want) {
			t.Fatalf("share page missing %q in %s", want, body)
		}
	}
}

func TestSocialCardPublicPaths(t *testing.T) {
	for _, path := range []string{"/cards/player/alice.svg", "/cards/achievement/alice/first-useful-change.svg", "/share/player/alice", "/share/leaderboard/contributors"} {
		if !isPublicPath(path) {
			t.Fatalf("%s should be public", path)
		}
	}
}
