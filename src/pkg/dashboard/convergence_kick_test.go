package dashboard

import (
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/convergence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// ── #4247: shared convergence admission for internal agent kicks ──────────────
//
// These tests pin the admitted-issue projection the eval-cycle dispatch
// boundary consumes. The load-bearing invariants:
//
//   1. SAME CONTRACT: the projection reaches the same judgment as the
//      contributor paths (ReadyQueue / selectTask) under unchanged authoritative
//      state, because it consumes the same observer and pure evaluator on the
//      same per-pass sweep discipline — not an independent filter.
//   2. LEVEL-TRIGGERED: nothing is latched; the next call derives from current
//      bead/retirement state, and a restart reconstructs the same answer.
//   3. READ-ONLY: the input population is never mutated.

func kickIssues() []ghpkg.Issue {
	return []ghpkg.Issue{
		{Repo: "projectbluefin/dakota", Number: 601, Title: "dependent work"},
		{Repo: "projectbluefin/dakota", Number: 700, Title: "unrelated ready work"},
	}
}

func admittedNumbers(issues []ghpkg.Issue) []int {
	out := []int{}
	for _, is := range issues {
		out = append(out, is.Number)
	}
	return out
}

// TestKickProjection_BlockedDependencyIsWithheld: A depends on open B → A is
// absent from the admitted kick projection with the evaluator's own decision,
// while unrelated C stays admitted; and the CONTRIBUTOR paths agree on the same
// authoritative state (shared-contract parity, not coincidence).
func TestKickProjection_BlockedDependencyIsWithheld(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	hub, s := depTestHub(t, map[string]*beads.Store{"scanner": store})

	admitted, withheld := s.ConvergenceKickProjection(kickIssues())
	if got := admittedNumbers(admitted); len(got) != 1 || got[0] != 700 {
		t.Fatalf("admitted %v, want [700]", got)
	}
	if len(withheld) != 1 || withheld[0].Issue.Number != 601 {
		t.Fatalf("withheld %+v, want dakota#601", withheld)
	}
	d := withheld[0].Decision
	if d.Reason != convergence.ReasonWaitingForDependency {
		t.Fatalf("reason %q, want WaitingForDependency", d.Reason)
	}
	if len(d.Blockers) != 1 || d.Blockers[0] != blockerID {
		t.Fatalf("blockers %v, want [%s]", d.Blockers, blockerID)
	}

	// Parity: the contributor queue and live assignment withhold the same
	// candidate under the same unchanged state.
	assertQueue(t, hub, 700)
	assertAssigns(t, hub, 700)
}

// TestKickProjection_FlipsWithoutRestart: closing the blocker admits the
// dependent on the NEXT pass, reopening withholds it again — no latched
// decision in either direction.
func TestKickProjection_FlipsWithoutRestart(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	_, s := depTestHub(t, map[string]*beads.Store{"scanner": store})

	if admitted, _ := s.ConvergenceKickProjection(kickIssues()); len(admitted) != 1 {
		t.Fatalf("expected 601 withheld initially, admitted %v", admittedNumbers(admitted))
	}
	if err := store.Close(blockerID); err != nil {
		t.Fatalf("closing blocker: %v", err)
	}
	if admitted, withheld := s.ConvergenceKickProjection(kickIssues()); len(admitted) != 2 || len(withheld) != 0 {
		t.Fatalf("after satisfaction: admitted %v withheld %d, want both admitted", admittedNumbers(admitted), len(withheld))
	}
	if err := store.Update(blockerID, func(b *beads.Bead) {
		b.Status = beads.StatusOpen
		b.ClosedAt = nil
	}); err != nil {
		t.Fatalf("reopening blocker: %v", err)
	}
	if admitted, _ := s.ConvergenceKickProjection(kickIssues()); len(admitted) != 1 {
		t.Fatalf("after reopening: admitted %v, want [700] — decision must not latch", admittedNumbers(admitted))
	}
}

// TestKickProjection_UnknownDependencyIsWithheldAsUnknown: an unresolvable,
// non-retired dependency withholds the candidate with Ready=Unknown, and
// unrelated work stays dispatchable.
func TestKickProjection_UnknownDependencyIsWithheldAsUnknown(t *testing.T) {
	store := depTestStore(t)
	dependent, err := store.Create("dependent work", beads.TypeTask, beads.PriorityMedium, "scanner",
		"gh-projectbluefin/dakota#601")
	if err != nil {
		t.Fatalf("creating dependent: %v", err)
	}
	if err := store.Update(dependent.ID, func(b *beads.Bead) {
		b.DependsOn = []string{"bd-nonexistent"}
	}); err != nil {
		t.Fatalf("adding dangling dependency: %v", err)
	}
	_, s := depTestHub(t, map[string]*beads.Store{"scanner": store})

	admitted, withheld := s.ConvergenceKickProjection(kickIssues())
	if got := admittedNumbers(admitted); len(got) != 1 || got[0] != 700 {
		t.Fatalf("admitted %v, want [700]", got)
	}
	if len(withheld) != 1 || withheld[0].Decision.Reason != convergence.ReasonDependencyUnknown {
		t.Fatalf("withheld %+v, want DependencyUnknown", withheld)
	}
}

// TestKickProjection_PositiveControls: a terminal-retired dependency and a
// candidate with no record at all are both admitted, so the projection cannot
// drift into reject-everything.
func TestKickProjection_PositiveControls(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	if err := store.Close(blockerID); err != nil {
		t.Fatalf("closing blocker: %v", err)
	}
	_, s := depTestHub(t, map[string]*beads.Store{"scanner": store})

	admitted, withheld := s.ConvergenceKickProjection(kickIssues())
	if len(admitted) != 2 || len(withheld) != 0 {
		t.Fatalf("admitted %v withheld %d, want both admitted", admittedNumbers(admitted), len(withheld))
	}
}

// TestKickProjection_SurvivesRestart: a fresh store loaded from the same
// directory (process restart) reconstructs the same withholding from durable
// state — no process-only verdict.
func TestKickProjection_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	seedDependentBead(t, store, "gh-projectbluefin/dakota#601")

	reloaded, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("reloading store: %v", err)
	}
	_, s := depTestHub(t, map[string]*beads.Store{"scanner": reloaded})
	admitted, withheld := s.ConvergenceKickProjection(kickIssues())
	if got := admittedNumbers(admitted); len(got) != 1 || got[0] != 700 {
		t.Fatalf("admitted %v after restart, want [700]", got)
	}
	if len(withheld) != 1 || withheld[0].Issue.Number != 601 {
		t.Fatalf("withheld %+v after restart, want dakota#601", withheld)
	}
}

// TestKickProjection_PartialLedgerKeepsFinalPolicy: readable blocked records
// stay withheld while unmapped misses remain admitted — the final #3904
// compromise, unchanged by this projection.
func TestKickProjection_PartialLedgerKeepsFinalPolicy(t *testing.T) {
	store := depTestStore(t)
	seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	_, s := depTestHub(t, map[string]*beads.Store{"scanner": store})
	s.deps.BeadStoreLoadFailures = 1

	admitted, withheld := s.ConvergenceKickProjection(kickIssues())
	if got := admittedNumbers(admitted); len(got) != 1 || got[0] != 700 {
		t.Fatalf("admitted %v, want unmapped miss #700 admitted", got)
	}
	if len(withheld) != 1 || withheld[0].Issue.Number != 601 {
		t.Fatalf("withheld %+v, want readable blocked record still gated", withheld)
	}
}

// TestKickProjection_NonGitHubItemsAreAdmitted: a string-keyed external item
// (no positive issue number) skips the GitHub-only bead observer and is
// admitted, exactly as the contributor path treats it (#4245 boundary).
func TestKickProjection_NonGitHubItemsAreAdmitted(t *testing.T) {
	store := depTestStore(t)
	seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	_, s := depTestHub(t, map[string]*beads.Store{"scanner": store})

	issues := []ghpkg.Issue{
		{Repo: "projectbluefin/dakota", Number: 0, SourceType: "linear", ExternalID: "ENG-42", Title: "external"},
	}
	admitted, withheld := s.ConvergenceKickProjection(issues)
	if len(admitted) != 1 || len(withheld) != 0 {
		t.Fatalf("external item must be admitted: admitted %d withheld %d", len(admitted), len(withheld))
	}
}

// TestKickProjection_NilSafety: a nil server or a server with no contribute hub
// admits everything rather than panicking or silently withholding.
func TestKickProjection_NilSafety(t *testing.T) {
	var nilServer *Server
	admitted, withheld := nilServer.ConvergenceKickProjection(kickIssues())
	if len(admitted) != 2 || len(withheld) != 0 {
		t.Fatalf("nil server: admitted %d withheld %d, want all admitted", len(admitted), len(withheld))
	}
}

// TestKickProjection_DoesNotMutateInput: the projection is read-only over the
// caller's slice.
func TestKickProjection_DoesNotMutateInput(t *testing.T) {
	store := depTestStore(t)
	seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	_, s := depTestHub(t, map[string]*beads.Store{"scanner": store})

	in := kickIssues()
	_, _ = s.ConvergenceKickProjection(in)
	if len(in) != 2 || in[0].Number != 601 || in[1].Number != 700 {
		t.Fatalf("input mutated: %+v", in)
	}
}

// TestKickProjection_IsRaceFreeAgainstConcurrentWriters: bead writers racing the
// projection must be race-free under -race — only copied projection values
// escape the store lock (same contract the contributor sweep pins).
func TestKickProjection_IsRaceFreeAgainstConcurrentWriters(t *testing.T) {
	store := depTestStore(t)
	blockerID := seedDependentBead(t, store, "gh-projectbluefin/dakota#601")
	_, s := depTestHub(t, map[string]*beads.Store{"scanner": store})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = store.Update(blockerID, func(b *beads.Bead) {
				if b.Status == beads.StatusOpen {
					b.Status = beads.StatusClosed
				} else {
					b.Status = beads.StatusOpen
					b.ClosedAt = nil
				}
			})
		}
	}()
	for i := 0; i < 50; i++ {
		_, _ = s.ConvergenceKickProjection(kickIssues())
	}
	close(stop)
	wg.Wait()
}
