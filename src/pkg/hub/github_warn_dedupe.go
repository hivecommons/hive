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

// commitMsgBackoff is how long a SHA whose commit-message fetch failed (e.g.
// 403 rate limit) is left alone before it is fetched again; callers carry on
// with the SHA alone meanwhile.
const commitMsgBackoff = 10 * time.Minute

var (
	commitMsgBackoffMu    sync.Mutex
	commitMsgBackoffUntil = map[string]time.Time{}
)

func commitMsgBackedOff(sha string) bool {
	commitMsgBackoffMu.Lock()
	defer commitMsgBackoffMu.Unlock()
	until, ok := commitMsgBackoffUntil[sha]
	return ok && githubWarnNow().Before(until)
}

func markCommitMsgBackoff(sha string) {
	commitMsgBackoffMu.Lock()
	defer commitMsgBackoffMu.Unlock()
	if len(commitMsgBackoffUntil) >= 256 {
		commitMsgBackoffUntil = map[string]time.Time{}
	}
	commitMsgBackoffUntil[sha] = githubWarnNow().Add(commitMsgBackoff)
}
