package hub

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

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
		// A push-reported cluster is reached only by answering its outbound
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
