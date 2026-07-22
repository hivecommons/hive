package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func seedPendingRecoveredCleanup(t *testing.T) (string, string, string, string, *Store, Attempt, visualhive.FindingLifecycle) {
	t.Helper()
	t.Setenv("GO_WANT_REPAIR_TOOL_MUTATION_HELPER", "1")
	repository, remote := seedGitRepository(t)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	worktree := filepath.Join(worktreeRoot, "pending-cleanup")
	branch := "hive/repair-pending-a4"
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", remote); err != nil {
		t.Fatal(err)
	}
	command := repairToolHelperCommand("generate-build")
	if err := runRepairCommand(context.Background(), worktree, command, time.Minute, nil, "preparation"); err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(gitOutput(t, worktree, "rev-parse", "HEAD"))
	generatedTree, err := captureWorktreeTree(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-broken\n+recovered crash-safe patch\n"
	patchDigest := sha256.Sum256([]byte(patch))
	patchSHA := hex.EncodeToString(patchDigest[:])
	providerSHA, err := readOnlyCodexProviderIdentity(CodexProvider{Command: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := "owner/repo:pending-cleanup"
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 4, Branch: branch, Worktree: worktree,
		Stage: StageModelComplete, Provider: "codex", ModelPatch: patch, RecoveredPatchAttempt: 3,
		RecoveredPatchSHA256: patchSHA, RecoveredProviderSHA256: providerSHA, PreparationRecoveryHead: head,
		PreparationReplayTree: generatedTree, StartedAt: time.Now().UTC(),
	}
	proof, err := preparationRecoveryProof(attempt, head, generatedTree, generatedTree, "build/lib/service/metrics.py", patchSHA, providerSHA, []Command{command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	state, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	guardWorker := &Worker{State: state}
	ref, commit, err := guardWorker.createPreparationCleanupGuard(context.Background(), attempt, generatedTree, head, proof)
	if err != nil {
		t.Fatal(err)
	}
	attempt.PreparationReplayProof = proof
	attempt.PreparationCleanupPending = true
	attempt.PreparationCleanupRef = ref
	attempt.PreparationCleanupCommit = commit
	if err := state.Put(attempt); err != nil {
		t.Fatal(err)
	}
	finding := standardRepairFinding(fingerprint)
	finding.Status, finding.RepairAttempts = visualhive.StatusRepairRunning, 3
	return repository, remote, worktreeRoot, stateDir, state, attempt, finding
}

func TestPendingRecoveryCleanupResumesPartialOrCompleteCrashWithoutApplying(t *testing.T) {
	for _, mode := range []string{"partial", "complete"} {
		t.Run(mode, func(t *testing.T) {
			repository, remote, worktreeRoot, stateDir, _, attempt, finding := seedPendingRecoveredCleanup(t)
			switch mode {
			case "partial":
				if err := os.Remove(filepath.Join(attempt.Worktree, "build", "lib", "service", "metrics.py")); err != nil {
					t.Fatal(err)
				}
			case "complete":
				if err := restoreWorktreeTree(context.Background(), attempt.Worktree, attempt.PreparationRecoveryHead, attempt.PreparationRecoveryHead, []string{"src/**"}); err != nil {
					t.Fatal(err)
				}
			}
			reloaded, err := NewStore(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			provider := &fakeProvider{}
			lifecycle := &fakeLifecycle{}
			pulls := &fakePRClient{state: reloaded}
			worker := &Worker{
				Config:   Config{RepositoryDir: repository, WorktreeRoot: worktreeRoot, BaseBranch: "main", ExpectedRemoteURL: remote, Policy: standardRepairPolicy(), AllowedRepairPaths: []string{"src/**"}},
				Provider: provider, State: reloaded, Lifecycle: lifecycle, GitHub: pulls,
			}
			_, runErr := worker.Run(context.Background(), finding)
			if !IsResumableFailureError(runErr) || provider.runs != 0 || pulls.calls != 0 {
				t.Fatalf("pending cleanup did not pause before provider/PR: err=%v provider=%d pulls=%d", runErr, provider.runs, pulls.calls)
			}
			recovered, _ := reloaded.Get(attempt.RepositoryFingerprint)
			if recovered.PreparationCleanupPending || recovered.Stage != StageFailed || recovered.ResumeStage != StageModelComplete || recovered.RecoveredPatchAuthorized || !recovered.AttemptCounted {
				t.Fatalf("pending cleanup did not reach an exact unauthorized pause: %+v", recovered)
			}
			for _, path := range []string{"build/lib/service/metrics.py", "build/lib/service/__init__.py"} {
				if _, err := os.Stat(filepath.Join(attempt.Worktree, filepath.FromSlash(path))); !os.IsNotExist(err) {
					t.Fatalf("generated path %s survived crash cleanup: %v", path, err)
				}
			}
		})
	}
}

func TestRecoveredPatchStateRejectsAlteredBytes(t *testing.T) {
	_, _, _, _, state, attempt, _ := seedPendingRecoveredCleanup(t)
	attempt.ModelPatch += "# substituted\n"
	if err := state.Put(attempt); err == nil || !strings.Contains(err.Error(), "invalid recovered-patch provenance") {
		t.Fatalf("state accepted recovered patch bytes that do not match the authorized digest: %v", err)
	}
}

func TestSealedModelAndCandidateTreesSurvivePruneAndRestart(t *testing.T) {
	repository, remote := seedGitRepository(t)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	worktree := filepath.Join(worktreeRoot, "sealed-prune")
	branch := "hive/repair-sealed-prune-a1"
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("cumulative uncommitted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	state, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:sealed-prune", Attempt: 1, AttemptCounted: true,
		Branch: branch, Worktree: worktree, Stage: StageModelComplete, Provider: "patch-model", StartedAt: time.Now().UTC(),
	}
	worker := &Worker{State: state}
	if err := worker.sealModelBaseTree(context.Background(), &attempt); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("sealed candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sealCandidateFromWorktree(context.Background(), &attempt); err != nil {
		t.Fatal(err)
	}
	if err := worker.guardCandidateTree(context.Background(), &attempt); err != nil {
		t.Fatal(err)
	}
	attempt.ChangedFiles, attempt.Stage = []string{"src/value.txt"}, StageValidated
	if err := state.Put(attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), repository, "gc", "--prune=now"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewStore(stateDir)
	if err != nil {
		t.Fatalf("reload sealed candidate after prune: %v", err)
	}
	recovered, _ := reloaded.Get(attempt.RepositoryFingerprint)
	if err := verifySealedTreeGuard(context.Background(), worktree, sealedTreeModelBase, recovered.ModelBaseGuardRef, recovered.ModelBaseGuardCommit,
		recovered.ModelBaseGuardBinding, recovered.ModelBaseTree, recovered.ModelBaseParent); err != nil {
		t.Fatalf("model-base guard did not survive prune: %v", err)
	}
	sha, err := commitSealedRepair(context.Background(), worktree, branch, recovered, "Sealed prune", 9, "123")
	if err != nil {
		t.Fatalf("commit pruned/reloaded sealed candidate: %v", err)
	}
	if content := strings.TrimSpace(gitOutput(t, worktree, "show", sha+":src/value.txt")); content != "sealed candidate" {
		t.Fatalf("sealed candidate changed across prune/restart: %q", content)
	}
}

func TestRepairRefTombstoneCancellationFailureRecoveryAndOriginalReuse(t *testing.T) {
	repository, _ := seedGitRepository(t)
	state, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD^{tree}"))
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: "owner/repo:tombstone", Attempt: 1,
		Branch: "hive/repair-tombstone-a1", Worktree: repository,
	}
	worker := &Worker{State: state}
	ref, commit, binding, err := worker.createSealedTreeGuard(context.Background(), attempt, sealedTreeCandidate, tree, head)
	if err != nil {
		t.Fatal(err)
	}
	tombstone := "refs/hive/repair-ref-tombstones/" + binding[:24] + "/" + strings.Repeat("a", 32)
	if err := state.recordRepairRefTombstoneIntent(context.Background(), repository, attempt.Repository, sealedTreeCandidate, ref, tombstone, commit, binding); err != nil {
		t.Fatal(err)
	}
	if err := moveRepairRefToTombstone(context.Background(), repository, ref, tombstone, commit); err != nil {
		t.Fatal(err)
	}
	wrong := head
	if wrong == commit {
		t.Fatal("test guard unexpectedly equals its parent")
	}
	if _, err := runGit(context.Background(), repository, "update-ref", tombstone, wrong, commit); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if err := sweepOrphanRepairRefs(context.Background(), repository, attempt.Repository, state, time.Now().Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("persistent changed tombstone did not fail closed on pass %d: %v", index+1, err)
		}
	}
	if _, err := runGit(context.Background(), repository, "update-ref", tombstone, commit, wrong); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	entry := repairRefJournalEntry{Kind: sealedTreeCandidate, Ref: ref, TombstoneRef: tombstone, Commit: commit, Binding: binding, Repository: attempt.Repository}
	if err := resumeRepairRefTombstone(canceled, repository, attempt.Repository, state, entry, false); err == nil {
		t.Fatal("canceled tombstone cleanup unexpectedly succeeded")
	}
	if value, err := readRepairRef(context.Background(), repository, tombstone); err != nil || value != commit {
		t.Fatalf("canceled cleanup lost exact tombstone: value=%q err=%v", value, err)
	}
	if err := sweepOrphanRepairRefs(context.Background(), repository, attempt.Repository, state, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("maintenance did not recover exact tombstone: %v", err)
	}
	if value, err := readRepairRef(context.Background(), repository, tombstone); err != nil || value != "" {
		t.Fatalf("recovered tombstone was not deleted: value=%q err=%v", value, err)
	}
	if _, err := runGit(context.Background(), repository, "update-ref", ref, commit, strings.Repeat("0", 40)); err != nil {
		t.Fatal(err)
	}
	if err := sweepOrphanRepairRefs(context.Background(), repository, attempt.Repository, state, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if value, err := readRepairRef(context.Background(), repository, ref); err != nil || value != commit {
		t.Fatalf("consumed authority deleted a recreated original ref: value=%q err=%v", value, err)
	}
}

func TestConsumedTombstoneNeverReplaysOriginalAcrossOriginChange(t *testing.T) {
	repository, _ := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	head := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD^{tree}"))
	attempt := Attempt{Repository: "owner/repo", RepositoryFingerprint: "owner/repo:origin-revocation", Attempt: 1, Branch: "hive/repair-origin-a1", Worktree: repository}
	worker := &Worker{State: state}
	ref, commit, binding, err := worker.createSealedTreeGuard(context.Background(), attempt, sealedTreeCandidate, tree, head)
	if err != nil {
		t.Fatal(err)
	}
	tombstone := "refs/hive/repair-ref-tombstones/" + binding[:24] + "/" + strings.Repeat("b", 32)
	if err := state.recordRepairRefTombstoneIntent(context.Background(), repository, attempt.Repository, sealedTreeCandidate, ref, tombstone, commit, binding); err != nil {
		t.Fatal(err)
	}
	if err := moveRepairRefToTombstone(context.Background(), repository, ref, tombstone, commit); err != nil {
		t.Fatal(err)
	}
	originalOrigin := strings.TrimSpace(gitOutput(t, repository, "config", "--get", "remote.origin.url"))
	runCommand(t, repository, "git", "remote", "set-url", "origin", "https://example.invalid/reconfigured.git")
	if err := state.recordRepairRefConsumed(context.Background(), repository, attempt.Repository, sealedTreeCandidate, ref, commit, binding); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), repository, "update-ref", "--no-deref", "-d", tombstone, commit); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), repository, "update-ref", "--no-deref", ref, commit, strings.Repeat("0", 40)); err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "remote", "set-url", "origin", originalOrigin)
	if err := sweepOrphanRepairRefs(context.Background(), repository, attempt.Repository, state, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if value, err := readRepairRef(context.Background(), repository, ref); err != nil || value != commit {
		t.Fatalf("origin A-B-A replay deleted recreated original: value=%q err=%v", value, err)
	}
}

func TestTombstoneIntentAcrossOriginChangeSuppressesOriginalReplay(t *testing.T) {
	repository, _ := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	head := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD^{tree}"))
	attempt := Attempt{Repository: "owner/repo", RepositoryFingerprint: "owner/repo:origin-tombstone-intent", Attempt: 1, Branch: "hive/repair-origin-intent-a1", Worktree: repository}
	worker := &Worker{State: state}
	ref, commit, binding, err := worker.createSealedTreeGuard(context.Background(), attempt, sealedTreeCandidate, tree, head)
	if err != nil {
		t.Fatal(err)
	}
	originalOrigin := strings.TrimSpace(gitOutput(t, repository, "config", "--get", "remote.origin.url"))
	runCommand(t, repository, "git", "remote", "set-url", "origin", "https://example.invalid/tombstone-b.git")
	tombstone := "refs/hive/repair-ref-tombstones/" + binding[:24] + "/" + strings.Repeat("d", 32)
	if err := state.recordRepairRefTombstoneIntent(context.Background(), repository, attempt.Repository, sealedTreeCandidate, ref, tombstone, commit, binding); err != nil {
		t.Fatal(err)
	}
	if err := moveRepairRefToTombstone(context.Background(), repository, ref, tombstone, commit); err != nil {
		t.Fatal(err)
	}
	runCommand(t, repository, "git", "remote", "set-url", "origin", originalOrigin)
	if _, err := runGit(context.Background(), repository, "update-ref", "--no-deref", ref, commit, strings.Repeat("0", 40)); err != nil {
		t.Fatal(err)
	}
	if err := sweepOrphanRepairRefs(context.Background(), repository, attempt.Repository, state, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if value, err := readRepairRef(context.Background(), repository, ref); err != nil || value != commit {
		t.Fatalf("cross-identity tombstone intent did not suppress original replay: value=%q err=%v", value, err)
	}
}

func TestRepairRefRetirementRejectsOriginalAndTombstoneSymrefs(t *testing.T) {
	for _, symbolicName := range []string{"original", "tombstone"} {
		t.Run(symbolicName, func(t *testing.T) {
			repository, _ := seedGitRepository(t)
			state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
			head := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
			tree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD^{tree}"))
			attempt := Attempt{Repository: "owner/repo", RepositoryFingerprint: "owner/repo:symref-" + symbolicName, Attempt: 1, Branch: "hive/repair-symref-a1", Worktree: repository}
			worker := &Worker{State: state}
			ref, commit, binding, err := worker.createSealedTreeGuard(context.Background(), attempt, sealedTreeCandidate, tree, head)
			if err != nil {
				t.Fatal(err)
			}
			tombstone := "refs/hive/repair-ref-tombstones/" + binding[:24] + "/" + strings.Repeat("c", 32)
			if symbolicName == "original" {
				runCommand(t, repository, "git", "update-ref", "--no-deref", "-d", ref, commit)
				runCommand(t, repository, "git", "symbolic-ref", ref, "refs/heads/main")
			} else {
				runCommand(t, repository, "git", "symbolic-ref", tombstone, "refs/heads/main")
			}
			if err := state.recordRepairRefTombstoneIntent(context.Background(), repository, attempt.Repository, sealedTreeCandidate, ref, tombstone, commit, binding); err != nil {
				t.Fatal(err)
			}
			entry := repairRefJournalEntry{Kind: sealedTreeCandidate, Ref: ref, TombstoneRef: tombstone, Commit: commit, Binding: binding, Repository: attempt.Repository}
			if err := resumeRepairRefTombstone(context.Background(), repository, attempt.Repository, state, entry, false); err == nil || !strings.Contains(err.Error(), "symbolic") {
				t.Fatalf("%s symref was accepted for cleanup: %v", symbolicName, err)
			}
			if main := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "refs/heads/main")); main != head {
				t.Fatalf("%s symref cleanup changed foreign main: %s", symbolicName, main)
			}
		})
	}
}

func TestSealedTreeGuardCannotBeTransplantedAcrossAttempts(t *testing.T) {
	repository, _ := seedGitRepository(t)
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	head := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD^{tree}"))
	original := Attempt{Repository: "owner/repo", RepositoryFingerprint: "owner/repo:guard-source", Recurrence: 1, Attempt: 2, Branch: "hive/repair-guard-source-a2", Worktree: repository, Stage: StageModelComplete, StartedAt: time.Now().UTC()}
	worker := &Worker{State: state}
	if err := worker.sealSpecificModelBaseTree(context.Background(), &original, tree, head); err != nil {
		t.Fatal(err)
	}
	transplanted := original
	transplanted.RepositoryFingerprint = "owner/repo:guard-target"
	transplanted.Attempt = 3
	transplanted.Branch = "hive/repair-guard-target-a3"
	if err := state.Put(transplanted); err == nil || !strings.Contains(err.Error(), "binding does not match") {
		t.Fatalf("model-base guard was transplantable across attempts: %v", err)
	}
	original.CandidateParent, original.CandidateTree = head, tree
	if err := worker.guardCandidateTree(context.Background(), &original); err != nil {
		t.Fatal(err)
	}
	original.ModelBaseTree, original.ModelBaseParent, original.ModelBaseGuardRef, original.ModelBaseGuardCommit, original.ModelBaseGuardBinding, original.ModelBaseGuardKind = "", "", "", "", "", ""
	transplanted = original
	transplanted.RepositoryFingerprint = "owner/repo:candidate-target"
	if err := state.Put(transplanted); err == nil || !strings.Contains(err.Error(), "binding does not match") {
		t.Fatalf("candidate guard was transplantable across fingerprints: %v", err)
	}
}

func TestRepairRefJournalQuarantinesTornTail(t *testing.T) {
	repository, _ := seedGitRepository(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	state, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(gitOutput(t, repository, "rev-parse", "HEAD^{tree}"))
	attempt := Attempt{Repository: "owner/repo", RepositoryFingerprint: "owner/repo:torn-ref-journal", Attempt: 1, Branch: "hive/repair-torn-a1", Worktree: repository}
	worker := &Worker{State: state}
	if _, _, _, err := worker.createSealedTreeGuard(context.Background(), attempt, sealedTreeCandidate, tree, head); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(stateDir, "repair-ref-journal.jsonl")
	file, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"phase":"tombstone_int`); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := state.repairRefJournalEntries()
	if err != nil || len(entries) != 1 || entries[0].Phase != "intent" {
		t.Fatalf("valid journal prefix was not recovered: entries=%+v err=%v", entries, err)
	}
	quarantines, err := filepath.Glob(journal + ".torn-*")
	if err != nil || len(quarantines) != 1 {
		t.Fatalf("torn repair-ref journal tail was not quarantined: %v err=%v", quarantines, err)
	}
}

func TestV5AppliedModelPatchRecoversExactBaseWithoutProviderRecall(t *testing.T) {
	repository, remote := seedGitRepository(t)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	worktree := filepath.Join(worktreeRoot, "v5-model-complete")
	branch := "hive/repair-v5-model-a1"
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", remote); err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/src/value.txt b/src/value.txt\n--- a/src/value.txt\n+++ b/src/value.txt\n@@ -1 +1 @@\n-broken\n+v5 exact recovery\n"
	if err := applyModelPatch(context.Background(), worktree, patch); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	fingerprint := "owner/repo:v5-applied"
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, AttemptCounted: true, LifecycleStarted: true,
		Branch: branch, Worktree: worktree, Stage: StageModelComplete, Provider: "codex", ModelPatch: patch, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	writeLegacyRepairState(t, stateDir, StateSchemaV5, attempt)
	state, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	provider := &runFailureProvider{}
	pulls := &fakePRClient{state: state}
	worker := &Worker{
		Config:   Config{RepositoryDir: repository, WorktreeRoot: worktreeRoot, BaseBranch: "main", ExpectedRemoteURL: remote, Policy: standardRepairPolicy(), AllowedRepairPaths: []string{"src/**"}, ValidationCommands: []Command{{Name: "git", Args: []string{"diff", "--check"}}}, CommandTimeout: time.Minute},
		Provider: provider, State: state, Lifecycle: &fakeLifecycle{}, GitHub: pulls,
	}
	finding := standardRepairFinding(fingerprint)
	finding.Status, finding.RepairAttempts, finding.Branch = visualhive.StatusRepairRunning, 1, branch
	result, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if provider.runs != 0 || result.PRNumber != 17 || strings.TrimSpace(gitOutput(t, remote, "show", result.Branch+":src/value.txt")) != "v5 exact recovery" {
		t.Fatalf("v5 applied patch was rerun or changed: runs=%d result=%+v", provider.runs, result)
	}
}

func TestV5ValidatedCandidateIsQuarantinedBeforeFreshBoundedAttempt(t *testing.T) {
	repository, remote := seedGitRepository(t)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	worktree := filepath.Join(worktreeRoot, "v5-validated")
	branch := "hive/repair-v5-validated-a1"
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("v5 validated recovery\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	fingerprint := "owner/repo:v5-validated"
	attempt := Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, AttemptCounted: true, LifecycleStarted: true,
		Branch: branch, Worktree: worktree, Stage: StageValidated, Provider: "codex", ChangedFiles: []string{"src/value.txt"}, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	writeLegacyRepairState(t, stateDir, StateSchemaV5, attempt)
	state, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	provider := &runFailureProvider{}
	pulls := &fakePRClient{state: state}
	lifecycle := &fakeLifecycle{}
	worker := &Worker{Config: Config{RepositoryDir: repository, WorktreeRoot: worktreeRoot, BaseBranch: "main", ExpectedRemoteURL: remote, Policy: standardRepairPolicy(), AllowedRepairPaths: []string{"src/**"}}, Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls}
	finding := standardRepairFinding(fingerprint)
	finding.Status, finding.RepairAttempts, finding.Branch = visualhive.StatusRepairRunning, 1, branch
	_, err = worker.Run(context.Background(), finding)
	if !IsRetryableAttemptError(err) || provider.runs != 0 || pulls.calls != 0 || len(lifecycle.decisions) != 0 {
		t.Fatalf("v5 validated quarantine caused a side effect: err=%v runs=%d pulls=%d lifecycle=%+v", err, provider.runs, pulls.calls, lifecycle)
	}
	quarantined, _ := state.Get(fingerprint)
	if quarantined.Stage != StageNoChange || quarantined.LegacyUnsealedCheckpoint || !strings.Contains(quarantined.LastFailure, "fresh bounded model attempt") {
		t.Fatalf("v5 validated checkpoint was not durably quarantined: %+v", quarantined)
	}
	freshProvider := &fakeProvider{}
	worker.Provider = freshProvider
	result, err := worker.Run(context.Background(), finding)
	if err != nil {
		t.Fatal(err)
	}
	if freshProvider.runs != 1 || result.PRNumber != 17 {
		t.Fatalf("normal next run did not consume exactly one fresh ordinal: runs=%d result=%+v", freshProvider.runs, result)
	}
}

func TestV5ModelCompleteWithoutPatchQuarantinesAndRestarts(t *testing.T) {
	repository, remote := seedGitRepository(t)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	worktree := filepath.Join(worktreeRoot, "v5-no-patch")
	branch := "hive/repair-v5-no-patch-a1"
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", remote); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "value.txt"), []byte("unproven legacy bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	fingerprint := "owner/repo:v5-no-patch"
	attempt := Attempt{Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, AttemptCounted: true, LifecycleStarted: true, Branch: branch, Worktree: worktree, Stage: StageModelComplete, Provider: "codex", StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	writeLegacyRepairState(t, stateDir, StateSchemaV5, attempt)
	state, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	provider := &runFailureProvider{}
	pulls, lifecycle := &fakePRClient{state: state}, &fakeLifecycle{}
	worker := &Worker{Config: Config{RepositoryDir: repository, WorktreeRoot: worktreeRoot, BaseBranch: "main", ExpectedRemoteURL: remote, Policy: standardRepairPolicy(), AllowedRepairPaths: []string{"src/**"}}, Provider: provider, State: state, Lifecycle: lifecycle, GitHub: pulls}
	finding := standardRepairFinding(fingerprint)
	finding.Status, finding.RepairAttempts, finding.Branch = visualhive.StatusRepairRunning, 1, branch
	_, runErr := worker.Run(context.Background(), finding)
	if !IsRetryableAttemptError(runErr) || provider.runs != 0 || pulls.calls != 0 || len(lifecycle.decisions) != 0 {
		t.Fatalf("unproven v5 model-complete quarantine caused side effects: err=%v runs=%d pulls=%d lifecycle=%+v", runErr, provider.runs, pulls.calls, lifecycle)
	}
	reloaded, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	quarantined, _ := reloaded.Get(fingerprint)
	if quarantined.Stage != StageNoChange || quarantined.LegacyUnsealedCheckpoint || quarantined.ModelPatch != "" || !strings.Contains(quarantined.LastFailure, "fresh bounded model attempt") {
		t.Fatalf("unproven v5 model-complete checkpoint did not restart durably: %+v", quarantined)
	}
}

func writeLegacyRepairState(t *testing.T, stateDir, schema string, attempt Attempt) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(State{SchemaVersion: schema, Attempts: map[string]*Attempt{attempt.RepositoryFingerprint: &attempt}}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "repair-worker-state.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestIndeterminateToolSnapshotPutRetainsRefForSafeSweep(t *testing.T) {
	repository, remote := seedGitRepository(t)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	worktree := filepath.Join(worktreeRoot, "indeterminate")
	branch := "hive/repair-indeterminate-a1"
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", remote); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), "state")
	state, _ := NewStore(stateDir)
	attempt := Attempt{Repository: "owner/repo", RepositoryFingerprint: "owner/repo:indeterminate", Attempt: 1, Branch: branch, Worktree: worktree, Stage: StagePrepared, Provider: "test", StartedAt: time.Now().UTC()}
	if err := state.Put(attempt); err != nil {
		t.Fatal(err)
	}
	state.renameFile = func(_, _ string) error { return fmt.Errorf("simulated indeterminate replacement") }
	worker := &Worker{Config: Config{CommandTimeout: time.Minute}, State: state}
	if err := worker.beginToolSnapshot(context.Background(), &attempt, toolSnapshotPreparation, 1); err == nil {
		t.Fatal("indeterminate state replacement unexpectedly succeeded")
	}
	if attempt.ToolSnapshotRef == "" {
		t.Fatal("snapshot ref was not created before the simulated state failure")
	}
	if _, err := runGit(context.Background(), worktree, "rev-parse", "--verify", attempt.ToolSnapshotRef+"^{commit}"); err != nil {
		t.Fatalf("indeterminate state failure deleted the only exact snapshot guard: %v", err)
	}
	reloaded, err := NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	persisted, _ := reloaded.Get(attempt.RepositoryFingerprint)
	if hasToolSnapshot(persisted) {
		t.Fatalf("failed replacement unexpectedly exposed an incomplete durable snapshot: %+v", persisted)
	}
	if err := sweepOrphanRepairRefs(context.Background(), repository, "owner/repo", reloaded, time.Now().Add(-orphanRepairRefGrace)); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), worktree, "rev-parse", "--verify", attempt.ToolSnapshotRef+"^{commit}"); err != nil {
		t.Fatalf("fresh unpersisted journaled ref was swept before its active-process grace: %v", err)
	}
	foreignBinding := strings.Repeat("b", 64)
	tree := strings.TrimSpace(gitOutput(t, worktree, "rev-parse", "HEAD^{tree}"))
	head := strings.TrimSpace(gitOutput(t, worktree, "rev-parse", "HEAD"))
	foreignCommit := strings.TrimSpace(gitOutput(t, worktree, "-c", "user.name=Foreign", "-c", "user.email=foreign@example.com", "commit-tree", tree, "-p", head, "-m", "Hive repair tool snapshot "+foreignBinding))
	foreignRef := "refs/hive/repair-tool-snapshots/" + foreignBinding[:24] + "/" + strings.Repeat("c", 32)
	runCommand(t, worktree, "git", "update-ref", foreignRef, foreignCommit, strings.Repeat("0", 40))
	wrongRepo := filepath.Join(t.TempDir(), "wrong-repository")
	runCommand(t, filepath.Dir(wrongRepo), "git", "clone", repository, wrongRepo)
	runCommand(t, wrongRepo, "git", "fetch", repository, attempt.ToolSnapshotRef+":"+attempt.ToolSnapshotRef)
	if err := sweepOrphanRepairRefs(context.Background(), wrongRepo, "owner/repo", reloaded, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), wrongRepo, "rev-parse", "--verify", attempt.ToolSnapshotRef+"^{commit}"); err != nil {
		t.Fatalf("state-dir reuse against another Git common directory deleted a foreign ref: %v", err)
	}
	if err := sweepOrphanRepairRefs(context.Background(), repository, "owner/repo", reloaded, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(context.Background(), worktree, "rev-parse", "--verify", attempt.ToolSnapshotRef+"^{commit}"); err == nil {
		t.Fatal("old unreferenced owned snapshot ref survived conservative sweep")
	}
	if _, err := runGit(context.Background(), worktree, "rev-parse", "--verify", foreignRef+"^{commit}"); err != nil {
		t.Fatalf("foreign unjournaled ref was deleted by orphan sweep: %v", err)
	}
}

func TestRecurrenceRolloverWaitsForPendingToolSnapshot(t *testing.T) {
	crashContainment := repairProcessTreeCrashContainmentGuaranteed
	repairProcessTreeCrashContainmentGuaranteed = func() bool { return false }
	defer func() { repairProcessTreeCrashContainmentGuaranteed = crashContainment }()
	repository, remote := seedGitRepository(t)
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	worktree := filepath.Join(worktreeRoot, "rollover")
	branch := "hive/repair-rollover-a1"
	if err := prepareWorktree(context.Background(), repository, worktree, branch, "main", "", remote); err != nil {
		t.Fatal(err)
	}
	state, _ := NewStore(filepath.Join(t.TempDir(), "state"))
	fingerprint := "owner/repo:rollover"
	attempt := Attempt{Repository: "owner/repo", RepositoryFingerprint: fingerprint, Attempt: 1, Branch: branch, Worktree: worktree, Stage: StagePrepared, Provider: "test", StartedAt: time.Now().UTC()}
	if err := state.Put(attempt); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{}
	worker := &Worker{
		Config:   Config{RepositoryDir: repository, WorktreeRoot: worktreeRoot, BaseBranch: "main", ExpectedRemoteURL: remote, Policy: standardRepairPolicy(), AllowedRepairPaths: []string{"src/**"}, CommandTimeout: time.Minute},
		Provider: provider, State: state, Lifecycle: &fakeLifecycle{}, GitHub: &fakePRClient{state: state},
	}
	if err := worker.beginToolSnapshot(context.Background(), &attempt, toolSnapshotPreparation, 1); err != nil {
		t.Fatal(err)
	}
	finding := standardRepairFinding(fingerprint)
	finding.Recurrences = 1
	_, err := worker.Run(context.Background(), finding)
	if !IsResumableFailureError(err) || provider.runs != 0 {
		t.Fatalf("recurrence rollover raced a pending tool snapshot: err=%v provider=%d", err, provider.runs)
	}
	saved, _ := state.Get(fingerprint)
	if saved.Recurrence != 0 || saved.Attempt != 1 || saved.Stage != StageFailed || saved.ResumeStage != StagePrepared || !hasToolSnapshot(saved) {
		t.Fatalf("pending old recurrence was not preserved for exact recovery: %+v", saved)
	}
}
