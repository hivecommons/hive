package dashboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

type fakeInceptionAgentManager struct {
	buffer       []string
	bufferErr    error
	sendErr      error
	restartErr   error
	sendCount    atomic.Int64
	restartCount atomic.Int64
	lastKick     string
}

func (m *fakeInceptionAgentManager) GetBufferOutput(string, int) ([]string, error) {
	return append([]string{}, m.buffer...), m.bufferErr
}

func (m *fakeInceptionAgentManager) SendKick(_ string, message string) error {
	m.sendCount.Add(1)
	m.lastKick = message
	return m.sendErr
}

func (m *fakeInceptionAgentManager) RestartWithBootstrap(context.Context, string, string) error {
	m.restartCount.Add(1)
	return m.restartErr
}

func withFastInceptionLoops(t *testing.T) {
	t.Helper()
	oldWatch, oldFast, oldPoll, oldSeek, oldLog := inceptionWatchIntervalS, inceptionFastPollInterval, plukSubscriberPollInterval, plukSubscriberSeekEnd, plukLogFile
	inceptionWatchIntervalS = 10 * time.Millisecond
	inceptionFastPollInterval = 10 * time.Millisecond
	plukSubscriberPollInterval = 5 * time.Millisecond
	plukSubscriberSeekEnd = false
	plukLogFile = func() string { return filepath.Join(t.TempDir(), "missing.jsonl") }
	t.Cleanup(func() {
		inceptionWatchIntervalS, inceptionFastPollInterval, plukSubscriberPollInterval, plukSubscriberSeekEnd, plukLogFile = oldWatch, oldFast, oldPoll, oldSeek, oldLog
	})
}

func TestInceptionWatcherRunPollsAndStopsOnCancel(t *testing.T) {
	withFastInceptionLoops(t)
	w, eng, _ := covFWatcher(t)
	if _, err := eng.Start("watcher loop idea"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		w.pollMu.Lock()
		gotSlug := w.lastSlug != ""
		w.pollMu.Unlock()
		if gotSlug {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
	w.pollMu.Lock()
	gotSlug := w.lastSlug
	w.pollMu.Unlock()
	if gotSlug == "" {
		t.Fatal("Run never reached poll loop")
	}
}

func TestRunPlukSubscriberReadsConfiguredLogAndHandlesEvent(t *testing.T) {
	withFastInceptionLoops(t)
	w, eng, _ := covFWatcher(t)
	if _, err := eng.Start("pluk subscriber idea"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	logPath := filepath.Join(t.TempDir(), "brainstorm.jsonl")
	line := `{"type":"raw_output","data":{"line":"bd create --title \"What language should we use?\""}}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0o644); err != nil {
		t.Fatalf("write pluk log: %v", err)
	}
	plukLogFile = func() string { return logPath }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.runPlukSubscriber(ctx)
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		w.plukMu.Lock()
		gotEvent := w.plukEventCount > 0
		w.plukMu.Unlock()
		if gotEvent {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runPlukSubscriber did not stop after context cancellation")
	}
	w.plukMu.Lock()
	eventCount := w.plukEventCount
	questions := append([]knowledge.Question(nil), w.plukQuestions...)
	w.plukMu.Unlock()
	if eventCount != 1 {
		t.Fatalf("expected one handled pluk event, got %d", eventCount)
	}
	if len(questions) != 1 || !strings.Contains(questions[0].Text, "language") {
		t.Fatalf("expected raw_output bd create question to be captured, got %+v", questions)
	}
}

func TestRetryKickIfStaleSendsKickWhenBrainstormIsReaping(t *testing.T) {
	w, _, _ := covFWatcher(t)
	fake := &fakeInceptionAgentManager{buffer: []string{"reap: close stale beads"}}
	w.agentMgr = fake
	state := &knowledge.InceptionState{
		Phase:     knowledge.PhaseCapture,
		IdeaText:  "recover stale kick",
		IdeaSlug:  "recover-stale-kick",
		StartedAt: time.Now().Add(-2 * kickRetryGracePeriodS),
	}
	w.lastKickRetry = time.Now().Add(-2 * kickRetryDelayS)

	w.retryKickIfStale(state)

	if fake.sendCount.Load() != 1 {
		t.Fatalf("expected SendKick once, got %d", fake.sendCount.Load())
	}
	if fake.restartCount.Load() != 0 {
		t.Fatalf("unexpected RestartWithBootstrap calls: %d", fake.restartCount.Load())
	}
	if w.kickRetryCount != 1 {
		t.Fatalf("kickRetryCount = %d, want 1", w.kickRetryCount)
	}
	if !strings.Contains(fake.lastKick, "recover stale kick") {
		t.Fatalf("kick message did not include idea text: %q", fake.lastKick)
	}
}

func TestRetryKickIfStaleFallsBackToBootstrapWhenSendKickFails(t *testing.T) {
	w, _, _ := covFWatcher(t)
	fake := &fakeInceptionAgentManager{
		bufferErr: errors.New("no session"),
		sendErr:   errors.New("send failed"),
	}
	w.agentMgr = fake
	state := &knowledge.InceptionState{
		Phase:     knowledge.PhaseStructure,
		IdeaText:  "recover missing session",
		IdeaSlug:  "recover-missing-session",
		StartedAt: time.Now().Add(-2 * kickRetryGracePeriodS),
		Questions: []knowledge.Question{{ID: "q1", Text: "language?", Category: "language"}},
		Answers:   map[string]string{"q1": "Go"},
	}
	w.lastKickRetry = time.Now().Add(-2 * kickRetryDelayS)

	w.retryKickIfStale(state)

	if fake.sendCount.Load() != 1 {
		t.Fatalf("expected SendKick once, got %d", fake.sendCount.Load())
	}
	if fake.restartCount.Load() != 1 {
		t.Fatalf("expected RestartWithBootstrap once, got %d", fake.restartCount.Load())
	}
}

func TestRetryKickIfStaleSuppressesWhileWorkingOrRateLimited(t *testing.T) {
	w, _, _ := covFWatcher(t)
	fake := &fakeInceptionAgentManager{buffer: []string{"extract requirements for inception"}}
	w.agentMgr = fake
	state := &knowledge.InceptionState{
		Phase:     knowledge.PhaseCapture,
		IdeaText:  "do not kick",
		StartedAt: time.Now().Add(-2 * kickRetryGracePeriodS),
	}
	w.lastKickRetry = time.Now().Add(-2 * kickRetryDelayS)

	w.retryKickIfStale(state)
	if fake.sendCount.Load() != 0 {
		t.Fatalf("working output should suppress kicks, got %d", fake.sendCount.Load())
	}

	fake.buffer = []string{"reap: close stale beads"}
	w.rateLimitedUntil = time.Now().Add(time.Minute)
	w.retryKickIfStale(state)
	if fake.sendCount.Load() != 0 {
		t.Fatalf("rate limit should suppress kicks, got %d", fake.sendCount.Load())
	}
}
