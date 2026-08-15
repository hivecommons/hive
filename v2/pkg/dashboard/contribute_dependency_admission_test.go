package dashboard

import (
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/convergence"
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

// TestDependencyAdmission_PartialLedgerWithholdsRatherThanAssumes covers the
// observer-unavailable row. With one configured store unreadable, a candidate
// whose record was NOT found might have that record in the store we could not
// read — so the miss is degraded, not admitted. No false satisfaction.
//
// Critically the degradation is per candidate: it does not claim convergence,
// and it does not touch work whose record WAS found and resolved.
func TestDependencyAdmission_PartialLedgerWithholdsRatherThanAssumes(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	if err := store.Close(blockerID); err != nil {
		t.Fatalf("closing blocker: %v", err)
	}

	// "readable" holds the dependent's record; "broken" is a store that failed
	// to initialise, leaving the ledger view incomplete.
	hub, _ := depTestHub(t, map[string]*beads.Store{"readable": store, "broken": nil})

	// #601 has a record and its dependency is satisfied → still offerable.
	// #700 has no record, and the miss is no longer trustworthy → withheld.
	assertQueue(t, hub, 601)
	assertAssigns(t, hub, 601)

	decision := hub.evaluateContributorNeutralAdmission(hub.newAdmissionSweep(),
		contributorAdmissionCandidate{repoFull: "projectbluefin/dakota", repoName: "dakota", number: 700})
	if decision.admitted {
		t.Fatal("an unresolvable miss on a partial ledger must not be admitted")
	}
	if decision.convergence.Reason != beadLedgerPartialReason {
		t.Fatalf("reason = %q, want %q", decision.convergence.Reason, beadLedgerPartialReason)
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
