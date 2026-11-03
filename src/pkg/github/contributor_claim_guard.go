package github

import (
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// ContributorClaimGuard decides when a live CONTRIBUTOR claim stops
// withholding an issue from the hive's own agents (hivecommons/hive#13579).
//
// A relay contributor's claim is a weaker promise than an agent's or a
// human's: the relay may drop the socket a minute after it was handed the
// issue, and nothing but the hub's own bookkeeping says whether it will come
// back. #13579 showed the failure mode — 18 relay disconnects in 12 hours,
// each renewing the claim, and the scanner/coder withheld from the issue for
// 15 hours because every eval cycle read "live issue claim". Two rules make
// contributor claims advisory instead of absolute:
//
//   - Cool-off: the contribute hub has put the issue in contributor cool-off
//     (repeated disconnect releases; pkg/dashboard contribute_flap.go). The
//     hub is already refusing to hand the issue to contributors, so a claim
//     it still carries is a straggler; hive agents may take the issue now.
//
//   - Starvation: the issue has been continuously withheld from hive agents
//     by contributor claims for Starvation or longer with no PR evidence on
//     the claim. Whoever holds it is not shipping; hive agents may take it.
//
// Agent, human and external claims are never overridden here. A zero guard
// (nil) is the pre-#13579 behaviour: every live claim withholds.
type ContributorClaimGuard struct {
	// Starvation is how long contributor claims may keep hive agents off an
	// issue before they become advisory. Zero disables the starvation rule.
	Starvation time.Duration
	// CoolOff reports the contribute hub's active cool-off for an issue and
	// its operator-facing reason. Nil disables the cool-off rule.
	CoolOff func(repo string, number int) (reason string, active bool)

	mu sync.Mutex
	// firstWithheld is when each issue was first seen withheld by a
	// contributor claim; cleared when a pass sees the issue unclaimed or
	// absent.
	firstWithheld map[string]time.Time
	// overridden is the set of issues the last pass let through despite a
	// live contributor claim; Overridden serves the scheduler's inflight
	// lookup so the same decision applies there.
	overridden map[string]string
}

// NewContributorClaimGuard builds a guard with the given starvation window
// and cool-off source.
func NewContributorClaimGuard(starvation time.Duration, coolOff func(repo string, number int) (string, bool)) *ContributorClaimGuard {
	return &ContributorClaimGuard{Starvation: starvation, CoolOff: coolOff}
}

// ClaimKindContributor is the marker kind= of a relay contributor claim
// (claims.KindContributor), spelled here because this package is the leaf.
const ClaimKindContributor = "contributor"

func contributorGuardKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

// Overridden reports whether the last filter pass let the issue through
// despite a live contributor claim, and why.
func (g *ContributorClaimGuard) Overridden(repo string, number int) (string, bool) {
	if g == nil {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	reason, ok := g.overridden[contributorGuardKey(repo, number)]
	return reason, ok
}

// WithheldSince reports when the issue was first withheld by a contributor
// claim in the current unbroken run of passes, for tests and the runs API.
func (g *ContributorClaimGuard) WithheldSince(repo string, number int) (time.Time, bool) {
	if g == nil {
		return time.Time{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.firstWithheld[contributorGuardKey(repo, number)]
	return t, ok
}

// coolOffFor is CoolOff with a nil-safe receiver and a default reason.
func (g *ContributorClaimGuard) coolOffFor(issue Issue) (string, bool) {
	if g == nil || g.CoolOff == nil || issue.Number <= 0 {
		return "", false
	}
	reason, active := g.CoolOff(issue.Repo, issue.Number)
	if !active {
		return "", false
	}
	if reason == "" {
		reason = "contributor cool-off"
	}
	return reason, true
}

// advisoryReason decides, for one issue carrying a live claim at now,
// whether the claim is advisory. It records first-withheld times and prunes
// state for issues not seen this pass via the seen set.
func (g *ContributorClaimGuard) advisoryReason(issue Issue, now time.Time, seen map[string]bool) (string, bool) {
	if g == nil || issue.ClaimKind != ClaimKindContributor {
		return "", false
	}
	key := contributorGuardKey(issue.Repo, issue.Number)
	seen[key] = true
	if reason, active := g.coolOffFor(issue); active {
		return reason, true
	}
	if issue.ClaimPRStarted {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.firstWithheld == nil {
		g.firstWithheld = make(map[string]time.Time)
	}
	first, ok := g.firstWithheld[key]
	if !ok {
		first = now
		g.firstWithheld[key] = now
	}
	if g.Starvation > 0 && now.Sub(first) >= g.Starvation {
		return fmt.Sprintf("contributor claims have withheld this issue from hive agents for %s with no PR; contributor claim is advisory",
			now.Sub(first).Round(time.Minute)), true
	}
	return "", false
}

// finishPass drops tracking for issues that were not withheld by a
// contributor claim this pass and records the override set.
func (g *ContributorClaimGuard) finishPass(seen map[string]bool, overridden map[string]string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for key := range g.firstWithheld {
		if !seen[key] {
			delete(g.firstWithheld, key)
		}
	}
	g.overridden = overridden
}

// FilterLiveIssueClaims is FilterLiveIssueClaims with this guard applied: a
// live contributor claim the guard rules advisory keeps its issue in the
// actionable set, annotated with Issue.ClaimAdvisory and a work-detail row
// in the "actionable" bucket, instead of withholding it. Withheld issues
// that are in contributor cool-off carry the cool-off clause in their
// "claimed" reason so the Withheld panel shows why contributors are not
// being offered them either.
func (g *ContributorClaimGuard) FilterLiveIssueClaims(result *ActionableResult, now time.Time, logger *slog.Logger) int {
	if result == nil || len(result.Issues.Items) == 0 {
		return 0
	}
	seen := make(map[string]bool)
	overridden := make(map[string]string)
	kept := make([]Issue, 0, len(result.Issues.Items))
	withheld := 0
	for _, issue := range result.Issues.Items {
		claim, live := issue.LiveClaim(now)
		if !live {
			kept = append(kept, issue)
			if reason, active := g.coolOffFor(issue); active {
				appendRepoWorkIssueDetail(result, issue, "actionable", reason+"; hive agents may claim it")
			}
			continue
		}
		if reason, advisory := g.advisoryReason(issue, now, seen); advisory {
			issue.ClaimAdvisory = reason
			overridden[contributorGuardKey(issue.Repo, issue.Number)] = reason
			kept = append(kept, issue)
			appendRepoWorkIssueDetail(result, issue, "actionable",
				"Contributor claim by "+claim.Identity+" is advisory: "+reason)
			if logger != nil {
				logger.Info("contributor claim advisory: issue stays actionable for hive agents (#13579)",
					"repo", issue.Repo, "issue", issue.Number,
					"claimed_by", claim.Identity, "claim_expires_at", claim.ExpiresAt,
					"reason", reason)
			}
			continue
		}
		withheld++
		detail := "Live issue claim by " + claim.Identity
		if reason, active := g.coolOffFor(issue); active {
			detail += "; " + reason
		}
		if logger != nil {
			logger.Info("withholding issue: live issue claim",
				"repo", issue.Repo, "issue", issue.Number,
				"claimed_by", claim.Identity, "claim_expires_at", claim.ExpiresAt,
				"claim_source", claim.Source)
		}
		appendRepoWorkIssueDetail(result, issue, "claimed", detail)
	}
	g.finishPass(seen, overridden)
	if withheld == 0 && len(overridden) == 0 {
		return 0
	}
	result.Issues = IssueResultFromItems(kept)
	return withheld
}
