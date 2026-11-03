package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

func rfStatusManager(t *testing.T, backend string) *Manager {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: backend, Model: "m"},
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	m.agents["scanner"].UID = 0
	m.mu.Unlock()
	return m
}

func rfStatus(m *Manager) RepeatFailureStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.agents["scanner"].snapshot().RepeatFailure
}

func rfWriteFailures(t *testing.T, m *Manager, command string, code, n int) {
	t.Helper()
	path := m.repeatFailureRecordPaths()["scanner"]
	if path == "" {
		t.Fatal("no record path for scanner")
	}
	if err := EnsureRepeatFailureRecordDir(path, 0); err != nil {
		t.Fatal(err)
	}
	for range n {
		c := code
		rec := RepeatFailureRecord{Agent: "scanner", Backend: "claude", RawCommand: command, ExitCode: &c, Failed: c != 0, Route: RepeatFailureRouteBashEnv}
		if err := AppendRepeatFailureRecord(path, rec); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFormatRepeatFailureNote(t *testing.T) {
	code := 1
	now := time.Now()
	if got := FormatRepeatFailureNote(nil, 10*time.Minute); got != "" {
		t.Errorf("no notes: %q, want empty", got)
	}
	one := []RepeatFailureNote{{Command: "ls /nonexistent", Result: RepeatFailureResult{ExitCode: &code}, Count: 5, Latest: now}}
	if got, want := FormatRepeatFailureNote(one, 10*time.Minute), "Repeating failure: `ls /nonexistent` failed 5 times in 10 min (exit 1)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	noCode := []RepeatFailureNote{
		{Command: "npm test", Result: RepeatFailureResult{Failed: true}, Count: 6, Latest: now},
		{Command: "gh pr view 123", Result: RepeatFailureResult{ExitCode: &code}, Count: 5, Latest: now.Add(-time.Minute)},
		{Command: "gh pr view 456", Result: RepeatFailureResult{ExitCode: &code}, Count: 5, Latest: now.Add(-2 * time.Minute)},
	}
	if got, want := FormatRepeatFailureNote(noCode, 10*time.Minute), "Repeating failure: `npm test` failed 6 times in 10 min (failed) and 2 others"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := FormatRepeatFailureNote(noCode[:2], 10*time.Minute); !strings.HasSuffix(got, " and 1 other") {
		t.Errorf("two notes: %q, want the most recent and 1 other", got)
	}
	long := []RepeatFailureNote{{Command: strings.Repeat("x", 500), Result: RepeatFailureResult{Failed: true}, Count: 5}}
	if got := FormatRepeatFailureNote(long, 10*time.Minute); len([]rune(got)) > repeatFailureNoteCommandMax+80 {
		t.Errorf("long command not cut: %d runes", len([]rune(got)))
	}
}

// TestRepeatFailureStatus_OffMeansOff is C-AC12/C14: with the switch off no
// row has a check state or note, also after it was on.
func TestRepeatFailureStatus_OffMeansOff(t *testing.T) {
	m := rfStatusManager(t, "claude")
	now := time.Now()
	m.CollectRepeatFailures(now)
	if st := rfStatus(m); st != (RepeatFailureStatus{}) {
		t.Fatalf("no resolver: status = %+v, want none", st)
	}
	on := true
	m.SetRepeatFailureEnabledResolver(func() bool { return on })
	rfWriteFailures(t, m, "false", 1, 5)
	m.CollectRepeatFailures(now)
	if st := rfStatus(m); st.Check == "" || st.Note == "" {
		t.Fatalf("on: status = %+v, want a check state and a note", st)
	}
	on = false
	m.CollectRepeatFailures(now.Add(time.Second))
	if st := rfStatus(m); st != (RepeatFailureStatus{}) {
		t.Fatalf("switched off: status = %+v, want none", st)
	}
}

// TestRepeatFailureStatus_NotCoveredRowAndNote is C-AC9 and the C11 note: an
// agent on a backend that is not covered says so, and repeated failures that
// still reach Hive raise the note, which clears after the window.
func TestRepeatFailureStatus_NotCoveredRowAndNote(t *testing.T) {
	m := rfStatusManager(t, "claude")
	m.SetRepeatFailureEnabledResolver(func() bool { return true })
	m.SetRepeatFailureConfigResolver(func() *config.RepeatFailureConfig { return &config.RepeatFailureConfig{Enabled: true} })
	now := time.Now()

	m.CollectRepeatFailures(now)
	st := rfStatus(m)
	if st.Check != RepeatFailureCheckNotCovered || st.Check.Message() != "repeat-failure check not available for this backend" {
		t.Fatalf("claude (not covered): check = %q (%q)", st.Check, st.Check.Message())
	}
	if st.Note != "" {
		t.Fatalf("no failures yet: note = %q", st.Note)
	}

	rfWriteFailures(t, m, "ls   /nonexistent", 2, 4)
	m.CollectRepeatFailures(now.Add(time.Minute))
	if st := rfStatus(m); st.Note != "" {
		t.Fatalf("4 failures raised a note: %q", st.Note)
	}
	rfWriteFailures(t, m, "ls /nonexistent", 2, 1)
	m.CollectRepeatFailures(now.Add(2 * time.Minute))
	if st, want := rfStatus(m), "Repeating failure: `ls /nonexistent` failed 5 times in 10 min (exit 2)"; st.Note != want {
		t.Fatalf("note = %q, want %q", st.Note, want)
	}
	m.mu.RLock()
	paused := m.agents["scanner"].Paused
	m.mu.RUnlock()
	if paused {
		t.Fatal("a note must never pause the agent (C10)")
	}
	m.CollectRepeatFailures(now.Add(13 * time.Minute))
	if st := rfStatus(m); st.Note != "" {
		t.Fatalf("note must clear once the last failure is older than the window, got %q", st.Note)
	}
}

func TestRepeatFailureStatus_ExclusionsAndSuccessClear(t *testing.T) {
	m := rfStatusManager(t, "claude")
	m.SetRepeatFailureEnabledResolver(func() bool { return true })
	now := time.Now()
	rfWriteFailures(t, m, "gh pr checks 123 --watch", 1, 10)
	m.CollectRepeatFailures(now)
	if st := rfStatus(m); st.Note != "" {
		t.Fatalf("an excluded waiting loop raised %q", st.Note)
	}
	rfWriteFailures(t, m, "false", 1, 5)
	m.CollectRepeatFailures(now.Add(time.Second))
	if st := rfStatus(m); st.Note == "" {
		t.Fatal("5 failures raised no note")
	}
	rfWriteFailures(t, m, "false", 0, 1)
	m.CollectRepeatFailures(now.Add(2 * time.Second))
	if st := rfStatus(m); st.Note != "" {
		t.Fatalf("a success must clear the note, got %q", st.Note)
	}
}

// TestRepeatFailureSafeguard is C12/C-AC10, driven through
// ObserveRepeatFailureTool and evaluated as for a covered backend.
func TestRepeatFailureSafeguard(t *testing.T) {
	const noData = 15 * time.Minute
	kickAt := time.Now()
	setup := func(t *testing.T) *Manager {
		m := rfStatusManager(t, "claude")
		m.SetRepeatFailureEnabledResolver(func() bool { return true })
		m.mu.Lock()
		a := m.agents["scanner"]
		a.State = StateRunning
		k := kickAt
		a.LastKick = &k
		m.mu.Unlock()
		return m
	}
	state := func(m *Manager, now time.Time) RepeatFailureCheckState {
		m.mu.RLock()
		defer m.mu.RUnlock()
		m.repeatFailure.mu.Lock()
		defer m.repeatFailure.mu.Unlock()
		return repeatFailureCheckState(true, repeatFailureTurnKick(m.agents["scanner"]), m.repeatFailure.agents["scanner"], noData, now)
	}

	t.Run("shell activity without records", func(t *testing.T) {
		m := setup(t)
		at := kickAt.Add(time.Minute)
		m.ObserveRepeatFailureTool("scanner", RepeatFailureToolShell, false, at)
		m.ObserveRepeatFailureTool("scanner", RepeatFailureToolShell, true, at)
		if got := state(m, at.Add(14*time.Minute)); got != RepeatFailureCheckOn {
			t.Errorf("before no_data_after_s: %q, want on", got)
		}
		if got := state(m, at.Add(noData)); got != RepeatFailureCheckNoData {
			t.Errorf("after no_data_after_s: %q, want no_data", got)
		}
		if RepeatFailureCheckNoData.Message() != "repeat-failure check not receiving data" {
			t.Errorf("no_data message = %q", RepeatFailureCheckNoData.Message())
		}
		m.recordRepeatFailure(RepeatFailureRecord{Agent: "scanner", RawCommand: "true"}, at.Add(noData))
		if got := state(m, at.Add(noData+time.Minute)); got != RepeatFailureCheckOn {
			t.Errorf("a record must reset the safeguard: %q", got)
		}
	})
	t.Run("reads and edits only", func(t *testing.T) {
		m := setup(t)
		at := kickAt.Add(time.Minute)
		for _, kind := range []RepeatFailureToolKind{RepeatFailureToolFileRead, RepeatFailureToolFileEdit} {
			m.ObserveRepeatFailureTool("scanner", kind, false, at)
			m.ObserveRepeatFailureTool("scanner", kind, true, at)
		}
		if got := state(m, at.Add(noData+time.Minute)); got != RepeatFailureCheckOn {
			t.Errorf("file reads and edits alone: %q, want on", got)
		}
	})
	t.Run("one long-running command", func(t *testing.T) {
		m := setup(t)
		at := kickAt.Add(time.Minute)
		m.ObserveRepeatFailureTool("scanner", RepeatFailureToolShell, false, at)
		if got := state(m, at.Add(20*time.Minute)); got != RepeatFailureCheckOn {
			t.Errorf("a still-running command: %q, want on", got)
		}
	})
	t.Run("turn ended", func(t *testing.T) {
		m := setup(t)
		at := kickAt.Add(time.Minute)
		m.ObserveRepeatFailureTool("scanner", RepeatFailureToolShell, false, at)
		m.ObserveRepeatFailureTool("scanner", RepeatFailureToolShell, true, at)
		m.mu.Lock()
		m.agents["scanner"].KickOutcome = KickOutcome{Kind: KickOutcomeEnded, KickAt: kickAt, At: at}
		m.mu.Unlock()
		if got := state(m, at.Add(noData+time.Minute)); got != RepeatFailureCheckOn {
			t.Errorf("no turn in progress: %q, want on", got)
		}
	})
	t.Run("not covered wins", func(t *testing.T) {
		if got := repeatFailureCheckState(false, &kickAt, &repeatFailureAgentState{kick: kickAt, pendingSince: kickAt}, noData, kickAt.Add(time.Hour)); got != RepeatFailureCheckNotCovered {
			t.Errorf("not covered: %q", got)
		}
	})
}
