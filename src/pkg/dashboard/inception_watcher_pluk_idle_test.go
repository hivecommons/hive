package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// inception_watcher_pluk_idle_test.go covers the previously-untested arms of
// InceptionWatcher.handlePlukEvent: the nil-state early return, the
// idle-during-structure fallback to autoGenerateFacts, and the idle kick-retry
// path (success, SendKick failure, and the maxKickRetries cap).
//
// Deliberately NOT covered here: the idle-during-structure branch with
// buffered plukFactLines. handlePlukEvent holds plukMu when it calls
// tryExtractFactsFromPluk, which re-locks the same non-reentrant plukMu —
// a test driving that branch self-deadlocks. That defect is tracked in its
// own issue; a test pinning the fixed behaviour belongs with the fix.

// plukIdleEvent is the state_change/idle event every retry-path test sends.
func plukIdleEvent() plukEvent {
	return plukEvent{Type: "state_change", Data: map[string]string{"state": "idle"}}
}

// fakeInceptionAgentMgr satisfies inceptionAgentManager and records SendKick
// calls on a channel so tests can join the re-kick goroutine without sleeps.
type fakeInceptionAgentMgr struct {
	kickErr error
	kicks   chan string // receives the agent name per SendKick call
}

func newFakeInceptionAgentMgr(kickErr error) *fakeInceptionAgentMgr {
	return &fakeInceptionAgentMgr{kickErr: kickErr, kicks: make(chan string, 4)}
}

func (f *fakeInceptionAgentMgr) GetBufferOutput(string, int) ([]string, error) { return nil, nil }
func (f *fakeInceptionAgentMgr) SendKick(name string, _ string) error {
	f.kicks <- name
	return f.kickErr
}
func (f *fakeInceptionAgentMgr) RestartWithBootstrap(context.Context, string, string) error {
	return nil
}

// structurePhaseWatcher builds a watcher whose engine is in the structure
// phase with the given questions answered, ready to drive the idle-in-structure
// branch of handlePlukEvent.
func structurePhaseWatcher(t *testing.T, questionCount int) (*InceptionWatcher, *knowledge.InceptionEngine) {
	t.Helper()
	w, eng, _ := covFWatcher(t)
	if _, err := eng.Start("structure idle fallback idea in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	questions := make([]knowledge.Question, 0, questionCount)
	answers := make(map[string]string, questionCount)
	for i := 0; i < questionCount; i++ {
		id := string(rune('a' + i))
		questions = append(questions, knowledge.Question{ID: id, Text: "Question " + id + "?"})
		answers[id] = "Answer " + id
	}
	if err := eng.SetQuestions(questions); err != nil {
		t.Fatalf("SetQuestions: %v", err)
	}
	if _, err := eng.SubmitAnswers(answers); err != nil {
		t.Fatalf("SubmitAnswers: %v", err)
	}
	if st := eng.GetState(); st == nil || st.Phase != knowledge.PhaseStructure {
		t.Fatalf("engine not in structure phase, state=%+v", eng.GetState())
	}
	return w, eng
}

// agedCaptureWatcher builds a watcher whose engine sits in the capture phase
// with StartedAt pushed past kickRetryGracePeriodS, by rewriting the persisted
// state file and reloading it through a fresh engine — the only supported way
// to age an inception, since GetState returns copies.
func agedCaptureWatcher(t *testing.T, mgr inceptionAgentManager) *InceptionWatcher {
	t.Helper()
	logger := covFWatcherLogger()
	dataDir := t.TempDir()
	kapi := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{}, logger)
	eng := knowledge.NewInceptionEngine(dataDir, kapi, logger)
	if _, err := eng.Start("kick retry idea in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	statePath := filepath.Join(dataDir, "inception", "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("reading persisted inception state: %v", err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("unmarshaling inception state: %v", err)
	}
	st["started_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
	aged, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshaling aged state: %v", err)
	}
	if err := os.WriteFile(statePath, aged, 0o644); err != nil {
		t.Fatalf("writing aged state: %v", err)
	}

	agedEng := knowledge.NewInceptionEngine(dataDir, kapi, logger)
	got := agedEng.GetState()
	if got == nil || got.Phase != knowledge.PhaseCapture {
		t.Fatalf("aged engine state = %+v, want capture phase", got)
	}
	if time.Since(got.StartedAt) <= kickRetryGracePeriodS {
		t.Fatalf("aged StartedAt %v is still inside the %v grace period", got.StartedAt, kickRetryGracePeriodS)
	}

	return &InceptionWatcher{
		inception: agedEng,
		agentMgr:  mgr,
		logger:    logger,
		ctx:       context.Background(),
	}
}

// TestPlukEventNilStateReturnsEarly pins the guard at the top of
// handlePlukEvent: with no inception in progress the event still counts as
// activity, but nothing below the guard runs — not even raw_output buffering.
func TestPlukEventNilStateReturnsEarly(t *testing.T) {
	w, eng, _ := covFWatcher(t)
	if st := eng.GetState(); st != nil {
		t.Fatalf("expected nil inception state, got %+v", st)
	}

	w.handlePlukEvent(plukEvent{
		Type: "raw_output",
		Data: map[string]string{"line": `bd create --title "Should not be buffered" --actor brainstorm`},
	})

	w.plukMu.Lock()
	events, buffered := w.plukEventCount, len(w.plukBdCreateLines)
	activity := w.plukLastActivity
	w.plukMu.Unlock()

	if events != 1 {
		t.Fatalf("plukEventCount = %d, want 1 — activity must be tracked before the state guard", events)
	}
	if activity.IsZero() {
		t.Fatal("plukLastActivity not set — idle detection loses its signal when state is nil")
	}
	if buffered != 0 {
		t.Fatalf("buffered %d bd create lines with nil state, want 0 — the nil-state guard did not return", buffered)
	}
}

// TestPlukIdleInStructureFallsBackToAutoFacts drives the idle-during-structure
// arm with no buffered fact lines: the watcher must mark plukIdleInStructure
// and immediately auto-generate facts from Q&A, which RecordFacts turns into a
// scaffold-phase advance (vision + 3 answered questions ≥ minFactsForAdvance).
func TestPlukIdleInStructureFallsBackToAutoFacts(t *testing.T) {
	w, eng := structurePhaseWatcher(t, 3)

	w.handlePlukEvent(plukIdleEvent())

	w.plukMu.Lock()
	idleInStructure := w.plukIdleInStructure
	w.plukMu.Unlock()
	if !idleInStructure {
		t.Fatal("plukIdleInStructure not set by an idle event during structure phase")
	}

	st := eng.GetState()
	if st == nil {
		t.Fatal("inception state vanished")
	}
	if st.Phase != knowledge.PhaseScaffold {
		t.Fatalf("phase = %s, want %s — autoGenerateFacts fallback did not record facts", st.Phase, knowledge.PhaseScaffold)
	}
	if st.AutoFactCount == 0 {
		t.Fatal("AutoFactCount = 0, want > 0 — fallback facts were not counted as auto-generated")
	}
}

// TestPlukIdleKickRetriesAfterGracePeriod pins the re-kick path: an idle event
// in capture phase past the grace period must bump kickRetryCount, stamp
// lastKickRetry, and send exactly one kick to the brainstorm agent.
func TestPlukIdleKickRetriesAfterGracePeriod(t *testing.T) {
	mgr := newFakeInceptionAgentMgr(nil)
	w := agedCaptureWatcher(t, mgr)

	w.handlePlukEvent(plukIdleEvent())

	select {
	case name := <-mgr.kicks:
		if name != "brainstorm" {
			t.Fatalf("SendKick target = %q, want %q", name, "brainstorm")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle past the grace period in capture phase never re-kicked the agent")
	}

	w.plukMu.Lock()
	retries, lastRetry := w.kickRetryCount, w.lastKickRetry
	w.plukMu.Unlock()
	if retries != 1 {
		t.Fatalf("kickRetryCount = %d, want 1", retries)
	}
	if lastRetry.IsZero() {
		t.Fatal("lastKickRetry not stamped")
	}
}

// TestPlukIdleKickRetrySendFailureIsNonFatal pins the goroutine's error arm:
// a failing SendKick is logged and swallowed — the retry still counts, and the
// watcher keeps functioning for the next event.
func TestPlukIdleKickRetrySendFailureIsNonFatal(t *testing.T) {
	mgr := newFakeInceptionAgentMgr(errors.New("tmux pane gone"))
	w := agedCaptureWatcher(t, mgr)

	w.handlePlukEvent(plukIdleEvent())

	select {
	case <-mgr.kicks:
	case <-time.After(5 * time.Second):
		t.Fatal("re-kick goroutine never called SendKick")
	}

	w.plukMu.Lock()
	retries := w.kickRetryCount
	w.plukMu.Unlock()
	if retries != 1 {
		t.Fatalf("kickRetryCount = %d, want 1 — a failed kick must still consume a retry", retries)
	}

	// The watcher must survive the failure: the next idle event retries again.
	w.handlePlukEvent(plukIdleEvent())
	select {
	case <-mgr.kicks:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher stopped retrying after a SendKick failure")
	}
}

// TestPlukIdleKickRetryStopsAtMaxRetries pins the retry cap: once
// kickRetryCount reaches maxKickRetries, an idle event must not kick again.
func TestPlukIdleKickRetryStopsAtMaxRetries(t *testing.T) {
	mgr := newFakeInceptionAgentMgr(nil)
	w := agedCaptureWatcher(t, mgr)
	w.plukMu.Lock()
	w.kickRetryCount = maxKickRetries
	w.plukMu.Unlock()

	w.handlePlukEvent(plukIdleEvent())

	w.plukMu.Lock()
	retries := w.kickRetryCount
	w.plukMu.Unlock()
	if retries != maxKickRetries {
		t.Fatalf("kickRetryCount = %d, want %d — the cap was ignored", retries, maxKickRetries)
	}
	select {
	case <-mgr.kicks:
		t.Fatal("SendKick called past maxKickRetries")
	default:
	}
}
