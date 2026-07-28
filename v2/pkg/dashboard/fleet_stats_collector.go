package dashboard

import (
	"context"
	"log/slog"
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
		return
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

func (fc *FleetStatsCollector) collect(ctx context.Context) {
	counts, err := fc.ghClient.ComputeFleetContribCounts(ctx, fc.author, fc.org)
	if err != nil {
		if fc.logger != nil {
			// Warn, not Debug: a failure here means this hive silently drops out
			// of the public fleet-stats total, and the only symptom is a blank
			// strip on the landing page. That is not something to hide at a log
			// level nobody enables.
			fc.logger.Warn("fleet stats collect failed; this hive will not contribute to the fleet total",
				"error", err, "author", fc.author, "org", fc.org)
		}
		return
	}
	fc.mu.Lock()
	fc.counts = counts
	fc.ready = true
	fc.mu.Unlock()
	if fc.logger != nil {
		fc.logger.Info("fleet stats collected",
			"prs_merged", counts.PRsMerged,
			"prs_rejected", counts.PRsRejected,
			"cves_closed", counts.CVEsClosed,
		)
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
