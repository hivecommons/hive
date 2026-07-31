package dashboard

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	ghpkg "github.com/kubestellar/hive/v2/pkg/github"
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

// Start runs the collect loop until ctx is cancelled. It computes once up
// front, then on fleetStatsCollectInterval. The ticker takes ctx and is
// stopped on return, so there is no uncancellable timer leak.
func (fc *FleetStatsCollector) Start(ctx context.Context) {
	if fc == nil || fc.ghClient == nil || fc.author == "" || fc.org == "" {
		if fc != nil && fc.logger != nil {
			fc.logger.Warn("fleet stats collector not started; this hive will never "+
				"contribute to the public fleet total",
				"author", fc.author, "org", fc.org, "has_github_client", fc != nil && fc.ghClient != nil)
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
