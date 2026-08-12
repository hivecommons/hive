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

func TestBuildImagePullsResponse_Deltas(t *testing.T) {
	series := []pullSnapshot{
		{Date: "2026-08-01", Cumulative: 1000},
		{Date: "2026-08-02", Cumulative: 1050}, // +50
		{Date: "2026-08-03", Cumulative: 1075}, // +25
		{Date: "2026-08-04", Cumulative: 1200}, // +125
	}
	got := buildImagePullsResponse(series)
	if got.Collecting {
		t.Fatalf("did not expect Collecting with %d snapshots", len(series))
	}
	if len(got.Points) != 3 {
		t.Fatalf("want 3 delta points, got %d", len(got.Points))
	}
	wantPulls := []int64{50, 25, 125}
	for i, p := range got.Points {
		if p.Pulls != wantPulls[i] {
			t.Errorf("point %d: want %d pulls, got %d", i, wantPulls[i], p.Pulls)
		}
	}
	if got.Total30d != 200 {
		t.Errorf("want Total30d=200, got %d", got.Total30d)
	}
	if got.Latest != 125 {
		t.Errorf("want Latest=125, got %d", got.Latest)
	}
}

func TestBuildImagePullsResponse_ColdStart(t *testing.T) {
	// Zero snapshots.
	if r := buildImagePullsResponse(nil); !r.Collecting || len(r.Points) != 0 {
		t.Errorf("empty series: want collecting/no points, got %+v", r)
	}
	// One snapshot — still no delta possible.
	one := []pullSnapshot{{Date: "2026-08-01", Cumulative: 42}}
	if r := buildImagePullsResponse(one); !r.Collecting || len(r.Points) != 0 {
		t.Errorf("single snapshot: want collecting/no points, got %+v", r)
	}
}

func TestBuildImagePullsResponse_NegativeDeltaClamped(t *testing.T) {
	// Counter appears to go backwards (reset / bad read): delta must clamp to 0,
	// never a negative spike, and must not drag the total below the good days.
	series := []pullSnapshot{
		{Date: "2026-08-01", Cumulative: 5000},
		{Date: "2026-08-02", Cumulative: 100}, // reset → clamped to 0
		{Date: "2026-08-03", Cumulative: 130}, // +30
	}
	got := buildImagePullsResponse(series)
	if got.Points[0].Pulls != 0 {
		t.Errorf("want clamped 0 for reset day, got %d", got.Points[0].Pulls)
	}
	if got.Points[1].Pulls != 30 {
		t.Errorf("want 30 for recovery day, got %d", got.Points[1].Pulls)
	}
	if got.Total30d != 30 {
		t.Errorf("want Total30d=30 (clamped), got %d", got.Total30d)
	}
}

func TestPruneOldSnapshots_WindowAndDedupe(t *testing.T) {
	// Build more than the window plus a duplicate date to exercise both paths.
	var in []pullSnapshot
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < pullHistoryWindowDays+5; i++ {
		d := base.AddDate(0, 0, i).Format("2006-01-02")
		in = append(in, pullSnapshot{Date: d, Cumulative: int64(i * 10)})
	}
	// Duplicate the last date with a newer cumulative — last-write should win.
	lastDate := in[len(in)-1].Date
	in = append(in, pullSnapshot{Date: lastDate, Cumulative: 99999})

	out := pruneOldSnapshots(in)
	if len(out) != pullHistoryWindowDays {
		t.Fatalf("want window of %d, got %d", pullHistoryWindowDays, len(out))
	}
	// Sorted ascending.
	for i := 1; i < len(out); i++ {
		if out[i-1].Date > out[i].Date {
			t.Fatalf("not sorted ascending at %d: %s > %s", i, out[i-1].Date, out[i].Date)
		}
	}
	// Dedupe kept the last write for the duplicated date.
	if out[len(out)-1].Cumulative != 99999 {
		t.Errorf("dedupe last-write-wins failed: got cumulative %d", out[len(out)-1].Cumulative)
	}
}

func TestFetchCumulativePulls_ScrapesTitle(t *testing.T) {
	// Serve a page shaped like the real GHCR package page.
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

func TestMaybeSnapshot_PersistsAndDailyGuard(t *testing.T) {
	resetImagePullsState()
	dir := t.TempDir()
	origPath := imagePullsPath
	imagePullsPath = filepath.Join(dir, "image-pulls.json")
	defer func() { imagePullsPath = origPath }()

	// Stub the network so no real HTTP happens.
	var cumulative int64 = 1000
	origFetch := fetchCumulativePulls
	fetchCumulativePulls = func(ctx context.Context, logger *slog.Logger) (int64, bool) {
		return cumulative, true
	}
	defer func() { fetchCumulativePulls = origFetch }()

	s := &HubServer{logger: slog.Default()}
	day1 := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)

	// Two calls on the SAME day → only one snapshot (daily guard).
	s.maybeSnapshotImagePulls(context.Background(), day1)
	cumulative = 1234 // would-be second read, same day
	s.maybeSnapshotImagePulls(context.Background(), day1.Add(3*time.Hour))

	imagePullsMu.Lock()
	n := len(imagePullSeries)
	imagePullsMu.Unlock()
	if n != 1 {
		t.Fatalf("same-day guard failed: want 1 snapshot, got %d", n)
	}

	// Next day → a second snapshot.
	day2 := day1.AddDate(0, 0, 1)
	cumulative = 1300
	s.maybeSnapshotImagePulls(context.Background(), day2)

	imagePullsMu.Lock()
	n = len(imagePullSeries)
	imagePullsMu.Unlock()
	if n != 2 {
		t.Fatalf("want 2 snapshots across two days, got %d", n)
	}

	// Persisted to disk.
	if _, err := os.Stat(imagePullsPath); err != nil {
		t.Fatalf("series not persisted: %v", err)
	}

	// Fresh load restores it (simulating a hub restart).
	resetImagePullsState()
	loadPersistedImagePulls(slog.Default())
	imagePullsMu.Lock()
	n = len(imagePullSeries)
	imagePullsMu.Unlock()
	if n != 2 {
		t.Fatalf("restore after restart failed: want 2, got %d", n)
	}

	// And the delta comes out as day2-day1 = 300.
	resp := buildImagePullsResponse(imagePullSeries)
	if resp.Latest != 300 || resp.Total30d != 300 {
		t.Errorf("want latest/total 300, got latest=%d total=%d", resp.Latest, resp.Total30d)
	}
}
