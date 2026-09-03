package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// resetImagePullsState clears the package-level series between tests so cases
// don't bleed into one another (mirrors how the SHA-cache tests reset state).
func resetImagePullsState() {
	imagePullsMu.Lock()
	imagePullSeriesByLine = map[string][]pullSnapshot{}
	imagePullsLoaded = false
	imagePullsMu.Unlock()
}

// setBranchLatestSHAs replaces the latest-SHA cache the release snapshotter
// keys on, returning a restore func so a test can't bleed branches into the
// rest of the package's tests.
func setBranchLatestSHAs(t *testing.T, shas map[string]string) {
	t.Helper()
	latestSHAMu.Lock()
	orig := latestSHAByBranch
	latestSHAByBranch = map[string]branchSHAInfo{}
	for b, sha := range shas {
		latestSHAByBranch[b] = branchSHAInfo{SHA: sha}
	}
	latestSHAMu.Unlock()
	t.Cleanup(func() {
		latestSHAMu.Lock()
		latestSHAByBranch = orig
		latestSHAMu.Unlock()
	})
}

func TestBuildImagePullsResponse_PerRelease(t *testing.T) {
	// Four release snapshots → three CLOSED windows (the newest release is still
	// accruing and gets no bar yet). Each bar belongs to the EARLIER release.
	series := []pullSnapshot{
		{SHA: "aaa1111", Date: "2026-08-01", Cumulative: 1000},
		{SHA: "bbb2222", Date: "2026-08-02", Cumulative: 1050}, // aaa window = +50
		{SHA: "ccc3333", Date: "2026-08-03", Cumulative: 1075}, // bbb window = +25
		{SHA: "ddd4444", Date: "2026-08-04", Cumulative: 1200}, // ccc window = +125
	}
	got := buildImagePullsResponse(series)
	if got.Collecting {
		t.Fatalf("did not expect Collecting with %d snapshots", len(series))
	}
	if len(got.Points) != 3 {
		t.Fatalf("want 3 release bars, got %d", len(got.Points))
	}
	wantSHA := []string{"aaa1111", "bbb2222", "ccc3333"}
	wantPulls := []int64{50, 25, 125}
	for i, p := range got.Points {
		if p.SHA != wantSHA[i] {
			t.Errorf("bar %d: want sha %s, got %s", i, wantSHA[i], p.SHA)
		}
		if p.Pulls != wantPulls[i] {
			t.Errorf("bar %d: want %d pulls, got %d", i, wantPulls[i], p.Pulls)
		}
	}
	if got.TotalWindow != 200 {
		t.Errorf("want TotalWindow=200, got %d", got.TotalWindow)
	}
	if got.Latest != 125 {
		t.Errorf("want Latest=125, got %d", got.Latest)
	}
	if got.Releases != 3 {
		t.Errorf("want Releases=3, got %d", got.Releases)
	}
}

func TestBuildImagePullsResponse_ColdStart(t *testing.T) {
	if r := buildImagePullsResponse(nil); !r.Collecting || len(r.Points) != 0 {
		t.Errorf("empty series: want collecting/no points, got %+v", r)
	}
	// One release snapshot — no window can be closed yet.
	one := []pullSnapshot{{SHA: "aaa1111", Date: "2026-08-01", Cumulative: 42}}
	if r := buildImagePullsResponse(one); !r.Collecting || len(r.Points) != 0 {
		t.Errorf("single snapshot: want collecting/no points, got %+v", r)
	}
}

func TestBuildImagePullsResponse_NegativeDeltaClamped(t *testing.T) {
	// Counter appears to go backwards (reset / bad read): delta must clamp to 0.
	series := []pullSnapshot{
		{SHA: "aaa1111", Date: "2026-08-01", Cumulative: 5000},
		{SHA: "bbb2222", Date: "2026-08-02", Cumulative: 100}, // reset → clamped to 0
		{SHA: "ccc3333", Date: "2026-08-03", Cumulative: 130}, // +30
	}
	got := buildImagePullsResponse(series)
	if got.Points[0].Pulls != 0 {
		t.Errorf("want clamped 0 for reset window, got %d", got.Points[0].Pulls)
	}
	if got.Points[1].Pulls != 30 {
		t.Errorf("want 30 for recovery window, got %d", got.Points[1].Pulls)
	}
	if got.TotalWindow != 30 {
		t.Errorf("want TotalWindow=30 (clamped), got %d", got.TotalWindow)
	}
}

func TestPruneOldSnapshots_WindowDedupeAndLegacyDrop(t *testing.T) {
	var in []pullSnapshot
	// More than window+1 releases, distinct SHAs.
	for i := 0; i < pullReleaseWindow+5; i++ {
		in = append(in, pullSnapshot{
			SHA:        "sha" + string(rune('a'+i)),
			Date:       "2026-01-01",
			Cumulative: int64(i * 10),
		})
	}
	// A legacy day-keyed entry (no SHA) must be dropped.
	in = append([]pullSnapshot{{Date: "2025-12-31", Cumulative: 1}}, in...)
	// Re-observe the last release with a newer cumulative — last-write wins.
	lastSHA := in[len(in)-1].SHA
	in = append(in, pullSnapshot{SHA: lastSHA, Date: "2026-01-02", Cumulative: 99999})

	out := pruneOldSnapshots(in)
	if len(out) != pullReleaseWindow+1 {
		t.Fatalf("want window+1 = %d, got %d", pullReleaseWindow+1, len(out))
	}
	for _, s := range out {
		if s.SHA == "" {
			t.Fatalf("legacy day-keyed entry leaked through: %+v", s)
		}
	}
	// Consecutive dup of the same SHA collapsed, last-write kept.
	if out[len(out)-1].Cumulative != 99999 {
		t.Errorf("dedupe last-write-wins failed: got cumulative %d", out[len(out)-1].Cumulative)
	}
}

func TestFetchCumulativePulls_ScrapesTitle(t *testing.T) {
	page := `<html><body>
	  <span>Total downloads</span>
	  <h3 title="127576">128K</h3>
	</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	orig := pullPackagePageURL
	pullPackagePageURL = srv.URL
	defer func() { pullPackagePageURL = orig }()

	n, ok := fetchCumulativePulls(context.Background(), slog.Default())
	if !ok {
		t.Fatal("expected successful scrape")
	}
	if n != 127576 {
		t.Errorf("want 127576, got %d", n)
	}
}

func TestFetchCumulativePulls_CounterMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><body>no counter here</body></html>"))
	}))
	defer srv.Close()
	orig := pullPackagePageURL
	pullPackagePageURL = srv.URL
	defer func() { pullPackagePageURL = orig }()

	if _, ok := fetchCumulativePulls(context.Background(), slog.Default()); ok {
		t.Error("expected failure when counter is absent")
	}
}

func TestMaybeSnapshot_PerLinePersistsAndReleaseGuard(t *testing.T) {
	resetImagePullsState()
	dir := t.TempDir()
	origPath := imagePullsPath
	imagePullsPath = filepath.Join(dir, "image-pulls.json")
	defer func() { imagePullsPath = origPath }()

	var cumulative int64 = 1000
	fetches := 0
	origFetch := fetchCumulativePulls
	fetchCumulativePulls = func(ctx context.Context, logger *slog.Logger) (int64, bool) {
		fetches++
		return cumulative, true
	}
	defer func() { fetchCumulativePulls = origFetch }()

	s := &HubServer{logger: slog.Default()}
	t0 := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)

	// Two lines resolve. Two ticks with the SAME SHAs → one snapshot per line,
	// and the package-wide counter is fetched ONCE per tick, shared by both.
	setBranchLatestSHAs(t, map[string]string{
		"v2": "aaa1111deadbeef",
		"v4": "ccc3333feedface",
	})
	s.maybeSnapshotImagePulls(context.Background(), t0)
	if fetches != 1 {
		t.Fatalf("want 1 shared cumulative fetch for both due lines, got %d", fetches)
	}
	cumulative = 1234 // would-be second read, same releases
	s.maybeSnapshotImagePulls(context.Background(), t0.Add(3*time.Hour))

	imagePullsMu.Lock()
	nV2, nV4 := len(imagePullSeriesByLine["v2"]), len(imagePullSeriesByLine["v4"])
	imagePullsMu.Unlock()
	if nV2 != 1 || nV4 != 1 {
		t.Fatalf("same-release guard failed: want 1 snapshot per line, got v2=%d v4=%d", nV2, nV4)
	}

	// A NEW release on v4 only (v2 is retired and never advances) → a second
	// v4 snapshot, v2 untouched.
	setBranchLatestSHAs(t, map[string]string{
		"v2": "aaa1111deadbeef",
		"v4": "ddd4444cafef00d",
	})
	cumulative = 1300
	s.maybeSnapshotImagePulls(context.Background(), t0.Add(6*time.Hour))

	imagePullsMu.Lock()
	nV2, nV4 = len(imagePullSeriesByLine["v2"]), len(imagePullSeriesByLine["v4"])
	imagePullsMu.Unlock()
	if nV2 != 1 || nV4 != 2 {
		t.Fatalf("want v2=1 v4=2 snapshots, got v2=%d v4=%d", nV2, nV4)
	}

	if _, err := os.Stat(imagePullsPath); err != nil {
		t.Fatalf("series not persisted: %v", err)
	}

	// Restore after restart.
	resetImagePullsState()
	loadPersistedImagePulls(slog.Default())
	imagePullsMu.Lock()
	nV2, nV4 = len(imagePullSeriesByLine["v2"]), len(imagePullSeriesByLine["v4"])
	v4Series := append([]pullSnapshot(nil), imagePullSeriesByLine["v4"]...)
	imagePullsMu.Unlock()
	if nV2 != 1 || nV4 != 2 {
		t.Fatalf("restore after restart failed: want v2=1 v4=2, got v2=%d v4=%d", nV2, nV4)
	}

	// The ccc3333 release window on v4 = 1300-1000 = 300, keyed to the short SHA.
	resp := buildImagePullsResponse(v4Series)
	if resp.Latest != 300 || resp.TotalWindow != 300 {
		t.Errorf("want latest/total 300, got latest=%d total=%d", resp.Latest, resp.TotalWindow)
	}
	if len(resp.Points) != 1 || resp.Points[0].SHA != "ccc3333" {
		t.Errorf("want one bar keyed to short SHA ccc3333, got %+v", resp.Points)
	}
}

func TestLoadPersistedImagePulls_LegacyArrayMigratesToV2(t *testing.T) {
	// The pre-per-line file was a bare snapshot array that only ever tracked
	// the v2 branch. It must load under the "v2" line, not vanish and not be
	// attributed to the active line.
	resetImagePullsState()
	dir := t.TempDir()
	origPath := imagePullsPath
	imagePullsPath = filepath.Join(dir, "image-pulls.json")
	defer func() { imagePullsPath = origPath }()

	legacy := `[{"sha":"aaa1111","date":"2026-08-01","cumulative":1000},
	            {"sha":"bbb2222","date":"2026-08-02","cumulative":1050}]`
	if err := os.WriteFile(imagePullsPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	loadPersistedImagePulls(slog.Default())

	imagePullsMu.Lock()
	series := append([]pullSnapshot(nil), imagePullSeriesByLine[legacyPullLine]...)
	imagePullsMu.Unlock()
	if len(series) != 2 || series[0].SHA != "aaa1111" {
		t.Fatalf("legacy array not migrated under %q: %+v", legacyPullLine, series)
	}
}

func TestActiveReleaseLine_FollowsStableChannel(t *testing.T) {
	// The stable channel's digest-resolved branch is the source of truth —
	// exactly what standalone-images.sh pins (ghcr.io/hivecommons/hive:stable).
	channels := []ChannelTarget{
		{Channel: ReleaseChannelStable, Branch: "v4", SHA: "ccc3333"},
		{Channel: ReleaseChannelEdge, Branch: "v5", SHA: "eee5555"},
	}
	shas := map[string]string{"v2": "aaa1111", "v4": "ccc3333", "v5": "eee5555"}
	if got := activeReleaseLine(channels, shas); got != "v4" {
		t.Errorf("want v4 (stable channel's branch), got %q", got)
	}
	// After a promotion, the SAME code follows with no hub change.
	channels[0].Branch = "v5"
	if got := activeReleaseLine(channels, shas); got != "v5" {
		t.Errorf("want v5 after stable re-points, got %q", got)
	}
}

func TestActiveReleaseLine_FallbackHighestNumberedLine(t *testing.T) {
	// Stable unresolved (GHCR blip / pinned off-branch): fall back to the
	// highest-numbered v<N> line with a resolved SHA. Non-release branches
	// (dd, mk) are never candidates.
	channels := []ChannelTarget{{Channel: ReleaseChannelStable}} // no Branch
	shas := map[string]string{"v2": "aaa1111", "v4": "ccc3333", "dd": "fff6666", "mk": "abc9999"}
	if got := activeReleaseLine(channels, shas); got != "v4" {
		t.Errorf("want v4 (highest resolved release line), got %q", got)
	}
	if got := activeReleaseLine(nil, map[string]string{"dd": "fff6666"}); got != "" {
		t.Errorf("want empty when no release line resolves, got %q", got)
	}
	if got := activeReleaseLine(nil, nil); got != "" {
		t.Errorf("want empty on no data, got %q", got)
	}
}

func TestBuildImagePullsMulti_ActiveAndPerLine(t *testing.T) {
	byLine := map[string][]pullSnapshot{
		"v2": {
			{SHA: "aaa1111", Date: "2026-08-01", Cumulative: 1000},
			{SHA: "bbb2222", Date: "2026-08-02", Cumulative: 1050},
		},
		"v4": {
			{SHA: "ccc3333", Date: "2026-08-03", Cumulative: 1100},
			{SHA: "ddd4444", Date: "2026-08-04", Cumulative: 1400},
		},
	}
	got := buildImagePullsMulti(byLine, "v4")
	if got.Line != "v4" {
		t.Errorf("want Line=v4, got %q", got.Line)
	}
	if got.Collecting || got.Latest != 300 || got.TotalWindow != 300 {
		t.Errorf("flat fields must describe the ACTIVE line: %+v", got)
	}
	if len(got.Lines) != 2 {
		t.Fatalf("want 2 per-line series, got %d", len(got.Lines))
	}
	if v2 := got.Lines["v2"]; v2.Latest != 50 || v2.Collecting {
		t.Errorf("v2 line series wrong: %+v", v2)
	}
	if v4 := got.Lines["v4"]; v4.Latest != 300 {
		t.Errorf("v4 line series wrong: %+v", v4)
	}
}

func TestBuildImagePullsMulti_MissingActiveLineDegradesToCollecting(t *testing.T) {
	// A freshly cut line (v5 promoted to stable before any snapshot exists)
	// must degrade to "collecting" — never an error, never another line's data.
	byLine := map[string][]pullSnapshot{
		"v4": {
			{SHA: "ccc3333", Date: "2026-08-03", Cumulative: 1100},
			{SHA: "ddd4444", Date: "2026-08-04", Cumulative: 1400},
		},
	}
	got := buildImagePullsMulti(byLine, "v5")
	if !got.Collecting || len(got.Points) != 0 || got.Latest != 0 {
		t.Errorf("missing active line must be Collecting with no points, got %+v", got)
	}
	if got.Line != "v5" {
		t.Errorf("want Line=v5 even without data, got %q", got.Line)
	}
	if _, ok := got.Lines["v5"]; ok {
		t.Errorf("a line with no snapshots must be absent from Lines, not fabricated")
	}
	if v4 := got.Lines["v4"]; v4.Latest != 300 {
		t.Errorf("other lines must still be served: %+v", v4)
	}
}

func TestHandleImagePulls_ServesActiveLineAndPerLineSeries(t *testing.T) {
	resetImagePullsState()
	dir := t.TempDir()
	origPath := imagePullsPath
	imagePullsPath = filepath.Join(dir, "image-pulls.json")
	defer func() { imagePullsPath = origPath }()

	imagePullsMu.Lock()
	imagePullsLoaded = true
	imagePullSeriesByLine = map[string][]pullSnapshot{
		"v4": {
			{SHA: "ccc3333", Date: "2026-08-03", Cumulative: 1100},
			{SHA: "ddd4444", Date: "2026-08-04", Cumulative: 1400},
		},
	}
	imagePullsMu.Unlock()
	defer resetImagePullsState()

	setBranchLatestSHAs(t, map[string]string{"v4": "ddd4444cafef00d"})

	// Pre-warm the channel cache so the handler resolves "stable"→v4 without a
	// registry round-trip (the cache is what production requests normally hit).
	channelTargetMu.Lock()
	origCache, origAt := channelTargetCache, channelTargetCachedAt
	channelTargetCache = []ChannelTarget{{Channel: ReleaseChannelStable, Branch: "v4", SHA: "ddd4444", Digest: "sha256:feed"}}
	channelTargetCachedAt = time.Now()
	channelTargetMu.Unlock()
	defer func() {
		channelTargetMu.Lock()
		channelTargetCache, channelTargetCachedAt = origCache, origAt
		channelTargetMu.Unlock()
	}()

	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	s.handleImagePulls(rec, httptest.NewRequest(http.MethodGet, "/api/hub/image-pulls", nil))

	var resp imagePullsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if resp.Line != "v4" {
		t.Errorf("want line=v4 from the stable channel, got %q", resp.Line)
	}
	if resp.Latest != 300 || resp.Collecting {
		t.Errorf("want active-line stats latest=300, got %+v", resp)
	}
	if v4, ok := resp.Lines["v4"]; !ok || v4.Latest != 300 {
		t.Errorf("want per-line v4 series in payload, got %+v", resp.Lines)
	}
}
