package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	// pullReleaseWindow is how many recent RELEASES (distinct SHAs) each
	// release line's series retains. Each release records the cumulative pull
	// count at the moment it was first seen; the pulls attributed to a release
	// are the cumulative delta from the PREVIOUS release to the next, i.e. the
	// pulls that landed while that release was newest. Older entries are pruned
	// on every write.
	//
	// One extra is kept internally (window+1 snapshots) because N release BARS
	// need N+1 cumulative readings to bound N windows.
	pullReleaseWindow = 10

	// pullPageTimeout bounds the single HTTP GET of the public package page.
	pullPageTimeout = 10 * time.Second

	// legacyPullLine is the release line the pre-per-line persisted series is
	// attributed to on load. The original implementation hard-coded the v2
	// branch as its release-boundary key (the bug this file no longer has), so
	// a legacy flat-array file IS v2 history — migrating it anywhere else would
	// fabricate data for a line that never had it.
	legacyPullLine = "v2"
)

// releaseLinePattern matches release-line branch names (v2, v4, v5, ...), the
// same `v<digits>` shape .github/release-lines.yml's release_lines list uses.
// Standing experimental branches (dd, mk, feat-x) are still snapshotted and
// charted per line, but only v* lines are candidates for the ACTIVE line.
var releaseLinePattern = regexp.MustCompile(`^v(\d+)$`)

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
// as read when a release line's latest SHA first became that line's SHA. Deltas
// between consecutive releases give the pulls that landed while a release was
// newest on its line.
type pullSnapshot struct {
	SHA        string `json:"sha"`        // short release SHA this snapshot bounds
	Date       string `json:"date"`       // YYYY-MM-DD (UTC) the release was first seen
	Cumulative int64  `json:"cumulative"` // cumulative total downloads at that time
}

// persistedImagePulls is the on-disk shape: one snapshot series per release
// line ("v2", "v4", "dd", ...). The pre-per-line format was a bare
// []pullSnapshot tracking only v2; loadPersistedImagePulls migrates it.
type persistedImagePulls struct {
	Lines map[string][]pullSnapshot `json:"lines"`
}

var (
	imagePullsMu sync.Mutex
	// imagePullSeriesByLine holds each release line's rolling snapshot series.
	imagePullSeriesByLine = map[string][]pullSnapshot{}
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
	var persisted persistedImagePulls
	if err := json.Unmarshal(data, &persisted); err == nil && persisted.Lines != nil {
		for line, series := range persisted.Lines {
			if pruned := pruneOldSnapshots(series); len(pruned) > 0 {
				imagePullSeriesByLine[line] = pruned
			}
		}
		return
	}
	// Legacy format: a bare snapshot array from when only the v2 branch was
	// tracked. Keep that history visible under the line it actually measured.
	var legacy []pullSnapshot
	if err := json.Unmarshal(data, &legacy); err != nil {
		logger.Warn("image pulls: persisted series unreadable, ignoring", "path", imagePullsPath, "error", err)
		return
	}
	if pruned := pruneOldSnapshots(legacy); len(pruned) > 0 {
		imagePullSeriesByLine[legacyPullLine] = pruned
	}
}

// persistImagePullsLocked writes the current series to disk (atomic tmp+rename,
// same pattern as persistLatestSHAs). Caller must hold imagePullsMu.
func persistImagePullsLocked(logger *slog.Logger) {
	if len(imagePullSeriesByLine) == 0 {
		return // never overwrite a good file with an empty series
	}
	data, err := json.MarshalIndent(persistedImagePulls{Lines: imagePullSeriesByLine}, "", "  ")
	if err != nil {
		logger.Warn("image pulls: persist marshal failed", "error", err)
		return
	}
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(filepath.Dir(imagePullsPath), 0o755)
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
	defer func() { _ = resp.Body.Close() }()
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

// maybeSnapshotImagePulls records a new snapshot for EVERY tracked release
// line whose current latest SHA differs from that line's newest snapshot —
// i.e. at each new release boundary, per line. It captures the cumulative pull
// count at that moment, so the pulls attributed to a line's PREVIOUS release
// are (this snapshot's cumulative − that release's cumulative). Called from
// the SHA poller each tick; a no-op on ticks where no line's SHA advanced. A
// retired line (v2) whose SHA never advances simply stops accruing snapshots
// but keeps its history. It restores from disk on first call. now is injected
// so tests control the clock.
//
// The cumulative counter is fetched ONCE per tick and shared by every line
// that is due: GitHub exposes a single package-wide counter (see the header
// comment), so per-line bars measure the package pulls that landed during
// each of that line's release windows — labelled as such, never as per-tag
// pull counts, which GitHub does not publish.
func (s *HubServer) maybeSnapshotImagePulls(ctx context.Context, now time.Time) {
	loadPersistedImagePulls(s.logger)

	shas := getLatestSHAs()
	if len(shas) == 0 {
		return // no line's SHA resolved yet (early boot) — nothing to key on
	}
	// Deterministic order so log lines and persisted files are stable.
	lines := make([]string, 0, len(shas))
	for line := range shas {
		lines = append(lines, line)
	}
	sort.Strings(lines)
	today := now.UTC().Format("2006-01-02")

	imagePullsMu.Lock()
	due := make([]string, 0, len(lines))
	for _, line := range lines {
		sha := shortSHA(shas[line])
		if sha == "" {
			continue
		}
		if series := imagePullSeriesByLine[line]; len(series) > 0 && series[len(series)-1].SHA == sha {
			continue // already recorded this release on this line
		}
		due = append(due, line)
	}
	imagePullsMu.Unlock()
	if len(due) == 0 {
		return
	}

	cumulative, ok := fetchCumulativePulls(ctx, s.logger)
	if !ok {
		return // transient fetch failure — try again next tick
	}

	imagePullsMu.Lock()
	defer imagePullsMu.Unlock()
	recorded := false
	for _, line := range due {
		sha := shortSHA(shas[line])
		// Re-check under lock in case a concurrent tick already recorded it.
		if series := imagePullSeriesByLine[line]; len(series) > 0 && series[len(series)-1].SHA == sha {
			continue
		}
		imagePullSeriesByLine[line] = pruneOldSnapshots(append(imagePullSeriesByLine[line], pullSnapshot{
			SHA:        sha,
			Date:       today,
			Cumulative: cumulative,
		}))
		recorded = true
		s.logger.Info("image pulls: recorded release snapshot", "line", line, "sha", sha, "date", today, "cumulative", cumulative)
	}
	if recorded {
		persistImagePullsLocked(s.logger)
	}
}

// activeReleaseLine names the release line whose per-release pulls the
// headline widget reports. Nothing here hardcodes a branch (hardcoding "v2"
// was exactly how the widget kept reporting the retired line, and hardcoding
// "v4" would re-create the bug at the v5 rollover):
//
//  1. The branch the "stable" release channel currently resolves to. That is
//     the runtime source of truth aligned with src/deploy/standalone-images.sh,
//     which pins standalone deployments to ghcr.io/kubestellar/hive:stable —
//     when CI promotes v5 to stable, the widget follows with no code change.
//  2. Fallback (stable unresolved — GHCR blip, or the channel pinned off any
//     tracked branch): the highest-numbered v<N> line that has a resolved SHA.
//  3. "" when nothing resolves; the caller renders "collecting", never an error.
func activeReleaseLine(channels []ChannelTarget, branchSHAs map[string]string) string {
	for _, ch := range channels {
		if ch.Channel == ReleaseChannelStable && ch.Branch != "" {
			return ch.Branch
		}
	}
	best, bestN := "", -1
	for line, sha := range branchSHAs {
		if sha == "" {
			continue
		}
		m := releaseLinePattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil && n > bestN {
			best, bestN = line, n
		}
	}
	return best
}

// imagePullPoint is one RELEASE's pulls in the series returned to the frontend:
// the pulls that landed while SHA was the newest release on its line (cumulative
// delta from the release BEFORE it to the release after it).
type imagePullPoint struct {
	SHA   string `json:"sha"`   // short release SHA
	Date  string `json:"date"`  // YYYY-MM-DD the release was first seen
	Pulls int64  `json:"pulls"` // pulls during this release's window
}

// imagePullLineStats is one release line's per-release pull series, rendered
// as the mini bar chart next to that line's "Latest available images" row.
type imagePullLineStats struct {
	Points      []imagePullPoint `json:"points"`
	TotalWindow int64            `json:"total_window"`
	Latest      int64            `json:"latest"`
	Releases    int              `json:"releases"`
	Collecting  bool             `json:"collecting"`
}

// imagePullsResponse is the /api/hub/image-pulls payload. The top-level fields
// describe the ACTIVE release line (see activeReleaseLine); Lines carries every
// tracked line's series for the per-row charts. Line and Lines are additive —
// pre-existing consumers of the flat fields keep working.
type imagePullsResponse struct {
	// Points is the active line's per-release series (oldest→newest), up to
	// pullReleaseWindow entries. N bars need N+1 cumulative snapshots, so with
	// fewer than two snapshots it is empty and the frontend shows "collecting…".
	Points []imagePullPoint `json:"points"`
	// TotalWindow is the sum of pulls across the releases in the window.
	TotalWindow int64 `json:"total_window"`
	// Latest is the most recent release's pulls, or 0 when not enough data yet.
	Latest int64 `json:"latest"`
	// Releases is how many release bars are in Points.
	Releases int `json:"releases"`
	// Collecting is true until the active line has at least two release
	// snapshots — i.e. until one release window can be closed. The frontend
	// renders "collecting…" instead of a broken zero-bar chart.
	Collecting bool `json:"collecting"`
	// Line is the active release line the flat fields above describe, or ""
	// when no line could be resolved yet.
	Line string `json:"line,omitempty"`
	// Lines maps every release line with recorded snapshots to its own series.
	// A line with no data (e.g. a freshly cut v5) is simply absent — the UI
	// renders "—" for it, never an error.
	Lines map[string]imagePullLineStats `json:"lines,omitempty"`
}

// buildImagePullsResponse converts one line's cumulative release-snapshot
// series into per-release pull windows. Split out from the handler so it is
// unit-testable without HTTP. Each bar i is the cumulative delta between
// release i and i+1, so the NEWEST snapshot (the current release, still
// accruing) does not yet get its own closed bar — its window closes when the
// next release lands.
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

// buildImagePullsMulti assembles the full payload: the active line's series in
// the flat fields plus every line's series under Lines. An active line with no
// recorded data (possible right after a rollover) degrades to Collecting.
func buildImagePullsMulti(byLine map[string][]pullSnapshot, active string) imagePullsResponse {
	resp := buildImagePullsResponse(byLine[active])
	resp.Line = active
	if len(byLine) > 0 {
		resp.Lines = make(map[string]imagePullLineStats, len(byLine))
		for line, series := range byLine {
			r := buildImagePullsResponse(series)
			resp.Lines[line] = imagePullLineStats{
				Points:      r.Points,
				TotalWindow: r.TotalWindow,
				Latest:      r.Latest,
				Releases:    r.Releases,
				Collecting:  r.Collecting,
			}
		}
	}
	return resp
}

// handleImagePulls serves the per-release pull series for the header widget
// and the per-row line charts.
func (s *HubServer) handleImagePulls(w http.ResponseWriter, r *http.Request) {
	loadPersistedImagePulls(s.logger)
	imagePullsMu.Lock()
	byLine := make(map[string][]pullSnapshot, len(imagePullSeriesByLine))
	for line, series := range imagePullSeriesByLine {
		byLine[line] = append([]pullSnapshot(nil), series...)
	}
	imagePullsMu.Unlock()

	// Resolve the active line from the stable channel's current branch (cached,
	// channelDigestTTL) — the same digest-derived association the dashboard's
	// channel rows use, so the widget and the rows can never disagree.
	branchSHAs := getDisplaySHAs()
	active := activeReleaseLine(getChannelTargets(branchSHAs, s.logger), branchSHAs)

	resp := buildImagePullsMulti(byLine, active)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
