package dashboard

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// factWatcher builds a watcher (via the shared covFWatcher fixture) whose
// engine has been walked to the fact-gathering phase: started, questions set,
// answers submitted. Beads created after this call count as inception beads.
func factWatcher(t *testing.T) (*InceptionWatcher, *knowledge.InceptionEngine, *beads.Store) {
	t.Helper()
	w, eng, store := covFWatcher(t)
	if _, err := eng.Start("a grace-period idea in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.SetQuestions([]knowledge.Question{{ID: "q1", Text: "lang?", Category: "language"}}); err != nil {
		t.Fatalf("SetQuestions: %v", err)
	}
	if _, err := eng.SubmitAnswers(map[string]string{"q1": "Go"}); err != nil {
		t.Fatalf("SubmitAnswers: %v", err)
	}
	// Backdate StartedAt slightly so freshly-created beads count as "after".
	time.Sleep(10 * time.Millisecond)
	return w, eng, store
}

func createFactBead(t *testing.T, store *beads.Store, title, factType string) *beads.Bead {
	t.Helper()
	b, err := store.Create(title, beads.TypeAdvisory, beads.PriorityHigh, "brainstorm", "inception/idea")
	if err != nil {
		t.Fatalf("create bead %q: %v", title, err)
	}
	if factType != "" {
		if err := store.SetMetadata(b.ID, "fact_type", factType); err != nil {
			t.Fatalf("set fact_type: %v", err)
		}
	}
	return b
}

// TestCheckForFactsBelowMinimumDoesNothing pins the floor: fewer than
// minFactsForAdvance facts must neither advance the phase nor start the
// enrichment grace clock — otherwise two early beads could freeze the
// grace window before the brainstorm has really produced anything.
func TestCheckForFactsBelowMinimumDoesNothing(t *testing.T) {
	w, eng, store := factWatcher(t)
	for i := 0; i < minFactsForAdvance-1; i++ {
		createFactBead(t, store, "Requirement early "+string(rune('A'+i)), "requirement")
	}

	w.checkForFacts(context.Background(), w.findInceptionBeads())

	if st := eng.GetState(); st == nil || st.Phase == knowledge.PhaseScaffold {
		t.Fatalf("phase advanced on %d facts (< min %d): %+v", minFactsForAdvance-1, minFactsForAdvance, st)
	}
	if !w.factGraceStart.IsZero() {
		t.Fatal("grace period started below the minimum fact count")
	}
}

// TestCheckForFactsGracePeriodLifecycle walks the between-min-and-target
// path: the first sighting starts the grace clock without advancing, more
// facts arriving during grace only update the running count, and once the
// grace period has elapsed the watcher records and advances to scaffold,
// resetting the clock.
func TestCheckForFactsGracePeriodLifecycle(t *testing.T) {
	w, eng, store := factWatcher(t)

	// minFactsForAdvance facts: enough to start grace, short of the target.
	titles := []string{"Vision statement", "Requirement one", "Constraint one"}
	types := []string{"vision", "requirement", "constraint"}
	for i, title := range titles {
		createFactBead(t, store, title, types[i])
	}

	// First sighting: grace starts, no advance.
	w.checkForFacts(context.Background(), w.findInceptionBeads())
	if w.factGraceStart.IsZero() {
		t.Fatal("grace period did not start at the minimum fact count")
	}
	if st := eng.GetState(); st == nil || st.Phase == knowledge.PhaseScaffold {
		t.Fatalf("phase advanced immediately at minimum count: %+v", st)
	}

	// One more fact during grace: count updates, still no advance.
	createFactBead(t, store, "Stakeholder one", "stakeholder")
	w.checkForFacts(context.Background(), w.findInceptionBeads())
	if w.lastFactCount != len(titles)+1 {
		t.Fatalf("lastFactCount = %d, want %d (updated during grace)", w.lastFactCount, len(titles)+1)
	}
	if st := eng.GetState(); st == nil || st.Phase == knowledge.PhaseScaffold {
		t.Fatalf("phase advanced during grace period: %+v", st)
	}

	// Grace elapsed (backdated) plus one more fact: record and advance.
	createFactBead(t, store, "Acceptance one", "acceptance")
	w.factGraceStart = time.Now().Add(-factEnrichmentGracePeriod - time.Second)
	w.checkForFacts(context.Background(), w.findInceptionBeads())
	if st := eng.GetState(); st == nil || st.Phase != knowledge.PhaseScaffold {
		t.Fatalf("expected scaffold after grace elapsed, got %+v", st)
	}
	if !w.factGraceStart.IsZero() {
		t.Fatal("grace clock not reset after recording facts")
	}
}

// TestCheckForFactsFallbacksAndSkips pins the per-bead salvage rules: a
// missing fact_type is inferred from the title (and an uninferable title
// skips the bead entirely), the body falls back through detail → Notes →
// Title, and fact_tags is split and trimmed. Advancing to scaffold on
// exactly targetFactCount salvageable beads — with an unsalvageable one in
// the mix — proves the skip didn't eat a real fact and the fallbacks didn't
// drop one.
func TestCheckForFactsFallbacksAndSkips(t *testing.T) {
	w, eng, store := factWatcher(t)

	// Unsalvageable: no fact_type metadata, title infers nothing. Must be
	// skipped without poisoning the batch.
	createFactBead(t, store, "hello world", "")

	// Inferred from title (no fact_type metadata), body falls back to Title.
	createFactBead(t, store, "Constraint: no external calls", "")

	// Body from the detail metadata fallback.
	b := createFactBead(t, store, "Requirement one", "requirement")
	if err := store.SetMetadata(b.ID, "detail", "detail body"); err != nil {
		t.Fatalf("set detail: %v", err)
	}

	// Body from the Notes fallback.
	b = createFactBead(t, store, "Requirement two", "requirement")
	if err := store.Update(b.ID, func(bd *beads.Bead) { bd.Notes = "notes body" }); err != nil {
		t.Fatalf("set notes: %v", err)
	}

	// fact_tags split and trimmed.
	b = createFactBead(t, store, "Vision statement", "vision")
	if err := store.SetMetadata(b.ID, "fact_tags", " alpha , beta "); err != nil {
		t.Fatalf("set fact_tags: %v", err)
	}

	// Top up to targetFactCount salvageable facts so the watcher advances
	// immediately (no grace period on a full batch).
	for i := 0; i < targetFactCount-4; i++ {
		createFactBead(t, store, "Requirement extra "+string(rune('A'+i)), "requirement")
	}

	w.checkForFacts(context.Background(), w.findInceptionBeads())
	if st := eng.GetState(); st == nil || st.Phase != knowledge.PhaseScaffold {
		t.Fatalf("expected scaffold from %d salvageable facts, got %+v", targetFactCount, st)
	}
}
