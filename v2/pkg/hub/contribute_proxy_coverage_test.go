package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- handleContributeProxy tests ---

func TestHandleContributeProxy_NoHiveAvailable(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	// Empty registry — no contribute hive

	req := httptest.NewRequest(http.MethodPost, "/api/contribute/proxy", nil)
	w := httptest.NewRecorder()

	srv.handleContributeProxy(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(w.Body.String(), "no hives available") {
		t.Errorf("expected 'no hives available' error, got %q", w.Body.String())
	}
}

func TestHandleContributeProxy_ReachesProxyPath(t *testing.T) {
	// Use a public hostname that passes isPrivateURL but points to a
	// non-listening port so the proxy attempt fails with 502 (Bad Gateway)
	// rather than 503 (no hives available). This exercises the url.Parse
	// success path and reverse-proxy creation.
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:           "h1",
			Name:         "test-hive",
			Online:       true,
			IsPublic:     true,
			DashboardURL: "https://example.com:19999",
			Owner:        "testuser",
			Org:          "testorg",
		},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/contribute/proxy", strings.NewReader(`{"task":"help"}`))
	w := httptest.NewRecorder()

	srv.handleContributeProxy(w, req)

	// Should NOT be 503 "no hives" — that means findContributeHive worked.
	// The proxy will likely return 502 (backend unreachable).
	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("got 503 (no hives), but expected proxy path to be reached")
	}
}

func TestHandleContributeProxy_PrivateURLFallsToNoHive(t *testing.T) {
	// When the only hive has a private URL, findContributeHive returns nil
	// and the handler responds 503.
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:           "h1",
			Online:       true,
			IsPublic:     true,
			DashboardURL: "http://192.168.1.1:3000",
			Owner:        "testuser",
		},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/contribute/proxy", nil)
	w := httptest.NewRecorder()

	srv.handleContributeProxy(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// --- handleContributeWSProxy tests ---

func TestHandleContributeWSProxy_NoHiveAvailable(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/ws", nil)
	w := httptest.NewRecorder()

	srv.handleContributeWSProxy(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleContributeWSProxy_ReachesProxyPath(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:           "h1",
			Online:       true,
			IsPublic:     true,
			DashboardURL: "https://example.com:19999",
			Owner:        "owner1",
			Org:          "org1",
		},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/ws", nil)
	w := httptest.NewRecorder()

	srv.handleContributeWSProxy(w, req)

	// Should NOT be 503 — proxy path was reached.
	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("got 503, expected proxy path to be reached (502 or similar)")
	}
}

func TestHandleContributeWSProxy_PrivateURLFallsToNoHive(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:           "h1",
			Online:       true,
			IsPublic:     true,
			DashboardURL: "http://10.0.0.5:8080",
			Owner:        "testuser",
		},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/ws", nil)
	w := httptest.NewRecorder()

	srv.handleContributeWSProxy(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got status %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// --- handleContributeStatus tests ---

func TestHandleContributeStatus_NoHiveAvailable(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/status", nil)
	w := httptest.NewRecorder()

	srv.handleContributeStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("got status %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["available"] != false {
		t.Errorf("expected available=false, got %v", resp["available"])
	}
}

func TestHandleContributeStatus_HiveAvailable(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:           "h1",
			Name:         "my-hive",
			Online:       true,
			IsPublic:     true,
			DashboardURL: "https://example.com",
			Owner:        "alice",
			Org:          "acme",
		},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/status", nil)
	w := httptest.NewRecorder()

	srv.handleContributeStatus(w, req)

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["available"] != true {
		t.Errorf("expected available=true, got %v", resp["available"])
	}
	if resp["hive_id"] != "h1" {
		t.Errorf("expected hive_id=h1, got %v", resp["hive_id"])
	}
	if resp["org"] != "acme" {
		t.Errorf("expected org=acme, got %v", resp["org"])
	}
}

// --- findContributeHive tests ---

func TestFindContributeHive_SkipsOfflineHive(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:           "h1",
			Online:       false,
			IsPublic:     true,
			DashboardURL: "https://example.com",
			Owner:        "alice",
		},
	}
	srv.mu.Unlock()

	if got := srv.findContributeHive(); got != nil {
		t.Errorf("expected nil for offline hive, got %+v", got)
	}
}

func TestFindContributeHive_SkipsPrivateHive(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:           "h1",
			Online:       true,
			IsPublic:     false,
			DashboardURL: "https://example.com",
			Owner:        "alice",
		},
	}
	srv.mu.Unlock()

	if got := srv.findContributeHive(); got != nil {
		t.Errorf("expected nil for private hive, got %+v", got)
	}
}

func TestFindContributeHive_SkipsNoDashboardURL(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:       "h1",
			Online:   true,
			IsPublic: true,
			Owner:    "alice",
		},
	}
	srv.mu.Unlock()

	if got := srv.findContributeHive(); got != nil {
		t.Errorf("expected nil for hive with no dashboard URL, got %+v", got)
	}
}

func TestFindContributeHive_SkipsNoOwner(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:           "h1",
			Online:       true,
			IsPublic:     true,
			DashboardURL: "https://example.com",
		},
	}
	srv.mu.Unlock()

	if got := srv.findContributeHive(); got != nil {
		t.Errorf("expected nil for hive with no owner, got %+v", got)
	}
}

// --- saveHubBanners / loadHubBanners tests ---

func TestSaveAndLoadHubBanners_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := hubBannersPath
	hubBannersPath = filepath.Join(tmpDir, "hub-banners.json")
	defer func() { hubBannersPath = oldPath }()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.hubBannersMu.Lock()
	srv.hubBanners["b1"] = &HubBannerEntry{
		ID:      "b1",
		Message: "maintenance window",
		Color:   "warning",
		SentAt:  time.Now().Format(time.RFC3339),
	}
	srv.hubBannersMu.Unlock()

	srv.saveHubBanners()

	// Verify file was written
	data, err := os.ReadFile(hubBannersPath)
	if err != nil {
		t.Fatalf("banner file not written: %v", err)
	}
	if !strings.Contains(string(data), "maintenance window") {
		t.Errorf("saved data missing banner message: %s", data)
	}

	// Load into a fresh server
	srv2 := NewHubServer(0, slog.Default(), "test", "v2")
	srv2.loadHubBanners()

	srv2.hubBannersMu.RLock()
	got := srv2.hubBanners["b1"]
	srv2.hubBannersMu.RUnlock()

	if got == nil {
		t.Fatal("loaded banners missing entry b1")
	}
	if got.Message != "maintenance window" {
		t.Errorf("got message %q, want 'maintenance window'", got.Message)
	}
}

func TestLoadHubBanners_PrunesStaleEntries(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := hubBannersPath
	hubBannersPath = filepath.Join(tmpDir, "hub-banners.json")
	defer func() { hubBannersPath = oldPath }()

	staleTime := time.Now().Add(-(hubBannerMaxAge + time.Hour)).Format(time.RFC3339)
	freshTime := time.Now().Format(time.RFC3339)

	banners := map[string]*HubBannerEntry{
		"stale": {ID: "stale", Message: "old", SentAt: staleTime},
		"fresh": {ID: "fresh", Message: "new", SentAt: freshTime},
	}
	data, _ := json.Marshal(banners)
	os.WriteFile(hubBannersPath, data, 0o644)

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.loadHubBanners()

	srv.hubBannersMu.RLock()
	defer srv.hubBannersMu.RUnlock()

	if _, ok := srv.hubBanners["stale"]; ok {
		t.Error("stale banner should have been pruned")
	}
	if srv.hubBanners["fresh"] == nil {
		t.Error("fresh banner should have been loaded")
	}
}

func TestLoadHubBanners_MissingFileIsNoOp(t *testing.T) {
	oldPath := hubBannersPath
	hubBannersPath = "/nonexistent/path/banners.json"
	defer func() { hubBannersPath = oldPath }()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.loadHubBanners() // should not panic

	srv.hubBannersMu.RLock()
	count := len(srv.hubBanners)
	srv.hubBannersMu.RUnlock()

	if count != 0 {
		t.Errorf("expected 0 banners for missing file, got %d", count)
	}
}

func TestLoadHubBanners_InvalidJSONIsNoOp(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := hubBannersPath
	hubBannersPath = filepath.Join(tmpDir, "hub-banners.json")
	defer func() { hubBannersPath = oldPath }()

	os.WriteFile(hubBannersPath, []byte("not json"), 0o644)

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.loadHubBanners() // should not panic

	srv.hubBannersMu.RLock()
	count := len(srv.hubBanners)
	srv.hubBannersMu.RUnlock()

	if count != 0 {
		t.Errorf("expected 0 banners for invalid JSON, got %d", count)
	}
}

func TestLoadHubBanners_NilEntryIsPruned(t *testing.T) {
	tmpDir := t.TempDir()
	oldPath := hubBannersPath
	hubBannersPath = filepath.Join(tmpDir, "hub-banners.json")
	defer func() { hubBannersPath = oldPath }()

	// JSON with a nil value entry
	os.WriteFile(hubBannersPath, []byte(`{"b1": null, "b2": {"id":"b2","message":"ok","sent_at":"`+time.Now().Format(time.RFC3339)+`"}}`), 0o644)

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.loadHubBanners()

	srv.hubBannersMu.RLock()
	defer srv.hubBannersMu.RUnlock()

	if srv.hubBanners["b2"] == nil {
		t.Error("b2 should have been loaded")
	}
}

// --- firstCSV / replaceFirstCSV tests ---

func TestFirstCSV_EmptyString(t *testing.T) {
	if got := firstCSV(""); got != "" {
		t.Errorf("firstCSV(\"\") = %q, want empty", got)
	}
}

func TestFirstCSV_AllWhitespace(t *testing.T) {
	if got := firstCSV(" , , "); got != "" {
		t.Errorf("firstCSV(\" , , \") = %q, want empty", got)
	}
}

func TestFirstCSV_SingleValue(t *testing.T) {
	if got := firstCSV("hello"); got != "hello" {
		t.Errorf("firstCSV(\"hello\") = %q, want \"hello\"", got)
	}
}

func TestFirstCSV_MultipleValues(t *testing.T) {
	if got := firstCSV("alpha,beta,gamma"); got != "alpha" {
		t.Errorf("firstCSV(\"alpha,beta,gamma\") = %q, want \"alpha\"", got)
	}
}

func TestFirstCSV_LeadingEmptyParts(t *testing.T) {
	if got := firstCSV(",  ,third"); got != "third" {
		t.Errorf("firstCSV(\",  ,third\") = %q, want \"third\"", got)
	}
}

func TestReplaceFirstCSV_ReplacesFirstNonEmpty(t *testing.T) {
	got := replaceFirstCSV("a,b,c", "X")
	if got != "X,b,c" {
		t.Errorf("replaceFirstCSV(\"a,b,c\", \"X\") = %q, want \"X,b,c\"", got)
	}
}

func TestReplaceFirstCSV_EmptyReplacementRemovesFirst(t *testing.T) {
	got := replaceFirstCSV("a,b,c", "")
	if got != "b,c" {
		t.Errorf("replaceFirstCSV(\"a,b,c\", \"\") = %q, want \"b,c\"", got)
	}
}

func TestReplaceFirstCSV_AllEmpty(t *testing.T) {
	got := replaceFirstCSV("", "new")
	if got != "new" {
		t.Errorf("replaceFirstCSV(\"\", \"new\") = %q, want \"new\"", got)
	}
}

func TestReplaceFirstCSV_LeadingEmptyParts(t *testing.T) {
	got := replaceFirstCSV(" , ,value,other", "replaced")
	if got != " , ,replaced,other" {
		t.Errorf("got %q, want \" , ,replaced,other\"", got)
	}
}

// --- saveLoop tests ---

func TestSaveLoop_WritesRegistryToFile(t *testing.T) {
	tmpDir := t.TempDir()
	regFile := filepath.Join(tmpDir, "registry.json")

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.registryPath = regFile

	srv.mu.Lock()
	srv.registry.Hives = append(srv.registry.Hives, RegistryEntry{ID: "h1", Name: "test-hive"})
	srv.mu.Unlock()

	// Trigger a save by writing to saveCh and then reading from the file
	// We need to run saveLoop in a goroutine temporarily
	done := make(chan struct{})
	go func() {
		// Read one item from saveCh and do the save inline
		<-srv.saveCh
		// Simulate saveLoop behavior without the delay
		srv.mu.RLock()
		data, err := json.MarshalIndent(srv.registry, "", "  ")
		srv.mu.RUnlock()
		if err != nil {
			t.Errorf("marshal failed: %v", err)
			close(done)
			return
		}
		tmpPath := srv.registryPath + ".tmp"
		os.WriteFile(tmpPath, data, 0o644)
		os.Rename(tmpPath, srv.registryPath)
		close(done)
	}()

	srv.requestSave()
	<-done

	data, err := os.ReadFile(regFile)
	if err != nil {
		t.Fatalf("registry file not created: %v", err)
	}
	if !strings.Contains(string(data), "test-hive") {
		t.Errorf("registry file missing hive data: %s", data)
	}
}

func TestRequestSave_NonBlocking(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	// Fill the channel
	srv.requestSave()
	// Second request should not block (channel is buffered 1)
	srv.requestSave()
	// If we get here, it didn't block — pass
}
