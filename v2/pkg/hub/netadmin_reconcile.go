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

	// hiveContainerSecurityContextPath is the JSON-Patch path to the hive
	// container's securityContext. The hive container is index 0 in the
	// provisioning template (containers[0], name: hive).
	hiveContainerSecurityContextPath = "/spec/template/spec/containers/0/securityContext"
)

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
// A single `add` op at the securityContext path REPLACES the whole
// securityContext object (JSON-Patch `add` on an existing path overwrites it,
// and on a missing path creates it — so one op covers BOTH the drifted
// `securityContext: {}` case and the never-set case). This matches the shape
// verified working live via `kubectl patch` on kubestellar-console-4vkt and
// projectbluefin-knuckle-gjvq (issue #2674). We only ever call this after the
// idempotent check found NET_ADMIN missing, so it never issues a pointless
// rollout on an already-correct hive.
//
// NOTE: this DOES change the podspec, so applying it triggers a one-time
// rolling update — the intended correction. The idempotent has-NET_ADMIN check
// guarantees the patch is issued at most once (the next sweep sees NET_ADMIN
// present and skips), so it can never loop.
func netAdminPatchJSON() string {
	return `[{"op":"add","path":"` + hiveContainerSecurityContextPath +
		`","value":{"capabilities":{"add":["` + netAdminCapability + `"]}}}]`
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
	for _, h := range hives {
		// Only hives with a live Deployment on a resolvable cluster. A hive
		// still provisioning has no stable deployment to reconcile yet, and one
		// in error/deprovisioned state may have none at all — both are picked up
		// on a later sweep once running. This mirrors how triggerAutoUpgrades
		// resolves the cluster before touching a hive.
		if h.Status != "running" {
			continue
		}
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
}
