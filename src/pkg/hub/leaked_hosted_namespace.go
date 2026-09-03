package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// Leaked hosted-namespace DETECTOR (issue #5768, ask 3).
//
// THE CONDITION. A console Live Promote canary on 2026-09-03 read 76 pod issues
// on one CI cluster — 64 Unschedulable and 12 Pending on unbound PVCs — spread
// across dozens of `hive-hosted-hosted-*` namespaces. Those namespaces are
// hosted-spoke provisioning namespaces (the `hosted-*` second segment is a pool
// PLACEHOLDER hive id, see hosted_namespace_identity.go) that were created on
// the cluster and never torn down. Nothing in the hub could see them, because
// nothing in the hub ever ASKS a cluster which hosted namespaces exist.
//
// WHY NOTHING SAW THEM. Every hub-side sweep over hosted namespaces derives the
// namespace list FROM THE REGISTRY — reapOrphanedPods walks listSaaSHives() and
// calls hostedNamespaceForHive on each (orphaned_pod_reaper.go), which is what
// confines that sweep to namespaces the hub provisioned. That derivation is
// exactly right for a reaper and exactly WRONG for finding a leak: a namespace
// with no registry entry is invisible to a registry-derived list BY
// CONSTRUCTION. The leak class this file detects is the complement of that set —
// namespaces the cluster has and the hub does not know about.
//
// The sibling stuck-pod signal (orphaned_pod_visibility.go) could not see them
// either. Its predicate requires a deletionTimestamp; the pods in the incident
// were never asked to terminate — they are Pending/Unschedulable because their
// namespace outlived whatever was supposed to delete it. Two different
// conditions, two different predicates, and neither existing one covers this.
//
// READ-ONLY. THIS FILE DELETES NOTHING. It issues exactly one `kubectl get
// namespaces -o json` per cluster per health build and reports what it finds.
// A janitor that DELETES leaked namespaces is ask 1 of the issue and is
// deliberately NOT shipped here: a namespace delete cascades to every PVC and
// every pod inside it, so it is the widest destructive verb the hub owns, and
// the input that would drive it — "which namespaces have no registry entry" —
// has never been measured against a real cluster. This report is that
// measurement. Building the deleter first, on a rule validated only by reading
// code, is how a cleanup tool becomes the outage it was meant to prevent.
//
// The provisioning-side half of ask 1 — a failed `kubectl apply` leaving behind
// the namespace it just created — IS fixed, in provision_namespace_rollback.go,
// because there the hub knows it created the namespace seconds earlier and can
// prove nothing else was using it. That is a bounded, provable delete. A fleet
// sweep driven by a registry read is not, and the two must not be conflated.
//
// THE REGISTRY-EMPTY GUARD IS THE LOAD-BEARING SAFETY PROPERTY. listSaaSHives()
// returns nil when it cannot read saasHivesDir at all (an os.ReadDir error —
// saas_provision.go ~1810), and nil is indistinguishable from "this hub hosts
// no hives". If the known-namespace set is empty, EVERY hosted namespace on
// every cluster satisfies "has no registry entry", so one transient unreadable
// registry would report the whole fleet as leaked — and would, if a janitor
// were ever hung off this signal, delete it. collectLeakedHostedNamespaces
// therefore returns nil (unknown) rather than a report when the known set is
// empty. Under-reporting a genuinely empty fleet is the cheap direction; the
// other one is unrecoverable.
//
// THE KNOWN SET IS FLEET-WIDE, NOT PER-CLUSTER. A hive recorded against
// cluster A whose namespace turns up on cluster B is NOT reported as leaked.
// The hub's per-hive ClusterID bookkeeping is not authoritative enough to
// convict a namespace of being orphaned — a reassignment, or a record written
// before a migration, would read as a leak. Matching on the namespace NAME
// alone, across all known hives, is the conservative rule: it can miss a leak,
// it cannot manufacture one.

const (
	// leakedNamespaceMinAge is how long a hosted namespace must have existed
	// with no registry entry before it is reported.
	//
	// The window exists because provisioning is not atomic: handleCreateHive
	// enqueues the work (provision_queue.go) and the namespace is created by
	// `kubectl apply` inside provisionHive, while the hive record reaches its
	// final state only after that apply returns. There is therefore a real
	// interval — normally seconds, bounded by queue depth and one kubectl
	// round-trip — in which a legitimate namespace exists that the registry
	// snapshot taken moments earlier does not name. Reporting inside that
	// interval would flag correct, in-flight provisioning as a leak.
	//
	// Six hours is orders of magnitude beyond that interval and beyond any
	// plausible queue backlog, while being short enough that a leak is legible
	// the same day rather than after the multi-week accumulation this issue was
	// eventually found by. Being late costs one health-build interval; being
	// early accuses a hive that is provisioning correctly right now.
	leakedNamespaceMinAge = 6 * time.Hour

	// leakedNamespaceReportLimit caps the per-namespace breakdown carried in the
	// report. Total is always exact and is never capped — the cap applies only
	// to the list of names, which stops being diagnostic and starts being a wall
	// of text well before the "dozens" the incident spanned.
	leakedNamespaceReportLimit = 25

	// namespacePhaseTerminating is the status.phase of a namespace whose delete
	// has already been issued. Spelled once so the predicate and its tests
	// cannot disagree on the literal.
	namespacePhaseTerminating = "Terminating"
)

// hostedNamespaceCandidate is the subset of namespace metadata the predicate
// needs. It exists so the decision is made over PLAIN DATA rather than over
// kubectl output, which is what makes the rule unit-testable in isolation — the
// same split orphanedPodCandidate makes for the pod reaper.
type hostedNamespaceCandidate struct {
	Name              string
	Phase             string
	CreationTimestamp time.Time
	// HiveID is the value of the hive.kubestellar.io/hive-id label stamped by
	// stampHostedNamespaceIdentity, or "" when the namespace carries no such
	// label. It is REPORTED but never consulted by the predicate, because the
	// two readings differ for whoever has to act: a stamped namespace whose
	// hive is gone is a deprovision that did not finish, while an UNSTAMPED one
	// never got past provisionHive's apply — the stamp happens only after that
	// apply succeeds (saas_provision.go ~2418).
	HiveID string
}

// namespaceListItem mirrors the fields of `kubectl get namespaces -o json` that
// this file consumes.
type namespaceListItem struct {
	Metadata struct {
		Name              string            `json:"name"`
		CreationTimestamp string            `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

// parseHostedNamespaceCandidates turns the stdout of `kubectl get namespaces
// -o json` into candidates. Pure and fully testable: no kubectl, no cluster.
//
// A namespace whose creationTimestamp is absent or unparseable yields a ZERO
// CreationTimestamp, which the predicate rejects. That is the safe direction —
// an unreadable timestamp can never be read as "old enough to be a leak".
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
			Phase:  it.Status.Phase,
			HiveID: it.Metadata.Labels[hiveIDLabel],
		}
		if ts := strings.TrimSpace(it.Metadata.CreationTimestamp); ts != "" {
			if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
				c.CreationTimestamp = parsed
			}
			// On a parse error the zero value stands and the predicate skips the
			// namespace. Deliberate: never guess an age.
		}
		out = append(out, c)
	}
	return out, nil
}

// registryHostedNamespaces builds the set of hosted namespace names the hub
// knows about from the hive records it holds. Derivation goes through
// hostedNamespaceForHive so this set and every other hosted-namespace
// computation in the package share one definition of the name.
func registryHostedNamespaces(hives []SaaSHive) map[string]struct{} {
	known := make(map[string]struct{}, len(hives))
	for i := range hives {
		if ns := hostedNamespaceForHive(&hives[i]); ns != "" {
			known[ns] = struct{}{}
		}
	}
	return known
}

// hostedNamespacesForHiveIDs is registryHostedNamespaces for callers that
// already hold a set of hive IDs rather than the records.
//
// buildClusterHealth unions the on-disk SaaS hive records and the in-memory
// registry into exactly such a set before it queries any cluster. Re-reading
// the records once per cluster would both cost a directory walk per cluster and
// let two clusters in the SAME health build disagree about what the registry
// said — which for this detector is not cosmetic, since the disagreement would
// land as a leak report on one cluster and not the other.
func hostedNamespacesForHiveIDs(ids map[string]bool) map[string]struct{} {
	known := make(map[string]struct{}, len(ids))
	for id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		known[hiveHostedNamespacePrefix+id] = struct{}{}
	}
	return known
}

// namespaceIsLeakedHosted reports whether a namespace is a leaked hosted-spoke
// namespace.
//
// THE RULE — all four clauses must hold:
//
//	name has the hive-hosted- prefix &&
//	  name is NOT in the known (registry-derived) set &&
//	  phase != "Terminating" &&
//	  age(creationTimestamp) > minAge
//
// Each clause is load-bearing and none may be relaxed:
//
//   - the hive-hosted- prefix. Without it this reports every namespace on the
//     cluster the hub does not recognise — kube-system, operator namespaces,
//     other tenants entirely. Those are not ours, and naming them would make
//     the signal unactionable. Same scoping rule summarizeStuckPods applies.
//
//   - not in the known set. This is the whole definition of the leak: the
//     cluster holds a namespace the hub has no hive for. A namespace WITH a
//     record is either live or is the deprovision path's business.
//
//   - phase != Terminating. A Terminating namespace already has its delete
//     issued — something IS cleaning it up. Counting it would report a cleanup
//     in progress as a leak, and would keep counting it for as long as the
//     terminate takes, which for a namespace held by a stuck finalizer is
//     forever. That is a different condition needing a different fix.
//
//   - age > minAge. See leakedNamespaceMinAge: a namespace younger than the
//     window is presumed to be a provision in flight whose record the snapshot
//     has not caught up with.
//
// now and minAge are passed in rather than read from the clock and the package
// constant, so both are testable.
func namespaceIsLeakedHosted(c hostedNamespaceCandidate, known map[string]struct{}, now time.Time, minAge time.Duration) bool {
	if !strings.HasPrefix(c.Name, hiveHostedNamespacePrefix) {
		return false
	}
	if _, ok := known[c.Name]; ok {
		return false
	}
	if strings.TrimSpace(c.Phase) == namespacePhaseTerminating {
		return false
	}
	// No usable creation time — never guess an age in the direction of guilt.
	if c.CreationTimestamp.IsZero() {
		return false
	}
	return now.Sub(c.CreationTimestamp) > minAge
}

// LeakedNamespace is one leaked hosted namespace in the report.
type LeakedNamespace struct {
	Namespace string `json:"namespace"`
	// Age is the human-readable time since creationTimestamp. It is what makes
	// an entry actionable: "seven hours" and "five weeks" call for very
	// different responses from whoever reads this.
	Age string `json:"age"`
	// HiveID is the hive.kubestellar.io/hive-id label, omitted when the
	// namespace was never stamped. See hostedNamespaceCandidate.HiveID for why
	// the ABSENCE of the stamp is itself diagnostic.
	HiveID string `json:"hive_id,omitempty"`
}

// LeakedNamespaceReport is the per-cluster leaked-namespace signal surfaced in
// fleet health.
//
// Total is authoritative and exact. Namespaces is the (possibly truncated, see
// leakedNamespaceReportLimit) breakdown, ordered oldest-first so the head of
// the list is the useful part when it is cut. Truncated marks a short list so a
// reader can tell one from a complete one rather than silently believing they
// saw everything — the same contract StuckPodReport carries.
type LeakedNamespaceReport struct {
	Total      int               `json:"total"`
	Namespaces []LeakedNamespace `json:"namespaces,omitempty"`
	Truncated  bool              `json:"truncated,omitempty"`
}

// summarizeLeakedHostedNamespaces builds the report from already-parsed
// candidates.
//
// PURE. No kubectl, no cluster, no clock of its own — now is passed in. This is
// what makes the count testable against real predicate matches rather than
// against the mere existence of a field.
func summarizeLeakedHostedNamespaces(candidates []hostedNamespaceCandidate, known map[string]struct{}, now time.Time, minAge time.Duration) LeakedNamespaceReport {
	type aged struct {
		entry LeakedNamespace
		age   time.Duration
	}
	var leaked []aged
	for _, c := range candidates {
		if !namespaceIsLeakedHosted(c, known, now, minAge) {
			continue
		}
		age := now.Sub(c.CreationTimestamp)
		leaked = append(leaked, aged{
			entry: LeakedNamespace{
				Namespace: c.Name,
				Age:       age.Truncate(time.Minute).String(),
				HiveID:    c.HiveID,
			},
			age: age,
		})
	}

	rep := LeakedNamespaceReport{Total: len(leaked)}
	if rep.Total == 0 {
		return rep
	}

	// Oldest first, then by name so equal ages have a stable, reproducible
	// order rather than inheriting kubectl's listing order. Determinism matters
	// because this list is truncated: a nondeterministic order would change
	// WHICH namespaces survive the cut between two reads of identical cluster
	// state, and an operator comparing two reads would see churn that is not
	// there.
	sort.Slice(leaked, func(i, j int) bool {
		if leaked[i].age != leaked[j].age {
			return leaked[i].age > leaked[j].age
		}
		return leaked[i].entry.Namespace < leaked[j].entry.Namespace
	})

	rep.Namespaces = make([]LeakedNamespace, 0, len(leaked))
	for _, l := range leaked {
		rep.Namespaces = append(rep.Namespaces, l.entry)
	}
	if len(rep.Namespaces) > leakedNamespaceReportLimit {
		rep.Namespaces = rep.Namespaces[:leakedNamespaceReportLimit]
		rep.Truncated = true
	}
	return rep
}

// collectLeakedHostedNamespaces lists the cluster's namespaces and summarizes
// the leaked hosted ones.
//
// READ-ONLY and best-effort: on any failure it returns nil, and the caller
// omits the signal entirely rather than reporting a false zero. "No leaks" and
// "could not tell" must not render alike — the entire cost of this incident was
// that nothing distinguished them, and it is the same rule the stuck-pod and
// disk fields on this surface already follow.
//
// Returns nil, LOUDLY, when known is empty. See the registry-empty guard note
// at the top of this file: that input cannot be told apart from an unreadable
// registry, and under it every hosted namespace in the fleet matches the
// predicate.
func collectLeakedHostedNamespaces(ctx context.Context, cluster *ClusterConfig, timeout time.Duration, known map[string]struct{}, now time.Time, logger *slog.Logger) *LeakedNamespaceReport {
	if cluster == nil || !cluster.KubectlReachable() {
		return nil
	}
	if len(known) == 0 {
		// Not a quiet skip. A hub whose registry reads as empty while it is
		// serving hosted spokes is itself the bug, and the leak detector going
		// dark is the second-order effect worth naming on the same line.
		if logger != nil {
			logger.Warn("leaked-namespace detection skipped: hub knows of NO hosted hives — cannot tell a leaked namespace from a live one",
				"cluster", cluster.ID)
		}
		return nil
	}
	out, err := kubectlForClusterContext(ctx, cluster,
		"--request-timeout", timeout.String(),
		"get", "namespaces", "-o", "json").Output()
	if err != nil {
		return nil
	}
	candidates, perr := parseHostedNamespaceCandidates(out)
	if perr != nil {
		return nil
	}
	rep := summarizeLeakedHostedNamespaces(candidates, known, now, leakedNamespaceMinAge)
	return &rep
}
