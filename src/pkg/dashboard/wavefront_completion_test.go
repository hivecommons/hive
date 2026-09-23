package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/worksource"
)

func TestTaskCompleteDrivesWavefrontCompleteAndCleansWorktree(t *testing.T) {
	hub, s := covK2Hub(t)
	started := time.Now().Add(-time.Minute)
	key := "acme/repo!graph-a:node-a"
	taskID := "task-wavefront"
	identity := "alice"
	if err := hub.recordLeaseForKeyStage(identity, taskID, "acme/repo", 0, key, "contributor", StageImplement, 7, started); err != nil {
		t.Fatalf("record lease: %v", err)
	}
	origRoot := agentWorkspaceRoot
	agentWorkspaceRoot = t.TempDir()
	t.Cleanup(func() { agentWorkspaceRoot = origRoot })
	worktree := runStageWorktreePath(identity, key, StageImplement, 7)
	if err := os.MkdirAll(filepath.Join(worktree, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	var completed struct {
		key, externalID, revision string
		startedAt                 time.Time
	}
	s.deps.WavefrontComplete = func(_ context.Context, key, externalID, revision string, startedAt time.Time) error {
		completed.key, completed.externalID, completed.revision, completed.startedAt = key, externalID, revision, startedAt
		return nil
	}
	conn := &ContributorConnection{
		profile:        &ContributorProfile{GitHubUsername: identity},
		role:           "contributor",
		currentTask:    &WSTaskAssign{TaskID: taskID, Stage: StageImplement, Repo: "acme/repo", Key: key, SourceType: worksource.SourceTypeRun, ExternalID: "graph-a:node-a"},
		currentLabels:  []string{"hive-run", "stage/implement", "wavefront", "graph-rev/rev-a"},
		taskAssignedAt: started,
	}
	(&wsSession{h: hub, contributor: conn}).handleTaskComplete(WSMessage{TaskID: taskID})

	if completed.key != key || completed.externalID != "graph-a:node-a" || completed.revision != "rev-a" || !completed.startedAt.Equal(started) {
		t.Fatalf("wavefront completion = %+v", completed)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists or stat failed: %v", err)
	}
}

func TestRestoredExpiredWavefrontLeaseTransitionsUnknown(t *testing.T) {
	hub, s := covK2Hub(t)
	now := time.Now()
	key := "acme/repo!graph-a:node-a"
	hub.leaseMu.Lock()
	hub.leases = map[string]*taskLease{
		leaseKey("alice", "task-wavefront"): {
			identity: "alice", taskID: "task-wavefront", repo: "acme/repo", key: key,
			stage: StageImplement, gen: 9, restored: true, expiresAt: now.Add(-time.Second),
		},
	}
	hub.leaseMu.Unlock()
	var unknown struct {
		externalID, reason string
		startedAt          time.Time
	}
	s.deps.WavefrontUnknown = func(_ context.Context, externalID, reason string, startedAt time.Time) error {
		unknown.externalID, unknown.reason, unknown.startedAt = externalID, reason, startedAt
		return nil
	}
	if dropped := hub.pruneExpiredLeases(now); dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if unknown.externalID != "graph-a:node-a" || unknown.reason == "" || unknown.startedAt.IsZero() {
		t.Fatalf("unknown transition = %+v", unknown)
	}
}
