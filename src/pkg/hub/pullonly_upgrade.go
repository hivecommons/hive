package hub

import (
	"time"
)

// upgradeCollectible reports whether this hive can COLLECT an upgrade
// instruction, i.e. whether it is heartbeating.
//
// lastHeartbeat is RegistryEntry.LastHeartbeat (RFC3339, empty when the hive has
// never checked in). A hive that has never heartbeated can never collect, and a
// hive silent past staleRemoveAge is being evicted anyway.
//
// DELIBERATELY NOT maxHeartbeatAge (5m). That constant drives the Online pill,
// and gating on it would refuse to arm an upgrade for a spoke that is merely one
// beat late or mid-restart — including, self-defeatingly, a spoke that is quiet
// precisely BECAUSE it is restarting into the upgrade we just armed. The
// question here is "will this hive ever come back to collect", not "is it green
// right now", so the bound is the same one the registry already uses to decide a
// hive is gone for good. Anything fresher would re-introduce a different silent
// opt-out.
//
// An unparseable timestamp is treated as NOT collectible: it is the same
// evidence-quality rule evaluateOrphanedUpgrade() applies, and we must not arm
// on a value we cannot read.
func upgradeCollectible(lastHeartbeat string, now time.Time) bool {
	if lastHeartbeat == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, lastHeartbeat)
	if err != nil {
		return false
	}
	return now.Sub(t) <= staleRemoveAge
}

// uncollectibleUpgradeReason explains, in operator-facing terms, why an upgrade
// was not armed. It never includes kubeconfig paths or credentials.
func uncollectibleUpgradeReason(lastHeartbeat string) string {
	if lastHeartbeat == "" {
		return "this hive has never heartbeated, so it cannot collect an upgrade " +
			"instruction — upgrades are delivered on the spoke's own outbound heartbeat, " +
			"not pushed by the hub. Typically an unassigned placeholder with no spoke running."
	}
	return "this hive last heartbeated at " + lastHeartbeat +
		", beyond the point where it is considered present, so it cannot collect " +
		"an upgrade instruction"
}

// autoUpgradeBlocked reports whether the fleet row should say the hub has
// REFUSED this hive's auto-upgrade, and why.
//
// It is the read-side twin of the upgradeCollectible() gate in
// triggerAutoUpgrades(): same predicate, same reason string, so the badge and
// the hub's actual behaviour cannot drift. Exposed on MyHiveEntry as
// AutoUpgradeBlocked/AutoUpgradeBlockedReason precisely so the dashboard never
// re-implements this in JavaScript — it previously derived "Queued for
// auto-upgrade · 1pm ET" from autoUpgradeMode alone, which consults nothing
// about eligibility and so kept promising an upgrade the hub had permanently
// declined.
//
// autoUpgrade gates the whole thing: a hive that never asked for auto-upgrades
// is manual, not blocked, and must not carry the badge.
//
// This covers ONLY the refusal. The other gates in triggerAutoUpgrades() —
// claim in flight, wave cap, provisioning, the daily schedule — are transient
// and clear without intervention, so "queued" eventually comes true for them.
// Uncollectible is the one that never does.
func autoUpgradeBlocked(autoUpgrade bool, lastHeartbeat string, now time.Time) (bool, string) {
	if !autoUpgrade {
		return false, ""
	}
	if upgradeCollectible(lastHeartbeat, now) {
		return false, ""
	}
	return true, uncollectibleUpgradeReason(lastHeartbeat)
}

// noteUncollectibleUpgrade records — once per (hive, target) — that the hub
// declined to arm an upgrade the hive could not collect, and why.
//
// The de-duplication memory is s.uncollectibleUpgradeNoted, a PER-SERVER field
// (server.go) rather than a package global. It used to be a global, which was
// wrong in two ways: two hubs in one process — the normal case in tests, and
// possible in-process generally — shared and clobbered each other's state, so
// one server's arming could suppress a refusal the other should have reported;
// and every test had to open by hand-scrubbing the global for hive IDs its own
// fresh server had never seen, which is the smell that gave the bug away.
//
// WHY DE-DUPLICATE AT ALL. StartLatestSHAPoller ticks every
// latestSHAPollInterval (2m) and calls triggerAutoUpgrades() each time, so an
// un-deduplicated timeline write would append an identical entry ~720 times a
// day per hive — trading an unbounded re-arm loop for an unbounded timeline, and
// burying the genuine events a timeline exists to show. Keying on the TARGET
// means a genuinely new upgrade opportunity (the branch advanced) is reported
// again, while the same refusal is not repeated.
//
// The timeline is the durable, operator-visible surface. Silence is how the
// original wedge went unnoticed: a hive with auto_upgrade=true that never
// upgrades is indistinguishable from one already at latest.
func (s *HubServer) noteUncollectibleUpgrade(hiveID, target, reason string) {
	s.uncollectibleUpgradeMu.Lock()
	if s.uncollectibleUpgradeNoted == nil {
		s.uncollectibleUpgradeNoted = map[string]string{}
	}
	if prev, ok := s.uncollectibleUpgradeNoted[hiveID]; ok && prev == target {
		s.uncollectibleUpgradeMu.Unlock()
		return
	}
	s.uncollectibleUpgradeNoted[hiveID] = target
	s.uncollectibleUpgradeMu.Unlock()

	s.recordTimeline(hiveID, TimelineUpgradeStale,
		"auto-upgrade to "+orDash(target)+" not armed — "+reason, "auto-upgrade")
}

// forgetUncollectibleUpgrade drops the de-duplication memory for a hive, so that
// if it later becomes uncollectible again the refusal is reported afresh rather
// than suppressed by a stale entry.
//
// Called from two places, which together give an entry a bounded lifetime:
//
//   - when a hive is successfully armed (saas.go) — the condition has genuinely
//     cleared;
//   - when a hive is REMOVED (removeRegistryEntry / handleRegistryDelete in
//     server.go). Without that second caller an entry leaked forever for every
//     deleted hive, because a hive that is uncollectible is by definition never
//     armed and so never reached the first caller. The population this file
//     exists to handle — unassigned placeholders that never heartbeat — is
//     exactly the population that could only ever be removed, so the growth was
//     unbounded across hive churn despite the comment above claiming otherwise.
func (s *HubServer) forgetUncollectibleUpgrade(hiveID string) {
	s.uncollectibleUpgradeMu.Lock()
	delete(s.uncollectibleUpgradeNoted, hiveID)
	s.uncollectibleUpgradeMu.Unlock()
}

// upgradeBranchOrDefault resolves the branch whose latest SHA should be used as
// an upgrade target for a hive reporting gitBranch.
//
// WHY THE OLD `if branch == "" { branch = "v2" }` WAS WRONG. That default was
// written when v2 was the only branch. It is now a CROSS-BRANCH bug: a hive whose
// GitBranch is unset — which is every unassigned placeholder, since
// RegistryEntry.GitBranch is only ever populated from a heartbeat payload — had
// its upgrade target resolved against v2 while running on a v4 hub. That is what
// was observed live: the target issued was 0b78dc0, a commit that is on v2 and
// NOT on v4, byte-identical to the v2 entry in the hub's own latest-shas.json,
// while that file correctly recorded v4=7cd059b. v2 is no longer maintained, so
// the default is unambiguously wrong rather than merely stale.
//
// It is a genuinely separate bug from the collection wedge and neither causes the
// other. On a hive that CAN collect, it is the more dangerous of the two: the
// instruction is picked up and the spoke rolls itself onto a foreign branch's
// build.
//
// The hub's OWN branch is the right fallback: it is what the hub is built from,
// so it is the only branch the hub can honestly assume when the hive has not said
// otherwise, and it self-corrects as the fleet moves between branches instead of
// pinning a constant that silently rots. This mirrors the existing precedent in
// StartLatestSHAPoller, which already stopped hardcoding "v2" for the hub's own
// upgrade target for the same reason: "hardcoding here made the badge and the
// poller disagree the moment a hub ran on v3".
//
// Falls back to the stable release line only when the hub's own branch is
// somehow unset, so a hub that cannot identify itself still resolves against a
// real branch rather than an empty one (which returns no SHA and silently
// disables upgrades). v2 was retired in #8061.
func (s *HubServer) upgradeBranchOrDefault(gitBranch string) string {
	if gitBranch != "" {
		return gitBranch
	}
	if s.hubGitBranch != "" {
		return s.hubGitBranch
	}
	return stableReleaseLine(s.logger)
}
