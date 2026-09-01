package hub

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Orphaned Terminating-pod reaper (issue #5328).
//
// THE CONDITION. A hosted spoke pod acquires a deletionTimestamp and then never
// goes away. A live sweep of one cluster found 27 such pods across 15 hive
// namespaces, the oldest stuck for three weeks; a sixteenth namespace held 5
// more, for 32 cleared by hand. Every one carried the SAME signature:
//
//   - deletionTimestamp set — the API server recorded the delete;
//   - finalizers: [] — nothing was legitimately holding the object;
//   - status.phase != "Running" — the pod was not serving;
//   - the namespace still had exactly 1 healthy Running pod, so the live spoke
//     was unaffected in every case.
//
// The ORIGINAL reading of that signature was "a node disappeared without
// draining": the API server records the deletion and waits for the kubelet to
// confirm it, the kubelet is gone, so confirmation never arrives, and with no
// finalizer and no owning controller the object persists forever. That reading
// was inferred from absent spec fields rather than measured, and MEASUREMENT
// HAS SINCE DISPROVEN IT. Recorded here because the wrong cause is the kind of
// thing that gets re-derived from the same signature by the next reader:
//
//   - No node was lost in the orphan window. Every node on the affected
//     cluster has been continuously Running with no Machine ever deleted, and
//     the newest node predates the oldest orphan by weeks. There is no
//     autoscaler and no spot/preemptible capacity in play.
//   - Orphan onsets arrive in tight BATCHES — several within the same second,
//     repeatedly, across ten separate days. Node loss would tie a batch to one
//     node; these batches span namespaces spread over many nodes.
//   - The largest batches land inside the hive AUTO-UPGRADE window (see
//     autoUpgradeDailyHour in upgrade_schedule.go), and per-namespace
//     ReplicaSet history shows one new ReplicaSet per namespace per day at
//     that same moment.
//
// The actual mechanism is ORDINARY ROLLING REDEPLOY, not node loss. The daily
// auto-upgrade re-applies each hosted spoke Deployment; the RollingUpdate
// replaces the pod; and occasionally the outgoing pod's delete is never
// confirmed, leaving exactly this signature. That is why the orphans recur on
// a healthy, static cluster, and why they concentrate in the namespaces that
// get redeployed most often.
//
// WHY A REAPER REMAINS THE RIGHT FIX. The trigger is a routine, deliberate,
// recurring operation that the fleet depends on — not an infrastructure fault
// to be engineered away. Slowing or suppressing redeploys to avoid a rare
// unconfirmed delete would trade a cosmetic accounting problem for a real loss
// of upgrade cadence. What makes the accumulation expensive is not any single
// orphan but that NOTHING sweeps them: the condition is self-perpetuating, so
// the count only ever grows between manual interventions, and it grew for
// three weeks with no alert. A sweep is the proportionate response, and it
// stays correct regardless of which delete goes unconfirmed or why.
//
// IMPACT. Orphans hold their scheduler slot until forcibly removed, and
// anything counting pods per namespace sees phantom replicas — a namespace
// showing 4 pods when 1 is live misreports fleet capacity and makes "is this
// spoke healthy" unanswerable from the dashboard.
//
// THE PREDICATE IS DELIBERATELY NARROW AND MUST NOT BE LOOSENED. See
// podIsOrphanedTerminating for the rule and the reasoning behind each clause.
// It is the exact predicate used for the manual remediation, verified against
// all 32 pods with zero collateral damage.
//
// WHY THE HUB CAN DO THIS WITHOUT NEW CREDENTIALS — AND THE ONE RBAC CAVEAT.
//
// This lane adds NO new credential, kubeconfig or client of any kind. It rides
// the existing kubectlForCluster path, which for a remote spoke cluster
// authenticates with the per-cluster kubeconfig the hub ALREADY holds — the
// same credential provisioning uses to issue strictly BROADER destructive
// operations against these clusters today: cleanupHiveResources runs
// `kubectl delete namespace` and `kubectl delete pv` (saas_provision.go ~2429,
// ~2441). Deleting one already-deleted pod inside one hive-hosted namespace is
// a strict NARROWING of blast radius relative to that.
//
// This is consistent with pullonly_upgrade.go's push-path retirement note,
// which retires the UPGRADE push path specifically and states plainly that the
// hub's kubeconfigs are NOT yet droppable, because provisioning (namespace,
// Deployment and RBAC creation on spoke clusters) is inherently a hub-side
// write. The reaper is that same class of hub-side write and rides that same
// existing grant. It does not extend the ordering constraint recorded there: if
// those kubeconfigs are ever dropped or downgraded to read-only, this sweep
// degrades exactly like the provisioning path and must move spoke-side with it.
//
// THE CAVEAT, STATED PLAINLY BECAUSE IT IS THE DRIFT CLASS THAT NOTE WARNS
// ABOUT. No manifest in this repo grants `pods` at all for the hub — the only
// in-repo pods rules belong to the separate hive-hub-backup ServiceAccount
// (deploy/k8s/backup-cronjob.yaml: pods get,list and pods/exec create, with NO
// delete). The hub nonetheless already runs `kubectl get pods
// --all-namespaces` (saas.go ~2627) successfully in production, so pod READ is
// present out-of-band on the live clusters. Pod DELETE is the same story: it
// comes from the kubeconfig's identity, not from anything this repo asserts.
//
// The consequence is bounded and non-silent BY CONSTRUCTION. If the credential
// lacks delete, `kubectl delete pod` fails, the failure is counted in
// deleteFailures and logged at Warn per pod, and the sweep retries next
// interval. It cannot fail closed-and-quiet, which is the specific failure mode
// the pullonly note calls worse than failing loudly. What this lane does NOT do
// is paper over that by minting a new grant: adding a cluster-wide pods-delete
// ClusterRole to close a gap the hub already operates through would broaden the
// blast radius on every cluster to fix a reporting problem on one.
//
// SCOPE OF THE VERBS USED: `get pods -n <hive-hosted-ns>` and
// `delete pod <name> -n <hive-hosted-ns>`. Both are namespace-scoped. Nothing
// here lists or deletes cluster-wide.
//
// PULL-ONLY CLUSTERS ARE SKIPPED, NOT FAILED. A cluster the hub cannot reach
// with kubectl is left alone via KubectlReachable, the same guard every other
// hub-side kubectl caller uses. Its orphans are not reaped from here; that is a
// known and accepted gap, and the sweep counters make it visible rather than
// silent.

const (
	// orphanedPodReapInterval throttles the sweep. Orphan production is a rare
	// cluster-level event, not a hot path — the measured accumulation was ~32
	// pods over three weeks — so a generous interval keeps this off the
	// per-tick critical path. The SHA poller ticks every 2 min; this sweep runs
	// at most once per this window. Matches netAdminReconcileInterval, the
	// sibling remediation sweep it is modelled on.
	orphanedPodReapInterval = 15 * time.Minute

	// orphanedPodMinAge is how long a pod must have carried its
	// deletionTimestamp before the reaper will touch it.
	//
	// A normal pod termination completes in SECONDS: the kubelet stops the
	// containers, honours terminationGracePeriodSeconds (30s by default), and
	// confirms. An hour is therefore ~120x the normal graceful path and orders
	// of magnitude beyond any legitimate slow shutdown, while being three weeks
	// short of the observed accumulation. The margin is deliberate: this bound
	// exists so a pod that is merely mid-shutdown — including one with a long
	// custom grace period — is never mistaken for an orphan. Being late costs
	// one extra sweep interval; being early force-deletes a pod that was about
	// to finish on its own.
	orphanedPodMinAge = time.Hour

	// orphanedPodKubectlTimeout bounds each per-namespace kubectl get/delete so
	// one unreachable cluster cannot stall the whole sweep. Mirrors
	// netAdminKubectlTimeout.
	orphanedPodKubectlTimeout = 30 * time.Second

	// orphanedPodMaxDeletesPerCycle caps how many pods one sweep will delete.
	//
	// A correct sweep on the measured fleet deletes ~32 pods ONCE and then
	// nothing. A sweep trying to delete hundreds means either the predicate has
	// been loosened by mistake or the cluster is in an unexpected state, and in
	// both cases stopping to be noticed beats grinding through. The remainder
	// is picked up next interval, so the cap delays cleanup rather than
	// abandoning it — orphans are inert, and the condition took three weeks to
	// matter.
	orphanedPodMaxDeletesPerCycle = 50
)

// orphanedPodCandidate is the subset of pod metadata the predicate needs. It
// exists so the decision is made over PLAIN DATA rather than over kubectl
// output, which is what makes the rule unit-testable in isolation.
type orphanedPodCandidate struct {
	Namespace         string
	Name              string
	DeletionTimestamp time.Time
	Finalizers        []string
	Phase             string
}

// podPhaseRunning is the pod phase that must never be force-deleted, spelled
// once so the predicate and its tests cannot disagree on the literal.
const podPhaseRunning = "Running"

// podIsOrphanedTerminating reports whether a pod matches the orphaned-
// Terminating signature and may be force-deleted.
//
// THE RULE — all four clauses must hold:
//
//	deletionTimestamp != null &&
//	  finalizers == [] &&
//	  phase != "Running" &&
//	  age(deletionTimestamp) > orphanedPodMinAge
//
// This is the predicate that cleared 32 pods by hand with zero collateral
// damage. Each clause is load-bearing and NONE may be relaxed:
//
//   - deletionTimestamp != null. Without it the pod was never asked to go away
//     and deleting it would be destroying a live workload, not cleaning up
//     after one. This clause is what makes the whole operation a completion of
//     an already-issued delete rather than a new delete.
//
//   - finalizers == []. A finalizer is an explicit statement that some
//     controller has unfinished work — volume detach, network cleanup,
//     external deregistration. Force-deleting past one strands exactly the
//     resource the finalizer existed to reclaim, and the damage is silent and
//     usually unrecoverable. Every one of the 32 observed orphans had an EMPTY
//     finalizer list, which is precisely why they were safe and precisely why
//     nothing was ever going to clean them up. A pod with finalizers is stuck
//     for a DIFFERENT reason and is out of scope for this lane.
//
//   - phase != "Running". A Running pod carrying a deletionTimestamp is a pod
//     mid-shutdown that is still serving traffic. It is the normal, healthy
//     path and it resolves on its own. Reaping it would be an outage caused by
//     the cleanup tool. Note every namespace in the measured incident kept
//     exactly one Running pod — that pod is the live spoke, and this clause is
//     what guarantees the reaper cannot touch it.
//
//   - age > orphanedPodMinAge. Termination normally completes in seconds, so
//     any pod inside the window is presumed to be shutting down correctly. See
//     orphanedPodMinAge.
//
// now is passed in rather than read from the clock so age is testable.
func podIsOrphanedTerminating(p orphanedPodCandidate, now time.Time, minAge time.Duration) bool {
	// Not deleted at all — a live pod. Never touch it.
	if p.DeletionTimestamp.IsZero() {
		return false
	}
	// Something owns unfinished work here. Out of scope, and unsafe.
	if len(p.Finalizers) > 0 {
		return false
	}
	// Still serving. Shutting down normally, or genuinely live.
	if strings.TrimSpace(p.Phase) == podPhaseRunning {
		return false
	}
	// Inside the grace window — presumed to be terminating correctly.
	return now.Sub(p.DeletionTimestamp) > minAge
}

// podListItem mirrors the fields of `kubectl get pods -o json` that the
// predicate consumes.
type podListItem struct {
	Metadata struct {
		Name              string   `json:"name"`
		Namespace         string   `json:"namespace"`
		DeletionTimestamp string   `json:"deletionTimestamp"`
		Finalizers        []string `json:"finalizers"`
	} `json:"metadata"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// parseOrphanCandidates turns the stdout of `kubectl get pods -n <ns> -o json`
// into candidates. Pure and fully testable: no kubectl, no cluster.
//
// A pod whose deletionTimestamp is absent or unparseable yields a ZERO
// DeletionTimestamp, which the predicate rejects. That is the safe direction —
// an unreadable timestamp can never be read as "old enough to delete".
func parseOrphanCandidates(raw []byte) ([]orphanedPodCandidate, error) {
	var list struct {
		Items []podListItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]orphanedPodCandidate, 0, len(list.Items))
	for _, it := range list.Items {
		c := orphanedPodCandidate{
			Namespace:  it.Metadata.Namespace,
			Name:       it.Metadata.Name,
			Finalizers: it.Metadata.Finalizers,
			Phase:      it.Status.Phase,
		}
		if ts := strings.TrimSpace(it.Metadata.DeletionTimestamp); ts != "" {
			if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
				c.DeletionTimestamp = parsed
			}
			// On a parse error the zero value stands and the predicate skips
			// the pod. Deliberate: never guess an age.
		}
		out = append(out, c)
	}
	return out, nil
}

// selectOrphanedPods applies the predicate across a parsed pod list. Split out
// from the sweep so the selection step is testable as a unit.
func selectOrphanedPods(candidates []orphanedPodCandidate, now time.Time, minAge time.Duration) []orphanedPodCandidate {
	var orphans []orphanedPodCandidate
	for _, c := range candidates {
		if podIsOrphanedTerminating(c, now, minAge) {
			orphans = append(orphans, c)
		}
	}
	return orphans
}

// reapOrphanedPodsIfDue runs the sweep only if orphanedPodReapInterval has
// elapsed. Safe to call from the poller loop every tick. Same shape and same
// guarding mutex as reconcileNetAdminIfDue — poller-loop-only state.
func (s *HubServer) reapOrphanedPodsIfDue() {
	s.clusterUnreachableMu.Lock()
	due := s.lastOrphanedPodReap.IsZero() ||
		time.Since(s.lastOrphanedPodReap) >= orphanedPodReapInterval
	if due {
		s.lastOrphanedPodReap = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	s.reapOrphanedPods()
}

// reapOrphanedPods sweeps every hub-managed hosted hive namespace and force-
// deletes pods matching podIsOrphanedTerminating.
//
// SCOPED TO HIVE NAMESPACES ONLY. Every kubectl call is `-n <hive-hosted-ID>`,
// derived from the registry — the sweep never lists or deletes cluster-wide and
// cannot reach a namespace the hub did not provision. There is no all-
// namespaces path in this file by construction.
//
// Idempotent and non-fatal: a namespace with no orphans is a no-op, and any
// kubectl error is logged and retried next sweep.
func (s *HubServer) reapOrphanedPods() {
	hives := listSaaSHives()
	now := time.Now()

	// Sweep accounting. A reaper that silently deletes nothing is
	// indistinguishable from a clean fleet, and a reaper that silently deletes
	// a lot is the failure mode worth catching early. Both are recorded and
	// logged, so "the sweep ran and found nothing" and "the sweep never
	// selected anybody" are different, readable outcomes. Trading one invisible
	// problem for another is the thing this lane exists to avoid.
	namespacesScanned := 0
	namespacesSkippedUnreachable := 0
	orphansFound := 0
	orphansDeleted := 0
	deleteFailures := 0
	capped := false

	for _, h := range hives {
		if orphansDeleted >= orphanedPodMaxDeletesPerCycle {
			capped = true
			break
		}

		cluster := s.clusterForHive(&h)
		if cluster == nil {
			continue
		}
		// A pull-only cluster is reached only by answering its outbound
		// heartbeat; the hub has no kubectl path into it. Skip rather than
		// burn a timeout, and count it so the gap is visible.
		if !cluster.KubectlReachable() {
			namespacesSkippedUnreachable++
			continue
		}
		// Skip clusters the hub just failed to dial, the same suppression the
		// upgrade and NET_ADMIN paths use, so one down cluster does not cost a
		// timeout per hive every sweep. Recovers on the next sweep after TTL.
		if s.clusterRecentlyUnreachable(cluster.ID) {
			namespacesSkippedUnreachable++
			continue
		}

		// Canonical derivation — the hub never enumerates namespaces, it
		// derives each one from the registry. This is what confines the sweep
		// to hive-managed namespaces.
		ns := hostedNamespaceForHive(&h)
		if ns == "" {
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), orphanedPodKubectlTimeout)
		out, err := kubectlForClusterContext(ctx, cluster, "get", "pods", "-n", ns, "-o", "json").Output()
		cancel()
		if err != nil {
			// Namespace missing, cluster unreachable, or a transient kubectl
			// error — all non-fatal. Debug, and the next sweep retries.
			s.logger.Debug("orphaned-pod reap: could not list pods",
				"hive_id", h.ID, "cluster", cluster.ID, "namespace", ns, "error", err)
			continue
		}
		s.markClusterReachable(cluster.ID)
		namespacesScanned++

		candidates, perr := parseOrphanCandidates(out)
		if perr != nil {
			s.logger.Warn("orphaned-pod reap: could not parse pod list",
				"hive_id", h.ID, "cluster", cluster.ID, "namespace", ns, "error", perr)
			continue
		}

		for _, orphan := range selectOrphanedPods(candidates, now, orphanedPodMinAge) {
			orphansFound++
			if orphansDeleted >= orphanedPodMaxDeletesPerCycle {
				capped = true
				break
			}

			age := now.Sub(orphan.DeletionTimestamp)

			dctx, dcancel := context.WithTimeout(context.Background(), orphanedPodKubectlTimeout)
			dout, derr := kubectlForClusterContext(dctx, cluster, "delete", "pod", orphan.Name,
				"-n", ns, "--force", "--grace-period=0", "--ignore-not-found").CombinedOutput()
			dcancel()
			if derr != nil {
				deleteFailures++
				s.logger.Warn("orphaned-pod reap: force-delete failed — will retry next sweep",
					"hive_id", h.ID, "cluster", cluster.ID, "namespace", ns,
					"pod", orphan.Name, "age", age.String(),
					"output", strings.TrimSpace(string(dout)), "error", derr)
				continue
			}
			orphansDeleted++
			// Every deletion is logged at INFO with namespace, name and age.
			// Silent reaping would trade one invisible problem for another:
			// the whole cost of this incident was that nothing was watching.
			s.logger.Info("reaped orphaned terminating pod",
				"namespace", ns, "pod", orphan.Name, "age", age.String(),
				"hive_id", h.ID, "cluster", cluster.ID)
		}
	}

	// Per-sweep summary. Logged at INFO only when the sweep actually did
	// something, so a healthy fleet stays quiet and a reaping sweep is
	// countable in the logs without reconstructing it from per-pod lines.
	if orphansFound > 0 || deleteFailures > 0 {
		s.logger.Info("orphaned-pod reap sweep complete",
			"namespaces_scanned", namespacesScanned,
			"orphans_found", orphansFound,
			"orphans_deleted", orphansDeleted,
			"delete_failures", deleteFailures,
			"namespaces_skipped_unreachable", namespacesSkippedUnreachable,
			"capped", capped)
	} else {
		s.logger.Debug("orphaned-pod reap sweep complete — no orphans",
			"namespaces_scanned", namespacesScanned,
			"namespaces_skipped_unreachable", namespacesSkippedUnreachable)
	}

	// Hitting the cap means either the predicate is wrong or the cluster is in
	// an unexpected state. Both deserve to be loud.
	if capped {
		s.logger.Warn("orphaned-pod reap hit the per-cycle delete cap — remainder deferred to next sweep",
			"cap", orphanedPodMaxDeletesPerCycle, "orphans_deleted", orphansDeleted)
	}

	// The registry is never legitimately empty on a hub that hosts spokes. A
	// sweep that scanned nothing while hives exist is a bug signal, not a quiet
	// no-op — the same failure mode that made the NET_ADMIN lane dead code for
	// its entire production life.
	if namespacesScanned == 0 && len(hives) > 0 {
		s.logger.Warn("orphaned-pod reap scanned NO namespaces — sweep is a no-op, orphaned pods will not be reaped",
			"hives_in_registry", len(hives),
			"namespaces_skipped_unreachable", namespacesSkippedUnreachable)
	}
}
