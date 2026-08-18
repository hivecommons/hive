package repair

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/visualhive"
)

const priorRejectedPatchMarker = "\n\nHive rejected this attempt before application. Do not repeat the same patch:\n"

// recoverRejectedPatchAfterPreparationContamination handles the one legacy
// checkpoint that predates tool snapshots. It never reruns the interrupted
// model. Instead, it proves in a detached worktree that every ambient dirty
// byte was produced by the exact preparation commands, then checkpoints the
// complete prior rejected patch for an explicit, audited retry-repair action.
func (w *Worker) recoverRejectedPatchAfterPreparationContamination(ctx context.Context, finding visualhive.FindingLifecycle, attempt *Attempt) (bool, error) {
	if attempt.Provider != "codex" || !isConcreteReadOnlyCodexProvider(w.Provider) || attempt.Stage != StageModelRunning || attempt.Attempt <= 1 ||
		attempt.Attempt != finding.RepairAttempts+1 || attempt.ModelPatch != "" || len(w.Config.PreparationCommands) == 0 ||
		hasToolSnapshot(*attempt) || attempt.RecoveredPatchAttempt != 0 {
		return false, nil
	}
	marker := strings.LastIndex(attempt.PriorModelSummary, priorRejectedPatchMarker)
	if marker < 0 || marker+len(priorRejectedPatchMarker) >= len(attempt.PriorModelSummary) {
		return false, nil
	}
	rejection := strings.TrimSpace(attempt.PriorModelSummary[marker+len(priorRejectedPatchMarker):])
	const rejectionPrefix = "repair changed "
	const rejectionSuffix = " outside the configured repair allowlist"
	if strings.Count(rejection, "\n") != 0 || !strings.HasPrefix(rejection, rejectionPrefix) || !strings.HasSuffix(rejection, rejectionSuffix) {
		return false, nil
	}
	rejectedPath := filepath.ToSlash(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(rejection, rejectionPrefix), rejectionSuffix)))
	if rejectedPath == "" || filepath.IsAbs(rejectedPath) || rejectedPath == ".." || strings.HasPrefix(rejectedPath, "../") {
		return false, nil
	}
	patch, err := extractModelPatch(attempt.PriorModelSummary[:marker])
	if err != nil || patch == "" {
		return false, nil
	}
	patchFiles, err := patchChangedFiles(patch)
	if err != nil {
		return false, nil
	}
	if w.validateRepairPaths(ctx, attempt.Worktree, patchFiles) != nil || validateFindingScope(finding, patchFiles) != nil || validateFindingPatchSemantics(finding, patch) != nil {
		return false, nil
	}
	providerSHA, err := readOnlyCodexProviderIdentity(w.Provider)
	if err != nil {
		return false, nil
	}
	currentFiles, err := changedFiles(ctx, attempt.Worktree)
	if err != nil || len(currentFiles) == 0 || !containsExactPath(currentFiles, rejectedPath) {
		return false, nil
	}
	ignored, err := ignoredNonCachePaths(ctx, attempt.Worktree, w.Config.AllowedRepairPaths)
	if err != nil || len(ignored) != 0 {
		return false, nil
	}
	staged, err := runGit(ctx, attempt.Worktree, "diff", "--cached", "--name-only", "--")
	if err != nil || strings.TrimSpace(staged) != "" {
		return false, nil
	}
	head, err := runGit(ctx, attempt.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return false, nil
	}
	head = strings.TrimSpace(head)
	branch, err := runGit(ctx, attempt.Worktree, "branch", "--show-current")
	if err != nil || strings.TrimSpace(branch) != attempt.Branch || !validGitCommitSHA(head) {
		return false, nil
	}
	currentTree, err := captureWorktreeTree(ctx, attempt.Worktree)
	if err != nil {
		return false, nil
	}
	replayTree, replayFiles, replayErr := w.replayPreparationTree(ctx, attempt.Worktree, head)
	if replayErr != nil || replayTree != currentTree || !containsExactPath(replayFiles, rejectedPath) || !equalStringSets(currentFiles, replayFiles) {
		return false, nil
	}
	patchDigest := sha256.Sum256([]byte(patch))
	patchSHA := hex.EncodeToString(patchDigest[:])
	proof, err := preparationRecoveryProof(*attempt, head, currentTree, replayTree, rejectedPath, patchSHA, providerSHA, w.Config.PreparationCommands, w.Config.Environment)
	if err != nil {
		return false, nil
	}
	cleanupRef, cleanupCommit, err := w.createPreparationCleanupGuard(ctx, *attempt, currentTree, head, proof)
	if err != nil {
		return false, nil
	}
	attempt.ModelSummary = safeExcerpt(fmt.Sprintf("Hive recovered the complete patch from bounded model attempt %d after exact detached replay proved that the prior rejection path %s was generated only by preparation tooling. Patch SHA-256: %s. Replay proof: %s.", attempt.Attempt-1, rejectedPath, patchSHA, proof))
	attempt.ModelPatch = patch
	attempt.RecoveredPatchAttempt = attempt.Attempt - 1
	attempt.RecoveredPatchSHA256 = patchSHA
	attempt.RecoveredPatchAuthorized = false
	attempt.RecoveredProviderSHA256 = providerSHA
	attempt.PreparationRecoveryHead = head
	attempt.PreparationReplayTree = replayTree
	attempt.PreparationReplayProof = proof
	attempt.PreparationCleanupPending = true
	attempt.PreparationCleanupRef = cleanupRef
	attempt.PreparationCleanupCommit = cleanupCommit
	attempt.Stage = StageModelComplete
	if err := w.State.Put(*attempt); err != nil {
		// Store.Put can report an indeterminate durable replacement. Retain the
		// guard so either the prior state remains untouched or the desired state
		// can recover its exact evidence after restart.
		return true, err
	}
	if err := w.completePreparationContaminationCleanup(ctx, attempt); err != nil {
		return true, w.checkpointPreparationCleanupFailure(finding, attempt, err)
	}
	if err := w.ensureAttemptCounted(finding, attempt); err != nil {
		return true, err
	}
	detail := fmt.Sprintf("source_attempt=%d current_attempt=%d patch_sha256=%s provider_sha256=%s preparation_tree=%s replay_proof=%s provider_attestation=%s", attempt.RecoveredPatchAttempt, attempt.Attempt, patchSHA, providerSHA, replayTree, proof, recoveredProviderAttestation(*attempt))
	w.Lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "recover_rejected_patch", true, detail)
	cause := fmt.Errorf("verified preparation-contamination recovery requires explicit retry authorization: %s", detail)
	return true, checkpointResumableFailure(w.State, attempt, FailurePatchEngine, StageModelComplete, cause)
}

func (w *Worker) checkpointPreparationCleanupFailure(finding visualhive.FindingLifecycle, attempt *Attempt, cause error) error {
	if attempt.Stage == StageFailed {
		return &ResumableFailureError{
			RepositoryFingerprint: attempt.RepositoryFingerprint, Recurrence: attempt.Recurrence, Attempt: attempt.Attempt,
			Class: attempt.LastFailureClass, FailureID: attempt.LastFailureID, Cause: fmt.Errorf("preparation cleanup remains quarantined: %w", cause),
		}
	}
	if err := w.ensureAttemptCounted(finding, attempt); err != nil {
		return fmt.Errorf("count recovered model attempt before quarantining preparation cleanup: %w", err)
	}
	return checkpointResumableFailure(w.State, attempt, FailurePatchEngine, StageModelComplete, fmt.Errorf("preparation cleanup is quarantined with its exact guard and requires correction before retry: %w", cause))
}

func (w *Worker) createPreparationCleanupGuard(ctx context.Context, attempt Attempt, tree, head, proof string) (string, string, error) {
	if !validGitCommitSHA(tree) || !validGitCommitSHA(head) || !recoveryDigestPattern.MatchString(proof) {
		return "", "", fmt.Errorf("preparation cleanup guard inputs are invalid")
	}
	message := "Hive preparation recovery " + proof
	commit, err := runGit(ctx, attempt.Worktree,
		"-c", "user.name=Hive Repair Agent", "-c", "user.email=hive-repair@users.noreply.github.com",
		"commit-tree", tree, "-p", head, "-m", message)
	if err != nil {
		return "", "", err
	}
	commit = strings.TrimSpace(commit)
	if !validGitCommitSHA(commit) {
		return "", "", fmt.Errorf("preparation cleanup guard commit is invalid")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}
	ref := fmt.Sprintf("refs/hive/preparation-recovery/%s/%s", proof[:24], hex.EncodeToString(nonce))
	if err := w.State.recordRepairRefIntent(ctx, attempt.Worktree, attempt.Repository, "preparation_recovery", ref, commit, proof); err != nil {
		return "", "", fmt.Errorf("journal preparation cleanup guard: %w", err)
	}
	if _, err := runGit(ctx, attempt.Worktree, "update-ref", "--no-deref", ref, commit, strings.Repeat("0", 40)); err != nil {
		return "", "", err
	}
	return ref, commit, nil
}

func (w *Worker) completePreparationContaminationCleanup(ctx context.Context, attempt *Attempt) error {
	if !attempt.PreparationCleanupPending {
		return nil
	}
	if !validRecoveredPatchProvenance(*attempt) || !recoveredPatchDigestMatches(*attempt) {
		return fmt.Errorf("preparation cleanup checkpoint has invalid recovered-patch provenance")
	}
	head, err := runGit(ctx, attempt.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	branch, err := runGit(ctx, attempt.Worktree, "branch", "--show-current")
	if err != nil || strings.TrimSpace(head) != attempt.PreparationRecoveryHead || strings.TrimSpace(branch) != attempt.Branch {
		return fmt.Errorf("preparation cleanup worktree identity changed")
	}
	anchored, err := readRepairRef(ctx, attempt.Worktree, attempt.PreparationCleanupRef)
	if err != nil || anchored != attempt.PreparationCleanupCommit {
		return fmt.Errorf("preparation cleanup guard ref changed")
	}
	subject, err := runGit(ctx, attempt.Worktree, "show", "-s", "--format=%s", attempt.PreparationCleanupCommit)
	if err != nil || strings.TrimSpace(subject) != "Hive preparation recovery "+attempt.PreparationReplayProof {
		return fmt.Errorf("preparation cleanup guard ownership is invalid")
	}
	parent, err := runGit(ctx, attempt.Worktree, "rev-parse", attempt.PreparationCleanupCommit+"^")
	if err != nil || strings.TrimSpace(parent) != attempt.PreparationRecoveryHead {
		return fmt.Errorf("preparation cleanup guard parent changed")
	}
	guardTree, err := runGit(ctx, attempt.Worktree, "rev-parse", attempt.PreparationCleanupCommit+"^{tree}")
	if err != nil || strings.TrimSpace(guardTree) != attempt.PreparationReplayTree {
		return fmt.Errorf("preparation cleanup guard tree changed")
	}
	currentTree, err := captureWorktreeTree(ctx, attempt.Worktree)
	if err != nil {
		return err
	}
	headTree, err := runGit(ctx, attempt.Worktree, "rev-parse", attempt.PreparationRecoveryHead+"^{tree}")
	if err != nil {
		return err
	}
	if ok, err := treeIsCleanupIntermediate(ctx, attempt.Worktree, strings.TrimSpace(headTree), attempt.PreparationReplayTree, currentTree); err != nil || !ok {
		if err != nil {
			return err
		}
		return fmt.Errorf("preparation cleanup worktree contains bytes outside the proven clean-to-generated transition")
	}
	if err := restoreWorktreeTree(ctx, attempt.Worktree, attempt.PreparationRecoveryHead, attempt.PreparationRecoveryHead, w.Config.AllowedRepairPaths); err != nil {
		return err
	}
	cleanTree, err := captureWorktreeTree(ctx, attempt.Worktree)
	if err != nil || cleanTree != strings.TrimSpace(headTree) {
		return fmt.Errorf("preparation contamination did not restore to the exact repair head")
	}
	ref, commit := attempt.PreparationCleanupRef, attempt.PreparationCleanupCommit
	attempt.PreparationCleanupPending = false
	attempt.PreparationCleanupRef = ""
	attempt.PreparationCleanupCommit = ""
	if err := w.State.Put(*attempt); err != nil {
		return err
	}
	_ = retireRepairRef(ctx, attempt.Worktree, attempt.Repository, w.State, "preparation_recovery", ref, commit, attempt.PreparationReplayProof)
	return nil
}

func isConcreteReadOnlyCodexProvider(provider Provider) bool {
	switch value := provider.(type) {
	case CodexProvider:
		return true
	case *CodexProvider:
		return value != nil
	default:
		return false
	}
}

func readOnlyCodexProviderIdentity(provider Provider) (string, error) {
	var configured CodexProvider
	switch value := provider.(type) {
	case CodexProvider:
		configured = value
	case *CodexProvider:
		if value == nil {
			return "", fmt.Errorf("Codex provider is nil")
		}
		configured = *value
	default:
		return "", fmt.Errorf("provider is not the read-only Codex wrapper")
	}
	attestation, err := prepareCodexProviderAttestation(configured)
	if err != nil {
		return "", fmt.Errorf("attest read-only Codex provider identity: %w", err)
	}
	defer attestation.cleanupSeal()
	return attestation.IdentitySHA256, nil
}

func recoveredProviderAttestation(attempt Attempt) string {
	return "attest-historical-read-only-codex:" + attempt.RecoveredProviderSHA256
}

// RequiredRecoveredProviderAttestation returns the exact token that an
// operator must retain in the bounded retry reason for a historically
// recovered read-only Codex patch. An empty value means the checkpoint does
// not require this exceptional migration attestation.
func RequiredRecoveredProviderAttestation(attempt Attempt) string {
	if attempt.RecoveredPatchAttempt <= 0 || !recoveryDigestPattern.MatchString(attempt.RecoveredProviderSHA256) {
		return ""
	}
	return recoveredProviderAttestation(attempt)
}

func containsExactPath(paths []string, expected string) bool {
	for _, path := range paths {
		if filepath.ToSlash(path) == expected {
			return true
		}
	}
	return false
}

func (w *Worker) replayPreparationTree(ctx context.Context, sourceWorktree, head string) (tree string, files []string, resultErr error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, err
	}
	replay := filepath.Join(w.Config.WorktreeRoot, ".preparation-replay-"+hex.EncodeToString(nonce))
	root, err := filepath.Abs(w.Config.WorktreeRoot)
	if err != nil {
		return "", nil, err
	}
	replayAbs, err := filepath.Abs(replay)
	if err != nil || replayAbs == root || !strings.HasPrefix(strings.ToLower(replayAbs), strings.ToLower(root+string(filepath.Separator))) {
		return "", nil, fmt.Errorf("preparation replay path escapes the Hive worktree root")
	}
	if _, err := runGit(ctx, sourceWorktree, "worktree", "add", "--detach", replay, head); err != nil {
		return "", nil, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := runGit(cleanupCtx, sourceWorktree, "worktree", "remove", "--force", replay); err != nil && resultErr == nil {
			resultErr = fmt.Errorf("remove preparation replay worktree: %w", err)
		}
	}()
	if err := normalizeTrackedLineEndings(ctx, replay); err != nil {
		return "", nil, err
	}
	for _, command := range w.Config.PreparationCommands {
		if err := runRepairCommand(ctx, replay, command, w.Config.CommandTimeout, w.Config.Environment, "preparation replay"); err != nil {
			return "", nil, err
		}
	}
	ignored, err := ignoredNonCachePaths(ctx, replay, w.Config.AllowedRepairPaths)
	if err != nil || len(ignored) != 0 {
		return "", nil, fmt.Errorf("preparation replay produced ignored non-cache paths")
	}
	tree, err = captureWorktreeTree(ctx, replay)
	if err != nil {
		return "", nil, err
	}
	files, err = changedFiles(ctx, replay)
	if err != nil {
		return "", nil, err
	}
	return tree, files, nil
}

func preparationRecoveryProof(attempt Attempt, head, currentTree, replayTree, rejectedPath, patchSHA, providerSHA string, commands []Command, environment map[string]string) (string, error) {
	configuration, err := json.Marshal(struct {
		Commands    []Command         `json:"commands"`
		Environment map[string]string `json:"environment"`
	}{Commands: commands, Environment: environment})
	if err != nil {
		return "", err
	}
	value := strings.Join([]string{
		attempt.Repository, attempt.RepositoryFingerprint, fmt.Sprint(attempt.Recurrence), fmt.Sprint(attempt.Attempt),
		fmt.Sprint(attempt.Attempt - 1), head, currentTree, replayTree, rejectedPath, patchSHA, providerSHA, string(configuration),
	}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:]), nil
}
