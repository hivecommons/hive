package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/convergence"
)

// ── #3845 first-slice acceptance matrix, proven on the PRODUCTION path ────────
//
// The design contract is explicit that an internal helper, a CLI, or
// Store.Ready() is NOT sufficient evidence: the same dependency decision must
// demonstrably change BOTH ReadyQueue offerability and actual selectTask
// assignment. So every scenario below asserts against those two entry points,
// not against the evaluator.
//
// The rule vocabulary itself (tri-state conditions, blocker precedence, the
// lookup-miss policy, purity) is pinned separately in pkg/convergence.

// depTestStore builds a real on-disk bead store, so these tests exercise the
// same persistence the hub reads in production rather than a fake.
func depTestStore(t *testing.T) *beads.Store {
	t.Helper()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating bead store: %v", err)
	}
	return store
}

// depTestHub wires a hub whose status carries two actionable issues in a repo
// whose Full (owner/repo) differs from its bare Name — the shape that has
// historically produced key-mismatch misses — plus the given bead stores.
//
// #601 is the DEPENDENT under test; #700 is the UNRELATED control that must
// stay visible, offerable, and selectable no matter what blocks #601.
func depTestHub(t *testing.T, stores map[string]*beads.Store) (*ContributeWSHub, *Server) {
	t.Helper()
	hub, s := covK2Hub(t)
	s.deps.BeadStores = stores
	s.statusMu.Lock()
	s.status = &StatusPayload{
		Repos: []FrontendRepo{{
			Name: "dakota",
			Full: "projectbluefin/dakota",
			ActionableIssues: []any{
				map[string]any{
					"number": float64(601),
					"title":  "dependent work",
					"url":    "https://github.com/projectbluefin/dakota/issues/601",
					"author": "someone",
				},
				map[string]any{
					"number": float64(700),
					"title":  "unrelated ready work",
					"url":    "https://github.com/projectbluefin/dakota/issues/700",
					"author": "someone",
				},
			},
		}},
	}
	s.statusMu.Unlock()
	return hub, s
}

func depTestConn() *ContributorConnection {
	return &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "ct-a", ContributorID: "c-a", TrustTier: "contributor"},
		lastPong: time.Now(),
	}
}

// queueNumbers reduces a ReadyQueue snapshot to the offerable issue numbers, so
// assertions read as "which work is on offer right now".
func queueNumbers(items []ReadyQueueItem) []int {
	out := []int{}
	for _, it := range items {
		if it.Held {
			continue
		}
		out = append(out, it.Number)
	}
	return out
}

func assertQueue(t *testing.T, hub *ContributeWSHub, want ...int) {
	t.Helper()
	got := queueNumbers(hub.ReadyQueue(readyQueueDefaultLimit))
	if len(got) != len(want) {
		t.Fatalf("ReadyQueue offered %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ReadyQueue offered %v, want %v", got, want)
		}
	}
}

// assertAssigns runs a FRESH selectTask (no connection registered, so nothing is
// counted as active work) and asserts which issue it hands out.
func assertAssigns(t *testing.T, hub *ContributeWSHub, want int) {
	t.Helper()
	msg := hub.selectTask(depTestConn())
	if msg == nil {
		t.Fatal("expected a message from selectTask")
	}
	if msg.Type != "task_assign" {
		t.Fatalf("expected task_assign, got %q (reason %q)", msg.Type, msg.Reason)
	}
	if msg.Number != want {
		t.Fatalf("selectTask assigned #%d, want #%d", msg.Number, want)
	}
}

// seedDependentBead mints the dependent bead for dakota#601 (keyed by the
// canonical planning external ref) and a blocker bead it depends on, returning
// the blocker's ID. The blocker starts OPEN, i.e. unsatisfied.
func seedDependentBead(t *testing.T, store *beads.Store, ref string) string {
	t.Helper()
	blocker, err := store.Create("blocking work", beads.TypeTask, beads.PriorityMedium, "scanner", "")
	if err != nil {
		t.Fatalf("creating blocker bead: %v", err)
	}
	dependent, err := store.Create("dependent work", beads.TypeTask, beads.PriorityMedium, "scanner", ref)
	if err != nil {
		t.Fatalf("creating dependent bead: %v", err)
	}
	if err := store.AddDependency(dependent.ID, blocker.ID); err != nil {
		t.Fatalf("adding dependency: %v", err)
	}
	return blocker.ID
}

// TestDependencyAdmission_UnsatisfiedBlocksBothPaths covers acceptance rows 1
// and 4 together: "A depends on unsatisfied B" must remove A from ReadyQueue
// offerability AND from live selectTask assignment, while unrelated C stays
// visible, offerable, and selectable — one blocked lane must never serialise
// the queue.
func TestDependencyAdmission_UnsatisfiedBlocksBothPaths(t *testing.T) {
	store := depTestStore(t)
	seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})

	assertQueue(t, hub, 700)
	assertAssigns(t, hub, 700)
}

// TestDependencyAdmission_SatisfactionFlipsBothWaysWithoutRestart covers rows 2
// and 3 on ONE long-lived hub: closing the blocker makes the dependent ready
// with no restart and no queue surgery, and re-opening it takes the dependent
// back out. Admission is reversible because it is recomputed from current
// ledger state on every sweep rather than latched.
func TestDependencyAdmission_SatisfactionFlipsBothWaysWithoutRestart(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})

	// Blocked.
	assertQueue(t, hub, 700)
	assertAssigns(t, hub, 700)

	// B becomes satisfied → A becomes ready, same process, same hub.
	if err := store.Close(blockerID); err != nil {
		t.Fatalf("closing blocker: %v", err)
	}
	assertQueue(t, hub, 601, 700)
	assertAssigns(t, hub, 601)

	// B becomes unsatisfied again → A leaves ready. Dependencies are
	// non-monotonic; admission must follow them back down.
	if err := store.Update(blockerID, func(b *beads.Bead) {
		b.Status = beads.StatusOpen
		b.ClosedAt = nil
	}); err != nil {
		t.Fatalf("reopening blocker: %v", err)
	}
	assertQueue(t, hub, 700)
	assertAssigns(t, hub, 700)
}

// TestDependencyAdmission_UnknownDependencyIsNotDispatched covers the
// unknown-observation row: a dependency edge naming a bead that exists in no
// store cannot be asserted satisfied, so the dependent is not mutably
// dispatched — and unrelated work is untouched, so the uncertainty stays local
// instead of globally serialising the queue.
func TestDependencyAdmission_UnknownDependencyIsNotDispatched(t *testing.T) {
	store := depTestStore(t)
	dependent, err := store.Create("dependent work", beads.TypeTask, beads.PriorityMedium, "scanner",
		"gh-projectbluefin/dakota#601")
	if err != nil {
		t.Fatalf("creating dependent bead: %v", err)
	}
	if err := store.AddDependency(dependent.ID, "bead-that-does-not-exist"); err != nil {
		t.Fatalf("adding dangling dependency: %v", err)
	}
	hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})

	assertQueue(t, hub, 700)
	assertAssigns(t, hub, 700)

	// And the refusal is reported as UNKNOWN, not as an established blocker,
	// so an operator can tell "cannot tell" from "definitely waiting".
	decision := hub.evaluateContributorNeutralAdmission(hub.newAdmissionSweep(),
		contributorAdmissionCandidate{repoFull: "projectbluefin/dakota", repoName: "dakota", number: 601})
	if decision.admitted {
		t.Fatal("dangling dependency must not admit")
	}
	if decision.reason != contributorAdmissionReasonDependencyUnknown {
		t.Fatalf("reason = %q, want %q", decision.reason, contributorAdmissionReasonDependencyUnknown)
	}
	if ready, _ := decision.convergence.Condition(convergence.ConditionReady); ready.Status != convergence.ConditionUnknown {
		t.Fatalf("Ready condition = %s, want Unknown", ready.Status)
	}
}

// TestDependencyAdmission_SurvivesRestart covers the restart row: admission is
// reconstructed from durable intent plus current source state. A second hub,
// reading the SAME bead directory through a fresh store, reaches the same
// decision with no in-memory carry-over.
func TestDependencyAdmission_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("creating bead store: %v", err)
	}
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")

	hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})
	assertQueue(t, hub, 700)

	// "Restart": a brand-new store and hub over the same durable files.
	reloaded, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("reloading bead store: %v", err)
	}
	restarted, _ := depTestHub(t, map[string]*beads.Store{"scanner": reloaded})
	assertQueue(t, restarted, 700)
	assertAssigns(t, restarted, 700)

	// Satisfy the dependency on disk, then restart again: the new process picks
	// the change up from durable state alone.
	if err := reloaded.Close(blockerID); err != nil {
		t.Fatalf("closing blocker: %v", err)
	}
	final, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("reloading bead store again: %v", err)
	}
	afterSatisfaction, _ := depTestHub(t, map[string]*beads.Store{"scanner": final})
	assertQueue(t, afterSatisfaction, 601, 700)
	assertAssigns(t, afterSatisfaction, 601)
}

// TestDependencyAdmission_CrossStoreDependencyResolves: a dependent bead in one
// agent's store may depend on a bead in another's. Resolving IDs across every
// store is what keeps that from degrading into a permanent Unknown.
func TestDependencyAdmission_CrossStoreDependencyResolves(t *testing.T) {
	architect := depTestStore(t)
	scanner := depTestStore(t)

	blocker, err := architect.Create("blocking work", beads.TypeTask, beads.PriorityMedium, "architect", "")
	if err != nil {
		t.Fatalf("creating blocker bead: %v", err)
	}
	dependent, err := scanner.Create("dependent work", beads.TypeTask, beads.PriorityMedium, "scanner",
		"gh-projectbluefin/dakota#601")
	if err != nil {
		t.Fatalf("creating dependent bead: %v", err)
	}
	if err := scanner.AddDependency(dependent.ID, blocker.ID); err != nil {
		t.Fatalf("adding cross-store dependency: %v", err)
	}

	hub, _ := depTestHub(t, map[string]*beads.Store{"architect": architect, "scanner": scanner})
	assertQueue(t, hub, 700)

	if err := architect.Close(blocker.ID); err != nil {
		t.Fatalf("closing cross-store blocker: %v", err)
	}
	assertQueue(t, hub, 601, 700)
	assertAssigns(t, hub, 601)
}

// TestDependencyAdmission_PartialLedgerStillGatesWhatItCanSee covers the
// truncated-ledger row. A store that fails to open is dropped from BeadStores
// entirely (every failure path in cmd/hive/main.go logs and continues), so the
// hub is told about it out of band via Dependencies.BeadStoreLoadFailures.
//
// The policy is deliberately NOT "withhold every miss". On a real hive most
// actionable issues have no bead, so withholding misses would turn one
// unreadable beads.json into a fleet-wide stall — an empty queue and endless
// no_matching_work, presenting as a hub bug. A partial view instead gates what
// it CAN see and admits what it cannot, and says so in the log.
func TestDependencyAdmission_PartialLedgerStillGatesWhatItCanSee(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")

	hub, s := depTestHub(t, map[string]*beads.Store{"readable": store})
	// One configured store failed to open: the ledger view is incomplete.
	s.deps.BeadStoreLoadFailures = 1

	if idx := hub.buildBeadDependencyIndex(); !idx.partial {
		t.Fatal("a reported store-load failure must mark the snapshot partial")
	}

	// #601 HAS a record with an open dependency → still withheld. Reduced
	// coverage must not weaken the gate where the evidence is present.
	// #700 has no record → admitted, exactly as on a healthy ledger.
	assertQueue(t, hub, 700)
	assertAssigns(t, hub, 700)

	decision := hub.evaluateContributorNeutralAdmission(hub.newAdmissionSweep(),
		contributorAdmissionCandidate{repoFull: "projectbluefin/dakota", repoName: "dakota", number: 700})
	if !decision.admitted {
		t.Fatalf("a miss on a partial ledger must stay admitted, got reason %q",
			decision.convergence.Reason)
	}

	// Once the blocker closes, the gated candidate comes back on the next sweep
	// — a partial ledger changes coverage, not level-triggering.
	if err := store.Close(blockerID); err != nil {
		t.Fatalf("closing blocker: %v", err)
	}
	assertQueue(t, hub, 601, 700)
}

// A nil entry in BeadStores cannot occur in production — main.go never inserts
// one — but the index must still not treat it as a readable store if some
// future caller builds the map differently.
func TestDependencyAdmission_NilStoreEntryIsNotCountedAsRead(t *testing.T) {
	store := depTestStore(t)
	hub, _ := depTestHub(t, map[string]*beads.Store{"readable": store, "broken": nil})

	idx := hub.buildBeadDependencyIndex()
	if idx.stores != 1 {
		t.Fatalf("stores read = %d, want 1 (the nil entry is not a store)", idx.stores)
	}
	if !idx.partial {
		t.Fatal("a nil store entry must still mark the snapshot partial")
	}
}

// TestDependencyAdmission_NoLedgerLeavesSelectionUnchanged is the positive
// control and the compatibility guarantee: a hive with no bead stores wired
// declares no dependencies anywhere, so every candidate is admitted exactly as
// before this feature existed. Failing closed here would stall the whole
// contributor queue for hives that never opted in.
func TestDependencyAdmission_NoLedgerLeavesSelectionUnchanged(t *testing.T) {
	hub, _ := depTestHub(t, nil)
	assertQueue(t, hub, 601, 700)
	assertAssigns(t, hub, 601)
}

// TestDependencyAdmission_NoBeadForCandidateIsAdmitted pins the explicit
// lookup-miss policy on the production path: a populated ledger that simply has
// no record for this issue admits it. A miss is "declared nothing", never
// "blocked" and never "an unknown dependency was satisfied".
func TestDependencyAdmission_NoBeadForCandidateIsAdmitted(t *testing.T) {
	store := depTestStore(t)
	// A bead for a DIFFERENT issue, so the ledger is populated but irrelevant.
	seedDependentBead(t, store, "gh-projectbluefin/dakota#999")

	hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})
	assertQueue(t, hub, 601, 700)
	assertAssigns(t, hub, 601)
}

// TestDependencyAdmission_IdentityKeySpellings: the bead ledger may record the
// source issue under the canonical "owner/repo" spelling, the bare config-form
// repo name, or via issue_repo/issue_number metadata when the external ref fell
// back to a URL. All three must resolve, or the gate silently misses exactly
// the hives whose config uses the other spelling (#2648 class).
func TestDependencyAdmission_IdentityKeySpellings(t *testing.T) {
	t.Run("canonical external ref", func(t *testing.T) {
		store := depTestStore(t)
		seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
		hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})
		assertQueue(t, hub, 700)
	})

	t.Run("bare config-form external ref", func(t *testing.T) {
		store := depTestStore(t)
		seedDependentBead(t, store, "gh-dakota#601")
		hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})
		assertQueue(t, hub, 700)
	})

	t.Run("mixed case external ref", func(t *testing.T) {
		store := depTestStore(t)
		seedDependentBead(t, store, "gh-ProjectBluefin/Dakota#601")
		hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})
		assertQueue(t, hub, 700)
	})

	t.Run("issue metadata with a url external ref", func(t *testing.T) {
		store := depTestStore(t)
		// External ref is the URL fallback planning.IssueRef emits when repo and
		// number are not both known; identity then lives in metadata.
		blockerID := seedDependentBead(t, store, "https://github.com/projectbluefin/dakota/issues/601")
		dependent := store.FindByExternalRef("https://github.com/projectbluefin/dakota/issues/601")
		if dependent == nil {
			t.Fatal("seeded dependent bead not found by ref")
		}
		if err := store.SetMetadata(dependent.ID, "issue_repo", "projectbluefin/dakota"); err != nil {
			t.Fatalf("setting issue_repo: %v", err)
		}
		if err := store.SetMetadata(dependent.ID, "issue_number", "601"); err != nil {
			t.Fatalf("setting issue_number: %v", err)
		}
		hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})
		assertQueue(t, hub, 700)

		if err := store.Close(blockerID); err != nil {
			t.Fatalf("closing blocker: %v", err)
		}
		assertQueue(t, hub, 601, 700)
	})
}

// TestDependencyAdmission_QueueAndSelectTaskCannotDrift is the anti-drift
// contract itself, stated as a test: for every candidate, ReadyQueue
// offerability and selectTask eligibility must agree, because both consume the
// SAME evaluateContributorNeutralAdmission decision. This is the regression
// that #3857 fixed for open-PR claims, held for dependency admission too.
func TestDependencyAdmission_QueueAndSelectTaskCannotDrift(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})

	for _, satisfied := range []bool{false, true} {
		if satisfied {
			if err := store.Close(blockerID); err != nil {
				t.Fatalf("closing blocker: %v", err)
			}
		}
		sweep := hub.newAdmissionSweep()
		offerable := map[int]bool{}
		for _, it := range hub.ReadyQueue(readyQueueDefaultLimit) {
			offerable[it.Number] = !it.Held
		}
		for _, number := range []int{601, 700} {
			decision := hub.evaluateContributorNeutralAdmission(sweep, contributorAdmissionCandidate{
				repoFull: "projectbluefin/dakota", repoName: "dakota", number: number,
			})
			if decision.admitted != offerable[number] {
				t.Fatalf("drift on #%d (blocker satisfied=%v): admission=%v but queue offerability=%v",
					number, satisfied, decision.admitted, offerable[number])
			}
		}
	}
}

// TestNormalizeIssueIdentity pins the identity normaliser directly, including
// the forms that must NOT be indexed — a malformed key that silently matched
// would be worse than no key at all.
func TestNormalizeIssueIdentity(t *testing.T) {
	cases := map[string]string{
		"projectbluefin/dakota#601": "projectbluefin/dakota#601",
		"ProjectBluefin/Dakota#601": "projectbluefin/dakota#601",
		"  dakota#601  ":            "dakota#601",
		"dakota#0601":               "dakota#601",
		"dakota":                    "",
		"#601":                      "",
		"dakota#":                   "",
		"dakota#abc":                "",
		"dakota#0":                  "",
		"dakota#-3":                 "",
		"https://github.com/projectbluefin/dakota/issues/601": "",
	}
	for in, want := range cases {
		if got := normalizeIssueIdentity(in); got != want {
			t.Errorf("normalizeIssueIdentity(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBeadSatisfied pins the legacy LegacyWorkCompleted projection: only a
// terminal bead satisfies a dependency. Anything still live — including
// "blocked", which is emphatically not completion — does not.
func TestBeadSatisfied(t *testing.T) {
	satisfied := map[beads.Status]bool{
		beads.StatusDone:       true,
		beads.StatusClosed:     true,
		beads.StatusOpen:       false,
		beads.StatusInProgress: false,
		beads.StatusBlocked:    false,
	}
	for status, want := range satisfied {
		if got := beadSatisfied(&beads.Bead{Status: status}); got != want {
			t.Errorf("beadSatisfied(%s) = %v, want %v", status, got, want)
		}
	}
}

// TestBeadDependencyIndex_IsRaceFreeAgainstConcurrentWriters pins the lock
// discipline of the admission sweep.
//
// beads.Store.List returns the store's LIVE *Bead pointers and releases the read
// lock before the caller touches them, so projecting a List result races any
// concurrent Update — which mutates beads in place (Status and DependsOn through
// the caller's fn, plus UpdatedAt and ClosedAt). That was tolerable while bead
// reads were occasional; this gate reads every bead in every store on every
// ReadyQueue and selectTask pass, alongside the inception watcher's Close and the
// planning decomposer's AddDependency. The sweep therefore projects inside the
// lock via Store.ReadEach.
//
// Under -race this fails within a few hundred iterations if the sweep goes back
// to reading live pointers outside the lock. Without -race it is a smoke test
// that concurrent admission and mutation neither panic nor deadlock — which is
// itself worth pinning, since ReadEach runs a caller-supplied callback while
// holding a non-reentrant lock.
func TestBeadDependencyIndex_IsRaceFreeAgainstConcurrentWriters(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})

	const iterations = 400
	var wg sync.WaitGroup
	wg.Add(3)

	// Reader 1: the sweep itself, the way ReadyQueue/selectTask build it.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			hub.newAdmissionSweep()
		}
	}()

	// Reader 2: the full production path, so the race surface under test is the
	// real one and not just the index constructor.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			hub.ReadyQueue(10)
		}
	}()

	// Writer: flips the blocker's terminal state and grows its dependency edges,
	// touching exactly the fields the sweep projects (Status, DependsOn,
	// UpdatedAt, ClosedAt).
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				_ = store.Close(blockerID)
			} else {
				_ = store.Update(blockerID, func(b *beads.Bead) {
					b.Status = beads.StatusOpen
					b.ClosedAt = nil
				})
			}
			_ = store.AddDependency(blockerID, "edge-"+strconv.Itoa(i))
		}
	}()

	wg.Wait()

	// The ledger is still coherent afterwards, and admission still answers: a
	// race-free sweep must also be a CORRECT one, not merely a quiet one.
	sweep := hub.newAdmissionSweep()
	if sweep == nil || sweep.deps == nil {
		t.Fatal("sweep did not survive concurrent mutation")
	}
	if _, ok := sweep.deps.byID[blockerID]; !ok {
		t.Fatalf("blocker %s missing from the index after concurrent mutation", blockerID)
	}
}

// TestDependencyAdmission_CulledSatisfiedDependencyStaysSatisfied covers the
// review's highest finding: closed beads are DELETED from the live ledger by two
// live mechanisms — knowledge lifecycle culling via Store.Archive, and
// Store.evictOldClosed past maxBeadCount — and both only ever take terminal
// beads. Resolving a culled edge to Unknown therefore punished exactly the
// dependencies that had been satisfied, withholding their dependents forever
// with no log and no Held badge.
func TestDependencyAdmission_CulledSatisfiedDependencyStaysSatisfied(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	if err := store.Close(blockerID); err != nil {
		t.Fatalf("closing blocker: %v", err)
	}

	hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})
	assertQueue(t, hub, 601, 700) // satisfied → offerable

	// Lifecycle culling removes the closed blocker from the live ledger.
	if err := store.Archive(blockerID); err != nil {
		t.Fatalf("archiving blocker: %v", err)
	}
	if !store.IsRetired(blockerID) {
		t.Fatal("an archived bead must be recorded as retired")
	}

	assertQueue(t, hub, 601, 700)
	assertAssigns(t, hub, 601)

	decision := hub.evaluateContributorNeutralAdmission(hub.newAdmissionSweep(),
		contributorAdmissionCandidate{repoFull: "projectbluefin/dakota", repoName: "dakota", number: 601})
	if !decision.admitted {
		t.Fatalf("a culled SATISFIED dependency must not withhold its dependent, got reason %q",
			decision.convergence.Reason)
	}
}

// A dependency ID that resolves nowhere and was never retired is still Unknown.
// The retirement carve-out must not become a general "unresolvable is fine" rule
// — a dangling reference is a record we cannot read, not a satisfied edge.
func TestDependencyAdmission_DanglingDependencyIsStillUnknown(t *testing.T) {
	store := depTestStore(t)
	dependent, err := store.Create("dependent work", beads.TypeTask, beads.PriorityMedium, "scanner",
		"gh-projectbluefin/dakota#601")
	if err != nil {
		t.Fatalf("creating dependent bead: %v", err)
	}
	if err := store.Update(dependent.ID, func(b *beads.Bead) {
		b.DependsOn = append(b.DependsOn, "bd-never-existed")
	}); err != nil {
		t.Fatalf("adding dangling dependency: %v", err)
	}

	hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})
	decision := hub.evaluateContributorNeutralAdmission(hub.newAdmissionSweep(),
		contributorAdmissionCandidate{repoFull: "projectbluefin/dakota", repoName: "dakota", number: 601})
	if decision.admitted {
		t.Fatal("an unresolvable, never-retired dependency must not be admitted")
	}
}

// TestDependencyAdmission_PRRefBeadDoesNotClaimIssueIdentity covers the review's
// identity-collision finding. pkg/retro mints advisory beads with
// ExternalRef = "owner/repo#<PR number>" into a store this index reads. Indexing
// any bare "x#n" ref as an issue identity let PR #601 and issue #601 share a key
// — and because byRef is first-wins, a retro advisory could shadow the real epic
// and silently bypass the dependencies that epic declared.
func TestDependencyAdmission_PRRefBeadDoesNotClaimIssueIdentity(t *testing.T) {
	store := depTestStore(t)
	// A retro-shaped bead: no "gh-" prefix, numbered like the issue.
	if _, err := store.Create("advisory for PR 601", beads.TypeTask, beads.PriorityMedium, "retro",
		"projectbluefin/dakota#601"); err != nil {
		t.Fatalf("creating retro-shaped bead: %v", err)
	}

	hub, _ := depTestHub(t, map[string]*beads.Store{"retro": store})
	idx := hub.buildBeadDependencyIndex()
	if _, claimed := idx.byRef["projectbluefin/dakota#601"]; claimed {
		t.Fatal("a PR-numbered external ref claimed the same-numbered ISSUE's identity")
	}

	// And the live issue is unaffected: no record, so it is admitted as before.
	assertQueue(t, hub, 601, 700)
}

// TestBeadDependencyIndex_IndexesOnlyWhatTheGateCanAsk pins the SHAPE of the
// snapshot the assignment path pays for on every sweep (#3845 review, perf). The
// gate asks the ledger exactly two questions, so the index carries exactly two
// answers:
//
//   - a RECORD, only for beads that answer to a candidate identity key — the
//     only beads byRef can ever hand back;
//   - a SATISFACTION BOOL for every bead in every store, because ANY bead may be
//     the target of a dependency edge, including one that no candidate maps to.
//
// A bead with no identity key must therefore be absent from byRef and present in
// byID. If that inverts, the sweep has either gone back to copying the whole
// ledger per selection or lost its ability to resolve an edge.
func TestBeadDependencyIndex_IndexesOnlyWhatTheGateCanAsk(t *testing.T) {
	store := depTestStore(t)
	plain, err := store.Create("no issue identity", beads.TypeTask, beads.PriorityMedium, "scanner", "")
	if err != nil {
		t.Fatalf("creating unkeyed bead: %v", err)
	}
	keyed, err := store.Create("issue work", beads.TypeTask, beads.PriorityMedium, "scanner",
		"gh-projectbluefin/dakota#601")
	if err != nil {
		t.Fatalf("creating issue-keyed bead: %v", err)
	}
	if err := store.Close(plain.ID); err != nil {
		t.Fatalf("closing unkeyed bead: %v", err)
	}

	hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})
	idx := hub.buildBeadDependencyIndex()

	if _, ok := idx.byRef["projectbluefin/dakota#601"]; !ok {
		t.Fatal("an issue-keyed bead must be reachable by candidate identity")
	}
	if len(idx.byRef) != 1 {
		t.Fatalf("byRef holds %d records, want 1: only identity-keyed beads get one", len(idx.byRef))
	}

	satisfied, ok := idx.byID[plain.ID]
	if !ok {
		t.Fatal("a bead with no identity key must still be resolvable as a dependency target")
	}
	if !satisfied {
		t.Fatal("a closed dependency target must read as satisfied")
	}
	if satisfied, ok := idx.byID[keyed.ID]; !ok || satisfied {
		t.Fatalf("byID[keyed] = (%v, %v), want (false, true): an open bead is not satisfied",
			satisfied, ok)
	}
}

// TestDependencyAdmission_CulledDependencyResolvesAcrossStores holds the
// retirement carve-out to the cross-store case. Retirement is now asked of each
// store ON DEMAND (Store.IsRetired) for the few edges that resolve nowhere,
// rather than every store's retired set being folded into the snapshot up front
// — so the fan-out over stores has to be real, or a dependency culled in the
// ARCHITECT's store would read as unresolvable to a dependent in the SCANNER's
// and withhold it forever.
func TestDependencyAdmission_CulledDependencyResolvesAcrossStores(t *testing.T) {
	architect := depTestStore(t)
	scanner := depTestStore(t)

	blocker, err := architect.Create("blocking work", beads.TypeTask, beads.PriorityMedium, "architect", "")
	if err != nil {
		t.Fatalf("creating blocker bead: %v", err)
	}
	dependent, err := scanner.Create("dependent work", beads.TypeTask, beads.PriorityMedium, "scanner",
		"gh-projectbluefin/dakota#601")
	if err != nil {
		t.Fatalf("creating dependent bead: %v", err)
	}
	if err := scanner.AddDependency(dependent.ID, blocker.ID); err != nil {
		t.Fatalf("adding cross-store dependency: %v", err)
	}

	// Satisfied, then culled out of the live ledger by lifecycle archiving.
	if err := architect.Close(blocker.ID); err != nil {
		t.Fatalf("closing cross-store blocker: %v", err)
	}
	if err := architect.Archive(blocker.ID); err != nil {
		t.Fatalf("archiving cross-store blocker: %v", err)
	}
	if _, err := architect.Get(blocker.ID); err == nil {
		t.Fatal("an archived bead must be gone from the live ledger")
	}

	hub, _ := depTestHub(t, map[string]*beads.Store{"architect": architect, "scanner": scanner})
	assertQueue(t, hub, 601, 700)
	assertAssigns(t, hub, 601)
}

// TestDependencyAdmission_ObservedGenerationIsTheRecordsUpdateTime pins the
// reported generation across the change that stopped formatting one RFC3339Nano
// timestamp per bead in the ledger and renders it only when a candidate actually
// resolves to a record. Same value, paid for once per lookup instead of once per
// bead.
func TestDependencyAdmission_ObservedGenerationIsTheRecordsUpdateTime(t *testing.T) {
	store := depTestStore(t)
	seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	dependent := store.FindByExternalRef("gh-projectbluefin/dakota#601")
	if dependent == nil {
		t.Fatal("seeded dependent bead not found by ref")
	}
	want := dependent.UpdatedAt.UTC().Format(time.RFC3339Nano)

	hub, _ := depTestHub(t, map[string]*beads.Store{"scanner": store})
	obs := hub.observeCandidateDependencies(hub.newAdmissionSweep(),
		contributorAdmissionCandidate{repoFull: "projectbluefin/dakota", repoName: "dakota", number: 601})
	if !obs.Found {
		t.Fatal("the seeded record must be observed as found")
	}
	if obs.Generation != want {
		t.Fatalf("observed generation = %q, want the record's UpdatedAt %q", obs.Generation, want)
	}
	if _, err := time.Parse(time.RFC3339Nano, obs.Generation); err != nil {
		t.Fatalf("generation %q is not RFC3339Nano: %v", obs.Generation, err)
	}
}

// ── Sweep cost (#3845 review, perf) ───────────────────────────────────────────

// benchBeadStore writes a store of `count` beads straight to beads.json and
// opens it, because Store.Create re-persists the whole file per bead and would
// make seeding a realistic ledger quadratic. Every tenth bead is issue-keyed
// (the shape byRef indexes) and depends on its neighbour, so the benchmark
// exercises identity keys and dependency-edge resolution, not just iteration.
func benchBeadStore(b *testing.B, count, firstIssue int) *beads.Store {
	b.Helper()
	dir := b.TempDir()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	all := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		bead := map[string]any{
			"id":         fmt.Sprintf("bd-%d-%d", firstIssue, i),
			"title":      "bench bead",
			"type":       "task",
			"status":     "closed",
			"priority":   2,
			"actor":      "bench",
			"created_at": now,
			"updated_at": now,
		}
		if i%10 == 0 {
			bead["external_ref"] = fmt.Sprintf("gh-projectbluefin/dakota#%d", firstIssue+i)
			bead["depends_on"] = []string{fmt.Sprintf("bd-%d-%d", firstIssue, i+1)}
		}
		all = append(all, bead)
	}
	data, err := json.Marshal(all)
	if err != nil {
		b.Fatalf("marshaling bench beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "beads.json"), data, 0o600); err != nil {
		b.Fatalf("writing bench beads: %v", err)
	}
	store, err := beads.NewStore(dir)
	if err != nil {
		b.Fatalf("opening bench bead store: %v", err)
	}
	return store
}

// BenchmarkAdmissionSweep costs ONE ReadyQueue/selectTask pass at the scale the
// review costed it: several agent stores near the 5000-bead cap, ~150 live
// candidates, and most of those candidates carrying no ledger record at all.
//
//	go test ./pkg/dashboard/ -run '^$' -bench AdmissionSweep -benchmem
//
// It exists to keep the sweep from silently going back to materializing a full
// record for every bead in every store on every selection.
func BenchmarkAdmissionSweep(b *testing.B) {
	const (
		storeCount    = 8
		beadsPerStore = 5000
		candidates    = 150
	)
	stores := map[string]*beads.Store{}
	for s := 0; s < storeCount; s++ {
		// Store 0's issue keys overlap the candidate range (so ~15 candidates
		// resolve to a record and walk their dependency edges); the rest are
		// ledger bulk that no candidate ever asks about.
		firstIssue := 1
		if s > 0 {
			firstIssue = 100000 * s
		}
		stores[fmt.Sprintf("agent-%02d", s)] = benchBeadStore(b, beadsPerStore, firstIssue)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := NewServer(0, logger)
	srv.deps = &Dependencies{BeadStores: stores}
	hub := NewContributeWSHub(logger, srv)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sweep := hub.newAdmissionSweep()
		for n := 1; n <= candidates; n++ {
			hub.evaluateContributorNeutralAdmission(sweep, contributorAdmissionCandidate{
				repoFull: "projectbluefin/dakota", repoName: "dakota", number: n,
			})
		}
	}
}
