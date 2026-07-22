package repair

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	sealedTreeModelBase = "model_base"
	sealedTreeCandidate = "candidate"
)

var sealedTreeRefPattern = regexp.MustCompile(`^refs/hive/repair-sealed-trees/(?:model_base|candidate)/[a-f0-9]{24}/[a-f0-9]{32}$`)

func validSealedTreeGuardState(attempt Attempt) bool {
	return sealedTreeGuardStateError(attempt) == nil
}

func sealedTreeGuardStateError(attempt Attempt) error {
	baseAny := attempt.ModelBaseTree != "" || attempt.ModelBaseParent != "" || attempt.ModelBaseGuardRef != "" ||
		attempt.ModelBaseGuardCommit != "" || attempt.ModelBaseGuardBinding != "" || attempt.ModelBaseGuardKind != ""
	baseRefValid := len(attempt.ModelBaseGuardBinding) >= 24 && (attempt.ModelBaseGuardKind == sealedTreeModelBase && sealedTreeRefPattern.MatchString(attempt.ModelBaseGuardRef) && strings.Contains(attempt.ModelBaseGuardRef, "/model_base/"+attempt.ModelBaseGuardBinding[:24]+"/") ||
		attempt.ModelBaseGuardKind == toolSnapshotPreparation && toolSnapshotRefPattern.MatchString(attempt.ModelBaseGuardRef) && strings.Contains(attempt.ModelBaseGuardRef, "/"+attempt.ModelBaseGuardBinding[:24]+"/"))
	if baseAny && (!validGitCommitSHA(attempt.ModelBaseTree) || !validGitCommitSHA(attempt.ModelBaseParent) || !baseRefValid || !validGitCommitSHA(attempt.ModelBaseGuardCommit) ||
		!recoveryDigestPattern.MatchString(attempt.ModelBaseGuardBinding)) {
		return fmt.Errorf("model-base guard fields are incomplete: tree=%t parent=%t ref=%t commit=%t binding=%t ref_binding=%t",
			validGitCommitSHA(attempt.ModelBaseTree), validGitCommitSHA(attempt.ModelBaseParent), baseRefValid,
			validGitCommitSHA(attempt.ModelBaseGuardCommit), recoveryDigestPattern.MatchString(attempt.ModelBaseGuardBinding),
			len(attempt.ModelBaseGuardBinding) >= 24 && strings.Contains(attempt.ModelBaseGuardRef, "/model_base/"+attempt.ModelBaseGuardBinding[:24]+"/"))
	}
	if baseAny && expectedModelBaseGuardBinding(attempt) != attempt.ModelBaseGuardBinding {
		return fmt.Errorf("model-base guard binding does not match its repository, attempt, tree, parent, and branch")
	}
	candidateAny := attempt.CandidateTree != "" || attempt.CandidateParent != "" || attempt.CandidateGuardRef != "" ||
		attempt.CandidateGuardCommit != "" || attempt.CandidateGuardBinding != "" || attempt.CandidateGuardKind != ""
	if !candidateAny {
		return nil
	}
	if !validGitCommitSHA(attempt.CandidateTree) || !validGitCommitSHA(attempt.CandidateParent) {
		return fmt.Errorf("candidate tree or parent is invalid")
	}
	guardAny := attempt.CandidateGuardRef != "" || attempt.CandidateGuardCommit != "" || attempt.CandidateGuardBinding != "" || attempt.CandidateGuardKind != ""
	// After the exact commit has been created, retain its sealed parent/tree as
	// durable remote-recovery evidence while retiring the temporary guard ref.
	// The commit object itself now anchors that identity.
	if !guardAny && (attempt.Stage == StageCommitted || attempt.Stage == StagePushed || attempt.Stage == StagePROpen ||
		attempt.Stage == StageNoChange && attempt.PRNumber > 0) && validGitCommitSHA(attempt.CommitSHA) {
		return nil
	}
	// While validation is running, its durable tool snapshot ref is already the
	// candidate guard; restore atomically transfers those exact fields below.
	if attempt.CandidateGuardRef == "" && hasToolSnapshot(attempt) && attempt.ToolSnapshotPhase == toolSnapshotValidation {
		return nil
	}
	if !validGitCommitSHA(attempt.CandidateGuardCommit) || !recoveryDigestPattern.MatchString(attempt.CandidateGuardBinding) {
		return fmt.Errorf("candidate guard commit or binding is invalid")
	}
	switch attempt.CandidateGuardKind {
	case toolSnapshotValidation:
		if !toolSnapshotRefPattern.MatchString(attempt.CandidateGuardRef) || !strings.Contains(attempt.CandidateGuardRef, "/"+attempt.CandidateGuardBinding[:24]+"/") {
			return fmt.Errorf("validation candidate guard ref is invalid")
		}
	case sealedTreeCandidate:
		if !sealedTreeRefPattern.MatchString(attempt.CandidateGuardRef) || !strings.Contains(attempt.CandidateGuardRef, "/candidate/"+attempt.CandidateGuardBinding[:24]+"/") {
			return fmt.Errorf("candidate guard ref is invalid")
		}
	default:
		return fmt.Errorf("candidate guard kind is invalid")
	}
	if expectedCandidateGuardBinding(attempt) != attempt.CandidateGuardBinding {
		return fmt.Errorf("candidate guard binding does not match its repository, attempt, tree, parent, and branch")
	}
	return nil
}

func expectedModelBaseGuardBinding(attempt Attempt) string {
	switch attempt.ModelBaseGuardKind {
	case sealedTreeModelBase:
		return sealedTreeBinding(attempt, sealedTreeModelBase, attempt.ModelBaseTree, attempt.ModelBaseParent)
	case toolSnapshotPreparation:
		return toolSnapshotBinding(attempt, toolSnapshotPreparation, attempt.ModelBaseParent, attempt.Branch)
	default:
		return ""
	}
}

func expectedCandidateGuardBinding(attempt Attempt) string {
	switch attempt.CandidateGuardKind {
	case sealedTreeCandidate:
		return sealedTreeBinding(attempt, sealedTreeCandidate, attempt.CandidateTree, attempt.CandidateParent)
	case toolSnapshotValidation:
		return toolSnapshotBinding(attempt, toolSnapshotValidation, attempt.CandidateParent, attempt.Branch)
	default:
		return ""
	}
}

// sealModelBaseTree binds a read-only provider invocation to the exact
// cumulative tree it was shown. Preparation daemons that write later cannot be
// mistaken for bytes in the provider's bounded patch.
func (w *Worker) sealModelBaseTree(ctx context.Context, attempt *Attempt) error {
	if attempt.ModelBaseGuardRef != "" {
		if expectedModelBaseGuardBinding(*attempt) != attempt.ModelBaseGuardBinding {
			return fmt.Errorf("model-base guard binding changed")
		}
		if err := verifySealedTreeGuard(ctx, attempt.Worktree, attempt.ModelBaseGuardKind, attempt.ModelBaseGuardRef, attempt.ModelBaseGuardCommit,
			attempt.ModelBaseGuardBinding, attempt.ModelBaseTree, attempt.ModelBaseParent); err != nil {
			return err
		}
		return verifyModelBaseWorktree(ctx, *attempt)
	}
	head, err := runGit(ctx, attempt.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read repair parent before model invocation: %w", err)
	}
	head = strings.TrimSpace(head)
	if !validGitCommitSHA(head) {
		return fmt.Errorf("repair parent before model invocation is invalid")
	}
	tree, err := captureWorktreeTree(ctx, attempt.Worktree)
	if err != nil {
		return fmt.Errorf("seal cumulative repair tree before model invocation: %w", err)
	}
	return w.sealSpecificModelBaseTree(ctx, attempt, strings.TrimSpace(tree), head)
}

func (w *Worker) sealSpecificModelBaseTree(ctx context.Context, attempt *Attempt, tree, parent string) error {
	if !validGitCommitSHA(tree) || !validGitCommitSHA(parent) {
		return patchEngineInfrastructureFailure(fmt.Errorf("specific model-base tree or parent is invalid"))
	}
	ref, commit, binding, err := w.createSealedTreeGuard(ctx, *attempt, sealedTreeModelBase, tree, parent)
	if err != nil {
		return err
	}
	attempt.ModelBaseTree = tree
	attempt.ModelBaseParent = parent
	attempt.CandidateTree = ""
	attempt.CandidateParent = ""
	attempt.ModelBaseGuardRef, attempt.ModelBaseGuardCommit, attempt.ModelBaseGuardBinding = ref, commit, binding
	attempt.ModelBaseGuardKind = sealedTreeModelBase
	return nil
}

func verifyModelBaseWorktree(ctx context.Context, attempt Attempt) error {
	head, err := runGit(ctx, attempt.Worktree, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != attempt.ModelBaseParent {
		return fmt.Errorf("repair parent changed after the model-base tree was sealed")
	}
	currentTree, err := captureWorktreeTree(ctx, attempt.Worktree)
	if err != nil || strings.TrimSpace(currentTree) != attempt.ModelBaseTree {
		return patchEngineInfrastructureFailure(fmt.Errorf("repair worktree changed after the model-base tree was sealed; possible delayed preparation process contamination"))
	}
	return nil
}

func (w *Worker) createSealedTreeGuard(ctx context.Context, attempt Attempt, kind, tree, parent string) (string, string, string, error) {
	if kind != sealedTreeModelBase && kind != sealedTreeCandidate || !validGitCommitSHA(tree) || !validGitCommitSHA(parent) {
		return "", "", "", fmt.Errorf("sealed repair tree guard inputs are invalid")
	}
	binding := sealedTreeBinding(attempt, kind, tree, parent)
	subject := "Hive repair sealed tree " + kind + " " + binding
	commit, err := runGit(ctx, attempt.Worktree,
		"-c", "user.name=Hive Repair Agent", "-c", "user.email=hive-repair@users.noreply.github.com",
		"commit-tree", tree, "-p", parent, "-m", subject)
	if err != nil {
		return "", "", "", fmt.Errorf("anchor sealed repair %s tree: %w", kind, err)
	}
	commit = strings.TrimSpace(commit)
	nonce := make([]byte, 16)
	if !validGitCommitSHA(commit) {
		return "", "", "", fmt.Errorf("sealed repair %s guard commit is invalid", kind)
	}
	if _, err := rand.Read(nonce); err != nil {
		return "", "", "", fmt.Errorf("create sealed repair %s nonce: %w", kind, err)
	}
	ref := fmt.Sprintf("refs/hive/repair-sealed-trees/%s/%s/%s", kind, binding[:24], hex.EncodeToString(nonce))
	if err := w.State.recordRepairRefIntent(ctx, attempt.Worktree, attempt.Repository, kind, ref, commit, binding); err != nil {
		return "", "", "", fmt.Errorf("journal sealed repair %s guard: %w", kind, err)
	}
	if _, err := runGit(ctx, attempt.Worktree, "update-ref", "--no-deref", ref, commit, strings.Repeat("0", 40)); err != nil {
		return "", "", "", fmt.Errorf("protect sealed repair %s tree: %w", kind, err)
	}
	return ref, commit, binding, nil
}

func sealedTreeBinding(attempt Attempt, kind, tree, parent string) string {
	value := strings.Join([]string{attempt.Repository, attempt.RepositoryFingerprint, strconv.Itoa(attempt.Recurrence), strconv.Itoa(attempt.Attempt), kind, tree, parent, attempt.Branch}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func verifySealedTreeGuard(ctx context.Context, worktree, kind, ref, commit, binding, tree, parent string) error {
	if !validGitCommitSHA(tree) || !validGitCommitSHA(parent) || !validGitCommitSHA(commit) || !recoveryDigestPattern.MatchString(binding) {
		return fmt.Errorf("sealed repair %s guard checkpoint is incomplete", kind)
	}
	anchored, err := readRepairRef(ctx, worktree, ref)
	if err != nil || anchored != commit {
		return fmt.Errorf("sealed repair %s guard ref is missing or changed", kind)
	}
	anchoredTree, err := runGit(ctx, worktree, "rev-parse", commit+"^{tree}")
	if err != nil || strings.TrimSpace(anchoredTree) != tree {
		return fmt.Errorf("sealed repair %s guard tree changed", kind)
	}
	anchoredParent, err := runGit(ctx, worktree, "rev-parse", commit+"^")
	if err != nil || strings.TrimSpace(anchoredParent) != parent {
		return fmt.Errorf("sealed repair %s guard parent changed", kind)
	}
	expectedSubject := "Hive repair sealed tree " + kind + " " + binding
	if kind == toolSnapshotValidation || kind == toolSnapshotPreparation {
		expectedSubject = "Hive repair tool snapshot " + binding
	}
	subject, err := runGit(ctx, worktree, "show", "-s", "--format=%s", commit)
	if err != nil || strings.TrimSpace(subject) != expectedSubject {
		return fmt.Errorf("sealed repair %s guard ownership changed", kind)
	}
	return nil
}

// expectedModelPatchTree applies only to an alternate index initialized from
// the immutable pre-model tree. It never reads candidate worktree bytes and
// never writes the real index or worktree.
func expectedModelPatchTree(ctx context.Context, worktree, baseTree, patchText string) (string, error) {
	if !validGitCommitSHA(baseTree) {
		return "", patchEngineInfrastructureFailure(fmt.Errorf("read-only model patch has no exact pre-model tree"))
	}
	indexDirectory, err := os.MkdirTemp("", "hive-repair-expected-index-")
	if err != nil {
		return "", patchEngineInfrastructureFailure(fmt.Errorf("create exact model patch index: %w", err))
	}
	defer func() { _ = os.RemoveAll(indexDirectory) }()
	indexFile := filepath.Join(indexDirectory, "index")
	if _, err := runGitWithIndex(ctx, worktree, indexFile, "read-tree", baseTree); err != nil {
		return "", patchEngineInfrastructureFailure(fmt.Errorf("initialize exact model patch index: %w", err))
	}
	applicable, detail, err := checkCachedModelPatch(ctx, worktree, indexFile, patchText, false)
	if err != nil {
		return "", err
	}
	if applicable {
		if err := applyCachedModelPatch(ctx, worktree, indexFile, patchText); err != nil {
			return "", err
		}
	} else {
		hunks, splitErr := splitModelPatchHunks(patchText)
		if splitErr != nil {
			return "", fmt.Errorf("exact model patch does not apply to its sealed base: %s: %w", safeExcerpt(detail), splitErr)
		}
		pending := 0
		for index, hunk := range hunks {
			alreadyApplied, _, checkErr := checkCachedModelPatch(ctx, worktree, indexFile, hunk, true)
			if checkErr != nil {
				return "", checkErr
			}
			if alreadyApplied {
				continue
			}
			canApply, hunkDetail, checkErr := checkCachedModelPatch(ctx, worktree, indexFile, hunk, false)
			if checkErr != nil {
				return "", checkErr
			}
			if !canApply {
				return "", fmt.Errorf("exact model patch hunk %d is neither applicable nor already present: %s", index+1, safeExcerpt(hunkDetail))
			}
			if err := applyCachedModelPatch(ctx, worktree, indexFile, hunk); err != nil {
				return "", err
			}
			pending++
		}
		if pending == 0 {
			return "", fmt.Errorf("exact model patch makes no change to its sealed base")
		}
	}
	tree, err := runGitWithIndex(ctx, worktree, indexFile, "write-tree")
	if err != nil {
		return "", patchEngineInfrastructureFailure(fmt.Errorf("write exact model patch tree: %w", err))
	}
	tree = strings.TrimSpace(tree)
	if !validGitCommitSHA(tree) {
		return "", patchEngineInfrastructureFailure(fmt.Errorf("exact model patch tree is invalid"))
	}
	return tree, nil
}

func deriveModelBaseTreeFromAppliedPatch(ctx context.Context, worktree, candidateTree, patchText string) (string, error) {
	if !validGitCommitSHA(candidateTree) {
		return "", patchEngineInfrastructureFailure(fmt.Errorf("legacy applied patch candidate tree is invalid"))
	}
	indexDirectory, err := os.MkdirTemp("", "hive-repair-reverse-index-")
	if err != nil {
		return "", patchEngineInfrastructureFailure(fmt.Errorf("create legacy reverse patch index: %w", err))
	}
	defer func() { _ = os.RemoveAll(indexDirectory) }()
	indexFile := filepath.Join(indexDirectory, "index")
	if _, err := runGitWithIndex(ctx, worktree, indexFile, "read-tree", candidateTree); err != nil {
		return "", patchEngineInfrastructureFailure(fmt.Errorf("initialize legacy reverse patch index: %w", err))
	}
	if err := applyCachedModelPatchDirection(ctx, worktree, indexFile, patchText, true); err != nil {
		return "", fmt.Errorf("cannot reconstruct exact pre-patch tree from legacy applied checkpoint: %w", err)
	}
	baseTree, err := runGitWithIndex(ctx, worktree, indexFile, "write-tree")
	if err != nil {
		return "", patchEngineInfrastructureFailure(fmt.Errorf("write reconstructed legacy model-base tree: %w", err))
	}
	baseTree = strings.TrimSpace(baseTree)
	if !validGitCommitSHA(baseTree) {
		return "", patchEngineInfrastructureFailure(fmt.Errorf("reconstructed legacy model-base tree is invalid"))
	}
	forward, err := expectedModelPatchTree(ctx, worktree, baseTree, patchText)
	if err != nil || forward != candidateTree {
		return "", patchEngineInfrastructureFailure(fmt.Errorf("legacy reverse/forward patch proof did not reproduce the exact candidate tree: %v", err))
	}
	return baseTree, nil
}

func checkCachedModelPatch(ctx context.Context, worktree, indexFile, patchText string, reverse bool) (bool, string, error) {
	args := []string{"apply", "--cached"}
	if reverse {
		args = append(args, "--reverse")
	}
	args = append(args, "--check", "--whitespace=error-all", "--recount", "-")
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = worktree
	command.Env = patchGitEnvironment(indexFile)
	command.Stdin = strings.NewReader(patchText)
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		failure := fmt.Errorf("git %s exact model patch check failed: %w: %s", strings.Join(args[:len(args)-1], " "), err, safeExcerpt(output.String()))
		classified := classifyPatchCommandFailure(ctx, err, output.String(), failure)
		if isPatchEngineInfrastructureFailure(classified) {
			return false, output.String(), classified
		}
		return false, output.String(), nil
	}
	return true, output.String(), nil
}

func applyCachedModelPatch(ctx context.Context, worktree, indexFile, patchText string) error {
	return applyCachedModelPatchDirection(ctx, worktree, indexFile, patchText, false)
}

func applyCachedModelPatchDirection(ctx context.Context, worktree, indexFile, patchText string, reverse bool) error {
	args := []string{"apply", "--cached"}
	if reverse {
		args = append(args, "--reverse")
	}
	args = append(args, "--whitespace=error-all", "--recount", "-")
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = worktree
	command.Env = patchGitEnvironment(indexFile)
	command.Stdin = strings.NewReader(patchText)
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		failure := fmt.Errorf("git %s exact model patch failed: %w: %s", strings.Join(args[:len(args)-1], " "), err, safeExcerpt(output.String()))
		return classifyPatchCommandFailure(ctx, err, output.String(), failure)
	}
	return nil
}

func sealCandidateFromWorktree(ctx context.Context, attempt *Attempt) error {
	head, err := runGit(ctx, attempt.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	tree, err := captureWorktreeTree(ctx, attempt.Worktree)
	if err != nil {
		return err
	}
	head, tree = strings.TrimSpace(head), strings.TrimSpace(tree)
	if !validGitCommitSHA(head) || !validGitCommitSHA(tree) {
		return fmt.Errorf("repair candidate has an invalid parent or tree")
	}
	attempt.CandidateParent, attempt.CandidateTree = head, tree
	return nil
}

func (w *Worker) guardCandidateTree(ctx context.Context, attempt *Attempt) error {
	if attempt.CandidateGuardRef != "" {
		return nil
	}
	ref, commit, binding, err := w.createSealedTreeGuard(ctx, *attempt, sealedTreeCandidate, attempt.CandidateTree, attempt.CandidateParent)
	if err != nil {
		return err
	}
	attempt.CandidateGuardRef, attempt.CandidateGuardCommit, attempt.CandidateGuardBinding = ref, commit, binding
	attempt.CandidateGuardKind = sealedTreeCandidate
	return nil
}

func validateSealedCandidate(ctx context.Context, worktree string, attempt Attempt) error {
	if !validGitCommitSHA(attempt.CandidateParent) || !validGitCommitSHA(attempt.CandidateTree) || len(attempt.ChangedFiles) == 0 {
		return fmt.Errorf("validated repair checkpoint has no exact sealed candidate")
	}
	if expectedCandidateGuardBinding(attempt) != attempt.CandidateGuardBinding {
		return fmt.Errorf("sealed repair candidate guard binding changed")
	}
	changed, err := runGitExact(ctx, worktree, "diff-tree", "--no-commit-id", "--no-renames", "--name-only", "-r", "-z", attempt.CandidateParent, attempt.CandidateTree)
	if err != nil {
		return fmt.Errorf("inspect exact sealed repair candidate: %w", err)
	}
	if !equalStringSets(splitGitPaths(changed), attempt.ChangedFiles) {
		return fmt.Errorf("sealed repair candidate paths do not match the authorized changed-file set")
	}
	if err := verifySealedTreeGuard(ctx, worktree, attempt.CandidateGuardKind, attempt.CandidateGuardRef, attempt.CandidateGuardCommit,
		attempt.CandidateGuardBinding, attempt.CandidateTree, attempt.CandidateParent); err != nil {
		return err
	}
	return nil
}

func commitSealedRepair(ctx context.Context, worktree, branch string, attempt Attempt, title string, issue int, repositoryID string) (string, error) {
	if !validHiveRepairBranch(branch) || branch != attempt.Branch {
		return "", fmt.Errorf("commit sealed repair requires the exact Hive branch")
	}
	if err := validateSealedCandidate(ctx, worktree, attempt); err != nil {
		return "", err
	}
	if err := rejectExecutableRepositoryGitConfig(ctx, worktree); err != nil {
		return "", fmt.Errorf("refuse sealed commit with unsafe repository Git configuration: %w", err)
	}
	currentBranch, err := runGit(ctx, worktree, "branch", "--show-current")
	if err != nil || strings.TrimSpace(currentBranch) != branch {
		return "", fmt.Errorf("repair branch changed before sealed commit")
	}
	currentHead, err := runGit(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	currentHead = strings.TrimSpace(currentHead)
	message := repairCommitMessage(title, issue, repositoryID)
	if currentHead != attempt.CandidateParent {
		if err := verifySealedRepairCommit(ctx, worktree, currentHead, attempt, message); err != nil {
			return "", fmt.Errorf("repair head moved before sealed commit: %w", err)
		}
		return currentHead, nil
	}
	commit, err := runGit(ctx, worktree,
		"-c", "user.name=Hive Repair Agent", "-c", "user.email=hive-repair@users.noreply.github.com",
		"commit-tree", attempt.CandidateTree, "-p", attempt.CandidateParent, "-m", message)
	if err != nil {
		return "", fmt.Errorf("create sealed repair commit: %w", err)
	}
	commit = strings.TrimSpace(commit)
	if !validGitCommitSHA(commit) {
		return "", fmt.Errorf("sealed repair commit has invalid SHA")
	}
	if _, err := runGit(ctx, worktree, "update-ref", "--no-deref", "refs/heads/"+branch, commit, attempt.CandidateParent); err != nil {
		return "", fmt.Errorf("advance exact repair branch to sealed commit: %w", err)
	}
	// Synchronize the disposable repair worktree for operator inspection. This
	// reset is not an authority boundary: the commit object and later push are
	// already bound to CandidateTree, so a surviving tool writer cannot alter it.
	if _, err := runGit(ctx, worktree, "reset", "--hard", commit); err != nil {
		return "", fmt.Errorf("synchronize worktree to sealed repair commit: %w", err)
	}
	if err := verifySealedRepairCommit(ctx, worktree, commit, attempt, message); err != nil {
		return "", err
	}
	return commit, nil
}

func verifySealedRepairCommit(ctx context.Context, worktree, commit string, attempt Attempt, expectedMessage string) error {
	if !validGitCommitSHA(commit) {
		return fmt.Errorf("candidate repair commit is invalid")
	}
	parent, err := runGit(ctx, worktree, "rev-parse", commit+"^")
	if err != nil || strings.TrimSpace(parent) != attempt.CandidateParent {
		return fmt.Errorf("candidate repair commit has an unexpected parent")
	}
	tree, err := runGit(ctx, worktree, "rev-parse", commit+"^{tree}")
	if err != nil || strings.TrimSpace(tree) != attempt.CandidateTree {
		return fmt.Errorf("candidate repair commit does not contain the sealed tree")
	}
	message, err := runGit(ctx, worktree, "show", "-s", "--format=%B", commit)
	if err != nil || strings.TrimSpace(message) != strings.TrimSpace(expectedMessage) {
		return fmt.Errorf("candidate repair commit has unexpected ownership metadata")
	}
	return validateSealedCandidate(ctx, worktree, attempt)
}

func (w *Worker) releaseSealedTreeGuards(ctx context.Context, attempt *Attempt) error {
	type guard struct{ kind, ref, commit, binding string }
	guards := []guard{
		{attempt.ModelBaseGuardKind, attempt.ModelBaseGuardRef, attempt.ModelBaseGuardCommit, attempt.ModelBaseGuardBinding},
		{attempt.CandidateGuardKind, attempt.CandidateGuardRef, attempt.CandidateGuardCommit, attempt.CandidateGuardBinding},
	}
	// The model-base identity and all temporary guard refs can be retired after
	// commit. CandidateParent/CandidateTree must survive through pushed/PR-open
	// recovery so Hive can rebind the remote commit exactly after a crash.
	attempt.ModelBaseTree = ""
	attempt.ModelBaseParent = ""
	attempt.ModelBaseGuardRef = ""
	attempt.ModelBaseGuardCommit = ""
	attempt.ModelBaseGuardBinding = ""
	attempt.ModelBaseGuardKind = ""
	attempt.CandidateGuardRef = ""
	attempt.CandidateGuardCommit = ""
	attempt.CandidateGuardBinding = ""
	attempt.CandidateGuardKind = ""
	if err := w.State.Put(*attempt); err != nil {
		return fmt.Errorf("release durable sealed-tree references: %w", err)
	}
	for _, guard := range guards {
		if guard.ref == "" {
			continue
		}
		if err := retireRepairRef(ctx, attempt.Worktree, attempt.Repository, w.State, guard.kind, guard.ref, guard.commit, guard.binding); err != nil {
			return fmt.Errorf("retire sealed-tree guard %s: %w", guard.ref, err)
		}
	}
	return nil
}
