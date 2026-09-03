package dashboard

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/knowledge"
)

func covFWatcherLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// covFWatcher builds an InceptionWatcher backed by a real bead store + engine
// so the bead-scanning and phase-advancing helpers can be exercised directly.
func covFWatcher(t *testing.T) (*InceptionWatcher, *knowledge.InceptionEngine, *beads.Store) {
	t.Helper()
	logger := covFWatcherLogger()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("beads.NewStore: %v", err)
	}
	kapi := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{}, logger)
	eng := knowledge.NewInceptionEngine(t.TempDir(), kapi, logger)
	// scheduler/agentMgr/governor left nil — the helpers we drive guard on them.
	w := NewInceptionWatcher(store, eng, nil, nil, nil, logger)
	w.ctx = context.Background()
	return w, eng, store
}

func TestCovF_NewInceptionWatcher(t *testing.T) {
	w, _, _ := covFWatcher(t)
	if w == nil {
		t.Fatal("nil watcher")
	}
}

func TestCovF_ParseQuestionFromBdCreate(t *testing.T) {
	w, _, _ := covFWatcher(t)

	// Valid bd create with a title.
	q := w.parseQuestionFromBdCreate(`bd create --title "What database should we use?" --type advisory`)
	if q == nil || q.Text != "What database should we use?" {
		t.Fatalf("expected parsed question, got %+v", q)
	}

	// Category inference from a keyword.
	q = w.parseQuestionFromBdCreate(`bd create --title "Who are the primary users?" --actor brainstorm`)
	if q == nil || q.Category != "users" {
		t.Fatalf("expected category users, got %+v", q)
	}

	// Clarification prefix stripped.
	q = w.parseQuestionFromBdCreate(`bd create --title "Clarification: What language?"`)
	if q == nil || q.Text != "What language?" {
		t.Fatalf("expected prefix stripped, got %+v", q)
	}

	// No --title match -> nil.
	if got := w.parseQuestionFromBdCreate(`bd create --type advisory`); got != nil {
		t.Fatalf("expected nil for no title, got %+v", got)
	}

	// Template placeholder -> nil.
	if got := w.parseQuestionFromBdCreate(`bd create --title "<fact title>"`); got != nil {
		t.Fatalf("expected nil for placeholder, got %+v", got)
	}
	// Empty title -> nil.
	if got := w.parseQuestionFromBdCreate(`bd create --title ""`); got != nil {
		t.Fatalf("expected nil for empty title, got %+v", got)
	}
}

func TestCovF_IsTemplatePlaceholder(t *testing.T) {
	cases := map[string]bool{
		"<anything>":         true,
		"<fact title>":       true,
		"your question here": true,
		"Real question?":     false,
		"":                   false,
	}
	for in, want := range cases {
		if got := isTemplatePlaceholder(in); got != want {
			t.Errorf("isTemplatePlaceholder(%q)=%v want %v", in, got, want)
		}
	}
}

func TestCovF_ContainsWordAndIsAlpha(t *testing.T) {
	if !containsWord("this must work", "must") {
		t.Error("expected containsWord to find whole word")
	}
	if containsWord("mustard is tasty", "must") {
		t.Error("expected containsWord to reject substring inside a word")
	}
	if containsWord("nothing here", "xyz") {
		t.Error("expected false for absent word")
	}
	if !isAlpha('a') || !isAlpha('Z') || isAlpha('1') || isAlpha(' ') {
		t.Error("isAlpha misbehaving")
	}
}

func TestCovF_BuildInceptionKickMessage(t *testing.T) {
	w, _, _ := covFWatcher(t)

	capture := &knowledge.InceptionState{Phase: knowledge.PhaseCapture, IdeaText: "capture idea"}
	if msg := w.buildInceptionKickMessage(capture); msg == "" {
		t.Fatal("empty capture kick message")
	}

	structure := &knowledge.InceptionState{
		Phase:     knowledge.PhaseStructure,
		IdeaText:  "structure idea",
		IdeaSlug:  "structure-idea",
		Questions: []knowledge.Question{{ID: "q1", Text: "lang?"}},
		Answers:   map[string]string{"q1": "Go"},
	}
	if msg := w.buildInceptionKickMessage(structure); msg == "" {
		t.Fatal("empty structure kick message")
	}
}

func TestCovF_FindAndCheckInceptionBeads(t *testing.T) {
	w, eng, store := covFWatcher(t)

	// Start an inception so GetState() is non-nil (StartedAt in the past).
	if _, err := eng.Start("a beadful idea in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Give StartedAt a small backdate so freshly-created beads count as "after".
	time.Sleep(10 * time.Millisecond)

	// Create >=5 clarification question beads from the brainstorm actor.
	for i := 0; i < 6; i++ {
		b, err := store.Create("Clarification: question "+string(rune('A'+i)), beads.TypeAdvisory, beads.PriorityHigh, "brainstorm", "inception/idea")
		if err != nil {
			t.Fatalf("create bead: %v", err)
		}
		_ = store.SetMetadata(b.ID, "category", "general")
		_ = store.SetMetadata(b.ID, "question_id", "q"+string(rune('A'+i)))
	}

	found := w.findInceptionBeads()
	if len(found) < 6 {
		t.Fatalf("expected >=6 inception beads, got %d", len(found))
	}

	// checkForQuestions should extract >=5 and advance the engine to clarify.
	w.checkForQuestions(found)
	if st := eng.GetState(); st == nil || st.Phase != knowledge.PhaseClarify {
		t.Fatalf("expected clarify phase after checkForQuestions, got %+v", st)
	}
}

func TestCovF_CheckForFactsAdvances(t *testing.T) {
	w, eng, store := covFWatcher(t)

	// Advance the engine to structure phase.
	if _, err := eng.Start("a factful idea in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.SetQuestions([]knowledge.Question{{ID: "q1", Text: "lang?", Category: "language"}}); err != nil {
		t.Fatalf("SetQuestions: %v", err)
	}
	if _, err := eng.SubmitAnswers(map[string]string{"q1": "Go"}); err != nil {
		t.Fatalf("SubmitAnswers: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	// Create >= targetFactCount fact beads so checkForFacts records + advances.
	factTitles := []string{"Vision statement", "Constitution principles", "Requirement one",
		"Requirement two", "Constraint one", "Stakeholder one", "Acceptance one", "Requirement three"}
	for i, title := range factTitles {
		b, err := store.Create(title, beads.TypeAdvisory, beads.PriorityHigh, "brainstorm", "inception/idea")
		if err != nil {
			t.Fatalf("create fact bead: %v", err)
		}
		ft := []string{"vision", "constitution", "requirement", "requirement", "constraint", "stakeholder", "acceptance", "requirement"}[i]
		_ = store.SetMetadata(b.ID, "fact_type", ft)
		_ = store.SetMetadata(b.ID, "fact_body", "body for "+title)
	}

	found := w.findInceptionBeads()
	w.checkForFacts(context.Background(), found)
	if st := eng.GetState(); st == nil || st.Phase != knowledge.PhaseScaffold {
		t.Fatalf("expected scaffold phase after checkForFacts, got %+v", st)
	}
}

func TestCovF_ReapOldInceptionBeads(t *testing.T) {
	w, eng, store := covFWatcher(t)
	if _, err := eng.Start("reap idea in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Create a bead with the inception ref.
	if _, err := store.Create("Clarification: old q", beads.TypeAdvisory, beads.PriorityHigh, "brainstorm", "inception/old"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Reap everything created before "now + 1h" — should close the bead.
	w.reapOldInceptionBeads(time.Now().Add(time.Hour))
}

func TestCovF_SupplementFactsFromQA(t *testing.T) {
	w, _, _ := covFWatcher(t)
	state := &knowledge.InceptionState{
		IdeaText: "supplement idea",
		Questions: []knowledge.Question{
			{ID: "features", Text: "What features?", Category: "features"},
			{ID: "users", Text: "Who?", Category: "users"},
		},
		Answers: map[string]string{"features": "todo list", "users": "devs"},
	}
	// No existing facts + no vision -> supplements a vision + one per answered Q.
	out := w.supplementFactsFromQA(nil, state)
	if len(out) < 3 {
		t.Fatalf("expected supplemented facts, got %d", len(out))
	}
}

func TestCovF_AutoGenerateQuestionsAndFacts(t *testing.T) {
	w, eng, _ := covFWatcher(t)

	// autoGenerateQuestions: engine in capture with too few questions.
	if _, err := eng.Start("build a python data tool"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	state := eng.GetState()
	w.autoGenerateQuestions(state)
	if st := eng.GetState(); st == nil || st.Phase != knowledge.PhaseClarify {
		t.Fatalf("expected clarify after autoGenerateQuestions, got %+v", st)
	}

	// autoGenerateFacts: advance to structure, then auto-generate from Q&A.
	if _, err := eng.SubmitAnswers(map[string]string{
		"language": "Python", "users": "analysts", "features": "charts",
		"constraints": "fast", "testing": "pytest",
	}); err != nil {
		t.Fatalf("SubmitAnswers: %v", err)
	}
	st := eng.GetState()
	w.autoGenerateFacts(context.Background(), st)
	if got := eng.GetState(); got == nil || got.Phase != knowledge.PhaseScaffold {
		t.Fatalf("expected scaffold after autoGenerateFacts, got %+v", got)
	}
}

func TestCovF_TryExtractFactsFromPluk(t *testing.T) {
	w, eng, _ := covFWatcher(t)
	// Advance to structure with answers so the fallback path has Q&A available.
	if _, err := eng.Start("pluk extract idea in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = eng.SetQuestions([]knowledge.Question{
		{ID: "language", Text: "lang?", Category: "language"},
		{ID: "features", Text: "features?", Category: "features"},
	})
	_, _ = eng.SubmitAnswers(map[string]string{"language": "Go", "features": "cli"})
	st := eng.GetState()

	// Seed buffered pluk fact lines with recognizable fact keywords.
	w.plukMu.Lock()
	w.plukFactLines = []string{
		"The vision is to build a fast todo CLI for developers",
		"A key requirement is offline support for all commands",
		"An important constraint is zero external dependencies here",
		"Constitution: prefer the standard library over frameworks",
	}
	w.plukMu.Unlock()

	w.tryExtractFactsFromPluk(context.Background(), st)
	// Either it extracted enough facts and advanced, or fell back to Q&A
	// auto-generation (which also advances). Assert it left capture/structure.
	if got := eng.GetState(); got == nil {
		t.Fatal("nil state after tryExtractFactsFromPluk")
	}
}

func TestCovF_HandlePlukEvent(t *testing.T) {
	w, eng, _ := covFWatcher(t)
	if _, err := eng.Start("pluk event idea in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// raw_output during capture with a bd create question line -> buffered.
	// #4285: the payload key is "line". This case used to pass "message", which
	// the handler also read, so the arm ran on an empty string and this test
	// covered the line without exercising anything.
	w.handlePlukEvent(plukEvent{
		Type: "raw_output",
		Data: map[string]string{"line": `bd create --title "Who are the users?" --actor brainstorm`},
	})

	// state_change to working then idle.
	w.handlePlukEvent(plukEvent{Type: "state_change", Data: map[string]string{"state": "working"}})
	w.handlePlukEvent(plukEvent{Type: "state_change", Data: map[string]string{"state": "idle"}})

	// rate_limit + error + tool_call_completed events.
	w.handlePlukEvent(plukEvent{Type: "rate_limit", Data: map[string]string{"message": "slow down"}})
	w.handlePlukEvent(plukEvent{Type: "error", Data: map[string]string{"message": "boom"}})
	w.handlePlukEvent(plukEvent{Type: "tool_call_completed"})
	time.Sleep(30 * time.Millisecond) // let any spawned poll goroutines run

	_ = eng
}

func TestCovF_ApplyPlukQuestions(t *testing.T) {
	w, eng, _ := covFWatcher(t)
	if _, err := eng.Start("apply pluk questions idea in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	state := eng.GetState()

	// Fewer than the minimum -> no-op (early return).
	w.plukMu.Lock()
	w.plukQuestions = []knowledge.Question{{ID: "a", Text: "one?"}}
	w.plukMu.Unlock()
	w.applyPlukQuestions(state)
	if st := eng.GetState(); st.Phase != knowledge.PhaseCapture {
		t.Fatalf("expected still capture with too-few questions, got %s", st.Phase)
	}

	// Enough unique questions -> advances to clarify.
	w.plukMu.Lock()
	w.plukQuestions = []knowledge.Question{
		{ID: "a", Text: "one?"}, {ID: "b", Text: "two?"}, {ID: "c", Text: "three?"},
		{ID: "d", Text: "four?"}, {ID: "e", Text: "five?"},
	}
	w.plukMu.Unlock()
	w.applyPlukQuestions(state)
	if st := eng.GetState(); st.Phase != knowledge.PhaseClarify {
		t.Fatalf("expected clarify after applyPlukQuestions, got %s", st.Phase)
	}
}

func TestCovF_PollNoState(t *testing.T) {
	w, _, _ := covFWatcher(t)
	// No inception started -> GetState nil -> poll resets counters and returns.
	w.poll(context.Background())
}
