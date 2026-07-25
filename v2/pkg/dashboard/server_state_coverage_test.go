package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func covStateServer() *Server {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewServer(0, logger)
}

// --- Fact history ring buffer / throttle / copy semantics ---

func TestCovE_FactHistoryAppendThrottleAndCopy(t *testing.T) {
	s := covStateServer()

	// First append records.
	s.AppendFactHistory(10)
	if got := s.FactHistory(); len(got) != 1 || got[0].Count != 10 {
		t.Fatalf("expected 1 entry count 10, got %+v", got)
	}

	// A second append within the min interval is throttled (dropped).
	s.AppendFactHistory(20)
	if got := s.FactHistory(); len(got) != 1 {
		t.Fatalf("expected throttle to drop the second append, got %d entries", len(got))
	}

	// FactHistory returns a copy — mutating it must not affect internal state.
	out := s.FactHistory()
	if len(out) > 0 {
		out[0].Count = 999
	}
	if s.FactHistory()[0].Count != 10 {
		t.Fatalf("FactHistory did not return an independent copy")
	}
}

func TestCovE_SeedFactHistoryCapAndOrdering(t *testing.T) {
	s := covStateServer()

	// Seed more than the cap; only the tail must be retained, in order.
	total := factHistoryMaxEntries + 5
	entries := make([]FactHistoryEntry, total)
	for i := range entries {
		entries[i] = FactHistoryEntry{Timestamp: int64(i), Count: i}
	}
	s.SeedFactHistory(entries)

	got := s.FactHistory()
	if len(got) != factHistoryMaxEntries {
		t.Fatalf("expected cap %d, got %d", factHistoryMaxEntries, len(got))
	}
	// Tail retained: first retained entry is index 5.
	if got[0].Count != 5 {
		t.Fatalf("expected tail retention starting at 5, got %d", got[0].Count)
	}
	if got[len(got)-1].Count != total-1 {
		t.Fatalf("expected last entry %d, got %d", total-1, got[len(got)-1].Count)
	}

	// Seeding below the cap keeps everything.
	small := []FactHistoryEntry{{Count: 1}, {Count: 2}}
	s.SeedFactHistory(small)
	if got := s.FactHistory(); len(got) != 2 {
		t.Fatalf("expected 2 entries after small seed, got %d", len(got))
	}
}

// --- Token sparkline history seed/copy ---

func TestCovE_TokenSparklineSeedAndCopy(t *testing.T) {
	s := covStateServer()

	total := tokenSparklineMaxEntries + 3
	entries := make([]TokenSparklineEntry, total)
	for i := range entries {
		entries[i] = TokenSparklineEntry{Timestamp: int64(i), Input: int64(i)}
	}
	s.SeedTokenSparklineHistory(entries)
	got := s.TokenSparklineHistory()
	if len(got) != tokenSparklineMaxEntries {
		t.Fatalf("expected token cap %d, got %d", tokenSparklineMaxEntries, len(got))
	}
	if got[0].Input != 3 {
		t.Fatalf("expected tail retention from index 3, got %d", got[0].Input)
	}

	// Copy independence.
	out := s.TokenSparklineHistory()
	if len(out) > 0 {
		out[0].Input = -1
	}
	if s.TokenSparklineHistory()[0].Input != 3 {
		t.Fatalf("TokenSparklineHistory did not return a copy")
	}

	// Below-cap seed keeps all.
	s.SeedTokenSparklineHistory([]TokenSparklineEntry{{Input: 7}})
	if len(s.TokenSparklineHistory()) != 1 {
		t.Fatalf("expected 1 entry after small token seed")
	}
}

// --- System alerts add/update/clear ---

func TestCovE_SystemAlertsAddUpdateClear(t *testing.T) {
	s := covStateServer()

	s.AddSystemAlert("a1", "warning", "first")
	// Update in place (same id) exercises the update branch.
	s.AddSystemAlert("a1", "critical", "updated")
	s.AddSystemAlert("a2", "info", "second")

	// Clearing a non-existent id is a no-op (loop finds nothing).
	s.ClearSystemAlert("nope")
	// Clearing an existing id removes it.
	s.ClearSystemAlert("a1")
	s.ClearSystemAlert("a2")
}

// --- Hub banner set/clear ---

func TestCovE_HubBannerSetClear(t *testing.T) {
	s := covStateServer()
	s.SetHubBanner("b1", "hello", "blue")
	s.ClearHubBanner()
}

// --- GitHub App state machine setters/getters ---

func TestCovE_GitHubAppStateSetters(t *testing.T) {
	s := covStateServer()
	s.RegisterAPI(testDeps(t))

	// Required=true with config present derives an install URL.
	s.SetGitHubAppRequired(true)
	if !s.IsGitHubAppRequired() {
		t.Fatalf("expected app required")
	}
	s.SetGitHubAppPermIssue("missing issues:write")
	if s.GetGitHubAppPermIssue() != "missing issues:write" {
		t.Fatalf("perm issue not stored")
	}

	s.SetPendingGitHubAppInstall()
	if !s.IsPendingGitHubAppInstall() {
		t.Fatalf("expected pending install to be true right after set")
	}
	s.ClearPendingGitHubAppInstall()
	if s.IsPendingGitHubAppInstall() {
		t.Fatalf("expected pending install cleared")
	}

	// Required=false clears URL and perm issue.
	s.SetGitHubAppRequired(false)
	if s.IsGitHubAppRequired() {
		t.Fatalf("expected app not required")
	}

	// Required=true on a server with nil deps.Config takes the hardcoded URL branch.
	s2 := covStateServer()
	s2.SetGitHubAppRequired(true)
	if !s2.IsGitHubAppRequired() {
		t.Fatalf("expected app required on bare server")
	}
}

func TestCovE_PendingInstallExpiry(t *testing.T) {
	s := covStateServer()
	s.SetPendingGitHubAppInstall()
	// Force the stored timestamp far into the past so the expiry branch fires.
	s.githubAppMu.Lock()
	s.pendingGitHubAppInstallAt = time.Now().Add(-20 * time.Minute)
	s.githubAppMu.Unlock()
	if s.IsPendingGitHubAppInstall() {
		t.Fatalf("expected expired pending install to report false")
	}
}

func TestCovE_UpdateGitHubClient(t *testing.T) {
	s := covStateServer()
	s.RegisterAPI(testDeps(t))
	// nil client/auth is fine; exercises the deps!=nil branch.
	s.UpdateGitHubClient(nil, nil)

	// deps==nil branch is a no-op.
	bare := covStateServer()
	bare.UpdateGitHubClient(nil, nil)
}

func TestCovE_RecheckGitHubApp(t *testing.T) {
	s := covStateServer()

	// No recheck fn wired → false no-op.
	if s.RecheckGitHubApp() {
		t.Fatalf("expected false with no recheck fn")
	}

	// Recheck fn returning true clears required/perm/pending state.
	s.SetGitHubAppRequired(true)
	s.SetGitHubAppPermIssue("x")
	s.SetPendingGitHubAppInstall()
	s.SetGitHubAppRecheckFn(func() bool { return true })
	if !s.RecheckGitHubApp() {
		t.Fatalf("expected recheck true")
	}
	if s.IsGitHubAppRequired() || s.GetGitHubAppPermIssue() != "" {
		t.Fatalf("expected recheck-true to clear app state")
	}

	// Recheck fn returning false leaves state as-is.
	s.SetGitHubAppRequired(true)
	s.SetGitHubAppRecheckFn(func() bool { return false })
	if s.RecheckGitHubApp() {
		t.Fatalf("expected recheck false")
	}
}

func TestCovE_HandleGitHubAppRecheckHandler(t *testing.T) {
	s := covStateServer()

	// Not configured → 501.
	rec := httptest.NewRecorder()
	s.handleGitHubAppRecheck(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", rec.Code)
	}

	// Configured + installed → installed JSON.
	s.SetGitHubAppRecheckFn(func() bool { return true })
	rec = httptest.NewRecorder()
	s.handleGitHubAppRecheck(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Configured but not installed, with a perm issue → insufficient_permissions.
	s.SetGitHubAppRecheckFn(func() bool { return false })
	s.SetGitHubAppRequired(true)
	s.SetGitHubAppPermIssue("issues:write")
	rec = httptest.NewRecorder()
	s.handleGitHubAppRecheck(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 body, got %d", rec.Code)
	}
}

// --- Advisory digest get/set ---

func TestCovE_AdvisoryDigest(t *testing.T) {
	s := covStateServer()
	if s.GetAdvisoryDigest() != nil {
		t.Fatalf("expected nil advisory digest initially")
	}
	s.SetAdvisoryDigest(map[string]int{"n": 1})
	if s.GetAdvisoryDigest() == nil {
		t.Fatalf("expected advisory digest set")
	}
}

// --- Liveness / health ---

func TestCovE_HandleLivezAndHealth(t *testing.T) {
	s := covStateServer()

	// Before MarkReady → 503 starting on both.
	rec := httptest.NewRecorder()
	s.handleLivez(rec, httptest.NewRequest(http.MethodGet, "/api/livez", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("livez before ready: expected 503, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health before ready: expected 503, got %d", rec.Code)
	}

	// After MarkReady → 200 (no hub heartbeat configured in tests, so the
	// heartbeat-staleness branch is skipped and it reports ok).
	s.MarkReady()
	rec = httptest.NewRecorder()
	s.handleLivez(rec, httptest.NewRequest(http.MethodGet, "/api/livez", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("livez after ready: expected 200, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health after ready: expected 200, got %d", rec.Code)
	}
}
