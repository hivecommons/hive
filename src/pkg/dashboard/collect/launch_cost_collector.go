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
}

type persistedLaunchCost struct {
	Snapshot    LaunchCostResponse `json:"snapshot"`
	CollectedAt time.Time          `json:"collected_at"`
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
	if stored.CollectedAt.IsZero() {
		return
	}
	lc.snap = stored.Snapshot
	lc.collectedAt = stored.CollectedAt
	lc.ready = true
}

func (lc *LaunchCostCollector) persistLocked() {
	if lc.persistPath == "" {
		return
	}
	data, err := json.Marshal(persistedLaunchCost{Snapshot: lc.snap, CollectedAt: lc.collectedAt})
	if err != nil {
		return
	}
	tmp := lc.persistPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		if lc.logger != nil {
			lc.logger.Warn("failed to write launch-cost store", "path", lc.persistPath, "error", err)
		}
		return
	}
	if err := os.Rename(tmp, lc.persistPath); err != nil && lc.logger != nil {
		lc.logger.Warn("failed to replace launch-cost store", "path", lc.persistPath, "error", err)
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
	resp := ComputeLaunchCost(lc.tokens.Summary(), lc.tokens.Launches(), now)
	lc.mu.Lock()
	lc.snap = resp
	lc.collectedAt = now
	lc.ready = resp.Ready
	lc.persistLocked()
	lc.mu.Unlock()
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
