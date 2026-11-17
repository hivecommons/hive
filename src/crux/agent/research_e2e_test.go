package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

// End-to-end guard for "research never ready": a meeting research request
// runs through the REAL runtime startup path the research pool
// (hive's pkg/research) drives — a runtime-added, on-demand, ADVISORY agent
// registered with AddAgent, started from Stopped, then a queued kick — on a
// real *Manager and a real tmux session, against a fake Claude Code pane
// that opens on the first-run security notice, exactly as a freshly
// provisioned research HOME did live. The kick must be delivered with no
// "did not reach input prompt" failure along the way. The crux module may
// not import pkg/research (crux outbound ratchet), so this test makes the
// pool's runtime calls itself; pkg/research's tests cover the pool's own
// cold-start wait and retry.

// researchE2ETimeout bounds the whole request: start, the notice and delivery.
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

	// The pool's agent definition (research.AgentConfig): on demand,
	// read-only, no repos.
	noRepos := false
	const name = "research-1"
	m.AddAgent(name, config.AgentConfig{Backend: "claude", Role: "research", Mode: "ADVISORY", OnDemand: true, IncludeRepos: &noRepos})
	forceSharedUID(t, m, name)
	defer cleanupAgent(t, m, name)

	ctx, cancel := context.WithTimeout(context.Background(), researchE2ETimeout)
	defer cancel()

	// Pool.Warm / Dispatch start a stopped agent with Start.
	if err := m.Start(ctx, name); err != nil {
		t.Fatalf("Start from Stopped: %v", err)
	}
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
	const id = "rsch-e2e0000000000001"
	if _, err := m.EnqueueKick(name, id, "Research this.\n\nQuestion: "+query); err != nil {
		t.Fatalf("EnqueueKick: %v", err)
	}

	delivered := func() bool {
		evMu.Lock()
		defer evMu.Unlock()
		for _, ev := range runtimeEvents {
			if ev.KickID == id && ev.Event == crux.KickEventDelivered {
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
	if !wasDelivered {
		t.Fatalf("research kick %s never delivered within %v; runtime events %+v", id, researchE2ETimeout, runtimeEvents)
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
