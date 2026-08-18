package repair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/kubestellar/hive/internal/gittransport"
)

const maxRepairRefreshDiffBytes = 16 << 20

type RefreshedBranchCheckpointRequest struct {
	Worktree                    string
	Branch                      string
	RepositoryID                string
	ExpectedRemoteURL           string
	BaselineProtection          BaselineProtection
	RemoteHeadSHA               string
	BaseBranch                  string
	BaseSHA                     string
	ExpectedChangedFiles        []string
	ExpectedContributionPatchID string
	AllowLegacyHeadAdoption     bool
	ValidationCommands          []Command
	Environment                 map[string]string
	CommandTimeout              time.Duration
}

type RefreshedBranchCheckpoint struct {
	HeadSHA             string   `json:"head_sha"`
	BaseSHA             string   `json:"base_sha"`
	ContributionPatchID string   `json:"contribution_patch_id"`
	ChangedFiles        []string `json:"changed_files"`
}

type RepairBranchRefreshConflictError struct {
	HeadSHA string
	BaseSHA string
	Cause   error
}

type RepairBranchRefreshRefMovedError struct {
	Ref      string
	Expected string
	Observed string
}

func (e *RepairBranchRefreshRefMovedError) Error() string {
	return fmt.Sprintf("fetched ref %s is %s, expected exact SHA %s", e.Ref, e.Observed, e.Expected)
}

func IsRepairBranchRefreshRefMoved(err error) bool {
	var moved *RepairBranchRefreshRefMovedError
	return errors.As(err, &moved)
}

func (e *RepairBranchRefreshConflictError) Error() string {
	return fmt.Sprintf("repair contribution at %s conflicts with exact protected base %s: %v", e.HeadSHA, e.BaseSHA, e.Cause)
}

func (e *RepairBranchRefreshConflictError) Unwrap() error { return e.Cause }

func IsRepairBranchRefreshConflict(err error) bool {
	var conflict *RepairBranchRefreshConflictError
	return errors.As(err, &conflict)
}

// CheckpointRepairRefreshConflict durably pauses an exact PR-open repair at a
// supported retry-repair checkpoint. It is idempotent for the deterministic
// failure identity, preserves the model-attempt count and PR binding, and
// resumes only to StagePROpen after explicit operator authorization.
func CheckpointRepairRefreshConflict(store *Store, attempt Attempt, headSHA, baseSHA, reason string) (Attempt, error) {
	if store == nil || attempt.Stage != StagePROpen || !attempt.AttemptCounted || !attempt.LifecyclePROpen || attempt.PRNumber <= 0 ||
		!validGitCommitSHA(headSHA) || !validGitCommitSHA(baseSHA) || !strings.EqualFold(attempt.CommitSHA, headSHA) || strings.TrimSpace(reason) == "" {
		return Attempt{}, fmt.Errorf("exact PR-open repair checkpoint is required for refresh conflict recovery")
	}
	failureID, err := RepairRefreshConflictFailureID(attempt, headSHA, baseSHA, reason)
	if err != nil {
		return Attempt{}, err
	}
	current, exists := store.Get(attempt.RepositoryFingerprint)
	if !exists {
		return Attempt{}, fmt.Errorf("repair refresh conflict checkpoint disappeared")
	}
	if current.Stage == StageFailed && current.LastFailureClass == FailureInfrastructure && current.LastFailureID == failureID && current.ResumeStage == StagePROpen {
		return current, nil
	}
	if current.Stage != StagePROpen || current.Recurrence != attempt.Recurrence || current.Attempt != attempt.Attempt ||
		!strings.EqualFold(current.CommitSHA, headSHA) || current.PRNumber != attempt.PRNumber || current.Branch != attempt.Branch {
		return Attempt{}, fmt.Errorf("repair refresh conflict checkpoint changed before it could be paused")
	}
	current.ModelSummary = safeExcerpt(current.ModelSummary + "\n\nHive's exact protected-base refresh conflicted before any remote mutation:\n" + reason)
	current.LastFailureClass = FailureInfrastructure
	current.LastFailureID = failureID
	current.LastFailure = safeExcerpt(reason)
	current.ResumeStage = StagePROpen
	clearRetryTransaction(&current)
	current.Stage = StageFailed
	if err := store.Put(current); err != nil {
		return Attempt{}, err
	}
	return current, nil
}

func RepairRefreshConflictFailureID(attempt Attempt, headSHA, baseSHA, reason string) (string, error) {
	if strings.TrimSpace(attempt.RepositoryFingerprint) == "" || attempt.Recurrence < 0 || attempt.Attempt <= 0 ||
		!validGitCommitSHA(headSHA) || !validGitCommitSHA(baseSHA) || strings.TrimSpace(reason) == "" {
		return "", fmt.Errorf("exact repair refresh conflict binding is required")
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%s\x00%s", attempt.RepositoryFingerprint, attempt.Recurrence, attempt.Attempt, strings.ToLower(headSHA), strings.ToLower(baseSHA), strings.TrimSpace(reason))))
	return hex.EncodeToString(digest[:16]), nil
}

// PrepareRefreshedRepairBranch fetches and verifies the exact current repair
// head and reviewed base, merges only that base in the Hive-owned worktree,
// preserves the exact semantic path set and stable patch contribution, reruns
// local validation, and creates an owned checkpoint. It never changes a remote
// ref. PushRefreshedRepairBranchExact is the sole remote mutation step.
func PrepareRefreshedRepairBranch(ctx context.Context, request RefreshedBranchCheckpointRequest) (RefreshedBranchCheckpoint, error) {
	result := RefreshedBranchCheckpoint{}
	if err := validateRefreshedBranchRequest(ctx, request); err != nil {
		return result, err
	}
	if err := requireCleanOwnedRefreshWorktree(ctx, request); err != nil {
		return result, err
	}

	headRef := "refs/hive/refresh/head/" + strings.ToLower(request.RemoteHeadSHA)
	baseRef := "refs/hive/refresh/base/" + strings.ToLower(request.BaseSHA)
	defer deleteRefreshRefs(request.Worktree, headRef, baseRef)
	if err := fetchExactRefreshRef(ctx, request.Worktree, request.ExpectedRemoteURL, "refs/heads/"+request.Branch, headRef, request.RemoteHeadSHA); err != nil {
		return result, fmt.Errorf("fetch exact repair head: %w", err)
	}
	if err := verifyRepairCommitOwnership(ctx, request.Worktree, request.RemoteHeadSHA, request.RepositoryID, "repair"); err != nil && !request.AllowLegacyHeadAdoption {
		return result, err
	}
	if err := fetchExactRefreshRef(ctx, request.Worktree, request.ExpectedRemoteURL, "refs/heads/"+request.BaseBranch, baseRef, request.BaseSHA); err != nil {
		return result, fmt.Errorf("fetch exact protected base: %w", err)
	}
	remoteHead, err := remoteRepairBranchHead(ctx, request.Worktree, request.ExpectedRemoteURL, request.Branch)
	if err != nil {
		return result, err
	}
	if !strings.EqualFold(remoteHead, request.RemoteHeadSHA) {
		return result, fmt.Errorf("remote repair branch moved from exact reviewed head %s to %s before local refresh", request.RemoteHeadSHA, remoteHead)
	}
	if err := validateRefreshedBranchPathsAtCommit(ctx, request, request.RemoteHeadSHA); err != nil {
		return result, err
	}

	if _, err := runGit(ctx, request.Worktree, "reset", "--hard", request.RemoteHeadSHA); err != nil {
		return result, fmt.Errorf("reset owned refresh worktree to exact remote head: %w", err)
	}
	contributionBase, err := runGit(ctx, request.Worktree, "merge-base", request.RemoteHeadSHA, request.BaseSHA)
	if err != nil {
		return result, fmt.Errorf("find exact repair contribution base: %w", err)
	}
	contributionBase = strings.TrimSpace(contributionBase)
	if !validGitCommitSHA(contributionBase) || strings.EqualFold(contributionBase, request.RemoteHeadSHA) {
		return result, fmt.Errorf("repair head is not behind the exact protected base")
	}
	beforeFiles, err := refreshChangedFiles(ctx, request.Worktree, contributionBase, request.RemoteHeadSHA)
	if err != nil {
		return result, err
	}
	if !equalStringSets(beforeFiles, request.ExpectedChangedFiles) {
		return result, fmt.Errorf("repair contribution paths changed before local refresh: got %s, expected %s", strings.Join(beforeFiles, ","), strings.Join(sortedRefreshPaths(request.ExpectedChangedFiles), ","))
	}
	beforePatchID, err := repairContributionPatchID(ctx, request.Worktree, contributionBase, request.RemoteHeadSHA)
	if err != nil {
		return result, err
	}
	if request.ExpectedContributionPatchID != "" && !strings.EqualFold(beforePatchID, request.ExpectedContributionPatchID) {
		return result, fmt.Errorf("repair contribution patch changed before local refresh")
	}

	message := fmt.Sprintf("chore: refresh Hive repair onto exact protected base\n\nHive-Repository-ID: %s\nHive-Operation: repair", strings.TrimSpace(request.RepositoryID))
	_, mergeErr := runGit(ctx, request.Worktree,
		"-c", "core.hooksPath="+os.DevNull,
		"-c", "commit.gpgsign=false",
		"-c", "merge.verifySignatures=false",
		"-c", "rerere.enabled=false",
		"-c", "user.name=Hive Repair Agent",
		"-c", "user.email=hive-repair@users.noreply.github.com",
		"merge", "--no-ff", "--no-edit", "--no-verify", "-m", message, request.BaseSHA)
	if mergeErr != nil {
		unmerged, _ := runGit(ctx, request.Worktree, "diff", "--name-only", "--diff-filter=U")
		cleanupErr := resetRefreshWorktree(ctx, request.Worktree, request.RemoteHeadSHA)
		if strings.TrimSpace(unmerged) != "" {
			if cleanupErr != nil {
				return result, fmt.Errorf("local exact-base merge conflicted and cleanup failed: %v; %w", mergeErr, cleanupErr)
			}
			return result, &RepairBranchRefreshConflictError{HeadSHA: request.RemoteHeadSHA, BaseSHA: request.BaseSHA, Cause: mergeErr}
		}
		if cleanupErr != nil {
			return result, fmt.Errorf("local exact-base merge failed: %v; cleanup failed: %w", mergeErr, cleanupErr)
		}
		return result, fmt.Errorf("merge exact protected base locally: %w", mergeErr)
	}

	checkpointSHA, err := runGit(ctx, request.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	checkpointSHA = strings.TrimSpace(checkpointSHA)
	if err := verifyRefreshParents(ctx, request.Worktree, checkpointSHA, request.RemoteHeadSHA, request.BaseSHA); err != nil {
		_ = resetRefreshWorktree(ctx, request.Worktree, request.RemoteHeadSHA)
		return result, err
	}
	if err := verifyRepairCommitOwnership(ctx, request.Worktree, checkpointSHA, request.RepositoryID, "repair"); err != nil {
		_ = resetRefreshWorktree(ctx, request.Worktree, request.RemoteHeadSHA)
		return result, err
	}
	afterFiles, err := refreshChangedFiles(ctx, request.Worktree, request.BaseSHA, checkpointSHA)
	if err != nil {
		return result, err
	}
	if !equalStringSets(afterFiles, request.ExpectedChangedFiles) {
		_ = resetRefreshWorktree(ctx, request.Worktree, request.RemoteHeadSHA)
		return result, fmt.Errorf("local exact-base merge changed the semantic repair path set")
	}
	afterPatchID, err := repairContributionPatchID(ctx, request.Worktree, request.BaseSHA, checkpointSHA)
	if err != nil {
		return result, err
	}
	if !strings.EqualFold(afterPatchID, beforePatchID) {
		_ = resetRefreshWorktree(ctx, request.Worktree, request.RemoteHeadSHA)
		return result, fmt.Errorf("local exact-base merge changed the repair contribution patch")
	}

	for _, command := range request.ValidationCommands {
		if err := runRepairCommand(ctx, request.Worktree, command, request.CommandTimeout, request.Environment, "refreshed repair validation"); err != nil {
			return result, err
		}
	}
	if _, err := runGit(ctx, request.Worktree, "diff", "--check", "HEAD"); err != nil {
		return result, fmt.Errorf("validate refreshed repair checkpoint: %w", err)
	}
	dirty, err := runGit(ctx, request.Worktree, "status", "--porcelain", "--untracked-files=no")
	if err != nil || strings.TrimSpace(dirty) != "" {
		return result, fmt.Errorf("local validation changed tracked files in the refreshed repair worktree")
	}
	return RefreshedBranchCheckpoint{
		HeadSHA: checkpointSHA, BaseSHA: strings.ToLower(request.BaseSHA), ContributionPatchID: beforePatchID,
		ChangedFiles: sortedRefreshPaths(afterFiles),
	}, nil
}

// VerifyRefreshedRepairBranch re-fetches the exact remote checkpoint and base
// after the PR re-read, then proves the same parents, ownership, semantic paths,
// and patch contribution that were validated before push.
func VerifyRefreshedRepairBranch(ctx context.Context, request RefreshedBranchCheckpointRequest, checkpoint RefreshedBranchCheckpoint) error {
	if err := validateRefreshedBranchRequest(ctx, request); err != nil {
		return err
	}
	if !validGitCommitSHA(checkpoint.HeadSHA) || !validGitCommitSHA(checkpoint.BaseSHA) || checkpoint.ContributionPatchID == "" ||
		!strings.EqualFold(checkpoint.BaseSHA, request.BaseSHA) || !equalStringSets(checkpoint.ChangedFiles, request.ExpectedChangedFiles) {
		return fmt.Errorf("refreshed repair checkpoint verification binding is incomplete")
	}
	headRef := "refs/hive/refresh/verify-head/" + strings.ToLower(checkpoint.HeadSHA)
	baseRef := "refs/hive/refresh/verify-base/" + strings.ToLower(request.BaseSHA)
	defer deleteRefreshRefs(request.Worktree, headRef, baseRef)
	if err := fetchExactRefreshRef(ctx, request.Worktree, request.ExpectedRemoteURL, "refs/heads/"+request.Branch, headRef, checkpoint.HeadSHA); err != nil {
		return fmt.Errorf("fetch pushed repair checkpoint: %w", err)
	}
	if err := fetchExactRefreshRef(ctx, request.Worktree, request.ExpectedRemoteURL, "refs/heads/"+request.BaseBranch, baseRef, request.BaseSHA); err != nil {
		return fmt.Errorf("fetch final protected base: %w", err)
	}
	if err := validateRefreshedBranchPathsAtCommit(ctx, request, checkpoint.HeadSHA); err != nil {
		return err
	}
	if err := verifyRefreshParents(ctx, request.Worktree, checkpoint.HeadSHA, request.RemoteHeadSHA, request.BaseSHA); err != nil {
		return err
	}
	if err := verifyRepairCommitOwnership(ctx, request.Worktree, checkpoint.HeadSHA, request.RepositoryID, "repair"); err != nil {
		return err
	}
	files, err := refreshChangedFiles(ctx, request.Worktree, request.BaseSHA, checkpoint.HeadSHA)
	if err != nil {
		return err
	}
	if !equalStringSets(files, request.ExpectedChangedFiles) {
		return fmt.Errorf("pushed repair checkpoint changed the exact semantic path set")
	}
	patchID, err := repairContributionPatchID(ctx, request.Worktree, request.BaseSHA, checkpoint.HeadSHA)
	if err != nil {
		return err
	}
	if !strings.EqualFold(patchID, checkpoint.ContributionPatchID) {
		return fmt.Errorf("pushed repair checkpoint changed the exact repair contribution patch")
	}
	return nil
}

// PushRefreshedRepairBranchExact performs the one refresh mutation: an exact
// force-with-lease against the old remote head. A moved head is never replaced.
func PushRefreshedRepairBranchExact(ctx context.Context, request RefreshedBranchCheckpointRequest, checkpoint RefreshedBranchCheckpoint) error {
	if err := validateRefreshedBranchRequest(ctx, request); err != nil {
		return err
	}
	if !validGitCommitSHA(checkpoint.HeadSHA) || !strings.EqualFold(checkpoint.BaseSHA, request.BaseSHA) || checkpoint.ContributionPatchID == "" {
		return fmt.Errorf("exact locally validated refresh checkpoint is required before push")
	}
	if err := validateRefreshedBranchPathsAtCommit(ctx, request, checkpoint.HeadSHA); err != nil {
		return err
	}
	if err := verifyRefreshParents(ctx, request.Worktree, checkpoint.HeadSHA, request.RemoteHeadSHA, request.BaseSHA); err != nil {
		return err
	}
	if err := verifyRepairCommitOwnership(ctx, request.Worktree, checkpoint.HeadSHA, request.RepositoryID, "repair"); err != nil {
		return err
	}
	remoteHead, err := remoteRepairBranchHead(ctx, request.Worktree, request.ExpectedRemoteURL, request.Branch)
	if err != nil {
		return err
	}
	if strings.EqualFold(remoteHead, checkpoint.HeadSHA) {
		return nil
	}
	if !strings.EqualFold(remoteHead, request.RemoteHeadSHA) {
		return fmt.Errorf("remote repair branch changed before exact refresh push: got %s, expected %s", remoteHead, request.RemoteHeadSHA)
	}
	lease := repairForceLease(request.Branch, request.RemoteHeadSHA)
	if _, err := runGitTransport(ctx, request.Worktree, request.ExpectedRemoteURL, "push", lease, request.ExpectedRemoteURL, checkpoint.HeadSHA+":refs/heads/"+request.Branch); err != nil {
		return fmt.Errorf("push exact locally validated repair refresh: %w", err)
	}
	remoteHead, err = remoteRepairBranchHead(ctx, request.Worktree, request.ExpectedRemoteURL, request.Branch)
	if err != nil || !strings.EqualFold(remoteHead, checkpoint.HeadSHA) {
		return fmt.Errorf("remote repair branch does not match pushed refresh checkpoint %s", checkpoint.HeadSHA)
	}
	return nil
}

func ResetRefreshedRepairBranch(ctx context.Context, worktree, headSHA string) error {
	if strings.TrimSpace(worktree) == "" || !validGitCommitSHA(headSHA) {
		return fmt.Errorf("exact refresh worktree and head are required")
	}
	return resetRefreshWorktree(ctx, worktree, headSHA)
}

func validateRefreshedBranchRequest(ctx context.Context, request RefreshedBranchCheckpointRequest) error {
	if strings.TrimSpace(request.Worktree) == "" || !validHiveRepairBranch(request.Branch) || !positiveRepositoryID(strings.TrimSpace(request.RepositoryID)) ||
		!validGitCommitSHA(request.RemoteHeadSHA) || !validGitCommitSHA(request.BaseSHA) || strings.TrimSpace(request.BaseBranch) == "" ||
		len(request.ExpectedChangedFiles) == 0 {
		return fmt.Errorf("exact worktree, Hive branch, positive repository identity, remote head, protected base, and changed files are required")
	}
	if request.ExpectedContributionPatchID != "" && !validGitCommitSHA(request.ExpectedContributionPatchID) {
		return fmt.Errorf("expected repair contribution patch ID must be an exact 40-character SHA-1")
	}
	if _, err := runGit(ctx, request.Worktree, "check-ref-format", "--branch", request.BaseBranch); err != nil {
		return fmt.Errorf("invalid protected base branch %q", request.BaseBranch)
	}
	if _, err := validateExpectedRemoteURL(ctx, request.Worktree, request.ExpectedRemoteURL); err != nil {
		return err
	}
	protection, err := normalizeBaselineProtection(request.BaselineProtection)
	if err != nil {
		return fmt.Errorf("validate refreshed repair baseline protection: %w", err)
	}
	if err := validateChangedFilesWithBaselineProtection(request.ExpectedChangedFiles, []string{"**"}, protection); err != nil {
		return fmt.Errorf("validate refreshed repair paths: %w", err)
	}
	return nil
}

func validateRefreshedBranchPathsAtCommit(ctx context.Context, request RefreshedBranchCheckpointRequest, commitSHA string) error {
	protection, err := baselineProtectionForCommit(ctx, request.Worktree, commitSHA, request.BaselineProtection)
	if err != nil {
		return fmt.Errorf("validate exact refreshed repair baseline protection: %w", err)
	}
	if err := validateChangedFilesWithBaselineProtection(request.ExpectedChangedFiles, []string{"**"}, protection); err != nil {
		return fmt.Errorf("validate exact refreshed repair paths: %w", err)
	}
	return nil
}

func requireCleanOwnedRefreshWorktree(ctx context.Context, request RefreshedBranchCheckpointRequest) error {
	branch, err := runGit(ctx, request.Worktree, "branch", "--show-current")
	if err != nil || strings.TrimSpace(branch) != request.Branch {
		return fmt.Errorf("repair worktree is not on the exact owned branch %s", request.Branch)
	}
	dirty, err := runGit(ctx, request.Worktree, "status", "--porcelain", "--untracked-files=no")
	if err != nil || strings.TrimSpace(dirty) != "" {
		return fmt.Errorf("repair worktree must be clean before exact base refresh")
	}
	return nil
}

func fetchExactRefreshRef(ctx context.Context, worktree, expectedRemoteURL, sourceRef, destinationRef, expectedSHA string) error {
	if _, err := runGitTransport(ctx, worktree, expectedRemoteURL, "fetch", "--no-tags", "--force", expectedRemoteURL, sourceRef+":"+destinationRef); err != nil {
		return err
	}
	fetched, err := runGit(ctx, worktree, "rev-parse", "--verify", destinationRef)
	if err != nil || !strings.EqualFold(strings.TrimSpace(fetched), expectedSHA) {
		return &RepairBranchRefreshRefMovedError{Ref: sourceRef, Expected: expectedSHA, Observed: strings.TrimSpace(fetched)}
	}
	return nil
}

func deleteRefreshRefs(worktree string, refs ...string) {
	for _, ref := range refs {
		_, _ = runGit(context.Background(), worktree, "update-ref", "-d", ref)
	}
}

func resetRefreshWorktree(ctx context.Context, worktree, headSHA string) error {
	_, _ = runGit(ctx, worktree, "merge", "--abort")
	if _, err := runGit(ctx, worktree, "reset", "--hard", headSHA); err != nil {
		return err
	}
	dirty, err := runGit(ctx, worktree, "status", "--porcelain", "--untracked-files=no")
	if err != nil || strings.TrimSpace(dirty) != "" {
		return fmt.Errorf("refresh worktree remains dirty after reset")
	}
	return nil
}

func verifyRefreshParents(ctx context.Context, worktree, checkpointSHA, oldHeadSHA, baseSHA string) error {
	parents, err := runGit(ctx, worktree, "rev-list", "--parents", "-n", "1", checkpointSHA)
	if err != nil {
		return err
	}
	fields := strings.Fields(parents)
	if len(fields) != 3 || !strings.EqualFold(fields[0], checkpointSHA) || !strings.EqualFold(fields[1], oldHeadSHA) || !strings.EqualFold(fields[2], baseSHA) {
		return fmt.Errorf("refreshed repair checkpoint is not the exact two-parent merge of head %s and base %s", oldHeadSHA, baseSHA)
	}
	if _, err := runGit(ctx, worktree, "merge-base", "--is-ancestor", oldHeadSHA, checkpointSHA); err != nil {
		return fmt.Errorf("old repair head is not an ancestor of refreshed checkpoint: %w", err)
	}
	return nil
}

func refreshChangedFiles(ctx context.Context, worktree, fromSHA, toSHA string) ([]string, error) {
	data, err := runGitBytes(ctx, worktree, maxRepairRefreshDiffBytes, "diff", "--name-only", "-z", "--no-renames", fromSHA, toSHA, "--")
	if err != nil {
		return nil, fmt.Errorf("inspect exact repair contribution paths: %w", err)
	}
	paths := make([]string, 0)
	for _, value := range bytes.Split(data, []byte{0}) {
		if len(value) > 0 {
			paths = append(paths, strings.ReplaceAll(string(value), "\\", "/"))
		}
	}
	return sortedRefreshPaths(paths), nil
}

func repairContributionPatchID(ctx context.Context, worktree, fromSHA, toSHA string) (string, error) {
	diff, err := runGitBytes(ctx, worktree, maxRepairRefreshDiffBytes, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-renames", fromSHA, toSHA, "--")
	if err != nil {
		return "", fmt.Errorf("read exact repair contribution: %w", err)
	}
	if len(diff) == 0 {
		return "", fmt.Errorf("repair contribution is empty")
	}
	command := exec.CommandContext(ctx, "git", "patch-id", "--stable")
	command.Dir = worktree
	command.Env = gittransport.LocalEnvironment(providerEnvironment())
	command.Stdin = bytes.NewReader(diff)
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("compute stable repair contribution patch ID: %w: %s", err, safeExcerpt(output.String()))
	}
	fields := strings.Fields(output.String())
	if len(fields) < 1 || !validGitCommitSHA(fields[0]) {
		return "", fmt.Errorf("git patch-id returned an invalid repair contribution identity")
	}
	return strings.ToLower(fields[0]), nil
}

func runGitBytes(ctx context.Context, dir string, limit int64, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = gittransport.LocalEnvironment(providerEnvironment())
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	capture := &boundedRefreshOutput{limit: limit}
	_, readErr := io.Copy(capture, stdout)
	waitErr := command.Wait()
	if readErr != nil {
		return nil, readErr
	}
	if waitErr != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), waitErr, safeExcerpt(stderr.String()))
	}
	if capture.total > limit {
		return nil, fmt.Errorf("git %s output exceeds %d bytes", strings.Join(args, " "), limit)
	}
	return capture.data.Bytes(), nil
}

type boundedRefreshOutput struct {
	data  bytes.Buffer
	total int64
	limit int64
}

func (b *boundedRefreshOutput) Write(value []byte) (int, error) {
	original := len(value)
	b.total += int64(original)
	remaining := b.limit - int64(b.data.Len())
	if remaining > 0 {
		_, _ = b.data.Write(value[:min(int64(len(value)), remaining)])
	}
	return original, nil
}

func sortedRefreshPaths(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"), "./")
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
