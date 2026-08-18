package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/integrated"
	"github.com/kubestellar/hive/pkg/visualhive/normalservice"
)

func TestNormalVisualServiceLeaseWithoutDashboardHTTPProofIsNotReady(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLease(lease)

	status := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, time.Now().UTC())
	if status.DashboardReady || status.ServiceReady || status.HealthExists {
		t.Fatalf("lease-only readiness = %+v", status)
	}
	if !strings.Contains(status.Message, "has not proven HTTP readiness") {
		t.Fatalf("lease-only message = %q", status.Message)
	}
	checks := normalVisualServiceDoctorChecks(status)
	if len(checks) != 2 || checks[0].Name != "normal_hive_dashboard" || checks[0].OK || checks[1].Name != "normal_visual_service" || checks[1].OK {
		t.Fatalf("lease-only doctor checks = %+v", checks)
	}
}

func TestNormalVisualServiceInitializationAloneIsNotReady(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLease(lease)
	reporter := testNormalVisualHealthReporter(t, stateDir, "owner/repo", time.Minute)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	reporter.now = func() time.Time { return now }
	if err := reporter.Initialize(); err != nil {
		t.Fatal(err)
	}

	status := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, now.Add(10*time.Second))
	if !status.DashboardReady || status.ServiceReady || !status.HealthExists || status.Health.Generation == "" || !strings.Contains(status.Message, "loop has not started") {
		t.Fatalf("initialization-only readiness = %+v", status)
	}
}

func TestNormalVisualServiceLongInFlightCycleRemainsReadyBeyondPoll(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLease(lease)
	reporter := testNormalVisualHealthReporter(t, stateDir, "owner/repo", time.Minute)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	reporter.now = func() time.Time { return now }
	if err := reporter.Initialize(); err != nil {
		t.Fatal(err)
	}
	started := now.Add(time.Second)
	reporter.StartCycle(started, started.Add(reporter.maxCycle))

	status := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, started.Add(20*time.Minute))
	if !status.ServiceReady || !status.Health.CycleStartedAt.Equal(started) || !status.Health.CycleDeadline.Equal(started.Add(reporter.maxCycle)) {
		t.Fatalf("twenty-minute in-flight readiness with one-minute poll = %+v", status)
	}
}

func TestNormalVisualServiceInFlightDeadlineOverrunIsNotReady(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLease(lease)
	reporter := testNormalVisualHealthReporter(t, stateDir, "owner/repo", time.Minute)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	reporter.now = func() time.Time { return now }
	if err := reporter.Initialize(); err != nil {
		t.Fatal(err)
	}
	started := now.Add(time.Second)
	reporter.StartCycle(started, started.Add(reporter.maxCycle))

	status := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, started.Add(reporter.maxCycle+time.Nanosecond))
	if status.ServiceReady || !strings.Contains(status.Message, "exceeded its bounded deadline") {
		t.Fatalf("deadline-overrun readiness = %+v", status)
	}
}

func TestNormalVisualServiceHardCycleErrorIsUnhealthy(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLease(lease)
	reporter := testNormalVisualHealthReporter(t, stateDir, "owner/repo", time.Minute)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	reporter.now = func() time.Time { return now }
	if err := reporter.Initialize(); err != nil {
		t.Fatal(err)
	}
	reporter.now = func() time.Time { return now.Add(time.Second) }
	reporter.StartCycle(now.Add(time.Second), now.Add(time.Second).Add(reporter.maxCycle))
	reporter.now = func() time.Time { return now.Add(2 * time.Second) }
	reporter.RecordCycle(errors.New("provider crashed with ghp_do-not-persist"))

	status := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, now.Add(3*time.Second))
	if status.ServiceReady || status.Health.LastError != "cycle failed" || status.Health.LastErrorAt.IsZero() || !status.Health.CycleStartedAt.IsZero() || !status.Health.CycleDeadline.IsZero() {
		t.Fatalf("hard-error readiness = %+v", status)
	}
}

func TestNormalVisualServiceRejectsStaleAndMismatchedGeneration(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLease(lease)
	reporter := testNormalVisualHealthReporter(t, stateDir, "owner/repo", time.Minute)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	reporter.now = func() time.Time { return now }
	if err := reporter.Initialize(); err != nil {
		t.Fatal(err)
	}
	reporter.now = func() time.Time { return now.Add(time.Second) }
	reporter.RecordCycle(nil)
	health, exists, err := readNormalVisualServiceHealth(stateDir)
	if err != nil || !exists {
		t.Fatalf("load initialized health: exists=%t err=%v", exists, err)
	}

	stale := health
	stale.LastCompletedAt = now.Add(-time.Minute - normalVisualServiceHealthGrace - time.Second)
	stale.LastHealthyAt = stale.LastCompletedAt
	stale.InitializedAt = stale.LastCompletedAt.Add(-time.Second)
	if err := writeNormalVisualServiceHealth(stateDir, stale); err != nil {
		t.Fatal(err)
	}
	if status := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, now); status.ServiceReady || !strings.Contains(status.Message, "stale") {
		t.Fatalf("stale readiness = %+v", status)
	}

	mismatch := health
	mismatch.Generation = strings.Repeat("a", 64)
	if err := writeNormalVisualServiceHealth(stateDir, mismatch); err != nil {
		t.Fatal(err)
	}
	if status := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, now); status.ServiceReady || !strings.Contains(status.Message, "does not match") {
		t.Fatalf("mismatched readiness = %+v", status)
	}
}

func TestNormalVisualServiceExpectedIdleResultsRemainHealthy(t *testing.T) {
	idleResults := []error{
		normalservice.ErrNoDispatch,
		normalservice.ErrOpenPullRequest,
		normalservice.ErrRepairRetirement,
		normalservice.ErrFinalVerdictPending,
		integrated.ErrRunInProgress,
		integrated.ErrNormalVisualSetupBaselinePending,
		integrated.ErrSetupBaselineLifecycleHold,
		fmt.Errorf("wrapped idle: %w", normalservice.ErrNoDispatch),
	}
	for index, idle := range idleResults {
		stateDir := t.TempDir()
		lease, err := claimNormalVisualDaemonLease(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		reporter := testNormalVisualHealthReporter(t, stateDir, "owner/repo", time.Minute)
		now := time.Date(2026, 7, 16, 12, 0, index, 0, time.UTC)
		reporter.now = func() time.Time { return now }
		if err := reporter.Initialize(); err != nil {
			releaseDaemonLease(lease)
			t.Fatal(err)
		}
		started := now.Add(time.Second)
		reporter.StartCycle(started, started.Add(reporter.maxCycle))
		reporter.now = func() time.Time { return now.Add(2 * time.Second) }
		reporter.RecordCycle(idle)
		status := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, now.Add(3*time.Second))
		releaseDaemonLease(lease)
		if !status.ServiceReady || status.Health.LastError != "" || status.Health.LastCompletedAt.IsZero() || status.Health.LastHealthyAt.IsZero() {
			t.Fatalf("idle result %v readiness = %+v", idle, status)
		}
	}
}

func TestNormalVisualServiceCancellationAndQuiescenceClearInFlightReadiness(t *testing.T) {
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLease(lease)
	reporter := testNormalVisualHealthReporter(t, stateDir, "owner/repo", time.Minute)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	reporter.now = func() time.Time { return now }
	if err := reporter.Initialize(); err != nil {
		t.Fatal(err)
	}
	started := now.Add(time.Second)
	reporter.StartCycle(started, started.Add(reporter.maxCycle))
	reporter.now = func() time.Time { return now.Add(2 * time.Second) }
	reporter.RecordCycle(context.Canceled)
	reporter.now = func() time.Time { return now.Add(3 * time.Second) }
	reporter.RecordInactive()

	status := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, now.Add(4*time.Second))
	if status.ServiceReady || status.Health.LastError != "cycle canceled" || status.Health.InactiveAt.IsZero() || !status.Health.CycleStartedAt.IsZero() || !status.Health.CycleDeadline.IsZero() || !strings.Contains(status.Message, "stopped or quiesced") {
		t.Fatalf("canceled/quiesced readiness = %+v", status)
	}

	restarted := now.Add(5 * time.Second)
	reporter.StartCycle(restarted, restarted.Add(reporter.maxCycle))
	status = inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, restarted.Add(time.Second))
	if !status.ServiceReady || !status.Health.InactiveAt.IsZero() {
		t.Fatalf("resumed in-flight readiness = %+v", status)
	}
}

func TestNormalVisualServiceHealthUsesOwnerOnlyAtomicFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix owner-only mode bits")
	}
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLease(lease)
	reporter := testNormalVisualHealthReporter(t, stateDir, "owner/repo", time.Minute)
	if err := reporter.Initialize(); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	reporter.StartCycle(started, started.Add(reporter.maxCycle))
	reporter.RecordCycle(nil)

	info, err := os.Stat(normalVisualServiceHealthPath(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if permission := info.Mode().Perm(); permission != 0o600 {
		t.Fatalf("health file permissions = %04o, want 0600", permission)
	}
}

func TestConfiguredNormalVisualServiceMaxCycleCoversBoundedWorkAndCapsPathologicalConfig(t *testing.T) {
	config := integrated.Config{TestCommands: [][]string{{"npm", "test"}, {"npm", "run", "visual"}}}
	want := normalVisualArtifactFetchTimeout + normalVisualRepairModelTimeout + normalVisualCycleFixedOverhead + 2*normalVisualRepairCommandTimeout
	if got := configuredNormalVisualServiceMaxCycle(config); got != want {
		t.Fatalf("configured maximum cycle = %v, want %v", got, want)
	}
	fixed := normalVisualArtifactFetchTimeout + normalVisualRepairModelTimeout + normalVisualCycleFixedOverhead
	perCommand := normalVisualRepairCommandTimeout
	pathologicalCount := int((normalVisualServiceMaxCycleCap-fixed)/perCommand) + 1
	config.TestCommands = make([][]string, pathologicalCount)
	if got := configuredNormalVisualServiceMaxCycle(config); got != normalVisualServiceMaxCycleCap {
		t.Fatalf("pathological maximum cycle = %v, want cap %v", got, normalVisualServiceMaxCycleCap)
	}
}

func TestNormalVisualServiceOwnerIntentAndStaleOwnerRecoveryStayOnOrdinaryHive(t *testing.T) {
	config := integrated.Config{ExecutionMode: integrated.ExecutionLocal, VisualHive: true, Automation: integrated.AutomationRepairPR}
	if intent := configuredRuntimeOwnerIntent(config); intent != runtimeOwnerNormalHive {
		t.Fatalf("configured owner intent = %q", intent)
	}
	stateDir := t.TempDir()
	lease, err := claimNormalVisualDaemonLease(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	releaseDaemonLease(lease)
	status := inspectNormalVisualServiceHealth(stateDir, "owner/repo", time.Minute, time.Now().UTC())
	if !status.OwnerRecordPresent || status.DashboardReady || status.ServiceReady || !strings.Contains(status.Message, "do not run hive start") {
		t.Fatalf("stale owner recovery = %+v", status)
	}
}

func testNormalVisualHealthReporter(t *testing.T, stateDir, repository string, poll time.Duration) *normalVisualServiceHealthReporter {
	t.Helper()
	readyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	t.Cleanup(readyServer.Close)
	listenerPort := readyServer.Listener.Addr().(*net.TCPAddr).Port
	if _, err := writeNormalVisualDashboardReady(stateDir, repository, listenerPort, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	reporter, err := newNormalVisualServiceHealthReporter(stateDir, repository, poll, 30*time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return reporter
}
