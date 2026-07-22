package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCancelForUninstallRetiresAllJournaledRefsAcrossCrashAndPreservesForeignRefs(t *testing.T) {
	ctx := context.Background()
	repository, _ := seedGitRepository(t)
	branch := "hive/repair-uninstall-a1"
	runCommand(t, repository, "git", "checkout", "-b", branch)
	stateDir := filepath.Join(t.TempDir(), "state")
	state, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD^{tree}"))
	worker := &Worker{State: state}

	guarded := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:uninstall-guards", Attempt: 1, AttemptCounted: true,
		Branch: branch, Worktree: repository, Stage: StageModelComplete, Provider: "test", StartedAt: time.Now().UTC(),
	}
	modelRef, modelCommit, modelBinding, err := worker.createSealedTreeGuard(ctx, guarded, sealedTreeModelBase, tree, head)
	if err != nil {
		t.Fatal(err)
	}
	guarded.ModelBaseTree, guarded.ModelBaseParent = tree, head
	guarded.ModelBaseGuardRef, guarded.ModelBaseGuardCommit, guarded.ModelBaseGuardBinding, guarded.ModelBaseGuardKind = modelRef, modelCommit, modelBinding, sealedTreeModelBase
	candidateRef, candidateCommit, candidateBinding, err := worker.createSealedTreeGuard(ctx, guarded, sealedTreeCandidate, tree, head)
	if err != nil {
		t.Fatal(err)
	}
	guarded.CandidateTree, guarded.CandidateParent = tree, head
	guarded.CandidateGuardRef, guarded.CandidateGuardCommit, guarded.CandidateGuardBinding, guarded.CandidateGuardKind = candidateRef, candidateCommit, candidateBinding, sealedTreeCandidate
	toolBinding := toolSnapshotBinding(guarded, toolSnapshotValidation, head, branch)
	toolCommit, err := runGit(ctx, repository, "-c", "user.name=Hive Repair Agent", "-c", "user.email=hive-repair@users.noreply.github.com", "commit-tree", tree, "-p", head, "-m", "Hive repair tool snapshot "+toolBinding)
	if err != nil {
		t.Fatal(err)
	}
	toolCommit = strings.TrimSpace(toolCommit)
	toolRef := "refs/hive/repair-tool-snapshots/" + toolBinding[:24] + "/" + strings.Repeat("1", 32)
	if err := state.recordRepairRefIntent(ctx, repository, guarded.Repository, toolSnapshotValidation, toolRef, toolCommit, toolBinding); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, repository, "update-ref", "--no-deref", toolRef, toolCommit, strings.Repeat("0", 40)); err != nil {
		t.Fatal(err)
	}
	guarded.ToolSnapshotRef, guarded.ToolSnapshotCommit, guarded.ToolSnapshotHead = toolRef, toolCommit, head
	guarded.ToolSnapshotBranch, guarded.ToolSnapshotPhase = branch, toolSnapshotValidation
	guarded.ToolSnapshotStartedAt, guarded.ToolSnapshotDeadline = time.Now().UTC(), time.Now().UTC().Add(time.Hour)
	if err := state.Put(guarded); err != nil {
		t.Fatal(err)
	}

	cleanup := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:uninstall-preparation", Attempt: 2, Branch: branch, Worktree: repository,
		Stage: StageModelComplete, Provider: "codex", ModelPatch: "exact recovered patch", RecoveredPatchAttempt: 1,
		RecoveredProviderSHA256: strings.Repeat("a", 64), PreparationRecoveryHead: head, PreparationReplayTree: tree, StartedAt: time.Now().UTC(),
	}
	patchDigest := sha256.Sum256([]byte(cleanup.ModelPatch))
	cleanup.RecoveredPatchSHA256 = hex.EncodeToString(patchDigest[:])
	cleanup.PreparationReplayProof = strings.Repeat("b", 64)
	cleanupRef, cleanupCommit, err := worker.createPreparationCleanupGuard(ctx, cleanup, tree, head, cleanup.PreparationReplayProof)
	if err != nil {
		t.Fatal(err)
	}
	cleanup.PreparationCleanupPending, cleanup.PreparationCleanupRef, cleanup.PreparationCleanupCommit = true, cleanupRef, cleanupCommit
	if err := state.Put(cleanup); err != nil {
		t.Fatal(err)
	}

	foreignRef := "refs/hive/repair-sealed-trees/candidate/" + strings.Repeat("c", 24) + "/" + strings.Repeat("d", 32)
	if _, err := runGit(ctx, repository, "update-ref", "--no-deref", foreignRef, head, strings.Repeat("0", 40)); err != nil {
		t.Fatal(err)
	}

	// Simulate a process loss after the exact candidate guard was atomically
	// moved but before consumed/tombstone-complete journal records were written.
	crashTombstone := "refs/hive/repair-ref-tombstones/" + candidateBinding[:24] + "/" + strings.Repeat("e", 32)
	if err := state.recordRepairRefTombstoneIntent(ctx, repository, guarded.Repository, sealedTreeCandidate, candidateRef, crashTombstone, candidateCommit, candidateBinding); err != nil {
		t.Fatal(err)
	}
	if err := moveRepairRefToTombstone(ctx, repository, candidateRef, crashTombstone, candidateCommit); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, fingerprint := range []string{guarded.RepositoryFingerprint, cleanup.RepositoryFingerprint} {
		if err := reloaded.CancelForUninstall(ctx, fingerprint); err != nil {
			t.Fatalf("cancel %s: %v", fingerprint, err)
		}
		cancelled, exists := reloaded.Get(fingerprint)
		if !exists || cancelled.Stage != StageCancelled || !UninstallRefsRetired(cancelled) {
			t.Fatalf("cancelled checkpoint retained authority: exists=%t attempt=%+v", exists, cancelled)
		}
	}
	for _, owned := range []string{modelRef, candidateRef, toolRef, cleanupRef, crashTombstone} {
		if value, err := readRepairRef(ctx, repository, owned); err != nil || value != "" {
			t.Fatalf("owned ref %s survived cancellation: value=%q err=%v", owned, value, err)
		}
	}
	if value, err := readRepairRef(ctx, repository, foreignRef); err != nil || value != head {
		t.Fatalf("foreign ref changed during cancellation: value=%q err=%v", value, err)
	}

	// Once consumed, the original name carries no stale cleanup authority. A
	// foreign recreation at that name must survive an idempotent retry.
	if _, err := runGit(ctx, repository, "update-ref", "--no-deref", candidateRef, head, strings.Repeat("0", 40)); err != nil {
		t.Fatal(err)
	}
	retried, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := retried.CancelForUninstall(ctx, guarded.RepositoryFingerprint); err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []string{foreignRef, candidateRef} {
		if value, err := readRepairRef(ctx, repository, foreign); err != nil || value != head {
			t.Fatalf("foreign ref %s was consumed by retry: value=%q err=%v", foreign, value, err)
		}
	}
}
