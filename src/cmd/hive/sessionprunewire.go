package main

import (
	"time"

	"github.com/hivecommons/hive/pkg/sessionprune"
)

// sessionPruneInterval is how often the janitor re-runs. Session directories
// accumulate at roughly 150-200/day on a busy spoke, so nothing is urgent here;
// this only needs to be frequent enough that a restart loop cannot outpace it.
const sessionPruneInterval = 6 * time.Hour

// wireSessionPrune starts the session-state janitor.
//
// Every Copilot CLI invocation creates a session directory, every agent on the
// spoke shares one session-state directory (they all have HOME=/data/home), and
// nothing ever deleted them. A long-lived spoke reached ~9,400 directories
// spanning three months, which both leaks a shared NFS PVC and slows every
// session start, since the directory is read on startup.
func (w *spokeWire) wireSessionPrune() {
	retentionDays := sessionprune.DefaultRetentionDays
	if w.cfg.Data.SessionRetentionDays != nil {
		retentionDays = *w.cfg.Data.SessionRetentionDays
	}

	dir := w.cfg.Data.CopilotSessionsDir
	if dir == "" {
		return
	}

	// An explicit 0 (or negative) in config disables the janitor. Say so once at
	// startup: an operator who has turned off the only thing bounding the PVC
	// should be able to see that in the log.
	if retentionDays <= 0 {
		w.logger.Info("session prune disabled by config", "dir", dir)
		return
	}

	maxAge := time.Duration(retentionDays) * 24 * time.Hour
	stop := make(chan struct{})

	go func() {
		// Run once at startup rather than waiting a full interval. A spoke that
		// restarts more often than the interval would otherwise never prune.
		runSessionPrune(w, dir, maxAge)

		ticker := time.NewTicker(sessionPruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				runSessionPrune(w, dir, maxAge)
			}
		}
	}()

	w.addCleanup(func() { close(stop) })
}

func runSessionPrune(w *spokeWire, dir string, maxAge time.Duration) {
	res, err := sessionprune.Prune(dir, maxAge, time.Now(), w.logger)
	if err != nil {
		w.logger.Warn("session prune failed", "dir", dir, "error", err)
		return
	}
	// Only speak up when something actually happened. A steady-state spoke
	// prunes nothing on most passes, and a line every 6 hours saying "removed 0"
	// is noise that trains operators to ignore the janitor.
	if res.Removed > 0 || res.Failed > 0 {
		w.logger.Info("session prune complete",
			"dir", dir,
			"scanned", res.Scanned,
			"removed", res.Removed,
			"failed", res.Failed,
			"retention_days", int(maxAge.Hours()/24))
	}
}
