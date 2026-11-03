package agent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// resumeTestManager is kickLogTestManager with a delivered kick on record
// (so the agent HAS a session) and a recorder for everything typed.
func resumeTestManager(t *testing.T) (*Manager, *AgentProcess, *[]string) {
	t.Helper()
	m, agent, _ := kickLogTestManager(t, "")
	agent.Config.ClearOnKick = true
	kicked := time.Now().Add(-time.Minute)
	agent.LastKick = &kicked
	var typed []string
	termSeams(m).sendLiteral = func(_ *AgentProcess, text string) { typed = append(typed, text) }
	termSeams(m).sendKeys = func(_ *AgentProcess, keys ...string) {}
	orig := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
	t.Cleanup(func() { tmuxSessionExists = orig })
	return m, agent, &typed
}

func TestSessionID_IdentifiesTheLiveConversation(t *testing.T) {
	m, agent, _ := resumeTestManager(t)

	if _, ok := m.SessionID("nobody"); ok {
		t.Fatal("unknown agent must have no session")
	}
	id, ok := m.SessionID("scanner")
	if !ok || !strings.HasPrefix(id, sessionBootNonce+":") {
		t.Fatalf("SessionID = %q, %v; want boot-nonce-prefixed id", id, ok)
	}

	// Every event that can replace or clear the conversation must change it.
	agent.kickEpoch++
	afterRestart, _ := m.SessionID("scanner")
	if afterRestart == id {
		t.Fatal("a restart teardown (kickEpoch) must change the session id")
	}
	later := agent.LastKick.Add(time.Second)
	agent.LastKick = &later
	afterKick, _ := m.SessionID("scanner")
	if afterKick == afterRestart {
		t.Fatal("a newer kick must change the session id")
	}
	origNonce := sessionBootNonce
	sessionBootNonce = newSessionBootNonce()
	t.Cleanup(func() { sessionBootNonce = origNonce })
	afterBoot, _ := m.SessionID("scanner")
	if afterBoot == afterKick {
		t.Fatal("a new process (boot nonce) must change the session id")
	}

	agent.LastKick = nil
	if _, ok := m.SessionID("scanner"); ok {
		t.Fatal("an agent never kicked has no conversation to resume")
	}
	agent.LastKick = &later
	agent.State = StateStopped
	if _, ok := m.SessionID("scanner"); ok {
		t.Fatal("a stopped agent has no live session")
	}
}

func TestSendResumeKick_DeliversWithoutClear(t *testing.T) {
	m, agent, typed := resumeTestManager(t)
	id, _ := m.SessionID("scanner")
	before := agent.LastKick

	if err := m.SendResumeKick("scanner", "follow up on PR 7", id); err != nil {
		t.Fatalf("SendResumeKick: %v", err)
	}
	for _, s := range *typed {
		if s == "/clear" {
			t.Fatalf("resume kick typed /clear, destroying the authoring context: %q", *typed)
		}
	}
	if !strings.Contains(strings.Join(*typed, ""), "follow up on PR 7") {
		t.Fatalf("follow-up text not delivered: %q", *typed)
	}
	if agent.LastKick == before {
		t.Fatal("delivery must be recorded (LastKick)")
	}
	if agent.resumeSkipClear {
		t.Fatal("one-shot skip-clear flag must be consumed by the delivery")
	}

	// The flag must not leak: the next ordinary kick clears as configured.
	*typed = nil
	m.deliverKickLocked(agent, "new work", "send-kick")
	cleared := false
	for _, s := range *typed {
		if s == "/clear" {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("ordinary kick after a resume must still /clear: %q", *typed)
	}
}

func TestSendResumeKick_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m *Manager, a *AgentProcess)
		agent   string
		session func(id string) string
		want    error
	}{
		{name: "unknown agent", agent: "nobody", want: ErrResumeSessionGone},
		{name: "session moved on", session: func(string) string { return "stale" }, want: ErrResumeSessionGone},
		{name: "paused", mutate: func(_ *Manager, a *AgentProcess) { a.Paused = true }, want: ErrResumeUnsupported},
		{name: "provider backoff", mutate: func(_ *Manager, a *AgentProcess) {
			a.ProviderErrorBackoffUntil = time.Now().Add(time.Hour)
		}, want: ErrResumeBusy},
		{name: "restart hold", mutate: func(_ *Manager, a *AgentProcess) {
			a.kickHoldUntil = time.Now().Add(time.Hour)
		}, want: ErrResumeBusy},
		{name: "tmux gone", mutate: func(*Manager, *AgentProcess) {
			tmuxSessionExists = func(*Manager, *AgentProcess) bool { return false }
		}, want: ErrResumeSessionGone},
		{name: "cli crashed", mutate: func(m *Manager, _ *AgentProcess) {
			termSeams(m).captureVisiblePane = func(*AgentProcess) string { return "user@pod:~$" }
		}, want: ErrResumeSessionGone},
		{name: "mid-turn", mutate: func(m *Manager, _ *AgentProcess) {
			termSeams(m).captureVisiblePane = func(*AgentProcess) string { return "❯\n◉ Working" }
		}, want: ErrResumeBusy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, agent, typed := resumeTestManager(t)
			id, _ := m.SessionID("scanner")
			if tc.mutate != nil {
				tc.mutate(m, agent)
			}
			name := "scanner"
			if tc.agent != "" {
				name = tc.agent
			}
			if tc.session != nil {
				id = tc.session(id)
			}
			err := m.SendResumeKick(name, "follow up", id)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(*typed) != 0 {
				t.Fatalf("a refused resume must type nothing, typed %q", *typed)
			}
			if agent.resumeSkipClear {
				t.Fatal("a refused resume must not arm skip-clear")
			}
		})
	}
}

func TestNewSessionBootNonce_Unique(t *testing.T) {
	if a, b := newSessionBootNonce(), newSessionBootNonce(); a == "" || a == b {
		t.Fatalf("boot nonces must be non-empty and distinct: %q %q", a, b)
	}
}

// RecordAudit writes through the manager's sink as the hive itself, and is a
// no-op on a nil manager or with no sink installed.
func TestRecordAudit_UsesManagerSink(t *testing.T) {
	m, sink := testManagerWithSink(t)
	m.RecordAudit("pr_followup_routed", "scanner", map[string]any{"outcome": "resumed"})
	got := sink.find("pr_followup_routed")
	if got == nil || got.Actor != auditActorSystem || got.Agent != "scanner" || got.Fields["outcome"] != "resumed" {
		t.Fatalf("recorded = %+v, want a system-attributed pr_followup_routed for scanner", got)
	}
	var nilMgr *Manager
	nilMgr.RecordAudit("pr_followup_routed", "scanner", nil) // must not panic
	m.SetAuditSink(nil)
	m.RecordAudit("pr_followup_routed", "scanner", nil)
	if sink.count() != 1 {
		t.Fatalf("events after sink removal = %d, want 1", sink.count())
	}
}
