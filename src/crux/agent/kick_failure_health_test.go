package agent

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/internal/testutil"
)

func TestKickFailuresTrackPreflightAndSnapshot(t *testing.T) {
	m, a, _ := kickLogTestManager(t, "")
	oldExists := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return false }
	t.Cleanup(func() { tmuxSessionExists = oldExists })

	// Validation alone is not a send attempt.
	if err := m.ValidateKick(a.Name); err == nil {
		t.Fatal("expected missing session")
	}
	for i := 1; i <= 3; i++ {
		var err error
		if i == 2 {
			_, err = m.SendKickAsync(a.Name, "task")
		} else {
			err = m.SendKick(a.Name, "task")
		}
		if err == nil {
			t.Fatal("missing session was accepted")
		}
		status, statusErr := m.GetStatusFast(a.Name)
		if statusErr != nil || status.ConsecutiveKickFailures != i || status.LastKickError != err.Error() {
			t.Fatalf("status after failure %d = %+v, %v; send error %v", i, status, statusErr, err)
		}
	}
	status, err := m.GetStatus(a.Name)
	if err != nil || status.ConsecutiveKickFailures != 3 || status.LastKickError == "" {
		t.Fatalf("full snapshot after failures = %+v, %v", status, err)
	}

	// A synchronous successful write clears both fields, not just the count.
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
	termSeams(m).sendKeysErr = func(*AgentProcess, ...string) error { return nil }
	termSeams(m).sendLiteralErr = func(*AgentProcess, string) error { return nil }
	if err := m.SendKick(a.Name, "recovery"); err != nil {
		t.Fatal(err)
	}
	status, err = m.GetStatus(a.Name)
	if err != nil || status.ConsecutiveKickFailures != 0 || status.LastKickError != "" {
		t.Fatalf("sync recovery: %+v, %v", status, err)
	}
}

func TestKickFailureTrackingAsyncDeliveryAndRecovery(t *testing.T) {
	m, a, _ := kickLogTestManager(t, "")
	oldExists := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
	t.Cleanup(func() { tmuxSessionExists = oldExists })
	termSeams(m).sendKeysErr = func(*AgentProcess, ...string) error { return nil }
	termSeams(m).sendLiteralErr = func(*AgentProcess, string) error { return errors.New("transport unavailable") }

	for i := 1; i <= 3; i++ {
		started, err := m.SendKickAsync(a.Name, "task")
		if !started || err != nil {
			t.Fatalf("starting attempt %d: %v, %v", i, started, err)
		}
		d := testutil.EventuallyValue(t, 5*time.Second, func() (KickDispatch, bool) {
			d, ok := m.KickDispatchState(a.Name)
			return d, ok && !d.Pending()
		}, "delivery did not settle")
		status, err := m.GetStatusFast(a.Name)
		if err != nil || d.Phase != KickPhaseFailed || status.ConsecutiveKickFailures != i || status.LastKickError != d.Error {
			t.Fatalf("attempt %d: dispatch=%+v status=%+v error=%v", i, d, status, err)
		}
	}

	termSeams(m).sendLiteralErr = func(*AgentProcess, string) error { return nil }
	started, err := m.SendKickAsync(a.Name, "recovery")
	if !started || err != nil {
		t.Fatalf("starting recovery: %v, %v", started, err)
	}
	d := testutil.EventuallyValue(t, 5*time.Second, func() (KickDispatch, bool) {
		d, ok := m.KickDispatchState(a.Name)
		return d, ok && !d.Pending()
	}, "recovery did not settle")
	status, err := m.GetStatus(a.Name)
	if err != nil || d.Phase != KickPhaseDelivered || status.ConsecutiveKickFailures != 0 || status.LastKickError != "" {
		t.Fatalf("recovery: dispatch=%+v status=%+v error=%v", d, status, err)
	}
}

func TestKickFailuresSurviveHoldRestartAndDeferral(t *testing.T) {
	m, a, _ := kickLogTestManager(t, "")
	oldExists := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
	t.Cleanup(func() { tmuxSessionExists = oldExists })
	m.mu.Lock()
	recordKickFailureLocked(a, errors.New("original failure"))
	m.invalidateKicksOnRestartLocked(a, "test", false, false)
	a.kickHoldUntil = time.Now().Add(time.Minute)
	m.mu.Unlock()
	if err := m.SendKick(a.Name, "task"); !errors.Is(err, errKickHeldAfterRestart) {
		t.Fatalf("sync hold = %v", err)
	}
	if started, err := m.SendKickAsync(a.Name, "task"); started || !errors.Is(err, errKickHeldAfterRestart) {
		t.Fatalf("async hold = %v, %v", started, err)
	}
	m.mu.Lock()
	a.kickHoldUntil = time.Time{}
	a.startupLaunchQueued = true
	m.mu.Unlock()
	if err := m.SendKick(a.Name, "deferred"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	a.startupLaunchQueued = false
	m.mu.Unlock()
	if _, fresh := m.kickDispatches.begin(a.Name); !fresh {
		t.Fatal("could not set up pending dispatch")
	}
	if started, err := m.SendKickAsync(a.Name, "deduplicated"); started || err != nil {
		t.Fatalf("deduplicated = %v, %v", started, err)
	}
	status, err := m.GetStatus(a.Name)
	if err != nil || status.ConsecutiveKickFailures != 1 || status.LastKickError != "original failure" {
		t.Fatalf("non-deliveries changed streak: %+v, %v", status, err)
	}
}

func TestKickFailureIgnoresBusyAndCancellation(t *testing.T) {
	m, a, _ := kickLogTestManager(t, "")
	m.mu.Lock()
	recordKickFailureLocked(a, errors.New("original failure"))
	for range 3 {
		recordKickFailureLocked(a, fmt.Errorf("deferred: %w", crux.ErrAgentBusy))
		recordKickFailureLocked(a, errKickCancelledByRestart)
	}
	m.mu.Unlock()
	status, err := m.GetStatus(a.Name)
	if err != nil || status.ConsecutiveKickFailures != 1 || status.LastKickError != "original failure" {
		t.Fatalf("deferrals changed failure status = %+v, %v", status, err)
	}
}

func TestStartupKickFailureTracksOnlyCurrentLaunch(t *testing.T) {
	m, a, _ := kickLogTestManager(t, "")
	a.launchGen = 2
	m.recordStartupKickFailure(a, 1, errors.New("stale"))
	m.recordStartupKickFailure(a, 2, errors.New("startup timeout"))
	status, err := m.GetStatus(a.Name)
	if err != nil || status.ConsecutiveKickFailures != 1 || status.LastKickError != "startup timeout" {
		t.Fatalf("startup status = %+v, %v", status, err)
	}
}
