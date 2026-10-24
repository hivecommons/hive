package hub

import (
	"log/slog"
	"sync"
	"time"
)

// githubWarnInterval is how long an identical GitHub fetch warning stays
// quiet after it fires. A persistent failure (e.g. HTTP 403 from a rate limit
// or a token without access) would otherwise WARN on every poll tick and trip
// the fleet self-report's repeated-error classifier.
const githubWarnInterval = time.Hour

var (
	githubWarnMu   sync.Mutex
	githubWarnLast = map[string]time.Time{}
	githubWarnNow  = time.Now
)

// warnGitHubFetchOnce logs at WARN the first time key is seen within
// githubWarnInterval and at DEBUG afterwards.
func warnGitHubFetchOnce(logger *slog.Logger, key, msg string, args ...any) {
	if logger == nil {
		return
	}
	now := githubWarnNow()
	githubWarnMu.Lock()
	last, seen := githubWarnLast[key]
	if !seen || now.Sub(last) >= githubWarnInterval {
		if len(githubWarnLast) >= 256 {
			githubWarnLast = map[string]time.Time{}
		}
		githubWarnLast[key] = now
		githubWarnMu.Unlock()
		logger.Warn(msg, args...)
		return
	}
	githubWarnMu.Unlock()
	logger.Debug(msg, args...)
}
