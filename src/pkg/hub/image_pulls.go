package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// image_pulls.go tracks external adoption of the hive spoke image by charting
// how many container-image PULLS (downloads) the public GHCR package receives
// per day.
//
// WHY THIS IS A DAILY-DELTA, NOT A UNIQUE-DOWNLOAD COUNT
// -----------------------------------------------------
// GitHub exposes exactly ONE adoption number for a container package: a single
// CUMULATIVE "Total downloads" counter, rendered on the public package page
//
//	https://github.com/kubestellar/hive/pkgs/container/hive
//
// as `<h3 title="127576">128K</h3>` under a "Total downloads" label. There is
// NO public per-day metric and NO unique-IP / unique-puller metric anywhere in
// the REST, GraphQL, or web surface — the REST Packages API's PackageVersion
// object (google/go-github v72) carries no download field at all.
//
// So we cannot measure "how many distinct people pulled today". What we CAN do
// is snapshot the cumulative counter once per day and plot the day-over-day
// DELTA (cumulative[today] - cumulative[yesterday]) = pulls that landed that
// day. That is what the sparkline shows, and it is labelled honestly as
// "pulls/day" — never "unique downloads".
//
// The counter is scraped from the PUBLIC package HTML page, which needs no
// authentication (the package is public). We deliberately do not depend on the
// GitHub App installation token here: the REST Packages API cannot serve this
// number even with the token, and the public page can, so scraping the public
// page is both the only option and the more robust one (works even if the app
// key is missing and the hive is in dashboard-only mode).

const (
	// pullReleaseWindow is how many recent v2 RELEASES (distinct SHAs) the series
	// retains. Each release records the cumulative pull count at the moment it was
	// first seen; the pulls attributed to a release are the cumulative delta from
	// the PREVIOUS release to the next, i.e. the pulls that landed while that
	// release was newest. Older entries are pruned on every write.
	//
	// One extra is kept internally (window+1 snapshots) because N release BARS
	// need N+1 cumulative readings to bound N windows.
	pullReleaseWindow = 10

	// pullPageTimeout bounds the single HTTP GET of the public package page.
	pullPageTimeout = 10 * time.Second
)

// pullPackagePageURL is the PUBLIC GHCR package page carrying the cumulative
// "Total downloads" counter for the spoke image (ghcrRepoSpoke). A var so tests
// can point it at a local test server.
var pullPackagePageURL = "https://github.com/" + ghcrRepoSpoke + "/pkgs/container/hive"

// imagePullsPath is the PVC-backed JSON file holding the rolling snapshot
// series, alongside the other /data/saas state files (latest-shas.json, etc.).
var imagePullsPath = "/data/saas/image-pulls.json"

// totalDownloadsRe extracts the exact cumulative count from the package page.
// The page renders `<h3 title="127576">128K</h3>` immediately after the
// "Total downloads" label; the title attribute carries the exact integer while
// the visible text is abbreviated ("128K"), so we read the title. The label and
// the <h3> are separated only by whitespace/markup, matched non-greedily.
var totalDownloadsRe = regexp.MustCompile(`(?s)Total downloads.*?<h3[^>]*\btitle="(\d+)"`)

// pullSnapshot is one RELEASE data point: the cumulative total-downloads counter
// as read when the v2 latest SHA first became SHA. Deltas between consecutive
// releases give the pulls that landed while a release was newest.
type pullSnapshot struct {
	SHA        string `json:"sha"`        // short v2 release SHA this snapshot bounds
	Date       string `json:"date"`       // YYYY-MM-DD (UTC) the release was first seen
	Cumulative int64  `json:"cumulative"` // cumulative total downloads at that time
}

var (
	imagePullsMu    sync.Mutex
	imagePullSeries []pullSnapshot
	// imagePullsLoaded gates the one-time restore of the series from disk so the
	// SHA poller can call maybeSnapshotImagePulls every tick without re-reading
	// the file each time.
	imagePullsLoaded bool
)

// loadPersistedImagePulls restores the rolling series from the PVC so a freshly
// restarted hub keeps its history (the hub restarts on every auto-upgrade). Safe
// to call repeatedly; it only reads disk the first time.
func loadPersistedImagePulls(logger *slog.Logger) {
	imagePullsMu.Lock()
	defer imagePullsMu.Unlock()
	if imagePullsLoaded {
		return
	}
	imagePullsLoaded = true
	data, err := os.ReadFile(imagePullsPath)
	if err != nil {
		return // first run or no PVC — start empty
	}
	var persisted []pullSnapshot
	if err := json.Unmarshal(data, &persisted); err != nil {
		logger.Warn("image pulls: persisted series unreadable, ignoring", "path", imagePullsPath, "error", err)
		return
	}
	imagePullSeries = pruneOldSnapshots(persisted)
}

// persistImagePullsLocked writes the current series to disk (atomic tmp+rename,
// same pattern as persistLatestSHAs). Caller must hold imagePullsMu.
func persistImagePullsLocked(logger *slog.Logger) {
	if len(imagePullSeries) == 0 {
		return // never overwrite a good file with an empty series
	}
	data, err := json.MarshalIndent(imagePullSeries, "", "  ")
	if err != nil {
		logger.Warn("image pulls: persist marshal failed", "error", err)
		return
	}
	os.MkdirAll(filepath.Dir(imagePullsPath), 0o755)
	tmpPath := imagePullsPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		logger.Warn("image pulls: persist write failed", "path", imagePullsPath, "error", err)
		return
	}
	if err := os.Rename(tmpPath, imagePullsPath); err != nil {
		logger.Warn("image pulls: persist rename failed", "path", imagePullsPath, "error", err)
	}
}

// pruneOldSnapshots keeps the newest (pullReleaseWindow + 1) RELEASE snapshots in
// insertion order (chronological — the poller only ever appends the current
// release), collapsing consecutive duplicates of the same SHA (last-write-wins)
// so a re-observed release never double-counts. window+1 snapshots bound window
// release BARS. Legacy day-keyed snapshots (no SHA, from before this change) are
// dropped so the series cleanly re-seeds on the release model.
func pruneOldSnapshots(in []pullSnapshot) []pullSnapshot {
	if len(in) == 0 {
		return nil
	}
	out := make([]pullSnapshot, 0, len(in))
	for _, s := range in {
		if s.SHA == "" {
			continue // drop legacy day-keyed entries
		}
		if n := len(out); n > 0 && out[n-1].SHA == s.SHA {
			out[n-1] = s // same release re-observed → last-write-wins
			continue
		}
		out = append(out, s)
	}
	if keep := pullReleaseWindow + 1; len(out) > keep {
		out = out[len(out)-keep:]
	}
	return out
}


// fetchCumulativePulls reads the exact cumulative "Total downloads" count from
// the public package page. A var so tests can stub the network round-trip.
var fetchCumulativePulls = func(ctx context.Context, logger *slog.Logger) (int64, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pullPackagePageURL, nil)
	if err != nil {
		logger.Warn("image pulls: build request failed", "error", err)
		return 0, false
	}
	// A UA is polite and avoids some bot-shaping; the page is public either way.
	req.Header.Set("User-Agent", "kubestellar-hive-hub")
	req.Header.Set("Accept", "text/html")
	client := &http.Client{Timeout: pullPageTimeout}
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("image pulls: fetch package page failed", "url", pullPackagePageURL, "error", err)
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("image pulls: package page non-200", "url", pullPackagePageURL, "status", resp.StatusCode)
		return 0, false
	}
	// The counter lives high in the page; cap the read so a huge/hostile body
	// can't blow up memory. 512 KiB comfortably covers the package page head.
	const maxPackagePageBytes = 512 * 1024
	body := make([]byte, 0, maxPackagePageBytes)
	buf := make([]byte, 32*1024)
	for len(body) < maxPackagePageBytes {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	m := totalDownloadsRe.FindSubmatch(body)
	if m == nil {
		logger.Warn("image pulls: 'Total downloads' counter not found on package page (page layout may have changed)")
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(m[1])), 10, 64)
	if err != nil {
		logger.Warn("image pulls: could not parse counter", "raw", string(m[1]), "error", err)
		return 0, false
	}
	return n, true
}

// maybeSnapshotImagePulls records a new snapshot when the current v2 RELEASE SHA
// differs from the newest snapshot's SHA — i.e. at each new release boundary. It
// captures the cumulative pull count at that moment, so the pulls attributed to
// the PREVIOUS release are (this snapshot's cumulative − that release's
// cumulative). Called from the SHA poller each tick; a no-op on ticks where the
// v2 SHA hasn't advanced. It restores from disk on first call. now is injected so
// tests control the clock.
func (s *HubServer) maybeSnapshotImagePulls(ctx context.Context, now time.Time) {
	loadPersistedImagePulls(s.logger)

	sha := getLatestSHAForBranch("v2")
	if sha == "" {
		return // v2 SHA not resolved yet (early boot) — nothing to key on
	}
	sha = shortSHA(sha)
	today := now.UTC().Format("2006-01-02")

	imagePullsMu.Lock()
	due := true
	if n := len(imagePullSeries); n > 0 && imagePullSeries[n-1].SHA == sha {
		due = false // already recorded this release
	}
	imagePullsMu.Unlock()
	if !due {
		return
	}

	cumulative, ok := fetchCumulativePulls(ctx, s.logger)
	if !ok {
		return // transient fetch failure — try again next tick
	}

	imagePullsMu.Lock()
	defer imagePullsMu.Unlock()
	// Re-check under lock in case a concurrent tick already recorded this release.
	if n := len(imagePullSeries); n > 0 && imagePullSeries[n-1].SHA == sha {
		return
	}
	imagePullSeries = pruneOldSnapshots(append(imagePullSeries, pullSnapshot{
		SHA:        sha,
		Date:       today,
		Cumulative: cumulative,
	}))
	persistImagePullsLocked(s.logger)
	s.logger.Info("image pulls: recorded release snapshot", "sha", sha, "date", today, "cumulative", cumulative)
}

// imagePullPoint is one RELEASE's pulls in the series returned to the frontend:
// the pulls that landed while SHA was the newest v2 release (cumulative delta from
// the release BEFORE it to the release after it).
type imagePullPoint struct {
	SHA   string `json:"sha"`   // short v2 release SHA
	Date  string `json:"date"`  // YYYY-MM-DD the release was first seen
	Pulls int64  `json:"pulls"` // pulls during this release's window
}

// imagePullsResponse is the /api/hub/image-pulls payload.
type imagePullsResponse struct {
	// Points is the per-release series (oldest→newest), up to pullReleaseWindow
	// entries. N bars need N+1 cumulative snapshots, so with fewer than two
	// snapshots it is empty and the frontend shows "collecting…".
	Points []imagePullPoint `json:"points"`
	// TotalWindow is the sum of pulls across the releases in the window.
	TotalWindow int64 `json:"total_window"`
	// Latest is the most recent release's pulls, or 0 when not enough data yet.
	Latest int64 `json:"latest"`
	// Releases is how many release bars are in Points.
	Releases int `json:"releases"`
	// Collecting is true until there are at least two release snapshots — i.e.
	// until one release window can be closed. The frontend renders "collecting…"
	// instead of a broken zero-bar chart.
	Collecting bool `json:"collecting"`
}

// buildImagePullsResponse converts the cumulative release-snapshot series into
// per-release pull windows. Split out from the handler so it is unit-testable
// without HTTP. Each bar i is the cumulative delta between release i and i+1, so
// the NEWEST snapshot (the current release, still accruing) does not yet get its
// own closed bar — its window closes when the next release lands.
func buildImagePullsResponse(series []pullSnapshot) imagePullsResponse {
	resp := imagePullsResponse{Points: []imagePullPoint{}}
	if len(series) < 2 {
		resp.Collecting = true
		return resp
	}
	for i := 1; i < len(series); i++ {
		delta := series[i].Cumulative - series[i-1].Cumulative
		// A negative delta means the counter reset or a snapshot was bad; clamp
		// to zero so one glitch can't paint a downward spike or skew the total.
		if delta < 0 {
			delta = 0
		}
		// The bar belongs to the EARLIER release (series[i-1]) — those are the
		// pulls that landed while it was the newest release, up to the next one.
		resp.Points = append(resp.Points, imagePullPoint{
			SHA:   series[i-1].SHA,
			Date:  series[i-1].Date,
			Pulls: delta,
		})
		resp.TotalWindow += delta
	}
	if n := len(resp.Points); n > 0 {
		resp.Latest = resp.Points[n-1].Pulls
	}
	resp.Releases = len(resp.Points)
	return resp
}

// handleImagePulls serves the daily-delta series for the sparkline.
func (s *HubServer) handleImagePulls(w http.ResponseWriter, r *http.Request) {
	loadPersistedImagePulls(s.logger)
	imagePullsMu.Lock()
	seriesCopy := make([]pullSnapshot, len(imagePullSeries))
	copy(seriesCopy, imagePullSeries)
	imagePullsMu.Unlock()

	resp := buildImagePullsResponse(seriesCopy)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
