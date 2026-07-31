package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
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

func TestCancelForUninstallRecoversJournaledRefAfterWorktreeLocatorLoss(t *testing.T) {
	for _, test := range []struct {
		name           string
		changeIdentity bool
		wantError      bool
	}{
		{name: "exact journaled repository"},
		{name: "changed repository identity", changeIdentity: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repository, _ := seedGitRepository(t)
			worktree := filepath.Join(t.TempDir(), "repair-worktree")
			branch := "hive/repair-uninstall-locator-loss"
			runCommand(t, repository, "git", "worktree", "add", "-b", branch, worktree)
			state, err := NewStore(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			head := strings.TrimSpace(gitOutput(t, worktree, "rev-parse", "HEAD"))
			tree := strings.TrimSpace(gitOutput(t, worktree, "rev-parse", "HEAD^{tree}"))
			attempt := Attempt{
				Repository: "owner/repo", RepositoryFingerprint: "owner/repo:locator-loss", Attempt: 1, AttemptCounted: true,
				Branch: branch, Worktree: worktree, Stage: StageModelComplete, Provider: "test", StartedAt: time.Now().UTC(),
			}
			worker := &Worker{State: state}
			ref, commit, binding, err := worker.createSealedTreeGuard(ctx, attempt, sealedTreeModelBase, tree, head)
			if err != nil {
				t.Fatal(err)
			}
			attempt.ModelBaseTree, attempt.ModelBaseParent = tree, head
			attempt.ModelBaseGuardRef, attempt.ModelBaseGuardCommit, attempt.ModelBaseGuardBinding, attempt.ModelBaseGuardKind = ref, commit, binding, sealedTreeModelBase
			if err := state.Put(attempt); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(worktree, ".git"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if test.changeIdentity {
				runCommand(t, repository, "git", "remote", "set-url", "origin", "https://example.invalid/other.git")
			}

			err = state.CancelForUninstall(ctx, attempt.RepositoryFingerprint)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "journaled repository identity changed") {
					t.Fatalf("expected fail-closed identity error, got %v", err)
				}
				if value, readErr := readRepairRef(ctx, repository, ref); readErr != nil || value != commit {
					t.Fatalf("guard ref changed after rejected recovery: value=%q err=%v", value, readErr)
				}
				current, exists := state.Get(attempt.RepositoryFingerprint)
				if !exists || current.Stage == StageCancelled {
					t.Fatalf("attempt was cancelled after rejected recovery: %+v", current)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if value, readErr := readRepairRef(ctx, repository, ref); readErr != nil || value != "" {
				t.Fatalf("journal-owned guard ref survived recovery: value=%q err=%v", value, readErr)
			}
			current, exists := state.Get(attempt.RepositoryFingerprint)
			if !exists || current.Stage != StageCancelled || !UninstallRefsRetired(current) {
				t.Fatalf("recovered attempt did not reach a retired terminal state: %+v", current)
			}
		})
	}
}

func TestRepairSymbolicRefMissingAcceptsOnlyExactGitMissingRefDiagnostic(t *testing.T) {
	ref := "refs/hive/repair-sealed-trees/model_base/" + strings.Repeat("a", 24) + "/" + strings.Repeat("b", 32)
	for _, test := range []struct {
		name     string
		exitCode int
		output   string
		want     bool
	}{
		{name: "traditional quiet missing ref", exitCode: 1, want: true},
		{name: "new Git missing ref", exitCode: 128, output: "fatal: No such ref: " + ref + "\n", want: true},
		{name: "different missing ref", exitCode: 128, output: "fatal: No such ref: refs/hive/other", want: false},
		{name: "repository corruption", exitCode: 128, output: "fatal: not a git repository", want: false},
		{name: "unexpected exit", exitCode: 2, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := repairSymbolicRefMissing(test.exitCode, test.output, ref); got != test.want {
				t.Fatalf("repairSymbolicRefMissing()=%t, want %t", got, test.want)
			}
		})
	}
}

func TestCancelForUninstallRemovesOnlyJournaledZeroByteBrokenRef(t *testing.T) {
	for _, test := range []struct {
		name      string
		refBytes  []byte
		wantError bool
	}{
		{name: "zero-byte disk truncation"},
		{name: "non-empty invalid foreign value", refBytes: []byte("foreign\n"), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repository, _ := seedGitRepository(t)
			state, err := NewStore(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			head := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
			tree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD^{tree}"))
			attempt := Attempt{
				Repository: "owner/repo", RepositoryFingerprint: "owner/repo:broken-ref", Attempt: 1, AttemptCounted: true,
				Branch: "main", Worktree: repository, Stage: StageModelComplete, Provider: "test", StartedAt: time.Now().UTC(),
			}
			worker := &Worker{State: state}
			ref, commit, binding, err := worker.createSealedTreeGuard(ctx, attempt, sealedTreeModelBase, tree, head)
			if err != nil {
				t.Fatal(err)
			}
			attempt.ModelBaseTree, attempt.ModelBaseParent = tree, head
			attempt.ModelBaseGuardRef, attempt.ModelBaseGuardCommit, attempt.ModelBaseGuardBinding, attempt.ModelBaseGuardKind = ref, commit, binding, sealedTreeModelBase
			if err := state.Put(attempt); err != nil {
				t.Fatal(err)
			}
			commonDir := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "--git-common-dir"))
			if !filepath.IsAbs(commonDir) {
				commonDir = filepath.Join(repository, commonDir)
			}
			refPath := filepath.Join(commonDir, filepath.FromSlash(ref))
			if err := os.WriteFile(refPath, test.refBytes, 0o644); err != nil {
				t.Fatal(err)
			}

			err = state.CancelForUninstall(ctx, attempt.RepositoryFingerprint)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "not an ordinary zero-byte loose ref") {
					t.Fatalf("expected fail-closed broken-ref error, got %v", err)
				}
				if data, readErr := os.ReadFile(refPath); readErr != nil || string(data) != string(test.refBytes) {
					t.Fatalf("non-empty broken ref changed: data=%q err=%v", data, readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, statErr := os.Lstat(refPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("zero-byte broken ref survived retirement: %v", statErr)
			}
			current, exists := state.Get(attempt.RepositoryFingerprint)
			if !exists || current.Stage != StageCancelled || !UninstallRefsRetired(current) {
				t.Fatalf("broken-ref recovery did not reach terminal retirement: %+v", current)
			}
		})
	}
}

func TestRetireBrokenUnpublishedBranchForUninstall(t *testing.T) {
	for _, test := range []struct {
		name         string
		remoteAbsent bool
		refBytes     []byte
		wantRemoved  bool
		wantError    string
	}{
		{name: "exact zero-byte unpublished branch", remoteAbsent: true, wantRemoved: true},
		{name: "remote absence required", wantError: "absence was not proven"},
		{name: "non-empty invalid ref is preserved", remoteAbsent: true, refBytes: []byte("foreign\n"), wantError: "not an ordinary zero-byte loose ref"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, _ := seedGitRepository(t)
			fingerprint := "owner/repo:unpublished-branch"
			attempt := Attempt{
				Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1,
				Branch: repairBranchName(fingerprint, 0, 1), Worktree: "discarded-worktree", Stage: StageCancelled,
			}
			commonDir := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "--git-common-dir"))
			if !filepath.IsAbs(commonDir) {
				commonDir = filepath.Join(repository, commonDir)
			}
			refPath := filepath.Join(commonDir, filepath.FromSlash("refs/heads/"+attempt.Branch))
			if err := os.MkdirAll(filepath.Dir(refPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(refPath, test.refBytes, 0o644); err != nil {
				t.Fatal(err)
			}

			removed, err := RetireBrokenUnpublishedBranchForUninstall(context.Background(), repository, attempt, test.remoteAbsent)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) || removed {
					t.Fatalf("result removed=%t err=%v", removed, err)
				}
				if data, readErr := os.ReadFile(refPath); readErr != nil || string(data) != string(test.refBytes) {
					t.Fatalf("denied ref changed: data=%q err=%v", data, readErr)
				}
				return
			}
			if err != nil || removed != test.wantRemoved {
				t.Fatalf("result removed=%t err=%v", removed, err)
			}
			if _, statErr := os.Lstat(refPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("zero-byte unpublished ref survived: %v", statErr)
			}
		})
	}
}

func TestRetireBrokenUnpublishedBranchRejectsDifferentBinding(t *testing.T) {
	repository, _ := seedGitRepository(t)
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:different-binding", Attempt: 1,
		Branch: "hive/repair-foreign-a1", Stage: StageCancelled,
	}
	removed, err := RetireBrokenUnpublishedBranchForUninstall(context.Background(), repository, attempt, true)
	if err != nil || removed {
		t.Fatalf("different branch binding removed=%t err=%v", removed, err)
	}
}
