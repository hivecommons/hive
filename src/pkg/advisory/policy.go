package advisory

import (
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
)

// PrimaryRepo returns the repo the advisory digest is posted to: the
// configured primary repo, falling back to the first listed repo. Shared by the
// boot ensure, the per-cycle re-ensure, and the post path so all three can never
// disagree about WHICH repo's pinned issue the digest belongs to.
func PrimaryRepo(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if cfg.Project.PrimaryRepo != "" {
		return cfg.Project.PrimaryRepo
	}
	if len(cfg.Project.Repos) > 0 {
		return cfg.Project.Repos[0]
	}
	return ""
}

// IssueUnresolved reports whether the pinned advisory issue for repo is
// still unknown, i.e. the digest has nowhere to go. A recorded 0 counts as
// unresolved: it is the zero value a failed ensure would leave behind, and
// posting to issue 0 is not a thing.
func IssueUnresolved(advisoryIssues map[string]int, repo string) bool {
	num, ok := advisoryIssues[repo]
	return !ok || num <= 0
}

// IssueNumber is the read-side twin of IssueUnresolved: it reports the pinned
// advisory issue for repo, treating a recorded 0 as "not resolved".
func IssueNumber(advisoryIssues map[string]int, repo string) (int, bool) {
	num, ok := advisoryIssues[repo]
	return num, ok && num > 0
}

// ShouldBuildDigest reports whether a digest is worth assembling this cycle.
//
// hasGitHubClient is the caller's own nil check on its GitHub client. The
// policy takes a bool rather than the client itself because pkg/github imports
// this package: accepting *github.Client here would close an import cycle.
func ShouldBuildDigest(beadStores map[string]*beads.Store, hasGitHubClient, hasExistingPinnedIssue bool) bool {
	if len(beadStores) > 0 {
		return true
	}
	return hasGitHubClient && hasExistingPinnedIssue
}

// ShouldPostDigest reports whether an assembled digest is worth writing to
// GitHub. See ShouldBuildDigest for why hasGitHubClient is a bool.
func ShouldPostDigest(digest *Digest, hasGitHubClient, hasPinnedIssue bool) bool {
	if digest == nil {
		return false
	}
	if digest.TotalCount > 0 || len(digest.RecentlyResolved) > 0 {
		return true
	}
	return hasGitHubClient && hasPinnedIssue
}

// postGate tracks, per repo, when the digest was last SUCCESSFULLY
// posted, so governor.advisory.update_interval_s (#4820) can throttle the
// GitHub round-trip. Package-level because runEvalCycle carries no state of
// its own, and mutex-guarded because startup/restart call sites exist besides
// the ticker. clampLogged makes the "interval clamped" warning a one-shot
// instead of a per-cycle drone.
var postGate = struct {
	mu          sync.Mutex
	lastSuccess map[string]time.Time
	clampLogged bool
}{lastSuccess: map[string]time.Time{}}

// PostDue reports whether the update-interval gate is open for a post
// attempt to repo, logging (once) if the configured value was clamped. An
// interval of 0 (unset knob) means the gate is ALWAYS open — the digest posts
// every eval cycle, exactly the pre-#4820 cadence — and a repo with no
// successful post since process start is open too, so the first post is never
// delayed. The gate advances only on SUCCESS (RecordPostSuccess,
// mirroring how the #4818 skip-guard records its hash): a failed attempt is
// retried on the very next cycle instead of waiting out the interval, keeping
// error recovery — and the hub's staleness signal — as prompt as today.
func PostDue(advCfg config.AdvisoryConfig, repo string, now time.Time, logger *slog.Logger) bool {
	interval := advCfg.UpdateInterval()
	postGate.mu.Lock()
	defer postGate.mu.Unlock()
	if raw := advCfg.UpdateIntervalS; raw > 0 && time.Duration(raw)*time.Second != interval && !postGate.clampLogged {
		postGate.clampLogged = true
		logger.Warn("advisory update_interval_s outside allowed bounds — clamped",
			"configured_s", raw, "effective_s", int(interval.Seconds()),
			"min_s", config.MinAdvisoryUpdateIntervalS, "max_s", config.MaxAdvisoryUpdateIntervalS)
	}
	if interval <= 0 {
		return true
	}
	last, ok := postGate.lastSuccess[repo]
	return !ok || now.Sub(last) >= interval
}

// RecordPostSuccess advances the update-interval gate for repo after a
// successful digest write. Skip-if-unchanged cycles count too: pkg/github
// returns nil for them by design so freshness advances (#4818/#4821), and an
// unchanged digest is exactly the case the throttle exists to quiet.
func RecordPostSuccess(repo string, now time.Time) {
	postGate.mu.Lock()
	defer postGate.mu.Unlock()
	postGate.lastSuccess[repo] = now
}
