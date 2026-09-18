package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// hive-contribute Ingress reconcile — closes the pre-#7457 auth-url drift.
//
// #7457 fixed the 401 on /api/contribute/me (#7453) in two halves: code on the
// hub and the spoke, and two nginx annotations on the hive-contribute Ingress
// in k8sManifestTemplate — auth-url, so nginx asks the hub who is calling, and
// auth-response-headers, so the answer (X-Hive-User / X-Hive-Role /
// X-Hive-Proxy-Auth) is copied onto the request. The code half rolled out with
// the next image; the Ingress half is `kubectl apply`ed ONLY by provisionHive,
// so every hosted spoke provisioned before it merged still serves
// /api/contribute through an Ingress with no auth-url. nginx never asks, the
// identity never arrives, and /api/contribute/me answers 401 to the hive's own
// owner — the exact symptom #7453 was filed on, now on a spoke whose
// served_sha is well past the fix (hivecommons/hive#7517).
//
// This sweep is the missing re-apply: for every hosted spoke on an nginx
// cluster it reads the live hive-contribute Ingress and merge-patches the two
// annotations on when they are absent or stale. It follows the NET_ADMIN and
// per-hive-env reconciles (netadmin_reconcile.go, perhive_env_reconcile.go),
// with two differences worth naming: an annotation patch does not roll the
// spoke's pod, so there is no per-cycle cap; and the expected values are
// derived from the same inputs the template renders from (hubPublicURL and
// the hive ID), with a test that pins them equal to the rendered Ingress so
// the sweep and the template cannot disagree about what "converged" means.
//
// More generally (from the issue): a change to k8sManifestTemplate that alters
// an existing object reaches only spokes provisioned after it, unless it has
// a reconcile path like this one. src/docs/security-model.md says so.

const (
	// contributeIngressName is the Ingress that routes /api/contribute on a
	// hosted spoke (k8sManifestTemplate, `name: hive-contribute`).
	contributeIngressName = "hive-contribute"

	// The two nginx annotations #7457 added to that Ingress. Named here so
	// the sweep's check and its patch can never disagree about the keys.
	ingressAuthURLAnnotation             = "nginx.ingress.kubernetes.io/auth-url"
	ingressAuthResponseHeadersAnnotation = "nginx.ingress.kubernetes.io/auth-response-headers"

	// ingressAuthResponseHeaders is the header list every gated Ingress in the
	// template forwards; hive-contribute carries the same one.
	ingressAuthResponseHeaders = "X-Hive-User,X-Hive-Role,X-Hive-Proxy-Auth"

	// contributeIngressReconcileInterval throttles the sweep. The drift is
	// static — an Ingress either carries the annotations or it does not, and
	// a correctly provisioned one never loses them — so this is remediation,
	// not a hot path. Same window as the NET_ADMIN sweep.
	contributeIngressReconcileInterval = 15 * time.Minute

	// contributeIngressKubectlTimeout bounds each per-hive get/patch so one
	// unreachable cluster cannot stall the whole sweep.
	contributeIngressKubectlTimeout = 15 * time.Second
)

// contributeIngressAuthAnnotations is what the hive-contribute Ingress of
// hive `hiveID` must carry for /api/contribute/me to learn who is calling:
// the per-hive auth-url and the identity headers nginx copies back. These are
// the values k8sManifestTemplate renders from the same two inputs
// (TestContributeIngressReconcileMatchesTheTemplate pins that), so a spoke the
// sweep converges is indistinguishable from one provisioned after #7457.
func contributeIngressAuthAnnotations(hubURL, hiveID string) map[string]string {
	return map[string]string{
		ingressAuthURLAnnotation:             hubURL + "/api/saas/auth-check?hive=" + hiveID + "&uri=$request_uri",
		ingressAuthResponseHeadersAnnotation: ingressAuthResponseHeaders,
	}
}

// contributeIngressAnnotationPatch is the PURE reconcile decision: given the
// live Ingress as `kubectl get ingress -o json` prints it and the annotations
// it must carry, return the strategic-merge patch body that installs the
// missing or stale ones — or "" when the Ingress already carries every one,
// so a converged spoke is never patched. Only the annotations that differ are
// in the patch; a merge patch on metadata.annotations leaves every other key
// (cert-manager's issuer, the proxy timeouts, a vanity-host mirror's marks)
// untouched.
//
// An Ingress that does not parse is an error rather than a patch: patching
// blind onto an object the sweep could not read is how a typo becomes a
// fleet-wide outage.
func contributeIngressAnnotationPatch(raw []byte, want map[string]string) (string, error) {
	var obj struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("parsing ingress: %w", err)
	}
	drift := map[string]string{}
	for key, value := range want {
		if obj.Metadata.Annotations[key] != value {
			drift[key] = value
		}
	}
	if len(drift) == 0 {
		return "", nil
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": drift},
	})
	if err != nil {
		return "", fmt.Errorf("encoding patch: %w", err)
	}
	return string(body), nil
}

// contributeIngressSweepEligible reports whether a hive should be examined.
// The only status excluded is "provisioning": its Ingress is being applied
// from the template right now and is born with the annotations. Every other
// status — including the unwritten "" most steady-state hives carry, and
// "available" placeholders, which must be right BEFORE they are claimed — is
// swept. See netAdminSweepEligible for why this is not a "running" check.
func contributeIngressSweepEligible(status string) bool {
	return strings.TrimSpace(status) != "provisioning"
}

// reconcileContributeIngressIfDue runs the sweep only if
// contributeIngressReconcileInterval has elapsed. Safe to call from the
// poller loop every tick; same shape and same guarding mutex as
// reconcileNetAdminIfDue — poller-loop-only state.
func (s *HubServer) reconcileContributeIngressIfDue() {
	s.clusterUnreachableMu.Lock()
	due := s.lastContributeIngressReconcile.IsZero() ||
		time.Since(s.lastContributeIngressReconcile) >= contributeIngressReconcileInterval
	if due {
		s.lastContributeIngressReconcile = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	s.reconcileContributeIngress()
}

// reconcileContributeIngress sweeps every hub-managed hosted hive on an nginx
// cluster and, for any whose live hive-contribute Ingress is missing the
// auth-url or auth-response-headers annotation (or carries a stale value),
// merge-patches the template's values on. Idempotent — a converged Ingress is
// a Debug-level no-op — and non-fatal on kubectl errors, which are retried
// on the next sweep. An annotation patch does not restart anything: nginx
// re-reads the Ingress and starts asking the hub on the next request.
func (s *HubServer) reconcileContributeIngress() {
	hives := listSaaSHives()
	hubURL := hubPublicURL()

	// Sweep accounting, so "the filter selected nobody" and "the fleet is
	// converged" are different, readable outcomes (the NET_ADMIN sweep
	// shipped with a filter that selected nothing and nobody could tell).
	considered := 0
	skippedByStatus := 0
	skippedNoIngress := 0
	converged := 0
	patched := 0
	failures := 0

	for _, h := range hives {
		if !contributeIngressSweepEligible(h.Status) {
			skippedByStatus++
			continue
		}
		cluster := s.clusterForHive(&h)
		if cluster == nil {
			continue
		}
		// An OpenShift-Route cluster has no nginx Ingress to annotate: the
		// template renders Routes there and /api/contribute has no auth-proxy
		// at all (see the IsNginxIngress conditional in k8sManifestTemplate).
		if cluster.IngressType == ingressTypeOpenShiftRoute {
			skippedNoIngress++
			continue
		}
		// A pull-only cluster is reached only by answering its outbound
		// heartbeat; the hub has no kubectl path into it.
		if !cluster.KubectlReachable() {
			skippedNoIngress++
			continue
		}
		// Skip clusters the hub just failed to dial — the same suppression the
		// upgrade and NET_ADMIN paths use, so one down cluster does not cost a
		// timeout per hive every sweep. Recovers on the next sweep after TTL.
		if s.clusterRecentlyUnreachable(cluster.ID) {
			continue
		}
		considered++

		ns := hostedNamespaceForHive(&h)
		if ns == "" {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), contributeIngressKubectlTimeout)
		raw, err := kubectlForClusterContext(ctx, cluster, "get", "ingress", contributeIngressName,
			"-n", ns, "-o", "json").Output()
		cancel()
		if err != nil {
			// Ingress missing (a spoke that predates hive-contribute entirely,
			// or one mid-teardown), cluster unreachable, or a transient kubectl
			// error — all non-fatal. Debug, and the next sweep retries.
			s.logger.Debug("contribute ingress reconcile: could not read hive-contribute ingress",
				"hive_id", h.ID, "cluster", cluster.ID, "namespace", ns, "error", err)
			continue
		}
		s.markClusterReachable(cluster.ID)

		patch, perr := contributeIngressAnnotationPatch(raw, contributeIngressAuthAnnotations(hubURL, h.ID))
		if perr != nil {
			failures++
			s.logger.Warn("contribute ingress reconcile: could not parse hive-contribute ingress — not patching blind",
				"hive_id", h.ID, "cluster", cluster.ID, "namespace", ns, "error", perr)
			continue
		}
		if patch == "" {
			converged++
			s.logger.Debug("contribute ingress reconcile: hive-contribute already carries the auth-url",
				"hive_id", h.ID, "cluster", cluster.ID)
			continue
		}

		pctx, pcancel := context.WithTimeout(context.Background(), contributeIngressKubectlTimeout)
		pout, perr := kubectlForClusterContext(pctx, cluster, "patch", "ingress", contributeIngressName,
			"-n", ns, "--type", "merge", "-p", patch).CombinedOutput()
		pcancel()
		if perr != nil {
			failures++
			s.markClusterUnreachable(cluster.ID)
			s.logger.Warn("contribute ingress reconcile: patch failed — will retry next sweep",
				"hive_id", h.ID, "cluster", cluster.ID, "namespace", ns,
				"output", strings.TrimSpace(string(pout)), "error", perr)
			continue
		}
		s.markClusterReachable(cluster.ID)
		patched++
		s.logger.Info("reconciled auth-url onto hive-contribute ingress (#7517)",
			"hive_id", h.ID, "cluster", cluster.ID, "namespace", ns)
	}

	// A sweep that admitted no hives at all on a hub that hosts spokes is a
	// bug signal, not a quiet no-op — the condition that made the NET_ADMIN
	// lane dead code for its whole production life.
	if considered == 0 && len(hives) > 0 {
		s.logger.Warn("contribute ingress reconcile: selected NO hives — sweep is a no-op, /api/contribute/me stays 401 on drifted spokes",
			"hives_in_registry", len(hives), "skipped_by_status", skippedByStatus,
			"skipped_no_nginx_ingress", skippedNoIngress)
		return
	}
	// The per-sweep summary is Info only when the sweep changed or failed
	// something, so a converged fleet stays quiet.
	if patched > 0 || failures > 0 {
		s.logger.Info("contribute ingress reconcile sweep complete",
			"considered", considered, "converged", converged, "patched", patched,
			"failures", failures, "skipped_by_status", skippedByStatus,
			"skipped_no_nginx_ingress", skippedNoIngress)
	}
}
