package agent

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/crux/testutil"
	"github.com/hivecommons/hive/pkg/crux/types"
)

// A ready pane is not proof that tmux accepted the kick. Failed writes must
// reach the caller and the existing undeliverable tracker, never LastKick.
func TestKickDeliveryWriteFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key       string
		literalAt int
		clear     bool
		message   string
	}{
		{name: "interrupt", key: "C-c", message: "task"},
		{name: "clear input", key: "C-u", message: "task"},
		{name: "clear command", literalAt: 1, clear: true, message: "task"},
		{name: "clear submit", key: "Enter", clear: true, message: "task"},
		{name: "short prompt", literalAt: 1, message: "task"},
		{name: "second chunk", literalAt: 2, message: strings.Repeat("x", 3*chunkSize)},
		{name: "submit", key: "Enter", message: "task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, a, _ := kickLogTestManager(t, "")
			a.Config.ClearOnKick = tc.clear
			prior := time.Now().Add(-time.Hour)
			a.LastKick, a.LastKickMessage = &prior, "previous task"
			writeErr := errors.New("tmux transport unavailable")
			failed, literals := false, 0
			checkNoFurtherWrites := func() {
				if failed {
					t.Fatal("continued typing or submitted a partial prompt after a failed write")
				}
			}
			termSeams(m).sendLiteralErr = func(*AgentProcess, string) error {
				checkNoFurtherWrites()
				literals++
				if literals == tc.literalAt {
					failed = true
					return writeErr
				}
				return nil
			}
			termSeams(m).sendKeysErr = func(_ *AgentProcess, keys ...string) error {
				checkNoFurtherWrites()
				if len(keys) == 1 && keys[0] == tc.key {
					failed = true
					return writeErr
				}
				return nil
			}
			m.mu.Lock()
			err := m.deliverKickLocked(a, tc.message, "send-kick")
			m.mu.Unlock()
			if !errors.Is(err, writeErr) {
				t.Fatalf("delivery error = %v, want transport error", err)
			}
			if a.LastKick != &prior || a.LastKickMessage != "previous task" || len(a.KickHistory) != 0 {
				t.Fatal("failed delivery changed successful-kick bookkeeping")
			}
			if a.KicksUndeliverable != 1 || a.kickDelivering.Load() {
				t.Fatalf("undeliverable = %d, delivery claim = %v", a.KicksUndeliverable, a.kickDelivering.Load())
			}
		})
	}
}

// A failed insurance Enter after the first was accepted means the prompt was
// submitted: the kick is recorded as delivered, not re-kicked as undeliverable.
func TestKickFailedRepeatEnterStillCountsAsDelivered(t *testing.T) {
	m, a, _ := kickLogTestManager(t, "")
	enters := 0
	termSeams(m).sendKeysErr = func(_ *AgentProcess, keys ...string) error {
		if len(keys) == 1 && keys[0] == "Enter" {
			enters++
			if enters > 1 {
				return errors.New("tmux transport unavailable")
			}
		}
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.deliverKickLocked(a, "task", "send-kick"); err != nil {
		t.Fatalf("delivery error = %v after the first Enter was accepted", err)
	}
	if enters != 2 || a.KicksUndeliverable != 0 || a.LastKick == nil || len(a.KickHistory) != 1 {
		t.Fatalf("enters=%d undeliverable=%d lastKick=%v history=%d", enters, a.KicksUndeliverable, a.LastKick, len(a.KickHistory))
	}
}

func TestSendKickPropagatesFinalDeliveryFailure(t *testing.T) {
	origExists := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
	t.Cleanup(func() { tmuxSessionExists = origExists })
	for _, async := range []bool{false, true} {
		for _, cause := range []string{"write", "pane changed", "already delivering"} {
			t.Run(cause+map[bool]string{false: "/sync", true: "/async"}[async], func(t *testing.T) {
				m, a, _ := kickLogTestManager(t, "")
				captures := 0
				termSeams(m).captureVisiblePane = func(*AgentProcess) string {
					captures++
					// Crash detection and the prompt wait both succeed; only
					// the final, locked readiness check sees the changed pane.
					if cause == "pane changed" && captures >= 3 {
						return "shell$"
					}
					return "❯"
				}
				if cause == "already delivering" {
					a.kickDelivering.Store(true)
				}
				termSeams(m).sendLiteralErr = func(*AgentProcess, string) error {
					return errors.New("literal send failed")
				}
				if async {
					started, err := m.SendKickAsync(a.Name, "task")
					if !started || err != nil {
						t.Fatalf("queue delivery = %v, %v", started, err)
					}
					d := testutil.EventuallyValue(t, 5*time.Second, func() (KickDispatch, bool) {
						d, _ := m.KickDispatchState(a.Name)
						return d, !d.Pending()
					}, "delivery did not settle")
					if d.Phase != KickPhaseFailed || d.Error == "" {
						t.Fatalf("dispatch phase = %q, want failed", d.Phase)
					}
				} else if err := m.SendKick(a.Name, "task"); err == nil {
					t.Fatal("SendKick returned success without delivering")
				}
				m.mu.Lock()
				defer m.mu.Unlock()
				if a.LastKick != nil || len(a.KickHistory) != 0 {
					t.Fatal("rejected delivery recorded a successful kick")
				}
			})
		}
	}
}

func TestKickWriteFailuresReportAndClearOnRecovery(t *testing.T) {
	m, a, _ := kickLogTestManager(t, "")
	tr := recordConditions(t, m)
	termSeams(m).sendLiteralErr = func(*AgentProcess, string) error { return errors.New("send failed") }
	m.mu.Lock()
	defer m.mu.Unlock()
	// Decision B on #12787: write failures only self-report once the
	// busy-no-activity restart has already been attempted for this episode.
	m.markBusyNoActivityRestartLocked(a, time.Now().Add(-time.Minute))
	for i := 0; i < 3; i++ {
		if err := m.deliverKickLocked(a, "task", "send-kick"); err == nil {
			t.Fatal("failed send reported success")
		}
	}
	found := false
	for _, ev := range tr.active(types.ClassKickFailedRepeatedly) {
		if strings.Contains(ev.Message, "send text") {
			found = true
		}
	}
	if !found || a.KicksUndeliverable != 3 {
		t.Fatalf("missing repeated delivery failure: count=%d, evidence=%+v", a.KicksUndeliverable, tr.observed)
	}
	termSeams(m).sendLiteralErr = nil
	if err := m.deliverKickLocked(a, "task", "send-kick"); err != nil {
		t.Fatal(err)
	}
	if ev := tr.active(types.ClassKickFailedRepeatedly); len(ev) != 0 {
		t.Fatalf("successful retry left repeated-failure evidence active: %+v", ev)
	}
	if a.KicksUndeliverable != 0 || !a.busyNoActivityRestartAt.IsZero() || a.LastKick == nil || len(a.KickHistory) != 1 {
		t.Fatal("successful retry did not clear failures and the stall-restart marker and record exactly one kick")
	}
}

func TestKickCancelledMidSendIsNotDelivered(t *testing.T) {
	m, a, _ := kickLogTestManager(t, "")
	writes := 0
	termSeams(m).sendLiteral = func(*AgentProcess, string) {
		writes++
		// Simulate the epoch invalidation that a restart can perform while
		// delivery releases the lock between chunks.
		a.kickEpoch++
	}
	termSeams(m).sendKeys = func(_ *AgentProcess, keys ...string) {
		if len(keys) == 1 && keys[0] == "Enter" {
			t.Fatal("submitted an abandoned partial prompt")
		}
	}
	m.mu.Lock()
	err := m.deliverKickLocked(a, strings.Repeat("x", 2*chunkSize), "send-kick")
	m.mu.Unlock()
	if !errors.Is(err, errKickCancelledByRestart) || writes != 1 {
		t.Fatalf("error=%v, chunks=%d", err, writes)
	}
	if a.LastKick != nil || a.KicksUndeliverable != 0 || a.kickDelivering.Load() {
		t.Fatal("cancelled delivery recorded success, blamed the new session, or retained the pane claim")
	}
}
