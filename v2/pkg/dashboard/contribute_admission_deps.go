package dashboard

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/convergence"
	"github.com/kubestellar/hive/v2/pkg/planning"
)

// ── Dependency observation for contributor-neutral admission (#3845) ──────────
//
// This is the FIRST observer feeding pkg/convergence: it reads the bead ledger —
// Hive's existing durable, dependency-bearing work record (ADR 0004) — and
// reports, per live GitHub candidate, whether that candidate's declared
// dependencies are satisfied. Nothing here decides admission; it only observes.
// pkg/convergence.Evaluate makes the judgment, and
// evaluateContributorNeutralAdmission applies it to BOTH live projections
// (ReadyQueue offerability and selectTask assignment) so they cannot drift.
//
// Deliberate non-goals, per the design contract:
//
//   - It creates NOTHING. Admission consumes authoritative state; it never mints
//     a shadow bead merely to have something to query. A candidate with no bead
//     is observed as "no record", not conjured into one.
//   - It is not a second scheduler. A blocked candidate is simply absent from
//     the admitted set; ordering, routing, and dispatch are untouched.
//   - It caches nothing across evaluations. Every ReadyQueue / selectTask sweep
//     re-reads the ledger, which is what makes admission LEVEL-TRIGGERED: when a
//     dependency becomes satisfied the dependent becomes offerable on the next
//     sweep with no restart, and when it becomes unsatisfied again the dependent
//     leaves the ready set just as promptly. Events are hints; current state is
//     the truth.
//
// Freshness horizon. The bead ledger is durable local state, so a restart
// reconstructs exactly the same admission decisions from the same files with no
// replay. Beads written by AGENT processes (the `bd` CLI persists straight to
// disk) reach the hub's in-memory stores through the bounded authoritative
// refresh the governor already performs — the per-eval-cycle Store.Reload() in
// cmd/hive/main.go — so a dependency closed by an agent gates admission within
// one eval cycle. This observer deliberately does NOT add its own disk read to
// the assignment path: that would race the governor's refresh and put I/O in
// front of every candidate for no freshness that the next cycle would not give.

// beadDependencyIndex is a per-sweep snapshot of the bead ledger, built once and
// reused for every candidate in that sweep.
//
// Why an index rather than a FindByExternalRef per candidate: a live hive scans
// on the order of 150 actionable issues per sweep, and FindByExternalRef is a
// linear scan of a store that may hold thousands of beads, across several agent
// stores — so the naive form is a multi-million-operation quadratic inside the
// task-assignment path. One pass builds both lookups instead.
//
// It is a SNAPSHOT, not a cache: it lives for exactly one sweep and is thrown
// away, so it can never serve a stale satisfaction judgment to a later sweep.
// It holds COPIED VALUES rather than the live *beads.Bead pointers List returns,
// for two reasons: an agent closing a bead mid-sweep must not make one candidate
// see the before-state and the next see the after-state, and reading a live bead
// outside the store lock while another goroutine mutates it is a data race.
type beadDependencyIndex struct {
	// byRef maps a candidate identity key (see candidateIdentityKeys) to the
	// record declaring that candidate's work.
	byRef map[string]beadRecord
	// byID maps bead ID to record across ALL stores, so a dependency edge can be
	// resolved even when the dependency bead lives in a different agent's store
	// than the dependent.
	byID map[string]beadRecord
	// stores is how many bead stores were read. Zero means the hive has no bead
	// ledger wired at all (a hub booted without one, or a test hub) — which is
	// "no declared intent anywhere", NOT a degraded observation. See
	// observeCandidateDependencies.
	stores int
	// partial is set when a configured store could not be read, so the snapshot
	// covers only PART of the ledger. A lookup miss is then not trustworthy —
	// the record may live in the store we could not read — and admission must
	// not treat it as "no declared dependencies".
	partial bool
}

// beadRecord is the immutable slice of a bead the dependency gate needs: its
// identity, its declared dependency edges, whether it is terminal, and the
// revision of desired state this snapshot observed.
type beadRecord struct {
	id         string
	dependsOn  []string
	satisfied  bool
	generation string
}

// beadLedgerPartialReason is the DegradedReason reported when the ledger
// snapshot could not cover every configured store.
const beadLedgerPartialReason = "BeadLedgerPartial"

// contributorAdmissionSweep carries the observation state shared by every
// candidate in ONE ReadyQueue / selectTask pass. Building it once per sweep
// keeps the shared admission contract cheap enough to sit in the assignment
// path, and — because a sweep is discarded when the pass ends — keeps every
// pass level-triggered against current ledger state.
type contributorAdmissionSweep struct {
	deps *beadDependencyIndex
}

// newAdmissionSweep snapshots the bead ledger for one admission pass. Callers
// build it ONCE before iterating candidates and pass it to every
// evaluateContributorNeutralAdmission call in that pass.
func (h *ContributeWSHub) newAdmissionSweep() *contributorAdmissionSweep {
	return &contributorAdmissionSweep{deps: h.buildBeadDependencyIndex()}
}

// buildBeadDependencyIndex reads every configured bead store once and indexes
// its beads by candidate identity and by bead ID.
//
// Store iteration order is SORTED by store name so a candidate identity claimed
// by beads in two different stores (two agents both minted work for the same
// issue) resolves to the same bead on every sweep rather than flapping with Go's
// randomised map order. Within a store, List returns creation order, so the
// EARLIEST bead wins — the original declaration, not a later duplicate.
func (h *ContributeWSHub) buildBeadDependencyIndex() *beadDependencyIndex {
	idx := &beadDependencyIndex{
		byRef: map[string]beadRecord{},
		byID:  map[string]beadRecord{},
	}
	if h == nil || h.server == nil || h.server.deps == nil || len(h.server.deps.BeadStores) == 0 {
		return idx
	}

	names := make([]string, 0, len(h.server.deps.BeadStores))
	for name := range h.server.deps.BeadStores {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		store := h.server.deps.BeadStores[name]
		if store == nil {
			// A configured agent whose store is unreadable. The ledger view is
			// now incomplete, which downgrades every lookup MISS from "nothing
			// declared" to "cannot tell" — see observeCandidateDependencies.
			idx.partial = true
			continue
		}
		idx.stores++
		for _, b := range store.List(beads.ListFilter{}) {
			if b == nil {
				continue
			}
			rec := newBeadRecord(b)
			// Bead IDs are UUID-derived and effectively unique across stores;
			// first-wins under the sorted store order keeps resolution stable
			// if they ever were not.
			if _, seen := idx.byID[rec.id]; !seen {
				idx.byID[rec.id] = rec
			}
			for _, key := range beadIdentityKeys(b) {
				if _, seen := idx.byRef[key]; !seen {
					idx.byRef[key] = rec
				}
			}
		}
	}
	return idx
}

// newBeadRecord copies the fields the dependency gate reads out of a live bead.
// DependsOn is copied rather than aliased so a later AddDependency cannot mutate
// the backing array underneath an in-flight sweep.
func newBeadRecord(b *beads.Bead) beadRecord {
	rec := beadRecord{
		id:        b.ID,
		satisfied: beadSatisfied(b),
	}
	if len(b.DependsOn) > 0 {
		rec.dependsOn = append([]string(nil), b.DependsOn...)
	}
	if !b.UpdatedAt.IsZero() {
		rec.generation = b.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return rec
}

// beadIdentityKeys returns the candidate identity keys a bead answers to.
//
// Hive records the GitHub issue behind a bead two ways, and BOTH must resolve or
// the mapping silently misses the exact cases it matters for:
//
//   - ExternalRef "gh-<repo>#<number>" (planning.IssueRef) — the idempotency key
//     issue-sourced epics are minted under.
//   - issue_repo / issue_number metadata (planning.MetaIssueRepo /
//     MetaIssueNumber) — which is also present when the external ref fell back
//     to the issue URL because the repo/number were not both known at mint time.
//
// The repo spelling is normalised but NOT rewritten: whatever spelling the bead
// recorded becomes a key, and the caller supplies both the canonical
// "owner/repo" and the bare config-form spelling when it looks up, mirroring
// issueClaimedByOpenPR. That is the #2648 key-mismatch class of bug, avoided in
// the same way.
func beadIdentityKeys(b *beads.Bead) []string {
	var keys []string
	if ref := strings.TrimSpace(b.ExternalRef); ref != "" {
		if key := normalizeIssueIdentity(strings.TrimPrefix(ref, "gh-")); key != "" {
			keys = append(keys, key)
		}
	}
	repo := strings.TrimSpace(b.Meta(planning.MetaIssueRepo))
	number := strings.TrimSpace(b.Meta(planning.MetaIssueNumber))
	if repo != "" && number != "" {
		if key := normalizeIssueIdentity(repo + "#" + number); key != "" && !contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

// candidateIdentityKeys returns the identity keys a live candidate may be known
// by, canonical "owner/repo#N" FIRST and the bare config-form "repo#N" second —
// the same precedence issueClaimedByOpenPR uses, so a hive configured with bare
// repo names behaves identically on both admission inputs.
func candidateIdentityKeys(candidate contributorAdmissionCandidate) []string {
	var keys []string
	if key := normalizeIssueIdentity(candidate.repoFull + "#" + strconv.Itoa(candidate.number)); key != "" {
		keys = append(keys, key)
	}
	if candidate.repoName != "" && candidate.repoName != candidate.repoFull {
		if key := normalizeIssueIdentity(candidate.repoName + "#" + strconv.Itoa(candidate.number)); key != "" && !contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

// normalizeIssueIdentity lower-cases and trims a "repo#number" identity so
// lookup is case-insensitive (GitHub owner/repo names are), and rejects
// malformed forms rather than indexing a key nothing can match. A ref that is
// not "<repo>#<positive int>" (an issue URL, a non-GitHub external ref) yields
// "" and is simply not indexed under this identity.
func normalizeIssueIdentity(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	repo, num, ok := strings.Cut(raw, "#")
	if !ok {
		return ""
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(num))
	if err != nil || n <= 0 {
		return ""
	}
	return repo + "#" + strconv.Itoa(n)
}

// observeCandidateDependencies is the bead-backed observer: it turns one live
// GitHub candidate into a convergence.Observation.
//
// The three outcomes it can report, and why each is what it is:
//
//   - NO BEAD STORES AT ALL (stores == 0): reported as "not found", i.e. no
//     declared dependencies, so admission is unchanged from before this feature.
//     This is deliberately NOT reported as degraded. A hive with no bead ledger
//     has not FAILED to observe anything — there is no intent to observe — and
//     treating it as degraded would refuse every candidate in every hub booted
//     without beads, stalling the whole contributor queue on a feature that
//     nobody opted into.
//
//   - NO BEAD FOR THIS CANDIDATE: "not found". The explicit lookup-miss policy
//     (convergence.Evaluate rule 2): a miss means the candidate declared no
//     dependencies. It never means a dependency was satisfied.
//
//     UNLESS the snapshot is PARTIAL — a configured store was unreadable. Then
//     the miss is reported DEGRADED instead, and the candidate is withheld:
//     the record we failed to find may be sitting in the store we failed to
//     read, and "I could not look" must never be rendered as "nothing to wait
//     for". That is the no-false-satisfaction rule for an unavailable observer.
//     Because degradation is judged per candidate, a partial ledger does not
//     stall anything whose record WAS found and whose dependencies resolve.
//
//   - A BEAD EXISTS: each DependsOn edge is resolved against the ledger.
//     Terminal statuses (done/closed) are satisfied; any live status is
//     unsatisfied; an edge naming a bead absent from every store is UNKNOWN
//     rather than either, because a dangling reference is a record we cannot
//     read, not a dependency we can wave through — the same fail-closed
//     instinct Store.Ready() already applies to a missing parent epic.
//
// The observed generation is the bead's last-update timestamp: the revision of
// desired state this judgment was made against. It is reported for logs and
// forward compatibility, never compared.
func (h *ContributeWSHub) observeCandidateDependencies(sweep *contributorAdmissionSweep, candidate contributorAdmissionCandidate) convergence.Observation {
	obs := convergence.Observation{
		Subject: convergence.Subject{Repo: candidate.repoFull, Number: candidate.number},
	}
	if sweep == nil || sweep.deps == nil {
		return obs
	}
	if sweep.deps.stores == 0 && !sweep.deps.partial {
		return obs // no ledger wired → nothing is declared → nothing to wait for
	}

	var record beadRecord
	var found bool
	for _, key := range candidateIdentityKeys(candidate) {
		if rec, ok := sweep.deps.byRef[key]; ok {
			record, found = rec, true
			break
		}
	}
	if !found {
		if sweep.deps.partial {
			obs.Degraded = true
			obs.DegradedReason = beadLedgerPartialReason
		}
		return obs // no record for this candidate
	}

	obs.Found = true
	obs.RecordID = record.id
	obs.Generation = record.generation
	for _, depID := range record.dependsOn {
		depID = strings.TrimSpace(depID)
		if depID == "" {
			continue
		}
		dep, ok := sweep.deps.byID[depID]
		switch {
		case !ok:
			obs.Dependencies = append(obs.Dependencies, convergence.Dependency{
				ID: depID, Status: convergence.ConditionUnknown,
				Detail: "no bead found for dependency",
			})
		case dep.satisfied:
			obs.Dependencies = append(obs.Dependencies, convergence.Dependency{
				ID: depID, Status: convergence.ConditionTrue,
				Detail: "dependency bead is closed",
			})
		default:
			obs.Dependencies = append(obs.Dependencies, convergence.Dependency{
				ID: depID, Status: convergence.ConditionFalse,
				Detail: "dependency bead is still open",
			})
		}
	}
	return obs
}

// beadSatisfied reports whether a dependency bead is in a terminal state.
//
// This is the deliberately narrow legacy projection the design contract calls
// for: a closed bead stands in for the synthetic condition
// LegacyWorkCompleted(beadID). It is NOT a claim that "a bead was historically
// closed" is the universal dependency ontology — that is explicitly a non-goal.
// Richer, non-monotonic, exact-subject conditions replace this projection in a
// later increment WITHOUT the live paths changing, because they consume
// convergence.Decision, not bead status.
func beadSatisfied(b *beads.Bead) bool {
	switch b.Status {
	case beads.StatusDone, beads.StatusClosed:
		return true
	default:
		return false
	}
}
