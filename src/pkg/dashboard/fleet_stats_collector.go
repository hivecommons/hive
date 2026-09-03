package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// fleetStatsCollectInterval is how often the spoke recomputes its fleet-stat
// contribution counts. Each collect is three GitHub search requests, so this
// is deliberately much slower than the heartbeat cadence — the heartbeat reads
// the cached snapshot, never triggers a fresh GitHub call. Cheap and steady.
// A var (not a const) so tests can drive the ticker branch quickly; production
// keeps the default.
var fleetStatsCollectInterval = 30 * time.Minute

// The GitHub search API allows only 30 requests per minute per token, and that
// budget is shared by every hive authenticating as the same App installation.
// A hub-wide rolling restart brings ~50 spokes up within seconds of each other,
// each firing three searches at once — far over the quota — so the first
// collect fails on most spokes. Before this jitter+retry existed those spokes
// simply gave up until the next 30-minute tick, and because a restart also
// clears the previously reported counts (they live only in memory), the public
// fleet total collapsed to whatever the handful of survivors reported. That is
// the "statistics are missing again" regression.
//
// fleetStatsStartupJitterMax spreads the initial collect across a window wider
// than the burst, so spokes queue up against the quota instead of colliding.
var fleetStatsStartupJitterMax = 5 * time.Minute

// fleetStatsRetryAttempts is the total number of tries for a single collect
// (one initial attempt plus retries) before giving up until the next tick.
const fleetStatsRetryAttempts = 4

// fleetStatsRetryBaseDelay is the first retry delay; each subsequent retry
// doubles it (1m, 2m, 4m). The search quota refills every minute, so a
// minute-scale backoff is the shortest delay that can actually clear a
// rate-limit rejection rather than burning an attempt on a still-empty budget.
var fleetStatsRetryBaseDelay = time.Minute

// FleetStatsCollector periodically computes this hive's AI-author contribution
// counts (PRs merged, PRs rejected, CVE-referencing PRs) across its org and
// caches them, so the heartbeat sender can attach a fresh-but-cheap snapshot to
// every beat. The hub aggregates these across all spokes into the public
// landing page's live fleet-stats strip.
type FleetStatsCollector struct {
	ghClient *ghpkg.Client
	author   string
	org      string
	logger   *slog.Logger

	mu     sync.RWMutex
	counts ghpkg.FleetContribCounts
	// ready is false until the first successful collect, so the heartbeat can
	// omit zero counts that merely mean "not computed yet" rather than "truly
	// zero" — the hub then hides the stat instead of showing a fake number.
	ready bool
	// collectedAt is when counts were last successfully computed. The heartbeat
	// reports it so the hub can tell a fresh count from one left over from a
	// collect that has since started failing, and surface the difference rather
	// than presenting stale numbers as current.
	collectedAt time.Time

	// persistPath is where the last successful collect is mirrored on the spoke
	// PVC so a restart resumes from the last-known counts instead of nil. Empty
	// disables persistence (the collector is purely in-memory). See #2329.
	persistPath string
}

// persistedFleetStats is the on-disk shape written to persistPath. It carries
// the raw counts plus the REAL collect time — never re-stamped to now — so the
// hub's fleetStatsMaxAge aging still applies to counts restored after a
// restart. A spoke that has been down long enough for its last collect to age
// out is aged out on the hub exactly as if it had never re-collected.
type persistedFleetStats struct {
	Counts      ghpkg.FleetContribCounts `json:"counts"`
	CollectedAt time.Time                `json:"collected_at"`
}

// NewFleetStatsCollector builds a collector for the given AI author and org.
// A nil ghClient, or empty author/org, yields a collector that never reports
// counts (Snapshot returns ready=false) — the hive simply contributes nothing
// to the fleet total rather than erroring.
func NewFleetStatsCollector(ghClient *ghpkg.Client, author, org string, logger *slog.Logger) *FleetStatsCollector {
	return &FleetStatsCollector{
		ghClient: ghClient,
		author:   author,
		org:      org,
		logger:   logger,
	}
}

// EnablePersistence makes the collected fleet-stat counts survive a process
// restart by mirroring each successful collect to path on the spoke PVC, and by
// loading whatever is already there NOW so the first heartbeat after a restart
// reports the last-known counts (with their real collectedAt) instead of nil.
// Without this the counts live only in memory: a mass restart clears them and
// the public fleet total collapses until every spoke re-collects (#2329). The
// hub already keeps + ages the last value it saw (#2328); this stops the spoke
// from forgetting in the first place. Call once at startup before Start.
//
// A missing file is a clean no-op (first boot). A corrupt file is logged and
// ignored — a restart that resumes from nothing is no worse than today.
func (fc *FleetStatsCollector) EnablePersistence(path string) {
	if fc == nil {
		return
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.persistPath = path

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) && fc.logger != nil {
			fc.logger.Warn("fleet stats store unreadable — starting with no restored counts",
				"path", path, "error", err)
		}
		return
	}
	var stored persistedFleetStats
	if err := json.Unmarshal(data, &stored); err != nil {
		if fc.logger != nil {
			fc.logger.Warn("fleet stats store corrupt — starting with no restored counts",
				"path", path, "error", err)
		}
		return
	}
	// A zero collectedAt means the file never held a real collect; treat it as
	// absent rather than reporting counts the hub would immediately age out
	// against a zero timestamp.
	if stored.CollectedAt.IsZero() {
		return
	}
	fc.counts = stored.Counts
	fc.collectedAt = stored.CollectedAt
	fc.ready = true
	if fc.logger != nil {
		fc.logger.Info("restored fleet stats from PVC",
			"prs_merged", stored.Counts.PRsMerged,
			"prs_rejected", stored.Counts.PRsRejected,
			"cves_closed", stored.Counts.CVEsClosed,
			"collected_at", stored.CollectedAt.Format(time.RFC3339),
			"path", path)
	}
}

// persistLocked mirrors the current counts + collectedAt to persistPath, if
// persistence is enabled. Callers must hold mu. The file is a single tiny
// record, so a full atomic rewrite (tmp + rename, mirroring the session store)
// per collect is cheaper than anything incremental. 0600: the counts are not
// secret, but this keeps every PVC-persisted dashboard file in one trust domain.
func (fc *FleetStatsCollector) persistLocked() {
	if fc.persistPath == "" {
		return
	}
	data, err := json.Marshal(persistedFleetStats{
		Counts:      fc.counts,
		CollectedAt: fc.collectedAt,
	})
	if err != nil {
		return
	}
	tmp := fc.persistPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		if fc.logger != nil {
			fc.logger.Warn("failed to write fleet stats store", "path", fc.persistPath, "error", err)
		}
		return
	}
	if err := os.Rename(tmp, fc.persistPath); err != nil && fc.logger != nil {
		fc.logger.Warn("failed to replace fleet stats store", "path", fc.persistPath, "error", err)
	}
}

// Start runs the collect loop until ctx is cancelled. It computes once up
// front, then on fleetStatsCollectInterval. The ticker takes ctx and is
// stopped on return, so there is no uncancellable timer leak.
func (fc *FleetStatsCollector) Start(ctx context.Context) {
	if fc == nil || fc.ghClient == nil || fc.author == "" || fc.org == "" {
		if fc != nil && fc.logger != nil {
			fc.logger.Warn("fleet stats collector not started; this hive will never "+
				"contribute to the public fleet total",
				"author", fc.author, "org", fc.org, "has_github_client", fc.ghClient != nil)
		}
		return
	}
	// Stagger the very first collect. Without this every spoke in a rolling
	// restart searches at the same instant and most are rate-limited away.
	if d := fc.startupDelay(); d > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
	fc.collect(ctx)
	ticker := time.NewTicker(fleetStatsCollectInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fc.collect(ctx)
		}
	}
}

// startupDelay returns a random delay in [0, fleetStatsStartupJitterMax) used
// to spread the fleet's first collect across the search-rate-limit window.
func (fc *FleetStatsCollector) startupDelay() time.Duration {
	if fleetStatsStartupJitterMax <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(fleetStatsStartupJitterMax)))
}

// collect computes the counts, retrying with exponential backoff. A single
// GitHub search rejection (rate limit, transient 5xx) must not cost this hive
// its place in the fleet total until the next 30-minute tick.
func (fc *FleetStatsCollector) collect(ctx context.Context) {
	delay := fleetStatsRetryBaseDelay
	var err error
	for attempt := 1; attempt <= fleetStatsRetryAttempts; attempt++ {
		var counts ghpkg.FleetContribCounts
		counts, err = fc.ghClient.ComputeFleetContribCounts(ctx, fc.author, fc.org)
		if err == nil {
			fc.mu.Lock()
			fc.counts = counts
			fc.ready = true
			fc.collectedAt = time.Now()
			// Mirror to the PVC while holding mu so a restart resumes from this
			// exact collect (with its real timestamp) instead of nil (#2329).
			fc.persistLocked()
			fc.mu.Unlock()
			if fc.logger != nil {
				fc.logger.Info("fleet stats collected",
					"prs_merged", counts.PRsMerged,
					"prs_rejected", counts.PRsRejected,
					"cves_closed", counts.CVEsClosed,
					"attempt", attempt,
				)
			}
			return
		}
		if attempt == fleetStatsRetryAttempts {
			break
		}
		if fc.logger != nil {
			fc.logger.Warn("fleet stats collect failed; retrying",
				"error", err, "attempt", attempt, "retry_in", delay.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay *= 2
	}
	if fc.logger != nil {
		// Warn, not Debug: a failure here means this hive silently drops out
		// of the public fleet-stats total, and the only symptom is a blank
		// strip on the landing page. That is not something to hide at a log
		// level nobody enables.
		fc.logger.Warn("fleet stats collect failed after all retries; this hive will not contribute to the fleet total",
			"error", err, "author", fc.author, "org", fc.org, "attempts", fleetStatsRetryAttempts)
	}
}

// Snapshot returns the last computed counts and whether a successful collect
// has ever completed. When ready is false the caller should omit the counts
// from the heartbeat so the hub does not treat "not computed yet" as zero.
func (fc *FleetStatsCollector) Snapshot() (ghpkg.FleetContribCounts, bool) {
	if fc == nil {
		return ghpkg.FleetContribCounts{}, false
	}
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return fc.counts, fc.ready
}

// CollectedAt returns when the cached counts were last successfully computed,
// or the zero time if no collect has ever succeeded. The heartbeat forwards it
// so the hub can age out stale contributions instead of summing them forever.
func (fc *FleetStatsCollector) CollectedAt() time.Time {
	if fc == nil {
		return time.Time{}
	}
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return fc.collectedAt
}
