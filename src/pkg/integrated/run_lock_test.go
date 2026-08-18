package integrated

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestProductionRunLeasePreventsOverlapAndReleases(t *testing.T) {
	stateDir := t.TempDir()
	release, err := acquireProductionRunLease(stateDir, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireProductionRunLease(stateDir, time.Minute); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("overlapping run was not rejected: %v", err)
	}
	release()
	releaseAgain, err := acquireProductionRunLease(stateDir, time.Minute)
	if err != nil {
		t.Fatalf("released lease could not be reclaimed: %v", err)
	}
	releaseAgain()
}

func TestProductionRunLeaseRecoversExpiredOwner(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expired := productionRunLease{
		SchemaVersion: "hive.production-run-lease.v1", ID: "expired", PID: 1 << 30,
		StartedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
	}
	data, _ := json.Marshal(expired)
	if err := os.WriteFile(filepath.Join(dir, "production-run.lock"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := acquireProductionRunLease(stateDir, time.Minute)
	if err != nil {
		t.Fatalf("expired lease was not recovered: %v", err)
	}
	release()
}

func TestProductionRunLeaseRecoversDeadOwnerBeforeExpiry(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dead := productionRunLease{
		SchemaVersion: "hive.production-run-lease.v1", ID: "dead", PID: 1 << 30,
		StartedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour),
	}
	data, _ := json.Marshal(dead)
	if err := os.WriteFile(filepath.Join(dir, "production-run.lock"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := acquireProductionRunLease(stateDir, time.Minute)
	if err != nil {
		t.Fatalf("dead lease owner was not recovered: %v", err)
	}
	release()
}

func TestProductionRunLeaseReclaimsUnlockedLegacyMetadataWhenPIDWasReused(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := productionRunLease{
		SchemaVersion: "hive.production-run-lease.v1", ID: "crashed-owner-reused-pid", PID: os.Getpid(),
		StartedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(-time.Minute),
	}
	data, _ := json.Marshal(legacy)
	path := filepath.Join(dir, "production-run.lock")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	originalMatcher := matchesLegacyProductionRunOwner
	t.Cleanup(func() { matchesLegacyProductionRunOwner = originalMatcher })
	matchesLegacyProductionRunOwner = func(int, time.Time) bool { return false }
	release, err := acquireProductionRunLease(stateDir, time.Minute)
	if err != nil {
		t.Fatalf("unlocked legacy lease was trusted solely because its PID was reused: %v", err)
	}
	defer release()
	persisted, err := os.ReadFile(path)
	if err != nil || bytes.Contains(persisted, []byte(legacy.ID)) {
		t.Fatalf("legacy lease metadata was not replaced: data=%q err=%v", persisted, err)
	}
}

func TestProductionRunLeaseMixedVersionProtocolBlocksLiveLegacyAndSurvivesLegacyUnlink(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(dir, legacyProductionRunLeaseFile)
	v2Path := filepath.Join(dir, productionRunLeaseFile)
	started := time.Now().Add(-time.Minute).UTC()
	legacy := productionRunLease{
		SchemaVersion: legacyProductionRunLeaseSchema, ID: "live-released-v1-owner", PID: 4242,
		StartedAt: started, ExpiresAt: started.Add(time.Hour),
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(legacyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	originalMatcher := matchesLegacyProductionRunOwner
	t.Cleanup(func() { matchesLegacyProductionRunOwner = originalMatcher })
	matchesLegacyProductionRunOwner = func(pid int, gotStarted time.Time) bool {
		return pid == legacy.PID && gotStarted.Equal(legacy.StartedAt)
	}
	if _, err := acquireProductionRunLease(stateDir, time.Minute); !errors.Is(err, ErrRunInProgress) || !strings.Contains(err.Error(), "legacy pid=4242") {
		t.Fatalf("new worker overlapped a live released-v1 owner: %v", err)
	}
	if persisted, err := os.ReadFile(legacyPath); err != nil || string(persisted) != string(data) {
		t.Fatalf("live legacy lease was modified: data=%q err=%v", persisted, err)
	}

	matchesLegacyProductionRunOwner = func(int, time.Time) bool { return false }
	release, err := acquireProductionRunLease(stateDir, time.Minute)
	if err != nil {
		t.Fatalf("stale legacy lease was not atomically replaced: %v", err)
	}
	defer release()
	if _, err := os.Stat(v2Path); err != nil {
		t.Fatalf("v2 kernel lease file was not created: %v", err)
	}
	// A released-v1 process only knows the old path and may unlink it during
	// migration. The separately locked v2 inode must still reject every new
	// worker until the current run releases it.
	if err := os.Remove(legacyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireProductionRunLease(stateDir, time.Minute); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("legacy unlink bypassed the v2 kernel lease: %v", err)
	}
	if _, err := os.Stat(v2Path); err != nil {
		t.Fatalf("legacy unlink removed the v2 lease inode: %v", err)
	}
}

func TestProductionRunLeaseKernelLockBlocksOverlapAndRecoversChildCrash(t *testing.T) {
	const helperEnv = "HIVE_TEST_PRODUCTION_RUN_LEASE_CHILD"
	if os.Getenv(helperEnv) == "1" {
		stateDir := os.Getenv("HIVE_TEST_PRODUCTION_RUN_LEASE_STATE")
		release, err := acquireProductionRunLease(stateDir, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = os.Stdout.WriteString("lease-ready\n")
		_ = os.Stdout.Sync()
		_, _ = io.Copy(io.Discard, os.Stdin)
		runtime.KeepAlive(release)
		os.Exit(0)
	}

	stateDir := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestProductionRunLeaseKernelLockBlocksOverlapAndRecoversChildCrash$")
	command.Env = append(os.Environ(), helperEnv+"=1", "HIVE_TEST_PRODUCTION_RUN_LEASE_STATE="+stateDir)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
	})

	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if line != "lease-ready\n" {
			t.Fatalf("child did not acquire lease: stdout=%q stderr=%q", line, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for lease child: %s", stderr.String())
	}

	if _, err := acquireProductionRunLease(stateDir, time.Minute); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("overlap was not rejected while child held the kernel lease: %v", err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err := command.Wait(); err == nil {
		t.Fatal("killed lease child unexpectedly exited successfully")
	}
	command.Process = nil

	deadline := time.Now().Add(5 * time.Second)
	for {
		release, err := acquireProductionRunLease(stateDir, time.Minute)
		if err == nil {
			release()
			break
		}
		if !errors.Is(err, ErrRunInProgress) || time.Now().After(deadline) {
			t.Fatalf("kernel lease was not reclaimed after child crash: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
