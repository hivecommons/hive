package hub

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// Leaked hosted-namespace janitor (issue #5768).
//
// THE CONDITION. A hosted-spoke provisioning attempt creates its Kubernetes
// namespace and then fails, and the namespace is never removed. A live sweep
// of one shared CI cluster found ~76 stuck pods — 64 Unschedulable and 12
// Pending on unbound PVCs — spread across dozens of leaked
// hive-hosted-hosted-* namespaces, none of which had any corresponding entry
// in the hub's registry.
//
// THE MECHANISM, precisely. `kubectl apply -f` is NOT transactional, and the
// Namespace is the FIRST object in k8sManifestTemplate. When a later object in
// that same manifest fails to apply — a PVC the cluster cannot bind, a
// Deployment rejected by admission, an Ingress class that does not exist — the
// namespace and every object that applied before the failing one are ALREADY
// on the cluster. provisionHive then returns an error, and both of its callers
// (handleCreateHive in saas.go and the lite-enrollment path in
// lite_enrollment.go) do exactly one thing with it: set Status="error" on the
// hive record and return. Neither deletes anything.
//
// WHY NOTHING ELSE EVER CLEANED IT UP. deprovisionHive is a complete and
// correct teardown — namespace, PV, OCI export, OCI file system, hive record,
// quota — but it is reachable ONLY from the user-initiated delete handler. A
// hive that never finished provisioning is not something a user is offered to
// delete, so the one function that could have removed the namespace was never
// called for precisely the hives that leaked it. The failure path had no
// teardown at all; this was not a delete racing a terminating PVC, and not a
// test harness forgetting a cleanup hook.
//
// THE PRIMARY FIX IS THE TEARDOWN, NOT THIS SWEEP. provisionHive now calls
// teardownPartialProvision on its failure path, which is what actually closes
// the hole. This janitor exists because that teardown is itself a network call
// against a cluster that just demonstrated it is unhealthy: the delete can
// fail, the hub can be killed between the failed apply and the delete, and
// neither case may leave residue that accumulates silently forever. A sweep is
// the backstop that makes the class self-correcting rather than dependent on
// every failure path being reached.
//
// THE INVARIANT: THIS SWEEP NEVER TOUCHES AN UNLABELLED NAMESPACE.
//
// Selection is by LABEL, never by name prefix. A namespace is eligible only if
// it carries hostedNamespaceEphemeralLabel="true" — a label this hub stamps
// itself, at creation, in the provisioning manifest. Matching on the
// "hive-hosted-" name prefix instead would be the dangerous design: an
// operator's hand-made hive-hosted-scratch namespace, a namespace restored
// from backup, or anything else that merely happens to share the prefix would
// become deletable by a background loop. The label is an explicit statement by
// the creator that the namespace is disposable, and nothing else in this file
// may be used as a substitute for it. See namespaceIsReapableLeak.
//
// This is the guard that must fail loudly if it is ever broken, and it is
// tested as a property rather than by example.
//
// THREE MORE GUARDS, each independently sufficient to spare a namespace:
//
//   - REGISTRY CROSS-CHECK. A namespace whose hive_id has a live entry in the
//     hub registry is a RUNNING SPOKE and is never eligible, whatever its
//     labels say. Every hosted namespace carries the ephemeral label,
//     including the ones that provisioned successfully, so this check — not
//     the label — is what separates "leaked" from "in service".
//   - AGE. Nothing younger than hostedNamespaceLeakMinAge is touched, so a
//     provisioning attempt that is still in flight, or one whose registry
//     write has not landed yet, cannot be reaped out from under itself.
//   - DRY RUN. The sweep can be run in report-only mode (see
//     hostedNamespaceJanitorDryRun) to be validated against a real cluster
//     before it is ever permitted to delete.
//
// WHERE IT RUNS. Inside the existing SHA-poller reconciliation lane, throttled
// internally like every sibling sweep (reconcileNetAdminIfDue,
// reapOrphanedPodsIfDue, replenishPoolsIfDue). No new goroutine and no new
// scheduler: the hub already has periodic machinery, and a leak that took
// weeks to matter does not warrant its own loop.
//
// CREDENTIALS. No new credential, kubeconfig or client. This rides the same
// kubectlForCluster path provisioning already uses, and issues a strict subset
// of what deprovisionHive issues against these same clusters today (`kubectl
// delete namespace`). Pull-only clusters are skipped, not failed, exactly as
// the orphaned-pod reaper skips them.

const (
	// hostedNamespaceOwnerLabel names what created the namespace. Stamped at
	// creation in k8sManifestTemplate so it is present from the namespace's
	// first moment — a label applied by a follow-up kubectl call would be
	// missing on exactly the failed-provision namespaces this exists to find.
	hostedNamespaceOwnerLabel = hiveLabelPrefix + "owner"

	// hostedNamespaceOwnerValue identifies the hub provisioner as the creator.
	hostedNamespaceOwnerValue = "provisioner"

	// hostedNamespaceEphemeralLabel marks a namespace as hub-disposable. It is
	// the ONLY selector this janitor may use to decide eligibility; see the
	// invariant above.
	hostedNamespaceEphemeralLabel = hiveLabelPrefix + "ephemeral"

	// hostedNamespaceCreatedAtAnnotation records when the hub created the
	// namespace, in RFC3339. Preferred over the namespace's own
	// metadata.creationTimestamp because it survives as OUR record of the
	// provisioning attempt; creationTimestamp is used as the fallback when
	// this is absent or unparseable.
	hostedNamespaceCreatedAtAnnotation = hiveLabelPrefix + "created-at"

	// hostedNamespaceJanitorDryRunEnv, when set to a true-ish value, makes the
	// sweep report what it WOULD delete and delete nothing. It exists so the
	// selection can be validated against a real cluster's real namespace list
	// before the reaper is trusted to act — a sweep that deletes namespaces is
	// not something to debug in production for the first time.
	hostedNamespaceJanitorDryRunEnv = "HIVE_HUB_NS_JANITOR_DRY_RUN"

	// hostedNamespaceLeakSweepInterval throttles the sweep. Leaks are produced
	// only by failed provisioning — rare, and inert once produced — so a
	// generous interval keeps this off the per-tick path. Matches
	// orphanedPodReapInterval, the sibling sweep this is modelled on.
	hostedNamespaceLeakSweepInterval = 15 * time.Minute

	// hostedNamespaceLeakMinAge is how long a labelled namespace with no
	// registry entry must have existed before it may be reaped.
	//
	// Provisioning is bounded by provisionTimeout (5 minutes), and the registry
	// write follows it. Six hours is therefore ~72x the longest legitimate
	// window in which a namespace can correctly exist without a registry entry,
	// while being far shorter than the weeks over which the observed leak
	// accumulated. The margin is deliberately large in the SAFE direction:
	// being late costs one sweep interval, being early deletes a namespace
	// whose spoke was about to come up.
	hostedNamespaceLeakMinAge = 6 * time.Hour

	// hostedNamespaceLeakKubectlTimeout bounds each kubectl list/delete so one
	// slow cluster cannot stall the sweep. Mirrors orphanedPodKubectlTimeout.
	hostedNamespaceLeakKubectlTimeout = 30 * time.Second

	// hostedNamespaceMaxDeletesPerCycle caps deletions per sweep.
	//
	// A correct sweep clears the accumulated backlog over a few cycles and then
	// finds nothing. A sweep trying to delete hundreds means the predicate has
	// been loosened by mistake or the registry failed to load — and in both
	// cases stopping to be noticed beats grinding through. The remainder is
	// picked up next interval, so the cap delays cleanup rather than
	// abandoning it.
	hostedNamespaceMaxDeletesPerCycle = 20
)

// hostedNamespaceJanitorDryRun reports whether the sweep is in report-only
// mode. Read per sweep rather than cached at startup so an operator can flip
// it without a hub restart.
func hostedNamespaceJanitorDryRun() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(hostedNamespaceJanitorDryRunEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// hostedNamespaceCandidate is the subset of namespace metadata the predicate
// needs. It exists so the decision is made over PLAIN DATA rather than over
// kubectl output, which is what makes the invariant testable in isolation.
type hostedNamespaceCandidate struct {
	Name string
	// Labels as read from the cluster. The ephemeral label must be present and
	// "true" here for the namespace to be eligible — nothing else substitutes.
	Labels map[string]string
	// CreatedAt is the hub's own creation stamp when present, else the
	// namespace's metadata.creationTimestamp. Zero when neither could be
	// parsed, which the predicate treats as "not old enough" (never delete on
	// an unknown age).
	CreatedAt time.Time
	// Phase is the namespace status phase. A namespace already Terminating is
	// skipped: the delete has been issued and re-issuing it achieves nothing.
	Phase string
}

// namespacePhaseTerminating is the phase of a namespace whose delete is
// already in flight, spelled once so predicate and tests cannot disagree.
const namespacePhaseTerminating = "Terminating"

// namespaceIsReapableLeak reports whether a namespace is a leaked hosted
// namespace that may be deleted.
//
// THE RULE — all clauses must hold:
//
//	labels[ephemeral] == "true" &&
//	  hiveID(name) not present in the registry &&
//	  phase != "Terminating" &&
//	  age(CreatedAt) > minAge
//
// Each clause is load-bearing and NONE may be relaxed:
//
//   - labels[ephemeral] == "true". THE INVARIANT. This is the hub's own
//     statement, made at creation time, that the namespace is disposable.
//     Without it the namespace was created by someone else — an operator, a
//     restore, another tool — and deleting it destroys work this hub never
//     owned. Name-prefix matching is deliberately NOT a substitute: the prefix
//     describes what a namespace is called, the label describes who made it
//     and why. A namespace with no labels at all, or with the label set to
//     anything other than "true", is out of scope. This clause is what makes
//     the sweep safe to run unattended, and it is tested as a property over
//     arbitrary unlabelled inputs, not by example.
//
//   - no registry entry. Every hosted namespace carries the ephemeral label,
//     including every healthy running spoke, so the label alone would select
//     the entire fleet. The registry is the hub's record of which hives exist;
//     a namespace whose hive_id is in it is IN SERVICE and must never be
//     touched no matter how it is labelled or how old it is. This clause is
//     what distinguishes a leak from a spoke.
//
//   - phase != "Terminating". The delete already happened and Kubernetes is
//     working through finalizers. Re-issuing it does nothing useful and makes
//     the sweep's own counters lie about how much it reaped.
//
//   - age > minAge. A provisioning attempt in flight has a namespace and not
//     yet a registry entry — which is the leak signature exactly. Age is what
//     separates "leaked" from "still being created". See
//     hostedNamespaceLeakMinAge.
//
// registered is the set of hive IDs the hub currently knows about. now is
// passed in rather than read from the clock so age is testable.
func namespaceIsReapableLeak(ns hostedNamespaceCandidate, registered map[string]bool, now time.Time, minAge time.Duration) bool {
	// THE INVARIANT. Not ours — never touch it. Checked first so no other
	// clause can be read as having authorised a delete.
	if ns.Labels[hostedNamespaceEphemeralLabel] != "true" {
		return false
	}
	// A live spoke. Labelled, possibly ancient, and absolutely not a leak.
	if registered[hiveIDFromHostedNamespace(ns.Name)] {
		return false
	}
	// Delete already in flight.
	if strings.TrimSpace(ns.Phase) == namespacePhaseTerminating {
		return false
	}
	// Unknown or recent creation time — presumed to be provisioning correctly.
	if ns.CreatedAt.IsZero() {
		return false
	}
	return now.Sub(ns.CreatedAt) > minAge
}

// hiveIDFromHostedNamespace recovers a hive ID from its namespace name by
// stripping the hosted prefix. Returns "" for a name that does not carry the
// prefix, which cannot match any registry entry — so a namespace whose name
// the hub does not recognise is never treated as registered, and is spared or
// reaped purely on its label, age and phase.
func hiveIDFromHostedNamespace(name string) string {
	n := strings.TrimSpace(name)
	if !strings.HasPrefix(n, hiveHostedNamespacePrefix) {
		return ""
	}
	return strings.TrimPrefix(n, hiveHostedNamespacePrefix)
}

// namespaceListItem mirrors the fields of `kubectl get ns -o json` the
// predicate consumes.
type namespaceListItem struct {
	Metadata struct {
		Name              string            `json:"name"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
		CreationTimestamp string            `json:"creationTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// parseHostedNamespaceCandidates turns the stdout of `kubectl get ns -o json`
// into candidates. Pure and fully testable: no kubectl, no cluster.
//
// A namespace whose creation time is absent or unparseable yields a ZERO
// CreatedAt, which the predicate rejects. That is the safe direction — an
// unreadable timestamp can never be read as "old enough to delete".
func parseHostedNamespaceCandidates(raw []byte) ([]hostedNamespaceCandidate, error) {
	var list struct {
		Items []namespaceListItem `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := make([]hostedNamespaceCandidate, 0, len(list.Items))
	for _, it := range list.Items {
		c := hostedNamespaceCandidate{
			Name:   it.Metadata.Name,
			Labels: it.Metadata.Labels,
			Phase:  it.Status.Phase,
		}
		// Prefer the hub's own stamp; fall back to the namespace's
		// creationTimestamp for namespaces provisioned before it shipped.
		if ts := strings.TrimSpace(it.Metadata.Annotations[hostedNamespaceCreatedAtAnnotation]); ts != "" {
			if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
				c.CreatedAt = parsed
			}
		}
		if c.CreatedAt.IsZero() {
			if ts := strings.TrimSpace(it.Metadata.CreationTimestamp); ts != "" {
				if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
					c.CreatedAt = parsed
				}
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// selectLeakedHostedNamespaces applies the predicate across a parsed list.
// Split out from the sweep so selection is testable as a unit.
func selectLeakedHostedNamespaces(candidates []hostedNamespaceCandidate, registered map[string]bool, now time.Time, minAge time.Duration) []hostedNamespaceCandidate {
	var leaks []hostedNamespaceCandidate
	for _, c := range candidates {
		if namespaceIsReapableLeak(c, registered, now, minAge) {
			leaks = append(leaks, c)
		}
	}
	return leaks
}

// registeredHiveIDs returns the set of hive IDs the hub currently knows about,
// used as the "is this a live spoke" cross-check.
func registeredHiveIDs() map[string]bool {
	hives := listSaaSHives()
	set := make(map[string]bool, len(hives))
	for _, h := range hives {
		if id := strings.TrimSpace(h.ID); id != "" {
			set[id] = true
		}
	}
	return set
}

// sweepLeakedHostedNamespacesIfDue runs the sweep only if
// hostedNamespaceLeakSweepInterval has elapsed. Safe to call from the poller
// loop every tick. Same shape and same guarding mutex as
// reconcileNetAdminIfDue — poller-loop-only state.
func (s *HubServer) sweepLeakedHostedNamespacesIfDue() {
	s.clusterUnreachableMu.Lock()
	due := s.lastHostedNamespaceSweep.IsZero() ||
		time.Since(s.lastHostedNamespaceSweep) >= hostedNamespaceLeakSweepInterval
	if due {
		s.lastHostedNamespaceSweep = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	s.sweepLeakedHostedNamespaces()
}

// sweepLeakedHostedNamespaces deletes labelled ephemeral hosted namespaces
// that have no corresponding hub registry entry and are older than
// hostedNamespaceLeakMinAge.
//
// Idempotent and non-fatal: a cluster with no leaks is a no-op, and any
// kubectl error is logged and retried next sweep.
func (s *HubServer) sweepLeakedHostedNamespaces() {
	now := time.Now()
	dryRun := hostedNamespaceJanitorDryRun()

	// The registry snapshot is taken ONCE, before any cluster is contacted, so
	// every cluster in this sweep is judged against the same set. It is also
	// the guard against the worst failure mode available to this lane: if the
	// registry were empty because it failed to load, EVERY hosted namespace on
	// every cluster would look unregistered and therefore reapable. An empty
	// registry is never legitimate on a hub that hosts spokes, so the sweep
	// refuses to run rather than interpret it.
	registered := registeredHiveIDs()
	if len(registered) == 0 {
		s.logger.Warn("hosted-namespace janitor: registry is empty — skipping sweep rather than treating every namespace as leaked")
		return
	}

	// Sweep accounting. A janitor that silently deletes nothing is
	// indistinguishable from a clean fleet, and one that silently deletes a lot
	// is the failure mode worth catching early. Both are recorded.
	clustersScanned := 0
	clustersSkippedUnreachable := 0
	leaksFound := 0
	leaksDeleted := 0
	deleteFailures := 0
	capped := false

	for id := range s.clusters {
		if leaksDeleted >= hostedNamespaceMaxDeletesPerCycle {
			capped = true
			break
		}
		cluster := s.clusters[id]
		// A pull-only cluster is reached only by answering its outbound
		// heartbeat; the hub has no kubectl path into it. Skip rather than burn
		// a timeout, and count it so the gap is visible.
		if !cluster.KubectlReachable() {
			clustersSkippedUnreachable++
			continue
		}
		if s.clusterRecentlyUnreachable(cluster.ID) {
			clustersSkippedUnreachable++
			continue
		}

		// Server-side label selector: the API server returns ONLY namespaces
		// carrying the ephemeral label. The predicate re-checks the label
		// anyway — the selector is an optimisation and must never be the only
		// thing standing between this sweep and an unlabelled namespace.
		ctx, cancel := context.WithTimeout(context.Background(), hostedNamespaceLeakKubectlTimeout)
		out, err := kubectlForClusterContext(ctx, &cluster, "get", "namespaces",
			"-l", hostedNamespaceEphemeralLabel+"=true", "-o", "json").Output()
		cancel()
		if err != nil {
			s.logger.Debug("hosted-namespace janitor: could not list namespaces",
				"cluster", cluster.ID, "error", err)
			continue
		}
		s.markClusterReachable(cluster.ID)
		clustersScanned++

		candidates, perr := parseHostedNamespaceCandidates(out)
		if perr != nil {
			s.logger.Warn("hosted-namespace janitor: could not parse namespace list",
				"cluster", cluster.ID, "error", perr)
			continue
		}

		for _, leak := range selectLeakedHostedNamespaces(candidates, registered, now, hostedNamespaceLeakMinAge) {
			leaksFound++
			if leaksDeleted >= hostedNamespaceMaxDeletesPerCycle {
				capped = true
				break
			}
			age := now.Sub(leak.CreatedAt)

			if dryRun {
				// Report-only. Logged at INFO with everything a reviewer needs
				// to confirm the selection before enabling deletion.
				s.logger.Info("hosted-namespace janitor: DRY RUN — would delete leaked namespace",
					"namespace", leak.Name, "cluster", cluster.ID,
					"age", age.String(), "hive_id", hiveIDFromHostedNamespace(leak.Name))
				continue
			}

			// Cascading delete: removes the Deployment, Service, Ingress,
			// ConfigMap, Secret and — the point of this issue — the PVCs whose
			// unbound Pending pods were the observed symptom.
			dctx, dcancel := context.WithTimeout(context.Background(), hostedNamespaceLeakKubectlTimeout)
			dout, derr := kubectlForClusterContext(dctx, &cluster, "delete", "namespace",
				leak.Name, "--ignore-not-found", "--wait=false").CombinedOutput()
			dcancel()
			if derr != nil {
				deleteFailures++
				s.logger.Warn("hosted-namespace janitor: delete failed — will retry next sweep",
					"namespace", leak.Name, "cluster", cluster.ID, "age", age.String(),
					"output", strings.TrimSpace(string(dout)), "error", derr)
				continue
			}
			leaksDeleted++
			// Every deletion is logged at INFO with namespace, cluster and age.
			// Silent reaping would trade one invisible problem for another: the
			// whole cost of this incident was that nothing was watching.
			s.logger.Info("hosted-namespace janitor: reaped leaked hosted namespace",
				"namespace", leak.Name, "cluster", cluster.ID, "age", age.String(),
				"hive_id", hiveIDFromHostedNamespace(leak.Name))
		}
	}

	if leaksFound > 0 || deleteFailures > 0 {
		s.logger.Info("hosted-namespace janitor sweep complete",
			"clusters_scanned", clustersScanned,
			"leaks_found", leaksFound,
			"leaks_deleted", leaksDeleted,
			"delete_failures", deleteFailures,
			"clusters_skipped_unreachable", clustersSkippedUnreachable,
			"dry_run", dryRun,
			"capped", capped)
	} else {
		s.logger.Debug("hosted-namespace janitor sweep complete — no leaks",
			"clusters_scanned", clustersScanned,
			"clusters_skipped_unreachable", clustersSkippedUnreachable,
			"dry_run", dryRun)
	}

	// Hitting the cap means either the predicate is wrong or the cluster is in
	// an unexpected state. Both deserve to be loud.
	if capped {
		s.logger.Warn("hosted-namespace janitor hit the per-cycle delete cap — remainder deferred to next sweep",
			"cap", hostedNamespaceMaxDeletesPerCycle, "leaks_deleted", leaksDeleted)
	}
}
