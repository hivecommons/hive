package repair

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/internal/gittransport"
)

const (
	toolSnapshotPreparation = "preparation"
	toolSnapshotValidation  = "validation"
)

var toolSnapshotRefPattern = regexp.MustCompile(`^refs/hive/repair-tool-snapshots/[a-f0-9]{24}/[a-f0-9]{32}$`)
var recoveryDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var preparationCleanupRefPattern = regexp.MustCompile(`^refs/hive/preparation-recovery/[a-f0-9]{24}/[a-f0-9]{32}$`)

func hasToolSnapshot(attempt Attempt) bool {
	return attempt.ToolSnapshotRef != "" || attempt.ToolSnapshotCommit != "" || attempt.ToolSnapshotHead != "" ||
		attempt.ToolSnapshotBranch != "" || attempt.ToolSnapshotPhase != "" || !attempt.ToolSnapshotStartedAt.IsZero() || !attempt.ToolSnapshotDeadline.IsZero()
}

func validToolSnapshot(attempt Attempt) bool {
	if !hasToolSnapshot(attempt) {
		return true
	}
	if !toolSnapshotRefPattern.MatchString(attempt.ToolSnapshotRef) || !validGitCommitSHA(attempt.ToolSnapshotCommit) ||
		!validGitCommitSHA(attempt.ToolSnapshotHead) || !validHiveRepairBranch(attempt.ToolSnapshotBranch) ||
		attempt.ToolSnapshotStartedAt.IsZero() || !attempt.ToolSnapshotDeadline.After(attempt.ToolSnapshotStartedAt) ||
		attempt.ToolSnapshotDeadline.Sub(attempt.ToolSnapshotStartedAt) > 24*time.Hour+time.Minute {
		return false
	}
	if !attempt.UpdatedAt.IsZero() && attempt.ToolSnapshotStartedAt.After(attempt.UpdatedAt.Add(time.Minute)) {
		return false
	}
	if attempt.ToolSnapshotPhase == toolSnapshotPreparation {
		return attempt.Stage == StagePrepared || attempt.Stage == StageFailed && attempt.ResumeStage == StagePrepared
	}
	if attempt.ToolSnapshotPhase == toolSnapshotValidation {
		return attempt.Stage == StageModelComplete || attempt.Stage == StageFailed && attempt.ResumeStage == StageModelComplete
	}
	return false
}

func validRecoveredPatchProvenance(attempt Attempt) bool {
	hasAny := attempt.RecoveredPatchAttempt != 0 || attempt.RecoveredPatchSHA256 != "" || attempt.RecoveredPatchAuthorized ||
		attempt.RecoveredProviderSHA256 != "" || attempt.PreparationRecoveryHead != "" || attempt.PreparationReplayTree != "" || attempt.PreparationReplayProof != "" ||
		attempt.PreparationCleanupPending || attempt.PreparationCleanupRef != "" || attempt.PreparationCleanupCommit != ""
	if !hasAny {
		return true
	}
	if attempt.RecoveredPatchAttempt <= 0 || attempt.RecoveredPatchAttempt != attempt.Attempt-1 ||
		!recoveryDigestPattern.MatchString(attempt.RecoveredPatchSHA256) || !recoveryDigestPattern.MatchString(attempt.RecoveredProviderSHA256) || !validGitCommitSHA(attempt.PreparationRecoveryHead) ||
		!validGitCommitSHA(attempt.PreparationReplayTree) || !recoveryDigestPattern.MatchString(attempt.PreparationReplayProof) {
		return false
	}
	if attempt.ModelPatch != "" && !recoveredPatchDigestMatches(attempt) {
		return false
	}
	if attempt.PreparationCleanupPending {
		if attempt.ModelPatch == "" || !preparationCleanupRefPattern.MatchString(attempt.PreparationCleanupRef) ||
			!validGitCommitSHA(attempt.PreparationCleanupCommit) || (attempt.Stage != StageModelComplete && attempt.Stage != StageFailed) {
			return false
		}
	} else if attempt.PreparationCleanupRef != "" || attempt.PreparationCleanupCommit != "" {
		return false
	}
	if attempt.RecoveredPatchAuthorized && (attempt.RetryTransactionID == "" || attempt.RetryResumeStage != StageModelComplete) {
		return false
	}
	switch attempt.Stage {
	case StageModelComplete:
		return attempt.ModelPatch != "" && recoveredPatchDigestMatches(attempt)
	case StageFailed:
		return attempt.ResumeStage == StageModelComplete && attempt.ModelPatch != "" && recoveredPatchDigestMatches(attempt)
	case StageValidated, StageCommitted, StagePushed, StagePROpen:
		return attempt.RecoveredPatchAuthorized && attempt.ModelPatch == "" && !attempt.PreparationCleanupPending
	default:
		return false
	}
}

func recoveredPatchDigestMatches(attempt Attempt) bool {
	if attempt.ModelPatch == "" || !recoveryDigestPattern.MatchString(attempt.RecoveredPatchSHA256) {
		return false
	}
	digest := sha256.Sum256([]byte(attempt.ModelPatch))
	return hex.EncodeToString(digest[:]) == attempt.RecoveredPatchSHA256
}

func clearRecoveredPatchProvenance(attempt *Attempt) {
	attempt.RecoveredPatchAttempt = 0
	attempt.RecoveredPatchSHA256 = ""
	attempt.RecoveredPatchAuthorized = false
	attempt.RecoveredProviderSHA256 = ""
	attempt.PreparationRecoveryHead = ""
	attempt.PreparationReplayTree = ""
	attempt.PreparationReplayProof = ""
	attempt.PreparationCleanupPending = false
	attempt.PreparationCleanupRef = ""
	attempt.PreparationCleanupCommit = ""
}

func clearToolSnapshot(attempt *Attempt) {
	attempt.ToolSnapshotRef = ""
	attempt.ToolSnapshotCommit = ""
	attempt.ToolSnapshotHead = ""
	attempt.ToolSnapshotBranch = ""
	attempt.ToolSnapshotPhase = ""
	attempt.ToolSnapshotStartedAt = time.Time{}
	attempt.ToolSnapshotDeadline = time.Time{}
}

func clearSealedCandidate(attempt *Attempt) {
	attempt.ModelBaseTree = ""
	attempt.ModelBaseParent = ""
	attempt.ModelBaseGuardRef = ""
	attempt.ModelBaseGuardCommit = ""
	attempt.ModelBaseGuardBinding = ""
	attempt.ModelBaseGuardKind = ""
	attempt.CandidateTree = ""
	attempt.CandidateParent = ""
	attempt.CandidateGuardRef = ""
	attempt.CandidateGuardCommit = ""
	attempt.CandidateGuardBinding = ""
	attempt.CandidateGuardKind = ""
}

// withToolSnapshot keeps dependency installation and test tooling outside the
// model-authored repair contribution. Ignored caches remain available, while
// every non-ignored byte is restored to the exact pre-tool Git tree.
func (w *Worker) withToolSnapshot(ctx context.Context, attempt *Attempt, phase string, commandCount int, operation func() error) error {
	if err := w.beginToolSnapshot(ctx, attempt, phase, commandCount); err != nil {
		return err
	}
	operationErr := operation()
	// Repository Git control-state mutation is a quarantine condition. Do not
	// invoke even read-only Git for delta inspection or snapshot restoration;
	// the target command may have replaced the linked-worktree locator or
	// installed executable config. The durable snapshot remains pending and a
	// resumed Worker performs the same direct filesystem preflight first.
	if isUnsafeRepositoryGitControlFailure(operationErr) {
		return operationErr
	}
	if phase == toolSnapshotValidation {
		if err := validateValidationToolDelta(ctx, *attempt, w.Config.AllowedRepairPaths); err != nil {
			if operationErr != nil {
				err = fmt.Errorf("validation command failed (%v) and changed non-generated repair bytes: %w", operationErr, err)
			}
			operationErr = patchEngineInfrastructureFailure(err)
		}
	}
	restoreErr := w.restoreToolSnapshot(ctx, attempt, false)
	if operationErr != nil && restoreErr != nil {
		return patchEngineInfrastructureFailure(fmt.Errorf("tool command failed (%v) and exact repair contribution restoration failed: %w", operationErr, restoreErr))
	}
	if restoreErr != nil {
		return patchEngineInfrastructureFailure(restoreErr)
	}
	return operationErr
}

func (w *Worker) beginToolSnapshot(ctx context.Context, attempt *Attempt, phase string, commandCount int) error {
	if phase != toolSnapshotPreparation && phase != toolSnapshotValidation {
		return fmt.Errorf("unsupported repair tool snapshot phase %q", phase)
	}
	if hasToolSnapshot(*attempt) {
		return fmt.Errorf("repair tool snapshot is already pending")
	}
	if err := validateToolSnapshotSupport(ctx, attempt.Worktree); err != nil {
		return err
	}
	ignored, err := ignoredNonCachePaths(ctx, attempt.Worktree, w.Config.AllowedRepairPaths)
	if err != nil {
		return fmt.Errorf("inspect ignored repair paths before %s: %w", phase, err)
	}
	if len(ignored) != 0 {
		return fmt.Errorf("repair worktree has ignored non-cache paths before %s; refusing to persist or trust ignored content: %s", phase, strings.Join(ignored, ","))
	}
	head, err := runGit(ctx, attempt.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read repair head before %s: %w", phase, err)
	}
	head = strings.TrimSpace(head)
	branch, err := runGit(ctx, attempt.Worktree, "branch", "--show-current")
	if err != nil {
		return fmt.Errorf("read repair branch before %s: %w", phase, err)
	}
	branch = strings.TrimSpace(branch)
	if !validGitCommitSHA(head) || branch != attempt.Branch || !validHiveRepairBranch(branch) {
		return fmt.Errorf("repair worktree identity changed before %s", phase)
	}
	staged, err := runGit(ctx, attempt.Worktree, "diff", "--cached", "--name-only", "--")
	if err != nil {
		return fmt.Errorf("inspect repair index before %s: %w", phase, err)
	}
	if strings.TrimSpace(staged) != "" {
		return fmt.Errorf("repair index is not clean before %s", phase)
	}
	tree := ""
	if phase == toolSnapshotValidation && validGitCommitSHA(attempt.CandidateTree) && validGitCommitSHA(attempt.CandidateParent) {
		if attempt.CandidateParent != head {
			return fmt.Errorf("sealed repair candidate parent changed before validation")
		}
		tree = attempt.CandidateTree
		actualTree, captureErr := captureWorktreeTree(ctx, attempt.Worktree)
		if captureErr != nil {
			return fmt.Errorf("capture repair contribution before %s: %w", phase, captureErr)
		}
		if strings.TrimSpace(actualTree) != tree {
			return patchEngineInfrastructureFailure(fmt.Errorf("repair worktree changed after the exact model patch tree was sealed; possible delayed preparation process contamination"))
		}
	} else {
		tree, err = captureWorktreeTree(ctx, attempt.Worktree)
		if err != nil {
			return fmt.Errorf("capture repair contribution before %s: %w", phase, err)
		}
	}
	binding := toolSnapshotBinding(*attempt, phase, head, branch)
	message := "Hive repair tool snapshot " + binding
	commit, err := runGit(ctx, attempt.Worktree,
		"-c", "user.name=Hive Repair Agent", "-c", "user.email=hive-repair@users.noreply.github.com",
		"commit-tree", tree, "-p", head, "-m", message)
	if err != nil {
		return fmt.Errorf("anchor repair contribution before %s: %w", phase, err)
	}
	commit = strings.TrimSpace(commit)
	if !validGitCommitSHA(commit) {
		return fmt.Errorf("repair tool snapshot commit is invalid")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("create repair tool snapshot nonce: %w", err)
	}
	ref := fmt.Sprintf("refs/hive/repair-tool-snapshots/%s/%s", binding[:24], hex.EncodeToString(nonce))
	if err := w.State.recordRepairRefIntent(ctx, attempt.Worktree, attempt.Repository, phase, ref, commit, binding); err != nil {
		return fmt.Errorf("journal repair contribution guard before %s: %w", phase, err)
	}
	if _, err := runGit(ctx, attempt.Worktree, "update-ref", "--no-deref", ref, commit, strings.Repeat("0", 40)); err != nil {
		return fmt.Errorf("protect repair contribution before %s: %w", phase, err)
	}
	attempt.ToolSnapshotRef = ref
	attempt.ToolSnapshotCommit = commit
	attempt.ToolSnapshotHead = head
	attempt.ToolSnapshotBranch = branch
	attempt.ToolSnapshotPhase = phase
	attempt.ToolSnapshotStartedAt = time.Now().UTC()
	timeout := w.Config.CommandTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	if commandCount < 1 {
		commandCount = 1
	}
	if commandCount > 100 || timeout > 24*time.Hour/time.Duration(commandCount) {
		_ = retireRepairRef(ctx, attempt.Worktree, attempt.Repository, w.State, phase, ref, commit, binding)
		clearToolSnapshot(attempt)
		return fmt.Errorf("repair tool command batch exceeds the snapshot recovery safety limit")
	}
	attempt.ToolSnapshotDeadline = attempt.ToolSnapshotStartedAt.Add(time.Duration(commandCount)*timeout + time.Minute)
	// Store.Put timestamps its value copy. Keep the in-memory checkpoint close
	// to that durable timestamp too, otherwise a legitimately resumed attempt
	// whose prior UpdatedAt is old can fail its own future-time invariant during
	// the immediate post-command restoration.
	attempt.UpdatedAt = time.Now().UTC()
	if err := w.State.Put(*attempt); err != nil {
		// Store.Put can fail after an indeterminate atomic replacement. Retain
		// the ref: an orphan is harmless, while deleting a ref referenced by the
		// desired on-disk state would make exact crash recovery impossible.
		return err
	}
	return nil
}

func (w *Worker) restoreToolSnapshot(ctx context.Context, attempt *Attempt, recovering bool) error {
	if !validToolSnapshot(*attempt) || !hasToolSnapshot(*attempt) {
		return fmt.Errorf("repair tool snapshot checkpoint is incomplete")
	}
	if recovering && time.Now().UTC().Before(attempt.ToolSnapshotDeadline) && !repairProcessTreeCrashContainmentGuaranteed() {
		return fmt.Errorf("interrupted repair tool process may remain active until %s; refusing to race exact contribution restoration", attempt.ToolSnapshotDeadline.Format(time.RFC3339Nano))
	}
	currentHead, err := runGit(ctx, attempt.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read repair head while restoring tool snapshot: %w", err)
	}
	currentBranch, err := runGit(ctx, attempt.Worktree, "branch", "--show-current")
	if err != nil {
		return fmt.Errorf("read repair branch while restoring tool snapshot: %w", err)
	}
	if strings.TrimSpace(currentHead) != attempt.ToolSnapshotHead || strings.TrimSpace(currentBranch) != attempt.ToolSnapshotBranch || strings.TrimSpace(currentBranch) != attempt.Branch {
		return fmt.Errorf("repair tool changed the exact branch or head; refusing automatic restoration")
	}
	anchored, err := readRepairRef(ctx, attempt.Worktree, attempt.ToolSnapshotRef)
	if err != nil || anchored != attempt.ToolSnapshotCommit {
		return fmt.Errorf("repair tool snapshot ref no longer resolves to its exact commit")
	}
	wantBinding := toolSnapshotBinding(*attempt, attempt.ToolSnapshotPhase, attempt.ToolSnapshotHead, attempt.ToolSnapshotBranch)
	subject, err := runGit(ctx, attempt.Worktree, "show", "-s", "--format=%s", attempt.ToolSnapshotCommit)
	if err != nil || strings.TrimSpace(subject) != "Hive repair tool snapshot "+wantBinding {
		return fmt.Errorf("repair tool snapshot ownership binding is invalid")
	}
	parent, err := runGit(ctx, attempt.Worktree, "rev-parse", attempt.ToolSnapshotCommit+"^")
	if err != nil || strings.TrimSpace(parent) != attempt.ToolSnapshotHead {
		return fmt.Errorf("repair tool snapshot is not anchored to the exact repair head")
	}
	if err := restoreWorktreeTree(ctx, attempt.Worktree, attempt.ToolSnapshotCommit, attempt.ToolSnapshotHead, w.Config.AllowedRepairPaths); err != nil {
		return err
	}
	actualTree, err := captureWorktreeTree(ctx, attempt.Worktree)
	if err != nil {
		return fmt.Errorf("verify restored repair contribution: %w", err)
	}
	expectedTree, err := runGit(ctx, attempt.Worktree, "rev-parse", attempt.ToolSnapshotCommit+"^{tree}")
	if err != nil || strings.TrimSpace(actualTree) != strings.TrimSpace(expectedTree) {
		return fmt.Errorf("repair contribution did not restore to its exact pre-tool tree")
	}
	ref, commit, kind := attempt.ToolSnapshotRef, attempt.ToolSnapshotCommit, attempt.ToolSnapshotPhase
	if kind == toolSnapshotPreparation {
		attempt.ModelBaseTree = strings.TrimSpace(expectedTree)
		attempt.ModelBaseParent = attempt.ToolSnapshotHead
		attempt.ModelBaseGuardRef = ref
		attempt.ModelBaseGuardCommit = commit
		attempt.ModelBaseGuardBinding = wantBinding
		attempt.ModelBaseGuardKind = toolSnapshotPreparation
	} else if kind == toolSnapshotValidation {
		attempt.CandidateTree = strings.TrimSpace(expectedTree)
		attempt.CandidateParent = attempt.ToolSnapshotHead
		attempt.CandidateGuardRef = ref
		attempt.CandidateGuardCommit = commit
		attempt.CandidateGuardBinding = wantBinding
		attempt.CandidateGuardKind = toolSnapshotValidation
	}
	clearToolSnapshot(attempt)
	if err := w.State.Put(*attempt); err != nil {
		return err
	}
	// The durable state no longer depends on this ref. A failed deletion leaves
	// only a harmless owned object anchor and never weakens contribution checks.
	if kind != toolSnapshotPreparation && kind != toolSnapshotValidation {
		_ = retireRepairRef(ctx, attempt.Worktree, attempt.Repository, w.State, kind, ref, commit, wantBinding)
	}
	return nil
}

func toolSnapshotBinding(attempt Attempt, phase, head, branch string) string {
	value := strings.Join([]string{
		attempt.Repository, attempt.RepositoryFingerprint, strconv.Itoa(attempt.Recurrence), strconv.Itoa(attempt.Attempt), phase, head, branch,
	}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func validateToolSnapshotSupport(ctx context.Context, worktree string) error {
	sparse, err := gitBooleanConfig(ctx, worktree, "core.sparseCheckout")
	if err != nil {
		return fmt.Errorf("inspect sparse-checkout before repair tooling: %w", err)
	}
	if sparse {
		return fmt.Errorf("repair tooling snapshots do not support sparse checkouts")
	}
	submodules, err := runGit(ctx, worktree, "submodule", "status", "--recursive")
	if err != nil {
		return fmt.Errorf("inspect submodules before repair tooling: %w", err)
	}
	if strings.TrimSpace(submodules) != "" {
		return fmt.Errorf("repair tooling snapshots do not support submodules")
	}
	return nil
}

func gitBooleanConfig(ctx context.Context, worktree, key string) (bool, error) {
	command := exec.CommandContext(ctx, "git", "config", "--bool", "--get", key)
	command.Dir = worktree
	command.Env = gitCommandEnvironment(nil)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && strings.TrimSpace(output.String()) == "" {
			return false, nil
		}
		return false, fmt.Errorf("git config --bool --get %s: %w: %s", key, err, safeExcerpt(output.String()))
	}
	value := strings.TrimSpace(output.String())
	return value == "true", nil
}

func captureWorktreeTree(ctx context.Context, worktree string) (string, error) {
	index, err := os.CreateTemp("", "hive-repair-snapshot-index-*")
	if err != nil {
		return "", err
	}
	indexPath := index.Name()
	if err := index.Close(); err != nil {
		_ = os.Remove(indexPath)
		return "", err
	}
	if err := os.Remove(indexPath); err != nil {
		return "", err
	}
	defer os.Remove(indexPath)
	environment := map[string]string{"GIT_INDEX_FILE": indexPath}
	if _, err := runGitWithEnvironment(ctx, worktree, environment, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	if _, err := runGitWithEnvironment(ctx, worktree, environment, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	tree, err := runGitWithEnvironment(ctx, worktree, environment, "write-tree")
	if err != nil {
		return "", err
	}
	tree = strings.TrimSpace(tree)
	if !validGitCommitSHA(tree) {
		return "", fmt.Errorf("captured worktree tree has invalid object ID")
	}
	return tree, nil
}

func runGitWithEnvironment(ctx context.Context, dir string, extra map[string]string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = gitCommandEnvironment(extra)
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return output.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, safeExcerpt(output.String()))
	}
	return output.String(), nil
}

type exactGitBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (b *exactGitBuffer) Write(value []byte) (int, error) {
	const limit = 32 << 20
	original := len(value)
	if b.Len() < limit {
		remaining := limit - b.Len()
		if len(value) > remaining {
			_, _ = b.Buffer.Write(value[:remaining])
			b.exceeded = true
		} else {
			_, _ = b.Buffer.Write(value)
		}
	} else if len(value) > 0 {
		b.exceeded = true
	}
	return original, nil
}

func runGitExact(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = gitCommandEnvironment(nil)
	var output exactGitBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return output.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, safeExcerpt(output.String()))
	}
	if output.exceeded {
		return "", fmt.Errorf("git %s exceeded the exact-output safety limit", strings.Join(args, " "))
	}
	return output.String(), nil
}

func gitCommandEnvironment(extra map[string]string) []string {
	blocked := map[string]bool{}
	for key := range extra {
		blocked[strings.ToUpper(key)] = true
	}
	result := make([]string, 0, len(os.Environ())+len(extra)+3)
	for _, pair := range providerEnvironment() {
		key, _, _ := strings.Cut(pair, "=")
		if !blocked[strings.ToUpper(key)] {
			result = append(result, pair)
		}
	}
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+extra[key])
	}
	return gittransport.LocalEnvironmentWithOverrides(result, extra)
}

func restoreWorktreeTree(ctx context.Context, worktree, snapshotCommit, head string, allowedPatterns []string) error {
	snapshotPaths, err := gitTreePaths(ctx, worktree, snapshotCommit)
	if err != nil {
		return fmt.Errorf("read repair tool snapshot paths: %w", err)
	}
	untracked, err := runGitExact(ctx, worktree, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return fmt.Errorf("read generated repair tool paths: %w", err)
	}
	currentUntracked := splitGitPaths(untracked)
	ignored, err := ignoredNonCachePaths(ctx, worktree, allowedPatterns)
	if err != nil {
		return fmt.Errorf("read ignored generated repair tool paths: %w", err)
	}
	currentUntracked = append(currentUntracked, ignored...)
	for _, path := range uniqueSortedPaths(currentUntracked) {
		if snapshotPaths[path] {
			continue
		}
		if err := removeWorktreePath(worktree, path); err != nil {
			return fmt.Errorf("remove generated repair tool path %s: %w", path, err)
		}
	}
	if _, err := runGit(ctx, worktree, "read-tree", "--reset", "-u", snapshotCommit); err != nil {
		return fmt.Errorf("restore exact pre-tool repair tree: %w", err)
	}
	if _, err := runGit(ctx, worktree, "reset", head, "--", "."); err != nil {
		return fmt.Errorf("restore clean repair index after tooling: %w", err)
	}
	staged, err := runGit(ctx, worktree, "diff", "--cached", "--name-only", "--")
	if err != nil || strings.TrimSpace(staged) != "" {
		return fmt.Errorf("repair index did not restore to the exact head")
	}
	ignored, err = ignoredNonCachePaths(ctx, worktree, allowedPatterns)
	if err != nil {
		return fmt.Errorf("verify restored ignored repair paths: %w", err)
	}
	if len(ignored) != 0 {
		return fmt.Errorf("ignored non-cache repair paths survived exact restoration: %s", strings.Join(ignored, ","))
	}
	return nil
}

func ignoredNonCachePaths(ctx context.Context, worktree string, allowedPatterns []string) ([]string, error) {
	output, err := runGitExact(ctx, worktree, "status", "--porcelain=v1", "-z", "--ignored=matching", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, record := range strings.Split(output, "\x00") {
		if len(record) < 4 || record[:2] != "!!" {
			continue
		}
		path := filepath.ToSlash(record[3:])
		path = strings.TrimSuffix(path, "/")
		if path == "" || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
			return nil, fmt.Errorf("git reported an unsafe ignored path")
		}
		if !repairPathRestricted(path) && !repairPathAllowed(path, allowedPatterns) && approvedIgnoredCachePath(path) {
			continue
		}
		paths = append(paths, path)
	}
	return minimalPathRoots(paths), nil
}

func approvedIgnoredCachePath(path string) bool {
	segments := strings.Split(strings.ToLower(filepath.ToSlash(path)), "/")
	for _, segment := range segments {
		switch segment {
		case "node_modules", ".venv", "venv", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox", ".nox",
			".gradle", ".cache", ".visual-hive", ".next", ".nuxt", ".turbo", "coverage", "target":
			return true
		}
		if strings.HasSuffix(segment, ".egg-info") {
			return true
		}
	}
	return false
}

func minimalPathRoots(paths []string) []string {
	paths = uniqueSortedPaths(paths)
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		covered := false
		for _, root := range result {
			if path == root || strings.HasPrefix(path, root+"/") {
				covered = true
				break
			}
		}
		if !covered {
			result = append(result, path)
		}
	}
	return result
}

func uniqueSortedPaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.ToSlash(path)
		if path != "" && !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result
}

func gitTreePaths(ctx context.Context, worktree, treeish string) (map[string]bool, error) {
	output, err := runGitExact(ctx, worktree, "ls-tree", "-r", "-z", "--name-only", treeish)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool)
	for _, path := range splitGitPaths(output) {
		result[path] = true
	}
	return result, nil
}

func gitTreeEntries(ctx context.Context, worktree, treeish string) (map[string]string, error) {
	output, err := runGitExact(ctx, worktree, "ls-tree", "-r", "-z", treeish)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]string)
	for _, record := range strings.Split(output, "\x00") {
		if record == "" {
			continue
		}
		tab := strings.IndexByte(record, '\t')
		if tab <= 0 || tab == len(record)-1 {
			return nil, fmt.Errorf("git tree contains an invalid entry")
		}
		path := filepath.ToSlash(record[tab+1:])
		if path == "" {
			return nil, fmt.Errorf("git tree contains an empty path")
		}
		entries[path] = record[:tab]
	}
	return entries, nil
}

func treeIsCleanupIntermediate(ctx context.Context, worktree, cleanTree, generatedTree, currentTree string) (bool, error) {
	clean, err := gitTreeEntries(ctx, worktree, cleanTree)
	if err != nil {
		return false, err
	}
	generated, err := gitTreeEntries(ctx, worktree, generatedTree)
	if err != nil {
		return false, err
	}
	current, err := gitTreeEntries(ctx, worktree, currentTree)
	if err != nil {
		return false, err
	}
	paths := make(map[string]bool, len(clean)+len(generated)+len(current))
	for path := range clean {
		paths[path] = true
	}
	for path := range generated {
		paths[path] = true
	}
	for path := range current {
		paths[path] = true
	}
	for path := range paths {
		if current[path] != clean[path] && current[path] != generated[path] {
			return false, nil
		}
	}
	return true, nil
}

func splitGitPaths(output string) []string {
	result := []string{}
	for _, path := range strings.Split(output, "\x00") {
		path = filepath.ToSlash(path)
		if path != "" {
			result = append(result, path)
		}
	}
	return result
}

func removeWorktreePath(worktree, path string) error {
	normalized := filepath.ToSlash(path)
	if normalized == "" || filepath.IsAbs(normalized) || normalized == ".." || strings.HasPrefix(normalized, "../") {
		return fmt.Errorf("unsafe generated path")
	}
	target := filepath.Join(worktree, filepath.FromSlash(normalized))
	relative, err := filepath.Rel(worktree, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("generated path escapes the worktree")
	}
	info, err := os.Lstat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return os.RemoveAll(target)
	}
	return os.Remove(target)
}

func validateValidationToolDelta(ctx context.Context, attempt Attempt, allowedPatterns []string) error {
	ignored, err := ignoredNonCachePaths(ctx, attempt.Worktree, allowedPatterns)
	if err != nil {
		return fmt.Errorf("inspect validation-generated ignored paths: %w", err)
	}
	for _, path := range ignored {
		if repairPathRestricted(path) || repairPathAllowed(path, allowedPatterns) {
			return fmt.Errorf("validation command generated ignored repair-eligible path %s", path)
		}
	}
	beforeTree, err := runGit(ctx, attempt.Worktree, "rev-parse", attempt.ToolSnapshotCommit+"^{tree}")
	if err != nil {
		return fmt.Errorf("read pre-validation repair tree: %w", err)
	}
	afterTree, err := captureWorktreeTree(ctx, attempt.Worktree)
	if err != nil {
		return fmt.Errorf("capture post-validation repair tree: %w", err)
	}
	beforeTree, afterTree = strings.TrimSpace(beforeTree), strings.TrimSpace(afterTree)
	if beforeTree == afterTree {
		return nil
	}
	changed, err := runGitExact(ctx, attempt.Worktree, "diff-tree", "--no-commit-id", "--no-renames", "--name-only", "-r", "-z", beforeTree, afterTree)
	if err != nil {
		return fmt.Errorf("inspect validation-generated paths: %w", err)
	}
	snapshotPaths, err := gitTreePaths(ctx, attempt.Worktree, beforeTree)
	if err != nil {
		return err
	}
	headPaths, err := gitTreePaths(ctx, attempt.Worktree, attempt.ToolSnapshotHead)
	if err != nil {
		return err
	}
	for _, path := range splitGitPaths(changed) {
		if snapshotPaths[path] || headPaths[path] {
			return fmt.Errorf("validation command modified repair candidate path %s", path)
		}
		if repairPathRestricted(path) || repairPathAllowed(path, allowedPatterns) {
			return fmt.Errorf("validation command generated repair-eligible path %s", path)
		}
	}
	return nil
}

func repairPathRestricted(file string) bool {
	normalized := strings.TrimPrefix(filepath.ToSlash(file), "./")
	lower := strings.ToLower(normalized)
	if strings.HasPrefix(lower, ".github/") || strings.Contains(lower, "baseline") || strings.Contains(lower, "secret") || strings.Contains(lower, "auth") ||
		strings.HasPrefix(lower, "deploy") || strings.Contains(lower, "terraform") {
		return true
	}
	parts := strings.Split(lower, "/")
	for _, part := range parts[:len(parts)-1] {
		if strings.HasSuffix(part, "-snapshots") {
			return true
		}
		switch part {
		case "__snapshots__", "__screenshots__", "__image_snapshots__":
			return true
		}
	}
	name := parts[len(parts)-1]
	if filepath.Ext(name) == ".snap" || strings.Contains(name, ".snapshot.") || strings.Contains(name, ".screenshot.") {
		return true
	}
	switch filepath.Ext(lower) {
	case ".avif", ".bmp", ".gif", ".ico", ".jpeg", ".jpg", ".png", ".tif", ".tiff", ".webp":
		for _, part := range parts[:len(parts)-1] {
			switch part {
			case "test", "tests", "__tests__", "testdata":
				return true
			}
		}
	}
	return false
}

func repairPathAllowed(file string, allowedPatterns []string) bool {
	if len(allowedPatterns) == 0 {
		allowedPatterns = []string{"src/**", "test/**", "tests/**", "**/*.test.*", "**/*.spec.*", "**/*_test.go"}
	}
	normalized := strings.TrimPrefix(filepath.ToSlash(file), "./")
	for _, pattern := range allowedPatterns {
		if matchPathPattern(pattern, normalized) {
			return true
		}
	}
	return false
}
