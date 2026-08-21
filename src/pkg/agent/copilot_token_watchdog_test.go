package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

func TestCopilotTokenWatchdogInterval_Default(t *testing.T) {
	t.Setenv(CopilotTokenWatchdogIntervalEnv, "")
	if got := copilotTokenWatchdogInterval(); got != defaultCopilotTokenWatchdogInterval {
		t.Errorf("expected default %v, got %v", defaultCopilotTokenWatchdogInterval, got)
	}
}

func TestCopilotTokenWatchdogInterval_EnvOverride(t *testing.T) {
	t.Setenv(CopilotTokenWatchdogIntervalEnv, "30s")
	if got := copilotTokenWatchdogInterval(); got != 30*time.Second {
		t.Errorf("expected 30s from env override, got %v", got)
	}
}

func TestCopilotTokenWatchdogInterval_InvalidFallsBack(t *testing.T) {
	for _, bad := range []string{"not-a-duration", "-5m", "0s"} {
		t.Setenv(CopilotTokenWatchdogIntervalEnv, bad)
		if got := copilotTokenWatchdogInterval(); got != defaultCopilotTokenWatchdogInterval {
			t.Errorf("env %q: expected default %v, got %v", bad, defaultCopilotTokenWatchdogInterval, got)
		}
	}
}

// TestCopilotTokenMissing_NoCopilotBackendNotChecked proves the watchdog stays
// silent on Claude/gateway-only hives: a missing file there is not a fault, so
// checked must be false regardless of whether the file exists.
func TestCopilotTokenMissing_NoCopilotBackendNotChecked(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude"},
		"b": {Backend: "llm-d"},
	}, discardLogger(), ProjectContext{})

	// Even with a definitely-absent path, a non-copilot hive is not checked.
	missing, checked := m.copilotTokenMissingAt(filepath.Join(t.TempDir(), "nope"))
	if checked {
		t.Fatalf("non-copilot hive must not be checked; got checked=%v missing=%v", checked, missing)
	}
}

// TestCopilotTokenMissing_CopilotBackendDetectsAbsence proves that on a
// copilot-backend hive the watchdog reports missing when the durable file is
// absent, present when it holds a token, and missing again when it is empty.
func TestCopilotTokenMissing_CopilotBackendDetectsAbsence(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "copilot"},
		"quality": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	path := filepath.Join(t.TempDir(), "copilot-user-token")

	// Absent.
	if missing, checked := m.copilotTokenMissingAt(path); !checked || !missing {
		t.Fatalf("absent file on copilot hive: expected checked=true missing=true, got checked=%v missing=%v", checked, missing)
	}

	// Present with a token.
	if err := os.WriteFile(path, []byte("ghu_exampletoken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if missing, checked := m.copilotTokenMissingAt(path); !checked || missing {
		t.Fatalf("populated file: expected checked=true missing=false, got checked=%v missing=%v", checked, missing)
	}

	// Present but empty must count as missing (a zero-byte file is not a token).
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if missing, checked := m.copilotTokenMissingAt(path); !checked || !missing {
		t.Fatalf("empty file: expected checked=true missing=true, got checked=%v missing=%v", checked, missing)
	}
}

// TestCopilotTokenMissing_BackendOverrideCounts proves effectiveBackend is used
// (an agent overridden to copilot at runtime is watched even if its configured
// backend is something else).
func TestCopilotTokenMissing_BackendOverrideCounts(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	m.agents["a"].BackendOverride = "copilot"
	m.mu.Unlock()

	if _, checked := m.copilotTokenMissingAt(filepath.Join(t.TempDir(), "nope")); !checked {
		t.Fatal("agent overridden to copilot backend must be watched")
	}
}

// TestStartCopilotTokenWatchdog_AuditsOnMissing proves the running loop emits
// exactly one copilot_token_missing audit event on the present->missing
// transition (and does not re-emit every tick while the condition persists).
func TestStartCopilotTokenWatchdog_AuditsOnMissing(t *testing.T) {
	t.Setenv(CopilotTokenWatchdogIntervalEnv, "10ms")

	m, sink := testManagerWithSink(t)
	m.mu.Lock()
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.mu.Unlock()

	// The default production path does not exist under the test sandbox, so the
	// watchdog observes "missing" from the first tick.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		m.StartCopilotTokenWatchdog(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for sink.find(AuditCopilotTokenMissing) == nil {
		select {
		case <-deadline:
			cancel()
			t.Fatal("watchdog never emitted a copilot_token_missing audit within 2s")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Let several more ticks pass; the transition-tracked loop must NOT flood.
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	got := 0
	for _, e := range sink.entries {
		if e.Action == AuditCopilotTokenMissing {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("expected exactly 1 copilot_token_missing audit (transition-only), got %d", got)
	}
}
