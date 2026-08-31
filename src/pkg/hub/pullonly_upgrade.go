package hub

import (
	"time"
)

// Auto-upgrade for a spoke that cannot COLLECT the instruction.
//
// DELIVERY IS PULL, NOT PUSH. This is the fact the whole file turns on, and it
// is worth stating precisely because an earlier version of this fix got it
// wrong. The hub never pushes an upgrade to a spoke. It records a target, and
// the spoke collects it on its own OUTBOUND heartbeat:
//
//   - the hub sets resp.UpgradeTo on the heartbeat response (server.go ~2194 /
//     ~2201, from the in-memory heartbeatUpgrade map);
//   - the spoke reads it and invokes its UpgradeCallback (heartbeat.go ~752);
//   - that callback runs entirely on the SPOKE (cmd/hive/main.go ~3358): it
//     patches its own Deployment image via UpgradeSelfToSHA, or failing that
//     restarts itself via RolloutRestartSelf (self_upgrade.go), using its OWN
//     in-cluster ServiceAccount at
//     /var/run/secrets/kubernetes.io/serviceaccount.
//
// The hub's kubectl write path is therefore NOT the delivery mechanism.
// rolloutRestartHive() is an OPTIMISATION — a fast-path that saves the spoke
// waiting for its next beat — which is exactly why every caller already treats
// its failure as benign and falls back to the heartbeat. A pull-only cluster
// costs the fast-path and nothing else.
//
// WHY THE PREVIOUS GATE WAS WRONG. An earlier revision of this fix keyed on
// cluster reachability (clusterRecentlyUnreachable / PullOnly). That predicate
// answers "can the hub WRITE into this cluster", which is not the question.
// Gating on it would have disabled auto-upgrade for every pull-only spoke that
// heartbeats perfectly well — 40+ of them — converting a visible wedge into a
// SILENT PERMANENT OPT-OUT. That is a worse failure than the bug: a wedge is at
// least loud, whereas a spoke that quietly stops receiving upgrades looks
// identical to one that is up to date. Reachability is not used here at all.
//
// THE ACTUAL WEDGE. What the 26 measured hives have in common is not their
// cluster — it is that they have NEVER HEARTBEATED. Confirmed on the live
// registry: every wedged entry carries no last_heartbeat, no git_branch, and no
// orphaned_upgrade_sweeps, while upgrading=true. They are unassigned
// placeholders. Under a pull model that alone explains everything:
//
//  1. triggerAutoUpgrades() arms a target and latches Upgrading=true.
//  2. Nothing ever collects it, because collection happens on a heartbeat and
//     this spoke does not send one.
//  3. staleUpgradeTimeout passes; stale-recovery re-arms. The target is
//     unchanged, so beginUpgrade() deliberately preserves the original
//     UpgradeStartedAt and the elapsed only ever GROWS.
//  4. sweepOrphanedUpgrades()'s retry budget — the mechanism meant to convert a
//     never-landing upgrade into a visible UpgradeFailed — cannot bound it:
//     evaluateOrphanedUpgrade() returns early on an unparseable LastHeartbeat,
//     because a hive that never checked in cannot PROVE its attempt is gone. So
//     the budget is never spent and step 3 repeats forever.
//
// Measured: 26 hives re-arming, stale_minutes climbing past 146 and still
// rising. The pull-only clusters were a red herring — a perfect correlation
// with the real cause, because those pools happen to be where the unassigned
// placeholders live.
//
// The gate below is therefore keyed on heartbeat history, which is the correct
// predicate on EVERY cluster: a hive that cannot collect an instruction must not
// be handed one, whether it sits on the hub-reachable cluster or the heartbeat-only cluster.

// PUSH-PATH RETIREMENT: WHAT THIS PR DOES AND DOES NOT MAKE DROPPABLE.
//
// This PR retires the UPGRADE push path only. Every upgrade/restart/branch-switch
// delivery is now pull; rolloutRestartHive() is deleted and no upgrade path shells
// out to kubectl against a spoke cluster.
//
// THE HUB'S KUBECONFIGS ARE NOT YET DROPPABLE. Two things still read them, and
// removing the kubeconfigs before those land would degrade them SILENTLY, which
// is worse than failing loudly:
//
//  1. NODE STATS (saas.go ~2000: `kubectl get nodes -o json`, ~1993 `top nodes`,
//     ~2126 node summary). The heartbeat fallback beside it fires only when the
//     kubectl query ERRORS or times out — on a reachable cluster like the hub-reachable cluster
//     the kubectl path succeeds, so the spoke is never asked. Drop the
//     kubeconfig and this does not fail over; it just stops reporting.
//
//     The spoke-side collector exists (CollectClusterHealth, cluster_metrics.go)
//     but cannot currently answer on either cluster:
//       - it requires the metrics API FIRST and returns nil if that fails
//         (cluster_metrics.go ~59), and nodes.metrics.k8s.io is not granted to
//         hive-sa on the hub-reachable cluster OR the heartbeat-only cluster;
//       - it is gated on HIVE_CLUSTER_ID (cmd/hive/main.go ~3310), which
//         provisioning does not emit;
//       - node-read RBAC is not in the provisioning template (saas_provision.go)
//         at all — where it exists on the heartbeat-only cluster it was granted out-of-band.
//     Verified live 2026-08-14: on the hub-reachable cluster, `auth can-i list nodes` and
//     `list nodes.metrics.k8s.io` as the spoke's hive-sa are both "no".
//
//  2. PROVISIONING itself (saas_provision.go) creates namespaces, Deployments
//     and RBAC on spoke clusters. That is inherently a hub-side write and is not
//     in scope for pull.
//
// So: this PR removes the upgrade path's need for write access. It does NOT make
// the kubeconfigs removable. The ordering constraint is explicit — the node-stats
// pull path and its provisioning prerequisites must land BEFORE any kubeconfig is
// dropped or downgraded to read-only. Anything relying on hand-patched RBAC is
// exactly the class of drift that nothing in code re-asserts.

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
// Falls back to "v2" only when the hub's own branch is somehow unset, preserving
// historical behaviour for a hub that cannot identify itself rather than
// resolving against an empty branch (which returns no SHA and silently disables
// upgrades).
func (s *HubServer) upgradeBranchOrDefault(gitBranch string) string {
	if gitBranch != "" {
		return gitBranch
	}
	if s.hubGitBranch != "" {
		return s.hubGitBranch
	}
	return "v2"
}
