package hub

import (
	"context"
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
	imagePullSeries = nil
	imagePullsLoaded = false
	imagePullsMu.Unlock()
}

// setV2LatestSHA sets the v2 branch's latest-SHA the release snapshotter keys on.
func setV2LatestSHA(sha string) {
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: sha}
	latestSHAMu.Unlock()
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

func TestMaybeSnapshot_PersistsAndReleaseGuard(t *testing.T) {
	resetImagePullsState()
	dir := t.TempDir()
	origPath := imagePullsPath
	imagePullsPath = filepath.Join(dir, "image-pulls.json")
	defer func() { imagePullsPath = origPath }()

	var cumulative int64 = 1000
	origFetch := fetchCumulativePulls
	fetchCumulativePulls = func(ctx context.Context, logger *slog.Logger) (int64, bool) {
		return cumulative, true
	}
	defer func() { fetchCumulativePulls = origFetch }()

	s := &HubServer{logger: slog.Default()}
	t0 := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)

	// Release 1 (SHA aaa1111). Two ticks with the SAME SHA → only one snapshot.
	setV2LatestSHA("aaa1111deadbeef")
	s.maybeSnapshotImagePulls(context.Background(), t0)
	cumulative = 1234 // would-be second read, same release
	s.maybeSnapshotImagePulls(context.Background(), t0.Add(3*time.Hour))

	imagePullsMu.Lock()
	n := len(imagePullSeries)
	imagePullsMu.Unlock()
	if n != 1 {
		t.Fatalf("same-release guard failed: want 1 snapshot, got %d", n)
	}

	// A NEW release (SHA bbb2222) → a second snapshot.
	setV2LatestSHA("bbb2222cafef00d")
	cumulative = 1300
	s.maybeSnapshotImagePulls(context.Background(), t0.Add(6*time.Hour))

	imagePullsMu.Lock()
	n = len(imagePullSeries)
	imagePullsMu.Unlock()
	if n != 2 {
		t.Fatalf("want 2 snapshots across two releases, got %d", n)
	}

	if _, err := os.Stat(imagePullsPath); err != nil {
		t.Fatalf("series not persisted: %v", err)
	}

	// Restore after restart.
	resetImagePullsState()
	loadPersistedImagePulls(slog.Default())
	imagePullsMu.Lock()
	n = len(imagePullSeries)
	imagePullsMu.Unlock()
	if n != 2 {
		t.Fatalf("restore after restart failed: want 2, got %d", n)
	}

	// The aaa1111 release window = 1300-1000 = 300, keyed to the short SHA.
	resp := buildImagePullsResponse(imagePullSeries)
	if resp.Latest != 300 || resp.TotalWindow != 300 {
		t.Errorf("want latest/total 300, got latest=%d total=%d", resp.Latest, resp.TotalWindow)
	}
	if len(resp.Points) != 1 || resp.Points[0].SHA != "aaa1111" {
		t.Errorf("want one bar keyed to short SHA aaa1111, got %+v", resp.Points)
	}
}
