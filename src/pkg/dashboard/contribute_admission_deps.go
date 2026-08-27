package dashboard

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/beads"
	"github.com/kubestellar/hive/pkg/convergence"
	"github.com/kubestellar/hive/pkg/planning"
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
// What that pass deliberately does NOT do is copy the ledger. The sweep sits on
// the assignment path and holds each store's read lock for the whole of its
// pass, so every byte it materialises is latency in front of task assignment and
// a writer (Close, AddDependency) kept waiting. Only two questions are ever
// asked of the ledger — "is there a record declaring THIS candidate's work?"
// (byRef) and "is the bead this edge names satisfied?" (byID) — so only the
// beads that can answer the first get a record built, the second is answered by
// one bool per bead, and retirement is resolved on demand for the few edges that
// resolve nowhere. At ~10 stores near the 5000-bead cap that is the difference
// between copying the whole ledger per selection and reading it (#3845 review).
//
// It is a SNAPSHOT, not a cache: it lives for exactly one sweep and is thrown
// away, so it can never serve a stale satisfaction judgment to a later sweep.
// It holds COPIED VALUES rather than the live *beads.Bead pointers the store
// owns, for two reasons: a writer closing a bead mid-sweep must not make one
// candidate see the before-state and the next the after-state, and reading a
// live bead outside the store lock while another goroutine mutates it is a data
// race. The copy is taken INSIDE the lock via Store.ReadEach — copying outside
// it would narrow that race, not remove it.
type beadDependencyIndex struct {
	// byRef maps a candidate identity key (see candidateIdentityKeys) to the
	// record declaring that candidate's work. Only beads that ANSWER to an
	// identity key are in here, because only those can ever be reached through
	// it; the rest of the ledger is dependency targets at most.
	byRef map[string]beadRecord
	// byID answers the one question a dependency edge asks of its target — is
	// that bead terminal — for EVERY bead in EVERY store, so an edge resolves
	// even when the dependency lives in a different agent's store than the
	// dependent, and even when no candidate maps to it. A bool rather than a
	// record on purpose: PRESENCE separates "no such bead" (Unknown) from "found
	// and still open" (blocked), and nothing else about a dependency bead is ever
	// read, so a full record per bead was pure cost.
	byID map[string]bool
	// sources holds the readable stores in the order they were read, so a
	// RETIREMENT lookup (see isRetired) can be made on demand rather than copying
	// every store's retired set into the snapshot up front. That set is backed by
	// archive.jsonl and only ever grows, so folding it in per sweep was an
	// unbounded cost to answer a question only a dangling edge ever asks. Holding
	// stores is safe in a way holding their beads is not: a *Store is a
	// lock-guarded owner, a *Bead is unsynchronised mutable state.
	sources []*beads.Store
	// stores is how many bead stores were read. Zero means the hive has no bead
	// ledger wired at all (a hub booted without one, or a test hub) — which is
	// "no declared intent anywhere", NOT a degraded observation. See
	// observeCandidateDependencies.
	stores int
	// partial is set when a configured store failed to open at startup, so the
	// snapshot covers only PART of the ledger and a lookup miss is not fully
	// trustworthy — the record may live in the store that would not load. It is
	// sourced from Dependencies.BeadStoreLoadFailures, because a failed store is
	// omitted from BeadStores entirely and is otherwise indistinguishable from a
	// hive that simply has fewer agents. It is REPORTED (newAdmissionSweep logs
	// it) rather than acted on: see the lookup-miss policy in
	// observeCandidateDependencies for why a partial view still admits what it
	// cannot see.
	partial bool
}

// beadRecord is the immutable slice of a bead the dependency gate needs from the
// record DECLARING a candidate's work: its identity, its declared dependency
// edges, and the revision of desired state this snapshot observed. Whether the
// bead is itself terminal is not part of it — that is only ever asked of a
// dependency TARGET, which byID answers.
type beadRecord struct {
	id        string
	dependsOn []string
	// updatedAt is kept as the raw timestamp and rendered only if a candidate
	// actually resolves to this record. Formatting it while indexing cost one
	// RFC3339Nano string per bead in the ledger to serve at most one lookup per
	// candidate.
	updatedAt time.Time
}

// generation renders the revision of desired state this record was observed at.
// It is reported for logs and forward compatibility, never compared.
func (r beadRecord) generation() string {
	if r.updatedAt.IsZero() {
		return ""
	}
	return r.updatedAt.UTC().Format(time.RFC3339Nano)
}

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
	idx := h.buildBeadDependencyIndex()
	if idx.partial && h != nil && h.logger != nil {
		// Say it out loud every sweep. A truncated ledger means the gate is
		// running on partial evidence, and the only alternative to admitting
		// what it cannot see is stalling the queue — so an operator has to be
		// able to find this in the log rather than infer it from behaviour.
		h.logger.Warn("[contribute-ws] dependency admission is running on a PARTIAL bead ledger",
			"stores_read", idx.stores,
			"stores_failed", h.server.deps.BeadStoreLoadFailures,
			"effect", "candidates with no readable record are admitted, not withheld")
	}
	return &contributorAdmissionSweep{deps: idx}
}

// buildBeadDependencyIndex reads every configured bead store once and indexes
// its beads by candidate identity (a record) and by bead ID (a satisfaction
// bool).
//
// Store iteration order is SORTED by store name so a candidate identity claimed
// by beads in two different stores (two agents both minted work for the same
// issue) resolves to the same bead on every sweep rather than flapping with Go's
// randomised map order. Within a store, ReadEach yields creation order, so the
// EARLIEST bead wins — the original declaration, not a later duplicate.
func (h *ContributeWSHub) buildBeadDependencyIndex() *beadDependencyIndex {
	idx := &beadDependencyIndex{
		byRef: map[string]beadRecord{},
		byID:  map[string]bool{},
	}
	if h == nil || h.server == nil || h.server.deps == nil {
		return idx
	}
	// A store that failed to open at startup is not in BeadStores at all — every
	// failure path in cmd/hive/main.go logs and `continue`s without inserting, so
	// the nil-entry check below can never fire in production and the ledger looks
	// merely SMALLER rather than incomplete. The producer's failure count is the
	// only signal that the view is partial.
	if h.server.deps.BeadStoreLoadFailures > 0 {
		idx.partial = true
	}
	if len(h.server.deps.BeadStores) == 0 {
		return idx
	}

	names := make([]string, 0, len(h.server.deps.BeadStores))
	for name := range h.server.deps.BeadStores {
		names = append(names, name)
	}
	sort.Strings(names)

	// Size byID for the whole ledger up front. It takes one entry per bead, and
	// growing it from empty rehashes tens of thousands of keys on a path that
	// runs per selection. Store.Count is O(1) under the store's read lock, and a
	// count that goes stale before the store is read costs at most one resize.
	total := 0
	for _, name := range names {
		if store := h.server.deps.BeadStores[name]; store != nil {
			total += store.Count()
		}
	}
	if total > 0 {
		idx.byID = make(map[string]bool, total)
	}

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
		idx.sources = append(idx.sources, store)
		// ReadEach, not List: List hands back the store's LIVE bead pointers and
		// drops the lock first, so projecting them here would race any concurrent
		// Update — the inception watcher's Close and the planning decomposer's
		// AddDependency both mutate beads in place while this sweep runs, and this
		// sweep is now on the assignment path rather than an occasional read.
		// `go test -race` reproduced exactly that (TestBeadDependencyIndex_
		// IsRaceFreeAgainstConcurrentWriters pins it). Everything below copies out
		// of the bead and retains nothing.
		//
		// The callback runs WITH THE LOCK HELD, so it does the least work that
		// answers the two questions the gate can ask (see the type comment) and
		// nothing more.
		store.ReadEach(beads.ListFilter{}, func(b *beads.Bead) {
			if b == nil {
				return
			}
			// Bead IDs are UUID-derived and effectively unique across stores;
			// first-wins under the sorted store order keeps resolution stable
			// if they ever were not.
			if _, seen := idx.byID[b.ID]; !seen {
				idx.byID[b.ID] = beadSatisfied(b)
			}
			// Only a bead that answers to a candidate identity is reachable
			// through byRef, so only those pay for a record — the copied
			// DependsOn and the timestamp. On a mature ledger that is a small
			// minority of beads, and the bool above has already said everything
			// an edge needs to know about the rest.
			keys := beadIdentityKeys(b)
			if len(keys) == 0 {
				return
			}
			rec := newBeadRecord(b)
			for _, key := range keys {
				if _, seen := idx.byRef[key]; !seen {
					idx.byRef[key] = rec
				}
			}
		})
	}
	return idx
}

// isRetired reports whether ANY store knows this bead ID as
// removed-after-terminal (Store.IsRetired): it existed, reached a terminal state,
// and was then culled by lifecycle archiving or maxBeadCount eviction. An edge
// naming one of those was SATISFIED and then swept up, which is not the same as
// an edge that never resolved.
//
// Asked on demand, and only for an ID that MISSED byID, instead of folding every
// store's retired set into the snapshot: that set is rebuilt from archive.jsonl
// at load and only ever grows, so a hive that has been running for months was
// paying to copy its entire archive history into a map on every ReadyQueue and
// selectTask call to answer the rare dangling edge.
//
// Per-sweep consistency survives the change. An ID that missed byID was already
// absent from every store's live map when that store was read, and retirement is
// monotonic (nothing is ever removed from a retired set), so the only way two
// candidates in one sweep could see different answers for the same edge is for a
// bead with that exact ID to be minted, closed and culled between them — which
// fresh-UUID minting rules out.
func (idx *beadDependencyIndex) isRetired(id string) bool {
	if idx == nil {
		return false
	}
	for _, store := range idx.sources {
		if store.IsRetired(id) {
			return true
		}
	}
	return false
}

// newBeadRecord copies the fields the dependency gate reads out of a live bead.
// It runs INSIDE the store's read lock and retains nothing the store owns:
// DependsOn is copied rather than aliased so a later AddDependency cannot mutate
// the backing array underneath an in-flight sweep, and the timestamp is taken by
// value.
func newBeadRecord(b *beads.Bead) beadRecord {
	rec := beadRecord{
		id:        b.ID,
		updatedAt: b.UpdatedAt.Time,
	}
	if len(b.DependsOn) > 0 {
		rec.dependsOn = append([]string(nil), b.DependsOn...)
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
	// The "gh-" prefix is REQUIRED, not merely stripped when present. Without it
	// any bare "<x>#<n>" external ref is indexed as an issue identity — and
	// pkg/retro mints advisory beads with ExternalRef = "owner/repo#<PR number>"
	// (retro.go, splitPRRef) into a store this index reads. Issue #123 and PR
	// #123 in one repo would then share a key, so a retro advisory could become
	// a live issue's "authoritative record" and, because byRef is first-wins
	// across sorted store names, SHADOW the genuine epic and silently bypass the
	// dependencies that epic declared. Only planning.IssueRef writes the prefix,
	// so requiring it keeps the issue namespace to records that mean an issue.
	if ref := strings.TrimSpace(b.ExternalRef); strings.HasPrefix(ref, "gh-") {
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
//     This holds even when the snapshot is PARTIAL — a configured store failed
//     to open. The no-false-satisfaction instinct says to withhold such a miss,
//     since the record might be in the store we could not read. That was the
//     original rule here and it is wrong at this scale: on a real hive most
//     actionable issues never get a bead, so withholding every miss turns one
//     unreadable beads.json into a fleet-wide stall — an empty queue, endless
//     no_matching_work, and a symptom that reads as a hub bug. This gate is
//     additive over having no gate at all, so a truncated view degrades to
//     "gate what we can actually see". A partial ledger is LOGGED every sweep
//     (newAdmissionSweep) so the reduced coverage is visible instead of silent,
//     and candidates whose record WAS found remain fully gated.
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
		// Deliberately NOT reported as degraded. A partial ledger means the
		// record might be in the store we could not read — but the candidates
		// that miss are the overwhelming majority on any real hive (most
		// actionable issues never get a bead), so withholding every miss turns
		// one unreadable beads.json into a fleet-wide stall: the queue empties,
		// contributors see only no_matching_work, and the symptom presents as a
		// hub bug. This gate is additive over having no gate at all, so a
		// truncated view degrades to "gate what we can actually see" rather than
		// "refuse everything we cannot". Candidates whose record WAS found stay
		// fully gated, and newAdmissionSweep logs the degradation so a partial
		// ledger is visible rather than silent.
		return obs // no record for this candidate
	}

	obs.Found = true
	obs.RecordID = record.id
	obs.Generation = record.generation()
	for _, depID := range record.dependsOn {
		depID = strings.TrimSpace(depID)
		if depID == "" {
			continue
		}
		satisfied, ok := sweep.deps.byID[depID]
		switch {
		case !ok && sweep.deps.isRetired(depID):
			// The dependency bead was removed from the live ledger AFTER
			// reaching a terminal state — lifecycle culling (Store.Archive) or
			// the maxBeadCount eviction, both of which only ever take
			// closed/done beads. Treating that as unresolvable would withhold
			// the dependent FOREVER, and would do it to exactly the dependencies
			// most likely to be culled: the satisfied ones.
			obs.Dependencies = append(obs.Dependencies, convergence.Dependency{
				ID: depID, Status: convergence.ConditionTrue,
				Detail: "dependency bead was closed and retired from the ledger",
			})
		case !ok:
			obs.Dependencies = append(obs.Dependencies, convergence.Dependency{
				ID: depID, Status: convergence.ConditionUnknown,
				Detail: "no bead found for dependency",
			})
		case satisfied:
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
