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

// commitMsgBackoff is how long a SHA whose commit-message fetch failed is left
// alone before it is fetched again; callers carry on with the SHA alone
// meanwhile. It is also the fallback global back-off for a 403/429 that carries
// no reset hint. A 403/429 is a client-wide condition (rate limit or a token
// without access), so it additionally parks every SHA until
// commitMsgGlobalBackoffUntil, capped at commitMsgGlobalBackoffMax.
const (
	commitMsgBackoff          = 10 * time.Minute
	commitMsgGlobalBackoffMax = time.Hour
)

var (
	commitMsgBackoffMu    sync.Mutex
	commitMsgBackoffUntil = map[string]time.Time{}

	commitMsgGlobalBackoffUntil time.Time
)

func commitMsgBackedOff(sha string) bool {
	commitMsgBackoffMu.Lock()
	defer commitMsgBackoffMu.Unlock()
	now := githubWarnNow()
	if now.Before(commitMsgGlobalBackoffUntil) {
		return true
	}
	until, ok := commitMsgBackoffUntil[sha]
	return ok && now.Before(until)
}

func markCommitMsgBackoff(sha string) {
	commitMsgBackoffMu.Lock()
	defer commitMsgBackoffMu.Unlock()
	if len(commitMsgBackoffUntil) >= 256 {
		commitMsgBackoffUntil = map[string]time.Time{}
	}
	commitMsgBackoffUntil[sha] = githubWarnNow().Add(commitMsgBackoff)
}

// markCommitMsgGlobalBackoff parks every commit-message fetch for d.
func markCommitMsgGlobalBackoff(d time.Duration) {
	commitMsgBackoffMu.Lock()
	defer commitMsgBackoffMu.Unlock()
	commitMsgGlobalBackoffUntil = githubWarnNow().Add(d)
}
