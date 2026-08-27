package hub

// save_loop_join_test.go pins the fix for kubestellar/hive#4774: the debounced
// registry-save goroutine used to be unjoinable, so a hub constructed in a test
// could wake up to registrySaveDelay after the test returned and recreate
// registry files inside a t.TempDir the test framework was already removing —
// the intermittent "TempDir RemoveAll cleanup: directory not empty" pkg/hub
// failure.

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStopSaveLoopJoinsAndFlushesPendingSave is the regression pin. It must:
//  1. return promptly (well inside registrySaveDelay — proving the debounce
//     sleep is cancelable, not merely waited out),
//  2. flush the pending save rather than abandon it, and
//  3. be idempotent.
//
// If StopSaveLoop ever stops joining the goroutine, the 2s timeout fails this
// test rather than letting the leak back in silently.
func TestStopSaveLoopJoinsAndFlushesPendingSave(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "save-loop-join-test-secret")
	oldRegistry := registryPath
	registryPath = filepath.Join(t.TempDir(), "hub-registry.json")
	t.Cleanup(func() { registryPath = oldRegistry })

	s := NewHubServer(0, slog.Default(), "test", "v2")
	s.mu.Lock()
	s.registry.Hives = []RegistryEntry{{ID: "pending-at-stop"}}
	s.mu.Unlock()
	s.requestSave()

	done := make(chan struct{})
	go func() {
		s.StopSaveLoop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("#4774: StopSaveLoop did not join the save loop promptly — the goroutine would outlive its test and race t.TempDir cleanup")
	}

	data, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatalf("the save pending at stop was abandoned instead of flushed: %v", err)
	}
	if !strings.Contains(string(data), "pending-at-stop") {
		t.Fatalf("flushed registry is missing the pending entry: %s", data)
	}

	// Idempotent: a second stop (e.g. explicit test cleanup after a Shutdown
	// that already stopped the loop) must neither panic nor block.
	s.StopSaveLoop()

	// Bare test literals never start the loop; StopSaveLoop must be nil-safe
	// for them.
	bare := &HubServer{logger: slog.Default()}
	bare.StopSaveLoop()
}
