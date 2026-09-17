// Advisory digest posting policy, extracted from cmd/hive's package main
// (#7238 stage 2).
//
// These are the decisions that surround a digest post rather than the digest
// itself: which repo it belongs to, whether the pinned issue is resolved yet,
// whether there is anything worth building or posting, and how often the
// GitHub round-trip is allowed to happen. pkg/advisory already owns Digest, so
// the policy that governs it belongs here too.
//
// Two shapes changed in the move, both forced and both improvements:
//
//   - The GitHub client arguments became plain booleans. pkg/github imports
//     pkg/advisory (see pkg/github/advisory.go), so this package cannot import
//     it back without an import cycle. Nothing was lost: both call sites only
//     ever asked whether the client was non-nil, and a bool named for that
//     question says so more plainly than a pointer that must not be
//     dereferenced.
//
//   - The update-interval gate became a *PostGate value instead of a mutable
//     package-level struct. In package main it was a global that tests had to
//     reach into and reset field-by-field under its own mutex before every
//     case -- the shared mutable state #7238 objects to, and a standing
//     cross-test ordering hazard. Callers now own an instance.
//
// advisoryIssueMissingError deliberately stayed behind in cmd/hive: it matches
// on *github.IssuesDisabledError, which is a real dependency on pkg/github and
// therefore the one piece of this concern that genuinely cannot move without
// the cycle.
package advisory

import (
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
)

// PrimaryRepo returns the repo the advisory digest is posted to: the configured
// primary repo, falling back to the first listed repo. Shared by the boot
// ensure, the per-cycle re-ensure, and the post path so all three can never
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

// IssueUnresolved reports whether the pinned advisory issue for repo is still
// unknown, i.e. the digest has nowhere to go. A recorded 0 counts as
// unresolved: it is the zero value a failed ensure would leave behind, and
// posting to issue 0 is not a thing.
func IssueUnresolved(advisoryIssues map[string]int, repo string) bool {
	num, ok := advisoryIssues[repo]
	return !ok || num <= 0
}

// IssueNumber returns the pinned advisory issue for repo, reporting false when
// it is unresolved on the same terms as IssueUnresolved.
func IssueNumber(advisoryIssues map[string]int, repo string) (int, bool) {
	num, ok := advisoryIssues[repo]
	return num, ok && num > 0
}

// ShouldBuildDigest reports whether a digest is worth assembling this cycle.
//
// hasGitHubClient replaces the *github.Client the caller used to pass, which
// was only ever nil-checked; see the package comment for why the pointer
// cannot cross this boundary.
func ShouldBuildDigest(beadStores map[string]*beads.Store, hasGitHubClient, hasExistingPinnedIssue bool) bool {
	if len(beadStores) > 0 {
		return true
	}
	return hasGitHubClient && hasExistingPinnedIssue
}

// ShouldPostDigest reports whether an assembled digest is worth writing.
// An empty digest still posts when a pinned issue exists, so the issue is
// actively emptied rather than left showing stale findings.
func ShouldPostDigest(digest *Digest, hasGitHubClient, hasPinnedIssue bool) bool {
	if digest == nil {
		return false
	}
	if digest.TotalCount > 0 || len(digest.RecentlyResolved) > 0 {
		return true
	}
	return hasGitHubClient && hasPinnedIssue
}

// PostGate tracks, per repo, when the digest was last SUCCESSFULLY posted, so
// governor.advisory.update_interval_s (#4820) can throttle the GitHub
// round-trip. It is mutex-guarded because the eval-cycle ticker is not the only
// caller — startup and restart paths post too. clampLogged makes the "interval
// clamped" warning a one-shot instead of a per-cycle drone.
//
// The zero value is not usable; call NewPostGate.
type PostGate struct {
	mu          sync.Mutex
	lastSuccess map[string]time.Time
	clampLogged bool
}

// NewPostGate returns a gate with no recorded posts, so the first attempt for
// any repo is always due.
func NewPostGate() *PostGate {
	return &PostGate{lastSuccess: map[string]time.Time{}}
}

// Due reports whether the update-interval gate is open for a post attempt to
// repo, logging (once) if the configured value was clamped. An interval of 0
// (unset knob) means the gate is ALWAYS open — the digest posts every eval
// cycle, exactly the pre-#4820 cadence — and a repo with no successful post
// since the gate was created is open too, so the first post is never delayed.
//
// The gate advances only on SUCCESS (RecordSuccess, mirroring how the #4818
// skip-guard records its hash): a failed attempt is retried on the very next
// cycle instead of waiting out the interval, keeping error recovery — and the
// hub's staleness signal — as prompt as today.
func (g *PostGate) Due(advCfg config.AdvisoryConfig, repo string, now time.Time, logger *slog.Logger) bool {
	interval := advCfg.UpdateInterval()
	g.mu.Lock()
	defer g.mu.Unlock()
	if raw := advCfg.UpdateIntervalS; raw > 0 && time.Duration(raw)*time.Second != interval && !g.clampLogged {
		g.clampLogged = true
		logger.Warn("advisory update_interval_s outside allowed bounds — clamped",
			"configured_s", raw, "effective_s", int(interval.Seconds()),
			"min_s", config.MinAdvisoryUpdateIntervalS, "max_s", config.MaxAdvisoryUpdateIntervalS)
	}
	if interval <= 0 {
		return true
	}
	last, ok := g.lastSuccess[repo]
	return !ok || now.Sub(last) >= interval
}

// RecordSuccess advances the update-interval gate for repo after a successful
// digest write. Skip-if-unchanged cycles count too: pkg/github returns nil for
// them by design so freshness advances (#4818/#4821), and an unchanged digest
// is exactly the case the throttle exists to quiet.
func (g *PostGate) RecordSuccess(repo string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastSuccess[repo] = now
}
