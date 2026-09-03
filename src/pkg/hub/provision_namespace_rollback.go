package hub

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// Hosted-namespace ROLLBACK on a failed provision (issue #5768, ask 1 — the
// production side of the leak).
//
// THE GAP. provisionHive renders one manifest containing the whole hosted
// spoke — Namespace, Deployment, Service, ConfigMap, Secret, PVC, Route — and
// applies it with a single `kubectl apply -f`. kubectl applies the objects in
// that file IN ORDER, and the Namespace is necessarily first, because every
// other object in the manifest is namespaced into it. So when the apply fails
// PARTWAY — an admission webhook rejects the Deployment, the PVC's
// StorageClass is missing, the quota is exhausted, the Route host collides —
// the namespace has ALREADY been created and kubectl's non-zero exit rolls
// back nothing. provisionHive then returns an error, the caller marks the hive
// record "error" (saas.go ~4118, lite_enrollment.go ~370), and the namespace
// stays on the cluster forever with whatever partial objects preceded the
// failure.
//
// That is a leak with no owner. Nothing retries the apply, nothing deprovisions
// an errored hive, and the registry-derived sweeps cannot see the namespace
// once the record is cleaned up or the placeholder is recycled. It is one of
// the two ways a cluster ends up holding dozens of `hive-hosted-hosted-*`
// namespaces with unbound PVCs and unschedulable pods, which is what #5768
// measured.
//
// WHY THE ROLLBACK IS SAFE HERE AND A FLEET JANITOR IS NOT. Deleting a
// namespace cascades to its PVCs and kills anything running in it — the widest
// destructive verb the hub owns. What makes it defensible at THIS call site,
// and only here, is that the hub can PROVE the namespace was not there a
// moment ago:
//
//   - hostedNamespaceExistedBeforeApply runs `kubectl get namespace <ns>
//     --ignore-not-found` immediately BEFORE the apply. With that flag kubectl
//     exits 0 and prints nothing for an absent namespace, so "absent" and
//     "could not tell" are distinguishable — a plain `get` conflates them into
//     one non-zero exit.
//   - Only an ABSENT-then-failed sequence rolls back. If the namespace already
//     existed, this is a re-apply over a live or previously-provisioned spoke
//     and the delete would destroy exactly what it was meant to protect.
//   - If the pre-check itself errored, NOTHING is deleted. "I could not tell"
//     must never be resolved in the direction of a delete; the namespace is
//     left for the read-only detector (leaked_hosted_namespace.go) and a human.
//
// The window between the pre-check and the apply is a genuine TOCTOU: another
// actor could create the namespace in between, and this would then delete it.
// It is accepted because the only writer of `hive-hosted-<id>` namespaces is
// the hub itself, provisioning for one hive id is serialized through
// enqueueProvision (provision_queue.go), and the id is minted fresh for the
// hive being provisioned. The alternative — no rollback — is the leak this
// issue exists about.
//
// BEST-EFFORT AND LOUD. A failed rollback never changes what provisionHive
// returns to its caller; the provision has already failed and the error the
// admin sees must stay the ORIGINAL failure, not a cleanup failure layered over
// it. Every outcome is logged, including the two non-delete decisions, because
// a rollback that silently declines to run is indistinguishable from one that
// ran — which is the failure mode the leak detector had to be written to catch
// in the first place.

// provisionRollbackTimeout bounds each kubectl call in the rollback path.
//
// Matches stampNamespaceIdentityTimeout: both are single synchronous kubectl
// calls on the provisioning path, and both must fail fast against an
// unreachable cluster rather than hold a queued provision slot (or, in tests
// with no kubectl on PATH, hold the test) for kubectl's own default timeout.
const provisionRollbackTimeout = 15 * time.Second

// namespacePresence is the tri-state result of the pre-apply existence check.
// A boolean cannot carry it: "absent" and "unknown" must drive different
// behaviour, and collapsing them is precisely what would turn an unreachable
// cluster into a namespace delete.
type namespacePresence int

const (
	// namespacePresenceUnknown means the check itself failed — no kubectl, an
	// unreachable API server, an RBAC denial. Never roll back on this.
	namespacePresenceUnknown namespacePresence = iota
	// namespacePresenceAbsent means kubectl succeeded and reported nothing: the
	// namespace did not exist before the apply.
	namespacePresenceAbsent
	// namespacePresentBeforeApply means the namespace already existed and is
	// therefore not ours to delete on a failed apply.
	namespacePresentBeforeApply
)

// hostedNamespaceExistedBeforeApply reports whether a hosted namespace exists
// on the cluster right now, distinguishing "no" from "could not tell".
//
// Uses `get namespace <ns> --ignore-not-found -o name`: with --ignore-not-found
// kubectl exits 0 and prints NOTHING when the namespace is absent, and prints
// "namespace/<ns>" when it is present. A non-zero exit therefore means the
// check failed rather than that the namespace is missing — the distinction the
// whole rollback decision rests on.
func hostedNamespaceExistedBeforeApply(cluster *ClusterConfig, namespace string) namespacePresence {
	if cluster == nil || strings.TrimSpace(namespace) == "" {
		return namespacePresenceUnknown
	}
	if !cluster.KubectlReachable() {
		return namespacePresenceUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), provisionRollbackTimeout)
	defer cancel()
	out, err := kubectlForClusterContext(ctx, cluster,
		"--request-timeout", provisionRollbackTimeout.String(),
		"get", "namespace", namespace, "--ignore-not-found", "-o", "name").Output()
	if err != nil {
		return namespacePresenceUnknown
	}
	if strings.TrimSpace(string(out)) == "" {
		return namespacePresenceAbsent
	}
	return namespacePresentBeforeApply
}

// rollbackProvisionNamespace deletes the hosted namespace a failed provision
// just created — and ONLY that case.
//
// before is the presence recorded by hostedNamespaceExistedBeforeApply BEFORE
// the apply ran. The three cases and why each behaves as it does are argued in
// the file comment above; the short version:
//
//	absent  -> delete (we created it, nothing else can be using it)
//	present -> keep   (pre-existing; deleting it destroys a live spoke)
//	unknown -> keep   (never resolve "could not tell" into a delete)
//
// The delete uses --wait=false so a namespace whose termination is slow (a
// finalizer on a PVC can hold one for minutes) does not pin the provisioning
// queue slot; the API server proceeds with the cascade on its own. It uses
// --ignore-not-found so the common case — the apply failed before creating
// anything at all — is a clean no-op rather than a spurious error line.
//
// Returns whether a delete was issued, purely so tests can assert the decision
// rather than infer it from a log line. Callers on the provisioning path ignore
// it: a failed rollback must not change the error the admin sees.
func rollbackProvisionNamespace(cluster *ClusterConfig, namespace string, before namespacePresence, logger *slog.Logger) bool {
	if cluster == nil || strings.TrimSpace(namespace) == "" {
		return false
	}

	switch before {
	case namespacePresentBeforeApply:
		if logger != nil {
			logger.Info("provision rollback skipped: namespace existed before this apply — not ours to delete",
				"namespace", namespace, "cluster", cluster.ID)
		}
		return false
	case namespacePresenceUnknown:
		if logger != nil {
			// Loud, because this is the branch that leaves a namespace behind.
			// The read-only detector in leaked_hosted_namespace.go is what
			// eventually surfaces it; saying so here is what connects the two
			// for whoever reads this line.
			logger.Warn("provision rollback skipped: could not determine whether the namespace pre-existed — leaving it in place for the leaked-namespace report",
				"namespace", namespace, "cluster", cluster.ID)
		}
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), provisionRollbackTimeout)
	defer cancel()
	out, err := kubectlForClusterContext(ctx, cluster,
		"--request-timeout", provisionRollbackTimeout.String(),
		"delete", "namespace", namespace, "--ignore-not-found", "--wait=false").CombinedOutput()
	if err != nil {
		if logger != nil {
			logger.Warn("provision rollback: namespace delete failed — namespace may be leaked",
				"namespace", namespace, "cluster", cluster.ID,
				"output", strings.TrimSpace(string(out)), "error", err)
		}
		return true
	}
	if logger != nil {
		logger.Info("provision rollback: deleted the namespace created by a failed provision",
			"namespace", namespace, "cluster", cluster.ID)
	}
	return true
}
