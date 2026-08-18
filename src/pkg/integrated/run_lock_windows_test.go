//go:build windows

package integrated

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestLegacyProductionRunOwnerUsesProcessStartIndependentOfExecutableName(t *testing.T) {
	if os.Getenv("HIVE_RUN_LOCK_TEST_CHILD") == "1" {
		time.Sleep(30 * time.Second)
		return
	}
	beforeStart := time.Now().UTC()
	child := exec.Command(os.Args[0], "-test.run=^TestLegacyProductionRunOwnerUsesProcessStartIndependentOfExecutableName$")
	child.Env = append(os.Environ(), "HIVE_RUN_LOCK_TEST_CHILD=1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()
	time.Sleep(100 * time.Millisecond)
	if !legacyProductionRunOwnerMatches(child.Process.Pid, time.Now().UTC()) {
		t.Fatal("current live owner was rejected because the test executable is not named hive")
	}
	if legacyProductionRunOwnerMatches(child.Process.Pid, beforeStart.Add(-10*time.Second)) {
		t.Fatal("reused live PID was accepted despite beginning after the legacy lease")
	}
}
