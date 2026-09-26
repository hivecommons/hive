package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

type fakeActivityExecutor struct {
	sig runActivitySignal
	ok  bool
}

func (f fakeActivityExecutor) Tick(_ context.Context, _ time.Time) {}

func (f fakeActivityExecutor) Status() FrontendSpektacularHubExecutor {
	return FrontendSpektacularHubExecutor{}
}

func (f fakeActivityExecutor) RunActivitySnapshot(_, _ string, _ uint64) (runActivitySignal, bool) {
	return f.sig, f.ok
}

func TestRunLiveActivityMergesExecutorLeaseAndTimeline(t *testing.T) {
	base := time.Date(2026, 9, 26, 14, 0, 0, 0, time.UTC)
	s, _ := runsTestServer(t)
	s.SetStageExecutor(fakeActivityExecutor{ok: true, sig: runActivitySignal{
		stageStartedAt: base,
		lastActivityAt: base.Add(3 * time.Minute),
		lastEvent:      "agent polled status (running)",
		agentPID:       os.Getpid(),
	}})
	run := Run{Key: "hivecommons/hive#23616", State: "active", Stage: StagePlan, Gen: 4, WaitingOn: RunWaitingOnAgent, LastActivity: base.Add(30 * time.Second).Format(time.RFC3339), ActivitySummary: "older timeline event"}
	lease := runLeaseSnapshot{stage: StagePlan, gen: 4, stageStarted: base, leaseHeartbeat: base.Add(time.Minute)}

	got := s.runLiveActivity(run, lease, nil, base.Add(5*time.Minute))
	if got == nil {
		t.Fatal("activity = nil")
	}
	if !got.Alive || !got.AgentPIDAlive {
		t.Fatalf("alive flags = alive:%v pid:%v, want both true", got.Alive, got.AgentPIDAlive)
	}
	if got.ElapsedSeconds != 300 {
		t.Fatalf("elapsed = %d, want 300", got.ElapsedSeconds)
	}
	if got.LastEvent != "agent polled status (running)" {
		t.Fatalf("last event = %q", got.LastEvent)
	}
	if got.LeaseHeartbeatAt != base.Add(time.Minute).Format(time.RFC3339) {
		t.Fatalf("lease heartbeat = %q", got.LeaseHeartbeatAt)
	}
}

func TestRunLiveActivityUsesNewestReceiptWrite(t *testing.T) {
	base := time.Date(2026, 9, 26, 14, 0, 0, 0, time.UTC)
	oldReceipts := runReceiptsDir
	dir, err := os.MkdirTemp(".", "run-activity-receipts-*")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		runReceiptsDir = oldReceipts
		_ = os.RemoveAll(dir)
	})
	runReceiptsDir = dir
	receiptDir := filepath.Join(runReceiptsDir, sanitizeReceiptSegment("hivecommons/hive#23616"))
	if err := os.MkdirAll(receiptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(receiptDir, "plan-gen4.json")
	if err := os.WriteFile(receipt, []byte(strings.Repeat("x", 2048)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(receipt, base.Add(4*time.Minute), base.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	s, _ := runsTestServer(t)
	run := Run{Key: "hivecommons/hive#23616", State: "active", Stage: StagePlan, Gen: 4, WaitingOn: RunWaitingOnAgent, LastActivity: base.Add(time.Minute).Format(time.RFC3339), ActivitySummary: "older timeline event"}
	lease := runLeaseSnapshot{stage: StagePlan, gen: 4, stageStarted: base, leaseHeartbeat: base.Add(2 * time.Minute)}

	got := s.runLiveActivity(run, lease, nil, base.Add(5*time.Minute))
	if got == nil {
		t.Fatal("activity = nil")
	}
	if !strings.Contains(got.LastEvent, "wrote plan-gen4.json") || !strings.Contains(got.LastEvent, "2.0 KB") {
		t.Fatalf("last event = %q, want receipt write with size", got.LastEvent)
	}
	if got.LastActivityAt != base.Add(4*time.Minute).Format(time.RFC3339) {
		t.Fatalf("last activity = %q", got.LastActivityAt)
	}
}

func TestRunLiveActivityPrefersAgentOutputOverHubLeaseRenewal(t *testing.T) {
	base := time.Date(2026, 9, 26, 19, 14, 0, 0, time.UTC)
	oldRoot := agentWorkspaceRoot
	root, err := os.MkdirTemp(".", "run-activity-agent-root-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		agentWorkspaceRoot = oldRoot
		_ = os.RemoveAll(root)
	})
	agentWorkspaceRoot = root

	runKey := "kubestellar/console#23615"
	worktree := spekHubRunWorktreePath(config.DefaultSpektacularHubExecutorIdentity, runKey)
	logDir := filepath.Join(worktree, ".hive")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "spek-stage-plan-2.log")
	if err := os.WriteFile(logPath, []byte("\x1b[32mStarting\x1b[0m\n\nNow assembling plan.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(logPath, base, base); err != nil {
		t.Fatal(err)
	}
	s, _ := runsTestServer(t)
	now := base.Add(12 * time.Minute)
	run := Run{Key: runKey, State: "active", Stage: StagePlan, Gen: 2, WaitingOn: RunWaitingOnAgent}
	lease := runLeaseSnapshot{
		identity:        config.DefaultSpektacularHubExecutorIdentity,
		stage:           StagePlan,
		gen:             2,
		stageStarted:    base.Add(-8 * time.Minute),
		leaseHeartbeat:  now,
		serverSideLease: true,
	}

	got := s.runLiveActivity(run, lease, nil, now)
	if got.LastActivityAt != base.Format(time.RFC3339) {
		t.Fatalf("last activity = %q, want agent output time", got.LastActivityAt)
	}
	if got.AgentOutputAt != base.Format(time.RFC3339) {
		t.Fatalf("agent output at = %q", got.AgentOutputAt)
	}
	if got.AgentOutputIdleSeconds != int64((12 * time.Minute).Seconds()) {
		t.Fatalf("agent output idle = %d", got.AgentOutputIdleSeconds)
	}
	if !strings.Contains(got.LastEvent, "agent output idle 12m") || !strings.Contains(got.LastEvent, `last: "Now assembling plan.md"`) {
		t.Fatalf("last event = %q, want idle output event with last log line", got.LastEvent)
	}
	if strings.Contains(got.LastEvent, "lease renewed") {
		t.Fatalf("last event used lease renewal despite agent output: %q", got.LastEvent)
	}
}

func TestSpekActivityEventArtifactMissing(t *testing.T) {
	got := spekActivityEvent("artifact_missing", map[string]string{stageAttrStage: StagePlan})
	if got != "waiting for agent to create the plan artifact" {
		t.Fatalf("artifact missing event = %q", got)
	}
}

func TestNewestAgentOutputActivityRejectsSymlink(t *testing.T) {
	base := time.Date(2026, 9, 26, 19, 14, 0, 0, time.UTC)
	oldRoot := agentWorkspaceRoot
	root, err := os.MkdirTemp(".", "run-activity-symlink-root-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		agentWorkspaceRoot = oldRoot
		_ = os.RemoveAll(root)
	})
	agentWorkspaceRoot = root

	runKey := "kubestellar/console#23615"
	worktree := spekHubRunWorktreePath(config.DefaultSpektacularHubExecutorIdentity, runKey)
	logDir := filepath.Join(worktree, ".hive")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("do not expose me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "spek-stage-plan-2.log")
	if err := os.Symlink(secret, logPath); err != nil {
		t.Fatal(err)
	}
	at, event, idle := newestAgentOutputActivity(config.DefaultSpektacularHubExecutorIdentity, runKey, StagePlan, 2, base)
	if !at.IsZero() || event != "" || idle != 0 {
		t.Fatalf("symlink activity = (%s, %q, %d), want empty", at, event, idle)
	}
}
