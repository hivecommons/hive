package hub

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withTempBannersPath points hubBannersPath at a temp file for the duration of a
// test and restores it afterward.
func withTempBannersPath(t *testing.T) {
	t.Helper()
	orig := hubBannersPath
	hubBannersPath = filepath.Join(t.TempDir(), "hub-banners.json")
	t.Cleanup(func() { hubBannersPath = orig })
}

// TestHubBannerPersistenceRoundTrip verifies a sent banner survives a hub
// restart: save to disk, then a fresh server loads it back and still delivers it
// (the spyre/upgrade "banner vanished after a pod roll" bug).
func TestHubBannerPersistenceRoundTrip(t *testing.T) {
	withTempBannersPath(t)

	s1 := &HubServer{logger: slog.Default(), hubBanners: make(map[string]*HubBannerEntry)}
	s1.hubBanners["hive-a"] = &HubBannerEntry{
		ID: "b-1", Message: "maintenance tonight", Color: "amber",
		SentAt: time.Now().UTC().Format(time.RFC3339),
	}
	s1.saveHubBanners()

	// A brand-new server (simulating the restarted/upgraded pod) loads it back.
	s2 := &HubServer{logger: slog.Default(), hubBanners: make(map[string]*HubBannerEntry)}
	s2.loadHubBanners()

	got, ok := s2.hubBanners["hive-a"]
	if !ok {
		t.Fatalf("banner not restored after restart; map = %+v", s2.hubBanners)
	}
	if got.ID != "b-1" || got.Message != "maintenance tonight" || got.Color != "amber" {
		t.Errorf("restored banner = %+v, want id=b-1 message/color preserved", got)
	}
}

// TestHubBannerPersistenceDropsStale verifies banners older than hubBannerMaxAge
// are pruned on load (so a long-forgotten banner does not resurrect across an
// upgrade), while fresh banners are kept, and the pruned set is rewritten.
func TestHubBannerPersistenceDropsStale(t *testing.T) {
	withTempBannersPath(t)

	s1 := &HubServer{logger: slog.Default(), hubBanners: make(map[string]*HubBannerEntry)}
	fresh := time.Now().UTC()
	stale := fresh.Add(-hubBannerMaxAge - time.Hour)
	s1.hubBanners["fresh"] = &HubBannerEntry{ID: "f", Message: "new", SentAt: fresh.Format(time.RFC3339)}
	s1.hubBanners["stale"] = &HubBannerEntry{ID: "s", Message: "old", SentAt: stale.Format(time.RFC3339)}
	s1.saveHubBanners()

	s2 := &HubServer{logger: slog.Default(), hubBanners: make(map[string]*HubBannerEntry)}
	s2.loadHubBanners()

	if _, ok := s2.hubBanners["fresh"]; !ok {
		t.Errorf("fresh banner should survive load, map = %+v", s2.hubBanners)
	}
	if _, ok := s2.hubBanners["stale"]; ok {
		t.Errorf("stale banner should be dropped on load, map = %+v", s2.hubBanners)
	}

	// The pruned state should have been rewritten to disk: a third load sees only
	// the fresh banner.
	s3 := &HubServer{logger: slog.Default(), hubBanners: make(map[string]*HubBannerEntry)}
	s3.loadHubBanners()
	if len(s3.hubBanners) != 1 {
		t.Errorf("expected pruned file to hold 1 banner, got %d", len(s3.hubBanners))
	}
}

// TestHubBannerPersistenceMissingFile verifies loadHubBanners is a no-op (no
// error, empty map) when no banners have ever been persisted.
func TestHubBannerPersistenceMissingFile(t *testing.T) {
	withTempBannersPath(t)
	// Ensure the file does not exist.
	_ = os.Remove(hubBannersPath)

	s := &HubServer{logger: slog.Default(), hubBanners: make(map[string]*HubBannerEntry)}
	s.loadHubBanners()
	if len(s.hubBanners) != 0 {
		t.Errorf("expected empty banners for missing file, got %d", len(s.hubBanners))
	}
}

// TestHubBannerPersistedOnSendAndClear verifies the send and clear handlers write
// through to disk so the state survives a restart.
func TestHubBannerPersistedOnSendAndClear(t *testing.T) {
	withTempBannersPath(t)
	s := &HubServer{logger: slog.Default(), hubBanners: make(map[string]*HubBannerEntry)}

	rec := httptest.NewRecorder()
	s.handleSendHubBanner(rec, postJSON("/api/hub/banner", `{"message":"hi","hive_ids":["h1"]}`))
	if rec.Code != 200 {
		t.Fatalf("send status = %d body=%s", rec.Code, rec.Body.String())
	}
	// A fresh server loads the persisted banner.
	s2 := &HubServer{logger: slog.Default(), hubBanners: make(map[string]*HubBannerEntry)}
	s2.loadHubBanners()
	if _, ok := s2.hubBanners["h1"]; !ok {
		t.Fatalf("send did not persist banner; map = %+v", s2.hubBanners)
	}

	// Clear on the original, then a fresh load sees nothing.
	rec = httptest.NewRecorder()
	s.handleClearHubBanner(rec, postJSON("/api/hub/banner/clear", ``))
	if rec.Code != 200 {
		t.Fatalf("clear status = %d", rec.Code)
	}
	s3 := &HubServer{logger: slog.Default(), hubBanners: make(map[string]*HubBannerEntry)}
	s3.loadHubBanners()
	if len(s3.hubBanners) != 0 {
		t.Errorf("clear did not persist empty state; %d banners remain", len(s3.hubBanners))
	}
}
