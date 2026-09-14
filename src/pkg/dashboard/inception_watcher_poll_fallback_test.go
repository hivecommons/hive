package dashboard

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// These tests pin the timing/fallback wiring inside InceptionWatcher.poll —
// the branches that decide WHEN autoGenerateQuestions/autoGenerateFacts fire.
// The helpers themselves are covered elsewhere; here we drive poll() with the
// watcher's timing fields set to each side of the thresholds.

// pollWatcherInCapture starts an inception and aligns lastSlug so poll skips
// the slug-change reset (which would clobber the timing fields under test).
func pollWatcherInCapture(t *testing.T) (*InceptionWatcher, *knowledge.InceptionEngine) {
	t.Helper()
	w, eng, _ := covFWatcher(t)
	if _, err := eng.Start("build a python data tool"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	w.lastSlug = eng.GetState().IdeaSlug
	return w, eng
}

// pollWatcherInStructure advances the inception to the structure phase with
// enough answered questions for autoGenerateFacts to produce facts.
func pollWatcherInStructure(t *testing.T) (*InceptionWatcher, *knowledge.InceptionEngine) {
	t.Helper()
	w, eng := pollWatcherInCapture(t)
	if err := eng.SetQuestions([]knowledge.Question{
		{ID: "language", Text: "lang?", Category: "language"},
		{ID: "users", Text: "who?", Category: "users"},
		{ID: "features", Text: "features?", Category: "features"},
		{ID: "constraints", Text: "constraints?", Category: "constraints"},
		{ID: "testing", Text: "testing?", Category: "testing"},
	}); err != nil {
		t.Fatalf("SetQuestions: %v", err)
	}
	if _, err := eng.SubmitAnswers(map[string]string{
		"language": "Python", "users": "analysts", "features": "charts",
		"constraints": "fast", "testing": "pytest",
	}); err != nil {
		t.Fatalf("SubmitAnswers: %v", err)
	}
	if st := eng.GetState(); st == nil || st.Phase != knowledge.PhaseStructure {
		t.Fatalf("expected structure phase, got %+v", eng.GetState())
	}
	return w, eng
}

func setPlukLastActivity(w *InceptionWatcher, at time.Time) {
	w.plukMu.Lock()
	w.plukLastActivity = at
	w.plukMu.Unlock()
}

func TestPoll_NilEngineReturnsEarly(t *testing.T) {
	w, _, _ := covFWatcher(t)
	w.inception = nil
	w.lastQuestionCount = 7
	w.poll(context.Background())
	if w.lastQuestionCount != 7 {
		t.Fatal("nil-engine poll must not touch watcher state")
	}
}

func TestPoll_SlugChangeResetsTrackingState(t *testing.T) {
	w, eng, _ := covFWatcher(t)
	if _, err := eng.Start("slug change idea"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Simulate a previous inception's residue.
	w.lastSlug = "previous-inception"
	w.lastQuestionCount = 9
	w.lastFactCount = 4
	w.kickRetryCount = 3
	w.plukMu.Lock()
	w.plukFactLines = []string{"stale"}
	w.plukLastActivity = time.Now().Add(-time.Hour)
	w.plukMu.Unlock()

	w.poll(context.Background())

	if w.lastSlug != eng.GetState().IdeaSlug {
		t.Fatalf("lastSlug not adopted: %q", w.lastSlug)
	}
	if w.lastQuestionCount != 0 || w.lastFactCount != 0 || w.kickRetryCount != 0 {
		t.Fatalf("counters not reset: q=%d f=%d retry=%d",
			w.lastQuestionCount, w.lastFactCount, w.kickRetryCount)
	}
	if w.captureSeenAt.IsZero() {
		t.Fatal("captureSeenAt must start on slug change")
	}
	w.plukMu.Lock()
	defer w.plukMu.Unlock()
	if w.plukFactLines != nil || !w.plukLastActivity.IsZero() {
		t.Fatal("pluk buffers not cleared on slug change")
	}
}

func TestPoll_CaptureBootstrapTimeoutGeneratesQuestions(t *testing.T) {
	w, eng := pollWatcherInCapture(t)
	// Past 2x the fallback timeout with zero pluk activity: the agent never
	// started, so the bootstrap branch must fire the question fallback.
	w.captureSeenAt = time.Now().Add(-2*autoQuestionFallbackTimeout - time.Minute)

	w.poll(context.Background())

	st := eng.GetState()
	if st == nil || st.Phase != knowledge.PhaseClarify {
		t.Fatalf("expected clarify after bootstrap fallback, got %+v", st)
	}
	if len(st.Questions) < minQuestionsForAdvance {
		t.Fatalf("expected >=%d auto questions, got %d", minQuestionsForAdvance, len(st.Questions))
	}
}

func TestPoll_CaptureBootstrapWithinGraceDefers(t *testing.T) {
	w, eng := pollWatcherInCapture(t)
	// Past the fallback timeout but NOT past 2x, with no pluk events yet:
	// the agent may still be bootstrapping, so nothing fires.
	w.captureSeenAt = time.Now().Add(-autoQuestionFallbackTimeout - 10*time.Second)

	w.poll(context.Background())

	st := eng.GetState()
	if st == nil || st.Phase != knowledge.PhaseCapture || len(st.Questions) != 0 {
		t.Fatalf("expected capture with no questions, got %+v", st)
	}
}

func TestPoll_CaptureIdleAgentGeneratesQuestions(t *testing.T) {
	w, eng := pollWatcherInCapture(t)
	w.captureSeenAt = time.Now().Add(-autoQuestionFallbackTimeout - 10*time.Second)
	// Agent produced output once, then went idle past the threshold.
	setPlukLastActivity(w, time.Now().Add(-agentIdleThreshold-10*time.Second))

	w.poll(context.Background())

	st := eng.GetState()
	if st == nil || st.Phase != knowledge.PhaseClarify {
		t.Fatalf("expected clarify after idle fallback, got %+v", st)
	}
}

func TestPoll_CaptureActiveAgentDefers(t *testing.T) {
	w, eng := pollWatcherInCapture(t)
	w.captureSeenAt = time.Now().Add(-autoQuestionFallbackTimeout - 10*time.Second)
	// Agent is actively producing output: fallback must keep waiting.
	setPlukLastActivity(w, time.Now().Add(-time.Second))

	w.poll(context.Background())

	st := eng.GetState()
	if st == nil || st.Phase != knowledge.PhaseCapture || len(st.Questions) != 0 {
		t.Fatalf("expected capture with no questions, got %+v", st)
	}
}

func TestPoll_StructureFirstSightStampsSeenAt(t *testing.T) {
	w, eng := pollWatcherInStructure(t)
	if !w.structureSeenAt.IsZero() {
		t.Fatal("precondition: structureSeenAt should start zero")
	}

	w.poll(context.Background())

	if w.structureSeenAt.IsZero() {
		t.Fatal("first structure poll must stamp structureSeenAt")
	}
	if st := eng.GetState(); st == nil || st.Phase != knowledge.PhaseStructure {
		t.Fatalf("first sight must not generate facts yet, got %+v", st)
	}
}

func TestPoll_StructureBootstrapTimeoutGeneratesFacts(t *testing.T) {
	w, eng := pollWatcherInStructure(t)
	w.structureSeenAt = time.Now().Add(-2*autoFactFallbackTimeout - time.Minute)

	w.poll(context.Background())

	st := eng.GetState()
	if st == nil || st.Phase != knowledge.PhaseScaffold {
		t.Fatalf("expected scaffold after fact fallback, got %+v", st)
	}
	if len(st.FactSlugs) == 0 {
		t.Fatal("expected auto-generated fact slugs")
	}
}

func TestPoll_StructureIdleAgentGeneratesFacts(t *testing.T) {
	w, eng := pollWatcherInStructure(t)
	w.structureSeenAt = time.Now().Add(-autoFactFallbackTimeout - 10*time.Second)
	setPlukLastActivity(w, time.Now().Add(-agentIdleThreshold-10*time.Second))

	w.poll(context.Background())

	if st := eng.GetState(); st == nil || st.Phase != knowledge.PhaseScaffold {
		t.Fatalf("expected scaffold after idle fact fallback, got %+v", st)
	}
}

func TestPoll_StructureActiveAgentDefers(t *testing.T) {
	w, eng := pollWatcherInStructure(t)
	w.structureSeenAt = time.Now().Add(-autoFactFallbackTimeout - 10*time.Second)
	setPlukLastActivity(w, time.Now().Add(-time.Second))

	w.poll(context.Background())

	if st := eng.GetState(); st == nil || st.Phase != knowledge.PhaseStructure {
		t.Fatalf("active agent must defer fact fallback, got %+v", st)
	}
}
