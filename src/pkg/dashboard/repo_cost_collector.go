package dashboard

// RepoCostCollector caches the /api/repo-cost interval join on a ticker,
// exactly the way ActivityCollector (activity_collector.go) already caches
// /api/repo-activity's read of the same audit files. Before this collector
// existed, handleRepoCost ran the full join — including OutputActionsSince,
// which globs the current audit file plus every rotated backup, decompressing
// .gz ones up to 64MB each — on every request, and the dashboard polls that
// endpoint every 60s per open tab (#4943).

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

// repoCostCollectInterval mirrors activityCollectInterval on purpose: this
// collector reads the SAME audit window as the activity collector (see
// repoCostWindow = activityWindow) to feed the interval join, so there is no
// reason for the two caches to go stale at different rates — a dashboard
// comparing repo-activity counts against repo-cost dollars would otherwise be
// looking at two different moments in time. If a reason to diverge ever shows
// up (e.g. cost being materially more expensive to compute than activity),
// change this constant alone; the join itself does not assume the value.
var repoCostCollectInterval = activityCollectInterval

// tokensSummaryReader is the subset of *tokens.Collector the collector needs,
// so it can be faked in tests without a full token collector.
type tokensSummaryReader interface {
	Summary() *tokens.AggregateSummary
}

// RepoCostCollector recomputes the per-repo cost interval join on a ticker and
// serves the last computed snapshot, instead of recomputing per HTTP request.
// Mirrors ActivityCollector's lifecycle (EnablePersistence/Start/Snapshot).
type RepoCostCollector struct {
	mu     sync.Mutex
	audit  auditReader
	tokens tokensSummaryReader
	// auditPath is where OutputActionsSince reads ("" -> the production audit
	// log). Kept in sync with the activity collector's path so both caches
	// read the identical file set — see auditPathForActivity.
	auditPath   string
	logger      *slog.Logger
	nowFn       func() time.Time
	persistPath string

	snap        repoCostResponse
	ready       bool
	collectedAt time.Time
}

// persistedRepoCost is the on-disk sidecar shape, mirroring persistedActivity.
type persistedRepoCost struct {
	Snapshot    repoCostResponse `json:"snapshot"`
	CollectedAt time.Time        `json:"collected_at"`
}

// NewRepoCostCollector builds a collector that joins audit output events
// against the token collector's usage timeline. audit or tokens nil makes it
// inert (Snapshot ready=false forever). auditPath is where OutputActionsSince
// reads ("" -> the production audit log; pass the same path the activity
// collector uses so both read one file set).
func NewRepoCostCollector(audit auditReader, tok tokensSummaryReader, auditPath string, logger *slog.Logger) *RepoCostCollector {
	return &RepoCostCollector{
		audit:     audit,
		tokens:    tok,
		auditPath: auditPath,
		logger:    logger,
		nowFn:     time.Now,
	}
}

// EnablePersistence mirrors each collect to path and restores whatever is
// there now, so the first poll after a restart reports the last computed
// snapshot (with its true CollectedAt, so staleness is still visible) rather
// than losing it and going back through "not ready". Missing file = clean
// first boot; corrupt = logged + ignored. Call once before Start.
func (rc *RepoCostCollector) EnablePersistence(path string) {
	if rc == nil {
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.persistPath = path
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) && rc.logger != nil {
			rc.logger.Warn("repo-cost store unreadable — starting fresh", "path", path, "error", err)
		}
		return
	}
	var stored persistedRepoCost
	if err := json.Unmarshal(data, &stored); err != nil {
		if rc.logger != nil {
			rc.logger.Warn("repo-cost store corrupt — starting fresh", "path", path, "error", err)
		}
		return
	}
	if stored.CollectedAt.IsZero() {
		return
	}
	rc.snap = stored.Snapshot
	rc.collectedAt = stored.CollectedAt
	rc.ready = true
}

func (rc *RepoCostCollector) persistLocked() {
	if rc.persistPath == "" {
		return
	}
	data, err := json.Marshal(persistedRepoCost{Snapshot: rc.snap, CollectedAt: rc.collectedAt})
	if err != nil {
		return
	}
	tmp := rc.persistPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		if rc.logger != nil {
			rc.logger.Warn("failed to write repo-cost store", "path", rc.persistPath, "error", err)
		}
		return
	}
	if err := os.Rename(tmp, rc.persistPath); err != nil && rc.logger != nil {
		rc.logger.Warn("failed to replace repo-cost store", "path", rc.persistPath, "error", err)
	}
}

// Start runs the collect loop until ctx is cancelled — once up front, then
// every repoCostCollectInterval. Inert (returns) when there is no audit
// reader or no token reader: the join needs both sides.
func (rc *RepoCostCollector) Start(ctx context.Context) {
	if rc == nil || rc.audit == nil || rc.tokens == nil {
		return
	}
	rc.collect()
	ticker := time.NewTicker(repoCostCollectInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rc.collect()
		}
	}
}

// collect runs the interval join over the current window and caches the
// result. Identical inputs to what handleRepoCost used to compute inline —
// same window, same audit path, same summary source — just on a ticker.
func (rc *RepoCostCollector) collect() {
	now := rc.nowFn()
	summary := rc.tokens.Summary()
	entries := rc.audit.OutputActionsSince(now.Add(-repoCostWindow), activityOutputActions, rc.auditPath)
	resp := computeRepoCost(summary, entries, now)

	rc.mu.Lock()
	rc.snap = resp
	rc.collectedAt = now
	rc.ready = resp.Ready
	rc.persistLocked()
	rc.mu.Unlock()
}

// Snapshot returns the last computed response and whether a collect has ever
// succeeded (i.e. produced a non-nil token summary; see computeRepoCost).
// Ready=false must never be papered over with a zero-valued response by the
// caller — a cost endpoint reporting $0.00 before its first collection is
// indistinguishable from a hive that genuinely spent nothing, which is
// exactly the misreporting #4836 exists to prevent.
func (rc *RepoCostCollector) Snapshot() (repoCostResponse, bool) {
	if rc == nil {
		return repoCostResponse{}, false
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.snap, rc.ready
}

// CollectedAt returns when the cached snapshot was last computed (zero if
// never).
func (rc *RepoCostCollector) CollectedAt() time.Time {
	if rc == nil {
		return time.Time{}
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.collectedAt
}
