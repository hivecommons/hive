package hub

import (
	"context"
	"strings"
	"time"
)

// NET_ADMIN reconcile — closes the pre-#1222 securityContext drift.
//
// The provisioning template (saas_provision.go) requests NET_ADMIN on the hive
// container's securityContext, but that manifest is `kubectl apply`ed ONLY
// inside provisionHive — at provision/assign time. Hives provisioned BEFORE
// #1222 (when NET_ADMIN was added to the template) still carry an empty
// `securityContext: {}` on their LIVE Deployment and were never re-applied.
//
// That drift is harmless today but becomes fatal the moment the F5 fatal-egress
// image (#2664) rolls to such a hive: without NET_ADMIN the forced iptables
// egress redirect can't be established, the F5 fatal check exits 1, and the pod
// crash-loops. This reconcile repairs the drift fleet-wide WITHOUT a full
// re-provision (which would re-deliver secrets/tokens and is far heavier).
//
// Scope: ALL hub-managed hosted hives with a resolvable cluster — not just
// auto-upgrade hives. A drifted hive with auto-upgrade OFF still crash-loops if
// it is ever manually rolled onto the F5 image, so the correction must reach it
// too (issue #2674).
//
// OpenShift caveat (#2674): on OpenShift/OVN clusters, NET_ADMIN in the podspec
// is necessary but NOT sufficient — `iptables -t nat -N` is denied at a layer
// below the SCC/pod-capability grant (node/CNI/OVN or seccomp). This reconcile
// only ensures the podspec REQUESTS NET_ADMIN, which is correct and necessary
// everywhere. It deliberately does NOT try to solve the node-level OpenShift
// question: those hives additionally rely on the SO_MARK path (#2678/#2696) and
// may still need HIVE_PROXY_ADVISORY_OK=true — out of scope here.

const (
	// netAdminCapability is the Linux capability the F5 forced-egress iptables
	// redirect requires. Named so the check and the patch can never disagree.
	netAdminCapability = "NET_ADMIN"

	// netAdminReconcileInterval throttles the sweep. Drift is STATIC (a hive
	// either has NET_ADMIN or it doesn't; a correctly-provisioned hive never
	// loses it), so this is remediation, not a hot path — a generous interval
	// keeps it off the per-beat/per-poll critical path. The SHA poller ticks
	// every 2 min; we run the reconcile at most once per this window.
	netAdminReconcileInterval = 15 * time.Minute

	// netAdminKubectlTimeout bounds each per-hive kubectl get/patch so one
	// unreachable cluster can't stall the whole sweep. Mirrors
	// upgradeKubectlTimeout in spirit.
	netAdminKubectlTimeout = 15 * time.Second

	// The reconcile patches only capabilities.add; replacing the whole
	// securityContext would erase seccomp and capability drops.
	hiveContainerCapabilitiesAddPath = "/spec/template/spec/containers/0/securityContext/capabilities/add"
)

// netAdminSweepEligible reports whether a hive with this lifecycle status
// should be examined by the NET_ADMIN drift sweep. Extracted as a named
// predicate — the same shape as securityContextHasNetAdmin — so the selection
// rule is a single testable decision that the sweep and its test share, rather
// than an inline comparison a test could only re-implement (and therefore keep
// passing after the sweep's own check broke).
//
// WHY THIS EXISTS. The original guard was `h.Status != "running"`. It selected
// NOTHING: across the live 66-hive registry the status distribution is 30
// "available", 28 "", 8 "assigned" — zero "running". Only two sites write
// "running": the provision watcher in saas_provision.go, which sets it on the
// transition OUT of "provisioning", and the stale-"error" clear on heartbeat in
// server.go, which is gated on Status == "error" and so only rescues a hive out
// of error. Neither puts or holds a steady-state hive at "running", and nothing
// re-asserts it, so a hive in steady state never carries it. This sweep
// therefore `continue`d on every hive of every cycle and has never patched a
// Deployment in production — the pre-#1222 drift it exists to close was only
// ever repaired by the out-of-band `kubectl patch` recorded in issue #2674.
//
// WHAT IT SHOULD HAVE EXPRESSED. Not "is running" but "is deployed" — does this
// hive plausibly have a hive Deployment to reconcile? The sibling sweeps over
// the same listSaaSHives() (triggerAutoUpgrades, sweepOrphanedUpgrades,
// sweepStuckAssignments) apply NO status filter at all and gate instead on the
// resources they actually need. This predicate follows that convention, naming
// only the one status that is genuinely premature.
//
//   - "" (28 live) — the common steady state: provisioned and deployed, with a
//     status that was simply never written. Must be swept.
//   - "available" (30 live) — an unclaimed pre-provisioned placeholder. Its
//     Deployment is live, and it must carry NET_ADMIN BEFORE it is claimed: a
//     placeholder handed to a user while drifted crash-loops the moment the F5
//     fatal-egress image rolls onto it, which is the exact failure this sweep
//     exists to prevent.
//   - "assigned" (8 live) — claimed and deployed. Obviously swept.
//   - "running" — kept so the pre-existing intent still selects, not because
//     anything reaches it.
//   - "error" — SWEPT. The status is frequently stale (only a heartbeat clears
//     it), the Deployment usually still exists, and missing NET_ADMIN is a
//     plausible CAUSE of the error (an F5 fatal-egress crash-loop presents
//     exactly this way) rather than a reason to leave it drifted.
//
// Only "provisioning" is excluded: the namespace and Deployment are still being
// applied, and the provisioning template has requested NET_ADMIN since #1222, so
// a hive created now is born correct. This is a cheap-cycle optimisation, NOT a
// safety gate. The sweep is safe on an unreadable hive for a stronger reason
// than its per-hive-env sibling: a failed `kubectl get` logs at Debug and
// `continue`s, and this sweep records NO observations of any kind — it has no
// seen-map and publishes no convergence surface, so there is no state in which
// an unread hive could ever be counted as healthy.
func netAdminSweepEligible(status string) bool {
	return strings.TrimSpace(status) != "provisioning"
}

// securityContextHasNetAdmin reports whether the jsonpath-extracted list of
// added capabilities already contains NET_ADMIN. This is the PURE decision
// function — no kubectl, fully unit-testable. `raw` is the trimmed stdout of:
//
//	kubectl get deploy hive -n <ns> \
//	  -o jsonpath={.spec.template.spec.containers[0].securityContext.capabilities.add}
//
// which renders a Go/JSON string slice like `[NET_ADMIN]` (jsonpath) or `[]` /
// empty when the field is absent. We match on the capability token rather than
// exact-parsing the shape so both the jsonpath list rendering and an empty
// result are handled identically: absent/empty ⇒ needs patch.
func securityContextHasNetAdmin(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" || raw == "<no value>" {
		return false
	}
	// jsonpath renders a string slice space-separated inside brackets, e.g.
	// "[NET_ADMIN]" or "[NET_ADMIN NET_RAW]". Tokenize on the bracket/space
	// boundaries and look for an exact NET_ADMIN token so a substring like
	// "NET_ADMIN_FOO" (should any future cap be named that) can't false-match.
	trimmed := strings.Trim(raw, "[]")
	for _, tok := range strings.Fields(trimmed) {
		if tok == netAdminCapability {
			return true
		}
	}
	return false
}

// netAdminPatchJSON returns the JSON-Patch body that installs NET_ADMIN onto
// the hive container's securityContext.
//
// A single `add` op at the capabilities path preserves the rest of the
// securityContext (including the RuntimeDefault seccomp profile and the
// SETUID/SETGID drops) while repairing the drifted NET_ADMIN field. Patching
// the whole object would silently undo the security hardening in the standard
// manifest. The path is present in current templates; JSON-Patch `add` also
// creates it for older deployments that have a securityContext without
// capabilities. We only call this after the idempotent check found NET_ADMIN
// missing, so it never issues a pointless rollout on an already-correct hive.
//
// NOTE: this DOES change the podspec, so applying it triggers a one-time
// rolling update — the intended correction. The idempotent has-NET_ADMIN check
// guarantees the patch is issued at most once (the next sweep sees NET_ADMIN
// present and skips), so it can never loop.
func netAdminPatchJSON() string {
	return `[{"op":"add","path":"` + hiveContainerCapabilitiesAddPath +
		`","value":["` + netAdminCapability + `"]}]`
}

// reconcileNetAdminIfDue runs the NET_ADMIN sweep only if at least
// netAdminReconcileInterval has elapsed since the last run, so the 2-min SHA
// poller doesn't fire this remediation sweep every cycle. Safe to call from the
// poller loop every tick.
func (s *HubServer) reconcileNetAdminIfDue() {
	s.clusterUnreachableMu.Lock()
	due := s.lastNetAdminReconcile.IsZero() ||
		time.Since(s.lastNetAdminReconcile) >= netAdminReconcileInterval
	if due {
		s.lastNetAdminReconcile = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	s.reconcileNetAdmin()
}

// reconcileNetAdmin sweeps every hub-managed hosted hive and, for any whose
// live Deployment is missing NET_ADMIN on the hive container's securityContext,
// patches it in. It is idempotent (already-correct hives are a no-op, logged at
// Debug) and non-fatal on kubectl errors (an unreachable cluster is simply
// retried on the next sweep).
func (s *HubServer) reconcileNetAdmin() {
	hives := listSaaSHives()
	// Selection accounting. A sweep that selects NOBODY is the exact failure
	// this lane shipped with, and nothing made it visible: every counter
	// downstream of the filter stayed at zero, which is indistinguishable from
	// a fleet with no drift. Recording the considered/skipped split makes "the
	// filter matches nothing" readable instead of silent.
	consideredHives := 0
	skippedByStatus := 0
	for _, h := range hives {
		// Only hives that plausibly have a live Deployment on a resolvable
		// cluster. A hive still provisioning has no stable deployment to
		// reconcile yet and is born with NET_ADMIN from the template; every
		// other status is swept. See netAdminSweepEligible for why this is not
		// a "running" check. This mirrors how triggerAutoUpgrades resolves the
		// cluster before touching a hive.
		if !netAdminSweepEligible(h.Status) {
			skippedByStatus++
			continue
		}
		consideredHives++
		cluster := s.clusterForHive(&h)
		if cluster == nil {
			continue
		}
		// Skip clusters the hub just failed to dial — the same suppression the
		// upgrade path uses, so one down cluster doesn't burn a timeout per hive
		// every sweep. It recovers on the next sweep after the TTL.
		if s.clusterRecentlyUnreachable(cluster.ID) {
			continue
		}

		ns := hiveHostedNamespacePrefix + h.ID

		ctx, cancel := context.WithTimeout(context.Background(), netAdminKubectlTimeout)
		getCmd := kubectlForClusterContext(ctx, cluster, "get", "deployment", "hive",
			"-n", ns, "-o",
			"jsonpath={.spec.template.spec.containers[0].securityContext.capabilities.add}")
		out, err := getCmd.Output()
		cancel()
		if err != nil {
			// Deployment missing (not-found), cluster unreachable, or transient
			// kubectl error — all non-fatal. Arm the unreachable cache only for a
			// dial failure so a merely-absent deployment doesn't suppress the
			// whole cluster; kubectl not-found still returns quickly, so treating
			// every error the same is acceptable but we prefer to only suppress on
			// genuine unreachability. Keep it simple: log Debug and continue; the
			// next sweep retries.
			s.logger.Debug("netadmin reconcile: could not read hive deployment securityContext",
				"hive_id", h.ID, "cluster", cluster.ID, "error", err)
			continue
		}
		s.markClusterReachable(cluster.ID)

		if securityContextHasNetAdmin(string(out)) {
			// Already correct — no rollout, stay quiet.
			s.logger.Debug("netadmin reconcile: hive deployment already has NET_ADMIN",
				"hive_id", h.ID, "cluster", cluster.ID)
			continue
		}

		// Missing NET_ADMIN — patch it in. This triggers a one-time rolling
		// update (podspec change); the idempotent check above ensures it happens
		// at most once per hive.
		pctx, pcancel := context.WithTimeout(context.Background(), netAdminKubectlTimeout)
		patchCmd := kubectlForClusterContext(pctx, cluster, "patch", "deployment", "hive",
			"-n", ns, "--type=json", "-p", netAdminPatchJSON())
		pout, perr := patchCmd.CombinedOutput()
		pcancel()
		if perr != nil {
			s.markClusterUnreachable(cluster.ID)
			s.logger.Warn("netadmin reconcile: patch failed — will retry next sweep",
				"hive_id", h.ID, "cluster", cluster.ID,
				"output", strings.TrimSpace(string(pout)), "error", perr)
			continue
		}
		s.markClusterReachable(cluster.ID)
		s.logger.Info("reconciled NET_ADMIN onto hive deployment",
			"hive_id", h.ID, "cluster", cluster.ID)
	}

	// A sweep that admitted no hives at all is a BUG signal, not a quiet no-op:
	// the registry is never legitimately empty on a hub that hosts spokes. Log
	// it at Warn so the condition that made this lane dead code for its whole
	// production life cannot recur silently. Unlike its per-hive-env sibling
	// this sweep keeps no observation state, so the log IS the surface.
	if consideredHives == 0 && len(hives) > 0 {
		s.logger.Warn("netadmin reconcile: status filter selected NO hives — sweep is a no-op, NET_ADMIN drift will not be repaired",
			"hives_in_registry", len(hives), "skipped_by_status", skippedByStatus)
	}
}
