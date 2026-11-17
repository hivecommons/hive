package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/pkg/research"
)

// End-to-end guard for "research never ready": a research request runs
// through the REAL research-pool startup path — research.NewPool over a real
// *Manager, Warm and Dispatch from Stopped, a real tmux session — against a
// fake Claude Code pane that opens on the first-run security notice, exactly
// as a freshly provisioned research HOME did live. The kick must be delivered
// with no "did not reach input prompt" failure along the way.
//
// It runs on real time: the pool's clock and its coldStartSettle (20s) are
// unexported, so the test takes a little over that.

// researchE2ETimeout bounds the whole request: coldStartSettle plus delivery.
const researchE2ETimeout = 90 * time.Second

// firstRunFakeCLI is a scripted Claude Code pane behind the terminal seam:
// a shell until the launch line is submitted, then the first-run notice until
// Enter, then the idle input prompt that records whatever is typed into it.
type firstRunFakeCLI struct {
	mu           sync.Mutex
	launched     bool // the launch line was typed
	state        string
	noticeEnters int
	typed        []string // literal text typed at the input prompt
	submitted    []string // typed text an Enter submitted
	pending      strings.Builder
}

const (
	fakeCLIShell  = "shell"
	fakeCLINotice = "notice"
	fakeCLIReady  = "ready"
)

func newFirstRunFakeCLI() *firstRunFakeCLI { return &firstRunFakeCLI{state: fakeCLIShell} }

func (f *firstRunFakeCLI) capture(*AgentProcess) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.state {
	case fakeCLINotice:
		return claudeFirstRunNoticePane
	case fakeCLIReady:
		return firstRunIdlePane
	default:
		return "$ "
	}
}

func (f *firstRunFakeCLI) sendLiteral(_ *AgentProcess, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch f.state {
	case fakeCLIShell:
		f.launched = true
	case fakeCLIReady:
		f.typed = append(f.typed, text)
		f.pending.WriteString(text)
	}
}

func (f *firstRunFakeCLI) sendKeys(_ *AgentProcess, keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range keys {
		if key != "Enter" {
			continue
		}
		switch f.state {
		case fakeCLIShell:
			if f.launched {
				f.state = fakeCLINotice
			}
		case fakeCLINotice:
			f.noticeEnters++
			f.state = fakeCLIReady
		case fakeCLIReady:
			if f.pending.Len() > 0 {
				f.submitted = append(f.submitted, f.pending.String())
				f.pending.Reset()
			}
		}
	}
}

func (f *firstRunFakeCLI) snapshot() (noticeEnters int, submitted []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.noticeEnters, append([]string(nil), f.submitted...)
}

func TestResearchPool_E2E_FirstRunNoticeStillDeliversKick(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())

	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{ACMMLevel: 6})
	fake := newFirstRunFakeCLI()
	ft := termSeams(m)
	ft.capturePane = fake.capture
	ft.captureVisiblePane = fake.capture
	ft.sendLiteral = fake.sendLiteral
	ft.sendKeys = fake.sendKeys
	ft.sleepDuringPromptDismiss = func(time.Duration) {}

	// Every runtime kick event, so a prompt-not-ready failure the pool hides
	// behind its retry is still caught.
	var evMu sync.Mutex
	var runtimeEvents []crux.KickEvent
	removeRuntime := m.AddKickObserver(func(ev crux.KickEvent) {
		evMu.Lock()
		runtimeEvents = append(runtimeEvents, ev)
		evMu.Unlock()
	})
	defer removeRuntime()

	pool, err := research.NewPool(m, 1)
	if err != nil {
		t.Fatalf("NewPool over a real Manager: %v", err)
	}
	var poolEvents []research.Event
	removePool := pool.Observe(func(ev research.Event) {
		evMu.Lock()
		poolEvents = append(poolEvents, ev)
		evMu.Unlock()
	})
	defer removePool()

	pool.Ensure(config.AgentConfig{Backend: "claude"})
	const name = research.NamePrefix + "1"
	forceSharedUID(t, m, name)
	defer cleanupAgent(t, m, name)

	ctx, cancel := context.WithTimeout(context.Background(), researchE2ETimeout)
	defer cancel()

	// Warm starts the stopped agent in the background; wait for it to run
	// before dispatching so Dispatch does not race Warm's launch.
	pool.Warm(ctx)
	for {
		st, err := m.GetStatusFast(name)
		if err == nil && st != nil && st.State == crux.StateRunning && !st.Starting {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("research agent never started from Stopped (last status %+v, err %v)", st, err)
		}
		time.Sleep(inputPromptPollInterval)
	}

	const query = "what is the CNCF sandbox?"
	id, err := pool.Dispatch(ctx, query, map[string]string{"meeting": "e2e"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	delivered := func() bool {
		evMu.Lock()
		defer evMu.Unlock()
		for _, ev := range poolEvents {
			if ev.KickID == id && ev.Kind == research.EventRunning {
				return true
			}
		}
		return false
	}
	wasDelivered := delivered()
	for !wasDelivered && ctx.Err() == nil {
		time.Sleep(inputPromptPollInterval)
		wasDelivered = delivered()
	}

	evMu.Lock()
	defer evMu.Unlock()
	for _, ev := range runtimeEvents {
		if ev.Event == crux.KickEventFailed && strings.Contains(ev.Detail, "did not reach input prompt") {
			t.Errorf("runtime reported a prompt-not-ready failure: %+v", ev)
		}
	}
	for _, ev := range poolEvents {
		if ev.KickID == id && ev.Kind == research.EventFailed {
			t.Errorf("pool reported the research request failed: %q", ev.Text)
		}
	}
	if !wasDelivered {
		t.Fatalf("research kick %s never delivered within %v; pool events %+v, runtime events %+v",
			id, researchE2ETimeout, poolEvents, runtimeEvents)
	}

	noticeEnters, submitted := fake.snapshot()
	if noticeEnters == 0 {
		t.Error("the pane never left the first-run notice; the test did not exercise it")
	}
	found := false
	for _, s := range submitted {
		if strings.Contains(s, "Question: "+query) {
			found = true
		}
	}
	if !found {
		t.Errorf("the research prompt was never typed and submitted; submitted = %q", submitted)
	}
}
