package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

// matrixIngestInterval is the cadence of the incremental sentinel pass that
// feeds the Matrix line source (#16276). The full sentinel sweep is bounded
// below by the governor tick, which on a loaded hive is tens of minutes of
// serial eval, kicks and sweeps, and a hive without inbound webhooks has no
// other path from a push to the kiosk. The pass costs nothing for unchanged
// heads (forgestate cache + sentinel heads map, no GitHub call), so it can
// run every couple of minutes on its own goroutine.
const matrixIngestInterval = 2 * time.Minute

// matrixLinesStatePath and sentinelHeadsStatePath are where the Matrix
// backlog and the sentinel's evaluated-head map persist on the PVC, each its
// own file like the other /data histories so a parse failure in one never
// takes the other down. Together they let a restarted hive refill the kiosk
// at once and diff only heads that moved while it was down.
const (
	matrixLinesStatePath   = "/data/matrix-lines.json"
	sentinelHeadsStatePath = "/data/sentinel-heads.json"
)

// matrixIngestState serializes the incremental passes: the ticker goroutine
// and the per-cycle call in runSweeps must not read the same new head twice.
type matrixIngestState struct {
	mu      sync.Mutex
	lastRun time.Time
}

// matrixIngestActive reports whether the incremental pass has anything to
// feed: dashboard.matrix active and the sentinel (whose evaluation and
// observer the pass reuses) enabled.
func matrixIngestActive(cfg *config.Config) bool {
	return cfg != nil && cfg.Sentinel.IsEnabled() && cfg.Dashboard.Matrix.Active()
}

// runMatrixIngest runs one incremental pass over the matrix-allowlisted
// repos when dashboard.matrix is active: open PRs come from the forgestate
// cache, only heads the sentinel has not evaluated are read, and each goes
// through the same evaluate → PatchObserver path the sweep uses, so a head
// is diffed once however it is reached. It records the pass on the dashboard
// so /api/matrix/lines can report last_ingest_at / next_ingest_in_s. It
// reports whether a pass ran and how many repos the forgestate cache could
// enumerate for it (0 while the cache is still warming after boot).
func runMatrixIngest(ctx context.Context, ghClient *github.Client, cfg *config.Config, dashSrv *dashboard.Server, st *matrixIngestState, now time.Time, logger *slog.Logger) (ran bool, cachedRepos int) {
	if ghClient == nil || !matrixIngestActive(cfg) || st == nil {
		return false, 0
	}
	if mode, _ := ghClient.APIBudgetMode(); mode == github.APIBudgetCritical {
		return false, 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.lastRun.IsZero() && now.Sub(st.lastRun) < matrixIngestInterval {
		return false, 0
	}
	st.lastRun = now
	opts := sentinelSweepOptions(cfg, ghClient, dashSrv)
	// The sentinel's own repo gate still applies; the matrix allowlist
	// narrows it to the repos the kiosk can show.
	sentinelAllowed, matrix := opts.RepoAllowed, cfg.Dashboard.Matrix
	opts.RepoAllowed = func(repo string) bool {
		if sentinelAllowed != nil && !sentinelAllowed(repo) {
			return false
		}
		owner, name := ghClient.SplitRepo(repo)
		return matrix.RepoAllowed(owner + "/" + name)
	}
	result, err := ghClient.SweepSentinelNewHeads(ctx, opts)
	if err != nil {
		logger.Warn("matrix ingest pass failed", "error", err)
		return true, 0
	}
	logSentinelResult(result, logger)
	if dashSrv != nil {
		dashSrv.RecordMatrixIngest(time.Now(), matrixIngestInterval)
	}
	if result.Fetched > 0 || len(result.Errors) > 0 {
		logger.Info("matrix ingest pass complete", "seen", result.Seen, "fetched", result.Fetched, "skipped", result.Skipped, "errors", len(result.Errors), "cached_repos", result.CachedRepos)
	}
	return true, result.CachedRepos
}

// matrixIngestState returns the shared pass state, creating it on first use.
func (b *boot) matrixIngest() *matrixIngestState {
	if b.matrixIngestSt == nil {
		b.matrixIngestSt = &matrixIngestState{}
	}
	return b.matrixIngestSt
}

// startMatrixIngest runs the incremental pass on its own ticker for the life
// of the boot context, independent of the governor tick. Idempotent.
func (b *boot) startMatrixIngest() {
	if b.matrixIngestStarted || b.ctx == nil {
		return
	}
	b.matrixIngestStarted = true
	st := b.matrixIngest()
	go b.matrixIngestLoop(st)
}

// matrixIngestBootRetry is the ticker spacing until the first pass finds the
// forgestate cache warm. After a restart only the heads that moved while the
// hive was down are new, and the kiosk should see them as soon as the cache
// can name them rather than a full interval later.
const matrixIngestBootRetry = 15 * time.Second

func (b *boot) matrixIngestLoop(st *matrixIngestState) {
	warm := false
	pass := func(now time.Time) {
		if _, cached := runMatrixIngest(b.ctx, b.ghClient, b.cfg, b.dashSrv, st, now, b.logger); cached > 0 {
			warm = true
		}
	}
	pass(time.Now())
	ticker := time.NewTicker(matrixIngestBootRetry)
	defer ticker.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case now := <-ticker.C:
			if warm {
				pass(now)
				continue
			}
			// The interval gate inside runMatrixIngest would hold a cold
			// retry back; clear it so the warm-up probes are not wasted.
			st.mu.Lock()
			st.lastRun = time.Time{}
			st.mu.Unlock()
			pass(now)
			if warm {
				ticker.Reset(matrixIngestInterval)
			}
		}
	}
}
