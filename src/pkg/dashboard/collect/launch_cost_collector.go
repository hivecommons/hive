package collect

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

// launchReader is the subset of *tokens.Collector the launch-cost join needs.
type launchReader interface {
	Summary() *tokens.AggregateSummary
	Launches() []tokens.LaunchRecord
}

// LaunchCostCollector caches the /api/launch-cost join on the same ticker as
// the repo-cost collector and serves the last snapshot (#12128). It follows
// RepoCostCollector's lifecycle exactly: EnablePersistence/Start/Snapshot.
type LaunchCostCollector struct {
	mu          sync.Mutex
	tokens      launchReader
	logger      *slog.Logger
	nowFn       func() time.Time
	persistPath string

	snap        LaunchCostResponse
	ready       bool
	collectedAt time.Time
	// rollup holds settled per-day, per-ref totals of launches the join no
	// longer sees; rollupLive holds the launches it still sees, keyed by
	// launch, so each is folded into rollup exactly once when it ages out.
	rollup         map[string]map[string]LaunchCostRollupRef
	rollupLive     map[string]launchCostLiveLaunch
	rollupDayCache map[string][]LaunchCostRollupRef
	// settledDayCache holds the sorted settled-only list per day for the store,
	// invalidated alongside rollupDayCache.
	settledDayCache map[string]LaunchCostDailyRollup
}

type persistedLaunchCost struct {
	Snapshot    LaunchCostResponse `json:"snapshot"`
	CollectedAt time.Time          `json:"collected_at"`
	// DailyRollup is absent in files written before the rollup existed.
	DailyRollup []LaunchCostDailyRollup `json:"daily_rollup,omitempty"`
	// LiveLaunches are the launches still visible to the join at write time,
	// keyed by agent@launched_at; they are not yet part of DailyRollup.
	LiveLaunches map[string]launchCostLiveLaunch `json:"live_launches,omitempty"`
}

// NewLaunchCostCollector builds a collector over the token collector. A nil
// reader makes it inert (Snapshot ready=false forever).
func NewLaunchCostCollector(tok launchReader, logger *slog.Logger) *LaunchCostCollector {
	return &LaunchCostCollector{tokens: tok, logger: logger, nowFn: time.Now}
}

// EnablePersistence mirrors each collect to path and restores whatever is
// there now. Missing file = clean first boot; corrupt = logged + ignored.
func (lc *LaunchCostCollector) EnablePersistence(path string) {
	if lc == nil {
		return
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	lc.persistPath = path
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) && lc.logger != nil {
			lc.logger.Warn("launch-cost store unreadable — starting fresh", "path", path, "error", err)
		}
		return
	}
	var stored persistedLaunchCost
	if err := json.Unmarshal(data, &stored); err != nil {
		if lc.logger != nil {
			lc.logger.Warn("launch-cost store corrupt — starting fresh", "path", path, "error", err)
		}
		return
	}
	lc.seedRollupLocked(stored.DailyRollup, stored.LiveLaunches)
	if stored.CollectedAt.IsZero() {
		return
	}
	lc.snap = stored.Snapshot
	lc.collectedAt = stored.CollectedAt
	lc.ready = true
}

// persistSnapshotLocked captures what persistLocked will write. It runs under
// lc.mu; the marshal and file I/O in writePersist do not, so a tick holds the
// lock only for the map-to-list fold of the days it changed (settled days are
// cached per day).
func (lc *LaunchCostCollector) persistSnapshotLocked() (persistedLaunchCost, bool) {
	if lc.persistPath == "" {
		return persistedLaunchCost{}, false
	}
	live := make(map[string]launchCostLiveLaunch, len(lc.rollupLive))
	for k, v := range lc.rollupLive {
		live[k] = v
	}
	return persistedLaunchCost{
		Snapshot:     lc.snap,
		CollectedAt:  lc.collectedAt,
		DailyRollup:  lc.settledRollupListLocked(),
		LiveLaunches: live,
	}, true
}

func (lc *LaunchCostCollector) writePersist(path string, stored persistedLaunchCost) {
	data, err := json.Marshal(stored)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		if lc.logger != nil {
			lc.logger.Warn("failed to write launch-cost store", "path", path, "error", err)
		}
		return
	}
	if err := os.Rename(tmp, path); err != nil && lc.logger != nil {
		lc.logger.Warn("failed to replace launch-cost store", "path", path, "error", err)
	}
}

// Start runs the collect loop until ctx is cancelled.
func (lc *LaunchCostCollector) Start(ctx context.Context) {
	if lc == nil || lc.tokens == nil {
		return
	}
	lc.collect()
	ticker := time.NewTicker(repoCostCollectInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lc.collect()
		}
	}
}

func (lc *LaunchCostCollector) collect() {
	now := lc.nowFn()
	launches := lc.tokens.Launches()
	resp := ComputeLaunchCost(lc.tokens.Summary(), launches, now)
	lc.mu.Lock()
	lc.snap = resp
	lc.collectedAt = now
	lc.ready = resp.Ready
	lc.mergeLaunchCostRollupLocked(resp, launches, now)
	stored, ok := lc.persistSnapshotLocked()
	path := lc.persistPath
	lc.mu.Unlock()
	if ok {
		lc.writePersist(path, stored)
	}
}

// Snapshot returns the last computed response and whether a collect has ever
// produced a ready result.
func (lc *LaunchCostCollector) Snapshot() (LaunchCostResponse, bool) {
	if lc == nil {
		return LaunchCostResponse{}, false
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return lc.snap, lc.ready
}
