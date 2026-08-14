package hub

import (
	"sync"
)

// Auto-upgrade on a cluster the hub cannot write to.
//
// THE WEDGE THIS FILE EXISTS TO PREVENT. `vllm-d` and `a-ks-wec2` are declared
// `pull_only` in clusters.json: the hub has no network path to one and no
// `deployments.apps` RBAC on the other, and — per audit-8 F21 — is not MEANT to
// have either. Giving the hub write access is explicitly out of scope. But
// triggerAutoUpgrades() did not consult that declaration before ARMING an
// upgrade. The resulting loop, measured live on 2026-08-14:
//
//  1. triggerAutoUpgrades() sees a hive behind latest with AutoUpgrade=true and
//     calls beginUpgrade(): Upgrading=true, UpgradeStartedAt=now.
//  2. rolloutRestartHive() returns false IMMEDIATELY — clusterRecentlyUnreachable()
//     short-circuits on PullOnly by declaration, so not even a dial is attempted.
//  3. The heartbeat fallback is armed instead. For the 26 hives actually seen
//     wedged — unassigned placeholders that have never heartbeated — there is no
//     spoke on the other end to consume it, so nothing ever converges.
//  4. staleUpgradeTimeout (10m) passes. The stale-recovery branch re-arms. Because
//     the target is unchanged, beginUpgrade() deliberately PRESERVES the original
//     UpgradeStartedAt (so a thrashing retry still crosses the stuck threshold),
//     so upgradeAge keeps growing rather than resetting.
//  5. Nothing bounds this. sweepOrphanedUpgrades()'s retry budget
//     (maxOrphanedUpgradeSweeps) is the mechanism that is SUPPOSED to convert a
//     structurally-undeliverable upgrade into a visible UpgradeFailed, but
//     evaluateOrphanedUpgrade() returns early on an unparseable LastHeartbeat —
//     a hive that never heartbeated cannot prove its attempt is gone. So the
//     budget is never spent, exhaustion never fires, and step 4 repeats forever.
//
// Measured result: 208 re-arms in 15 minutes, stale_minutes 90–104, hives reading
// "offline" on the dashboard while showing "Upgrading 1h39m". The overlap was
// exact — 26 hives stale, 26 hives with auto_upgrade on a pull-only cluster,
// intersection 26, and zero stale hives outside that set.
//
// WHY REFUSING TO ARM IS THE RIGHT BEHAVIOUR, not tracking it by heartbeat.
// A correct v4 target would have been equally undeliverable, so the wedge is not
// about WHICH commit was chosen — it is about the hub instructing work it has no
// mechanism to perform. Of the three candidate fixes, "track it by what the spoke
// self-reports" still requires a spoke to be reporting, which is exactly what an
// unassigned placeholder is not doing; it would fix the 3 assigned cases and leave
// the 26 measured ones wedged. "Clear the flag on delivery failure" treats the
// symptom one cycle later and still burns an arm/re-arm every 10 minutes forever.
// Refusing at the ARM is the only one that makes the wedge unreachable rather than
// merely self-healing, and it is honest: the spokes are NOT broken. They run
// current code and self-derive every per-hive key from HIVE_HUB_SECRET + HIVE_ID.
// A hub-pushed upgrade is an optimisation for them, never a dependency.
//
// AND IT MUST NOT BE SILENT. Silently skipping is how this went unnoticed for as
// long as it did — a hive with auto_upgrade=true that simply never upgrades is
// indistinguishable from one that is up to date. So the refusal is recorded on the
// hive's timeline and surfaced through the same F21 reachability vocabulary the
// per-hive env sweep already established (UnreachableHives / UnreachableClusters /
// FleetFullyObserved on PerHiveEnvStatus): "the hub cannot reach this spoke" is a
// state an operator must be able to SEE, never one that renders as "fine".
//
// This deliberately reuses clusterRecentlyUnreachable() rather than testing
// PullOnly directly, so it is one notion of reachability, not a second one. That
// predicate already returns true by DECLARATION for a pull-only cluster (no dial,
// no timeout) and also covers a cluster inside the learned unreachable-breaker
// window — an upgrade armed at a cluster the hub just failed to dial is just as
// undeliverable as one aimed at a declared pull-only cluster, and wedges the same
// way.

// upgradeDeliverable reports whether the hub has any mechanism to DELIVER an
// upgrade it arms for a hive on this cluster.
//
// The hub's only write path to a spoke is `kubectl rollout restart` against the
// hive's Deployment. When the cluster is unreachable — declared pull_only, or
// inside the learned unreachable window — that path does not exist, and the
// heartbeat fallback is not a substitute the hub can rely on: it requires a spoke
// that is already checking in, which an unassigned placeholder is not.
//
// A nil cluster is NOT deliverable. The caller already treats "no cluster config"
// as a skip, and answering "deliverable" for a hive whose cluster cannot even be
// resolved would arm an upgrade aimed at nothing.
//
// The caller must NOT hold s.mu: clusterRecentlyUnreachable takes
// s.clusterUnreachableMu and reads s.clusters.
func (s *HubServer) upgradeDeliverable(cluster *ClusterConfig) bool {
	if cluster == nil {
		return false
	}
	// An in-cluster hive is reachable through the hub's own API server; the
	// unreachable breaker is about REMOTE clusters and is not consulted for it.
	if cluster.InCluster {
		return true
	}
	return !s.clusterRecentlyUnreachable(cluster.ID)
}

// undeliverableUpgradeReason explains, in operator-facing terms, why an upgrade
// for a hive on this cluster cannot be armed. It names the cluster and the
// declaration responsible so the log/timeline entry is actionable rather than a
// bare "skipped". It never includes kubeconfig paths or credentials — cluster IDs
// only, matching the UnreachableClusters convention.
func (s *HubServer) undeliverableUpgradeReason(cluster *ClusterConfig) string {
	if cluster == nil {
		return "no cluster config resolved for this hive"
	}
	if cluster.PullOnly {
		return "cluster " + cluster.ID + " is pull-only: the hub has no write path to this spoke, " +
			"so an armed upgrade could never be delivered. The spoke is not broken — it self-derives " +
			"its configuration and upgrades independently of the hub."
	}
	return "cluster " + cluster.ID + " is currently unreachable from the hub, " +
		"so an armed upgrade could not be delivered"
}

// undeliverableUpgradeNoted remembers the (hive, target) pairs already written to
// a timeline, so the refusal is recorded ONCE per target rather than on every
// poll.
//
// This matters for the same reason the bug it fixes matters. StartLatestSHAPoller
// ticks every latestSHAPollInterval (2m) and calls triggerAutoUpgrades() each
// time, so an un-deduplicated timeline write would append an identical entry ~720
// times a day per hive — trading an unbounded re-arm loop for an unbounded
// timeline, and burying the genuine events a timeline exists to show. Keying on
// the TARGET (not just the hive) means a genuinely new upgrade opportunity — the
// branch advanced, so there is a new SHA the hive is now behind — is reported
// again, while the same refusal is not repeated.
//
// Unbounded growth is not a concern at fleet scale: one entry per hive per
// distinct target, and the entry is dropped as soon as the hive becomes
// deliverable (forgetUndeliverableUpgrade), which is the transition that makes
// the memory stale.
var (
	undeliverableUpgradeMu    sync.Mutex
	undeliverableUpgradeNoted = map[string]string{}
)

// noteUndeliverableUpgrade records — once per (hive, target) — that the hub
// declined to arm an upgrade it could not deliver, and why.
//
// The timeline is the durable, operator-visible surface: it is what makes this
// refusal auditable after the fact, rather than a log line that ages out. Being
// able to SEE that a hive is deliberately not being upgraded is the whole point;
// silence here is what let the original wedge run unnoticed.
func (s *HubServer) noteUndeliverableUpgrade(hiveID, target, reason string) {
	undeliverableUpgradeMu.Lock()
	if prev, ok := undeliverableUpgradeNoted[hiveID]; ok && prev == target {
		undeliverableUpgradeMu.Unlock()
		return
	}
	undeliverableUpgradeNoted[hiveID] = target
	undeliverableUpgradeMu.Unlock()

	s.recordTimeline(hiveID, TimelineUpgradeStale,
		"auto-upgrade to "+orDash(target)+" not armed — "+reason, "auto-upgrade")
}

// upgradeBranchOrDefault resolves the branch whose latest SHA should be used as
// an upgrade target for a hive reporting gitBranch.
//
// WHY THE OLD `if branch == "" { branch = "v2" }` WAS WRONG. That default was
// written when v2 was the only branch. It is now a CROSS-BRANCH bug: a hive whose
// GitBranch is unset — which is every unassigned placeholder, since
// RegistryEntry.GitBranch is only ever populated from a heartbeat payload and a
// placeholder has never heartbeated — had its upgrade target resolved against v2
// while running on a v4 hub. That is exactly what was observed live: the target
// issued was 0b78dc0, a commit that is on v2 and NOT on v4, while the hub's own
// latest-shas.json correctly recorded v4=7cd059b. The hub was instructing spokes
// to move to a commit that does not exist on their branch.
//
// It is a genuinely separate bug from the pull-only wedge and neither causes the
// other: a CORRECT v4 target would have been equally undeliverable to a pull-only
// cluster, so fixing this alone would not have unwedged anything. But it would
// have made the wedge much easier to diagnose, and on a REACHABLE cluster it is
// the more dangerous of the two — there, delivery succeeds, and the hub would be
// rolling a v4 spoke onto a v2 build.
//
// The hub's OWN branch is the right fallback. It is what the hub is built from,
// so it is the only branch the hub can honestly assume when the hive has not said
// otherwise, and it makes the default self-correcting as the fleet moves between
// branches instead of pinning a constant that silently rots. This mirrors the
// existing precedent in StartLatestSHAPoller, which already stopped hardcoding
// "v2" for the hub's own upgrade target for the same reason: "hardcoding here
// made the badge and the poller disagree the moment a hub ran on v3".
//
// Falls back to "v2" only when the hub's own branch is somehow unset, preserving
// the historical behaviour for a hub that cannot identify itself rather than
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

// forgetUndeliverableUpgrade drops the de-duplication memory for a hive, so that
// if it later becomes undeliverable again the refusal is reported afresh rather
// than suppressed by a stale entry from a previous episode. Called when a hive is
// successfully armed — i.e. the condition has genuinely cleared.
func forgetUndeliverableUpgrade(hiveID string) {
	undeliverableUpgradeMu.Lock()
	delete(undeliverableUpgradeNoted, hiveID)
	undeliverableUpgradeMu.Unlock()
}
