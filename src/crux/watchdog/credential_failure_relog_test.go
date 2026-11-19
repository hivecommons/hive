package watchdog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe log sink: the reconciler logs from restart
// goroutines too.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) count(sub string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Count(s.b.String(), sub)
}

// A copilot agent with no credential file reads CredentialMissing while its
// CLI is stopped and "no opinion" (Unknown) while it runs, so every relaunch
// re-enters False. The hive in hivecommons/hive#19205 logged that flap as 8
// ERRORs in 10m and self-reported it fleet-wide; one unchanged failure must
// produce one ERROR per credentialFailureRelogInterval.
func TestCredentialFailureFlapLogsOncePerWindow(t *testing.T) {
	clock := newFakeClock()
	fleet := newFakeFleet("redteam")
	alerter := newFakeAlerter()
	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r := New(fastSettings(), fleet, alerter, logger, WithClock(clock.Now))

	missing := readyObs()
	missing.Backend = "copilot"
	missing.AuthKnown, missing.AuthAvailable = true, false
	running := readyObs()
	running.Backend = "copilot" // AuthKnown=false: no opinion while running

	const errLine = `level=ERROR msg="watchdog: agent credential failure"`
	for i := 0; i < 8; i++ {
		fleet.setObs("redteam", missing)
		r.Tick(context.Background())
		auth, _ := FindCondition(r.Conditions("redteam"), ConditionAuthenticated)
		if auth.Status != ConditionFalse || auth.Reason != "CredentialMissing" {
			t.Fatalf("flap %d: Authenticated = %+v, want False/CredentialMissing", i, auth)
		}
		if !alerter.has(authAlertID("redteam")) {
			t.Fatalf("flap %d: the dashboard alert must still be raised", i)
		}
		fleet.setObs("redteam", running)
		r.Tick(context.Background())
		clock.Advance(time.Minute)
	}
	if got := buf.count(errLine); got != 1 {
		t.Fatalf("flapping CredentialMissing logged %d ERRORs, want 1", got)
	}
	if got := buf.count(`msg="watchdog: agent credential still failing"`); got != 7 {
		t.Fatalf("repeats logged %d INFO lines, want 7", got)
	}

	// Past the window the unchanged failure is logged at ERROR again.
	clock.Advance(credentialFailureRelogInterval)
	fleet.setObs("redteam", missing)
	r.Tick(context.Background())
	if got := buf.count(errLine); got != 2 {
		t.Fatalf("after the relog window: %d ERRORs, want 2", got)
	}

	// A recovery (True) resets the window: a fresh failure is news.
	present := missing
	present.AuthAvailable = true
	fleet.setObs("redteam", present)
	r.Tick(context.Background())
	if auth, _ := FindCondition(r.Conditions("redteam"), ConditionAuthenticated); auth.Status != ConditionTrue {
		t.Fatalf("credential present: Authenticated = %+v, want True", auth)
	}
	fleet.setObs("redteam", missing)
	r.Tick(context.Background())
	if got := buf.count(errLine); got != 3 {
		t.Fatalf("failure after recovery: %d ERRORs, want 3", got)
	}
}
