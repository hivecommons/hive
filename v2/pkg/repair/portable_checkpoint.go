package repair

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Keep the Git object closure below the hosted state's 32 MiB decoded total so
// repair metadata and lifecycle journals always retain bounded headroom.
const portableRepairBundleMaxBytes int64 = 24 << 20

var portableRepairBundlePathPattern = regexp.MustCompile(`^portable-git/[a-f0-9]{24}-r[0-9]+-a[1-9][0-9]*-(?:validated|committed)-[a-f0-9]{24}\.bundle$`)

type portableRepairBundleMetadata struct {
	Path    string
	SHA256  string
	Bytes   int64
	Stage   Stage
	Binding string
}

func portableRepairBundleStateError(attempt Attempt) error {
	any := attempt.PortableBundlePath != "" || attempt.PortableBundleSHA256 != "" || attempt.PortableBundleBytes != 0 ||
		attempt.PortableBundleStage != "" || attempt.PortableBundleBinding != "" || attempt.PortableBundleHeadsSHA256 != ""
	if !any {
		return nil
	}
	if !portableRepairBundlePathPattern.MatchString(filepath.ToSlash(attempt.PortableBundlePath)) ||
		!recoveryDigestPattern.MatchString(attempt.PortableBundleSHA256) || attempt.PortableBundleBytes <= 0 ||
		attempt.PortableBundleBytes > portableRepairBundleMaxBytes || !recoveryDigestPattern.MatchString(attempt.PortableBundleBinding) ||
		!recoveryDigestPattern.MatchString(attempt.PortableBundleHeadsSHA256) {
		return fmt.Errorf("portable bundle metadata is incomplete or outside its bounds")
	}
	if attempt.PortableBundleBinding != portableRepairBundleBinding(attempt, attempt.PortableBundleStage) {
		return fmt.Errorf("portable bundle binding does not match the repair identity and exact objects")
	}
	wantPath, err := portableRepairBundleRelativePath(attempt, attempt.PortableBundleStage, attempt.PortableBundleBinding)
	if err != nil || filepath.ToSlash(attempt.PortableBundlePath) != wantPath {
		return fmt.Errorf("portable bundle path does not match its immutable binding")
	}
	switch attempt.PortableBundleStage {
	case StageValidated:
		if attempt.Stage != StageValidated || !validGitCommitSHA(attempt.CandidateGuardCommit) || attempt.CandidateGuardRef == "" {
			return fmt.Errorf("validated portable bundle has no exact candidate guard")
		}
	case StageCommitted:
		if attempt.Stage != StageCommitted && attempt.Stage != StagePushed && attempt.Stage != StagePROpen || !validGitCommitSHA(attempt.CommitSHA) {
			return fmt.Errorf("committed portable bundle has no exact repair commit")
		}
	default:
		return fmt.Errorf("portable bundle stage %q is unsupported", attempt.PortableBundleStage)
	}
	return nil
}

func hasPortableRepairBundle(attempt Attempt) bool {
	return attempt.PortableBundlePath != "" || attempt.PortableBundleSHA256 != "" || attempt.PortableBundleBytes != 0 ||
		attempt.PortableBundleStage != "" || attempt.PortableBundleBinding != "" || attempt.PortableBundleHeadsSHA256 != ""
}

func portableRepairBundleBinding(attempt Attempt, stage Stage) string {
	parts := []string{
		attempt.Repository, attempt.RepositoryFingerprint, strconv.Itoa(attempt.Recurrence), strconv.Itoa(attempt.Attempt),
		attempt.Branch, string(stage), strings.Join(sortedPortableStrings(attempt.ChangedFiles), "\x00"),
	}
	switch stage {
	case StageValidated:
		parts = append(parts,
			attempt.ModelBaseTree, attempt.ModelBaseParent, attempt.ModelBaseGuardRef, attempt.ModelBaseGuardCommit, attempt.ModelBaseGuardBinding, attempt.ModelBaseGuardKind,
			attempt.CandidateTree, attempt.CandidateParent, attempt.CandidateGuardRef, attempt.CandidateGuardCommit, attempt.CandidateGuardBinding, attempt.CandidateGuardKind,
		)
	case StageCommitted:
		parts = append(parts, attempt.CommitSHA)
	default:
		return ""
	}
	parts = append(parts, attempt.PortableBundleHeadsSHA256)
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:])
}

func portableRepairBundleRelativePath(attempt Attempt, stage Stage, binding string) (string, error) {
	if !recoveryDigestPattern.MatchString(binding) || stage != StageValidated && stage != StageCommitted || attempt.Attempt <= 0 || attempt.Recurrence < 0 {
		return "", fmt.Errorf("portable bundle path inputs are invalid")
	}
	fingerprint := sha256.Sum256([]byte(attempt.RepositoryFingerprint))
	return fmt.Sprintf("portable-git/%x-r%d-a%d-%s-%s.bundle", fingerprint[:12], attempt.Recurrence, attempt.Attempt, stage, binding[:24]), nil
}

func sortedPortableStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func (s *Store) portableRepairBundlePath(relative string) (string, error) {
	relative = filepath.ToSlash(strings.TrimSpace(relative))
	if !portableRepairBundlePathPattern.MatchString(relative) {
		return "", fmt.Errorf("portable repair bundle path is unsafe")
	}
	root := filepath.Join(filepath.Dir(s.path), "portable-git")
	path := filepath.Join(filepath.Dir(s.path), filepath.FromSlash(relative))
	cleanRoot, cleanPath := filepath.Clean(root), filepath.Clean(path)
	if filepath.Dir(cleanPath) != cleanRoot {
		return "", fmt.Errorf("portable repair bundle escapes its private directory")
	}
	return cleanPath, nil
}

func ensurePrivatePortableBundleDirectory(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !portableModeIsPrivate(info.Mode()) {
		return fmt.Errorf("portable repair bundle directory is not a private real directory")
	}
	return nil
}

func portableBundleRefs(attempt Attempt, stage Stage) ([]string, error) {
	refs := []string{}
	if stage == StageCommitted {
		if !validHiveRepairBranch(attempt.Branch) || !validGitCommitSHA(attempt.CommitSHA) {
			return nil, fmt.Errorf("committed portable bundle identity is invalid")
		}
		refs = append(refs, "refs/heads/"+attempt.Branch)
	}
	for _, guard := range []struct {
		ref, commit string
	}{{attempt.ModelBaseGuardRef, attempt.ModelBaseGuardCommit}, {attempt.CandidateGuardRef, attempt.CandidateGuardCommit}} {
		if guard.ref == "" && guard.commit == "" {
			continue
		}
		if guard.ref == "" || !validGitCommitSHA(guard.commit) {
			return nil, fmt.Errorf("portable bundle guard identity is incomplete")
		}
		refs = append(refs, guard.ref)
	}
	if stage == StageValidated && len(refs) == 0 {
		return nil, fmt.Errorf("validated portable bundle has no guard refs")
	}
	sort.Strings(refs)
	for index := 1; index < len(refs); index++ {
		if refs[index] == refs[index-1] {
			return nil, fmt.Errorf("portable bundle repeats a Git ref")
		}
	}
	return refs, nil
}

func portableBundleHeadsDigest(ctx context.Context, worktree string, refs []string) (string, error) {
	heads := make(map[string]string, len(refs))
	for _, ref := range refs {
		value, err := runGit(ctx, worktree, "rev-parse", "--verify", ref)
		if err != nil || !validGitCommitSHA(strings.TrimSpace(value)) {
			return "", fmt.Errorf("resolve portable repair bundle head %s: %w", ref, err)
		}
		heads[ref] = strings.ToLower(strings.TrimSpace(value))
	}
	return portableListedHeadsDigest(heads), nil
}

func portableListedHeadsDigest(heads map[string]string) string {
	values := make([]string, 0, len(heads))
	for ref, object := range heads {
		values = append(values, strings.ToLower(strings.TrimSpace(object))+"\x00"+ref)
	}
	sort.Strings(values)
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(digest[:])
}

func portableBundlePrerequisites(ctx context.Context, attempt Attempt, stage Stage) ([]string, error) {
	values := []string{}
	if validGitCommitSHA(attempt.CandidateParent) {
		values = append(values, attempt.CandidateParent)
	}
	if validGitCommitSHA(attempt.ModelBaseParent) {
		values = append(values, attempt.ModelBaseParent)
	}
	if stage == StageCommitted && len(values) == 0 {
		parent, err := runGit(ctx, attempt.Worktree, "rev-parse", attempt.CommitSHA+"^")
		if err != nil || !validGitCommitSHA(strings.TrimSpace(parent)) {
			return nil, fmt.Errorf("read committed portable bundle prerequisite: %w", err)
		}
		values = append(values, strings.TrimSpace(parent))
	}
	values = sortedPortableStrings(values)
	unique := values[:0]
	for _, value := range values {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	if len(unique) == 0 {
		return nil, fmt.Errorf("portable bundle must exclude an exact repository prerequisite")
	}
	return unique, nil
}

func (w *Worker) persistPortableRepairBundle(ctx context.Context, attempt *Attempt, stage Stage) error {
	if w == nil || w.State == nil || attempt == nil || stage != StageValidated && stage != StageCommitted {
		return fmt.Errorf("portable repair bundle request is invalid")
	}
	refs, err := portableBundleRefs(*attempt, stage)
	if err != nil {
		return err
	}
	prerequisites, err := portableBundlePrerequisites(ctx, *attempt, stage)
	if err != nil {
		return err
	}
	headsDigest, err := portableBundleHeadsDigest(ctx, attempt.Worktree, refs)
	if err != nil {
		return err
	}
	bundleAttempt := *attempt
	bundleAttempt.PortableBundleHeadsSHA256 = headsDigest
	binding := portableRepairBundleBinding(bundleAttempt, stage)
	relative, err := portableRepairBundleRelativePath(bundleAttempt, stage, binding)
	if err != nil {
		return err
	}
	path, err := w.State.portableRepairBundlePath(relative)
	if err != nil {
		return err
	}
	if err := ensurePrivatePortableBundleDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".pending-portable-*.bundle")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	args := []string{"bundle", "create", temporaryPath}
	args = append(args, refs...)
	for _, prerequisite := range prerequisites {
		args = append(args, "^"+prerequisite)
	}
	if _, err := runGit(ctx, attempt.Worktree, args...); err != nil {
		return fmt.Errorf("create bounded portable repair bundle: %w", err)
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return err
	}
	data, info, err := readPortableRepairBundle(temporaryPath, "", 0, false)
	if err != nil {
		return err
	}
	if err := replaceFileSynced(path, data); err != nil {
		return fmt.Errorf("publish portable repair bundle: %w", err)
	}
	removeTemporary = false
	_ = os.Remove(temporaryPath)
	digest := sha256.Sum256(data)
	attempt.PortableBundlePath = relative
	attempt.PortableBundleSHA256 = hex.EncodeToString(digest[:])
	attempt.PortableBundleBytes = info.Size()
	attempt.PortableBundleStage = stage
	attempt.PortableBundleBinding = binding
	attempt.PortableBundleHeadsSHA256 = headsDigest
	return portableRepairBundleStateError(*attempt)
}

func readPortableRepairBundle(path, expectedDigest string, expectedBytes int64, allowMissing bool) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && allowMissing {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !portableModeIsPrivate(info.Mode()) || info.Size() <= 0 || info.Size() > portableRepairBundleMaxBytes {
		return nil, nil, fmt.Errorf("portable repair bundle is not a bounded private regular file")
	}
	if expectedBytes > 0 && info.Size() != expectedBytes {
		return nil, nil, fmt.Errorf("portable repair bundle size changed")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, after) || after.Size() != info.Size() || !after.Mode().IsRegular() || !portableModeIsPrivate(after.Mode()) {
		return nil, nil, fmt.Errorf("portable repair bundle changed while it was read")
	}
	digest := sha256.Sum256(data)
	if expectedDigest != "" && hex.EncodeToString(digest[:]) != expectedDigest {
		return nil, nil, fmt.Errorf("portable repair bundle digest changed")
	}
	return data, info, nil
}

func (w *Worker) restorePortableRepairCheckpoint(ctx context.Context, findingTitle string, issue int, repositoryID string, attempt *Attempt) error {
	if err := portableRepairBundleStateError(*attempt); err != nil {
		return err
	}
	path, err := w.State.portableRepairBundlePath(attempt.PortableBundlePath)
	if err != nil {
		return err
	}
	if err := ensurePrivatePortableBundleDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if _, _, err := readPortableRepairBundle(path, attempt.PortableBundleSHA256, attempt.PortableBundleBytes, false); err != nil {
		return fmt.Errorf("verify portable repair bundle bytes: %w", err)
	}
	if _, err := runGit(ctx, w.Config.RepositoryDir, "bundle", "verify", path); err != nil {
		return fmt.Errorf("verify portable repair bundle prerequisites: %w", err)
	}
	refs, err := portableBundleRefs(*attempt, attempt.PortableBundleStage)
	if err != nil {
		return err
	}
	heads, err := runGit(ctx, w.Config.RepositoryDir, "bundle", "list-heads", path)
	if err != nil {
		return fmt.Errorf("list portable repair bundle heads: %w", err)
	}
	expectedHeads := make(map[string]string, len(refs))
	for _, ref := range refs {
		switch ref {
		case "refs/heads/" + attempt.Branch:
			expectedHeads[ref] = attempt.CommitSHA
		case attempt.ModelBaseGuardRef:
			expectedHeads[ref] = attempt.ModelBaseGuardCommit
		case attempt.CandidateGuardRef:
			expectedHeads[ref] = attempt.CandidateGuardCommit
		default:
			return fmt.Errorf("portable repair bundle contains an unexpected requested ref")
		}
	}
	listed := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(heads), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !validGitCommitSHA(fields[0]) {
			return fmt.Errorf("portable repair bundle returned malformed head data")
		}
		listed[fields[1]] = strings.ToLower(fields[0])
	}
	if portableListedHeadsDigest(listed) != attempt.PortableBundleHeadsSHA256 {
		return fmt.Errorf("portable repair bundle head set changed")
	}
	for ref, want := range expectedHeads {
		if listed[ref] != strings.ToLower(want) {
			return fmt.Errorf("portable repair bundle head %s changed", ref)
		}
	}
	for ref := range listed {
		if ref != "refs/heads/"+attempt.Branch && !sealedTreeRefPattern.MatchString(ref) && !toolSnapshotRefPattern.MatchString(ref) {
			return fmt.Errorf("portable repair bundle contains an unsupported internal ref %s", ref)
		}
	}
	worktree := filepath.Join(w.Config.WorktreeRoot, shortFingerprint(attempt.RepositoryFingerprint))
	controllerRepository, err := captureRepositoryGitControl(w.Config.RepositoryDir, "")
	if err != nil {
		return fmt.Errorf("inspect controller repository before portable repair restore: %w", err)
	}
	if _, statErr := os.Lstat(worktree); statErr == nil {
		if _, err := captureRepositoryGitControl(worktree, controllerRepository.CommonDir.Path); err != nil {
			return fmt.Errorf("refuse unsafe existing portable repair worktree: %w", err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect portable repair worktree destination: %w", statErr)
	}
	if err := prepareWorktree(ctx, w.Config.RepositoryDir, worktree, attempt.Branch, w.Config.BaseBranch, "", w.Config.ExpectedRemoteURL); err != nil {
		return fmt.Errorf("prepare fresh portable repair worktree: %w", err)
	}
	if _, err := captureRepositoryGitControl(worktree, controllerRepository.CommonDir.Path); err != nil {
		return fmt.Errorf("bind restored portable repair worktree Git control state: %w", err)
	}
	for ref, want := range expectedHeads {
		if _, err := runGit(ctx, worktree, "fetch", "--no-tags", path, ref); err != nil {
			return fmt.Errorf("import portable repair object %s: %w", ref, err)
		}
		fetched, err := runGit(ctx, worktree, "rev-parse", "FETCH_HEAD")
		if err != nil || strings.TrimSpace(fetched) != want {
			return fmt.Errorf("portable repair object %s did not import at its exact ID", ref)
		}
		if ref == "refs/heads/"+attempt.Branch {
			continue
		}
		current, err := readRepairRef(ctx, worktree, ref)
		if err != nil {
			return err
		}
		if current == "" {
			if _, err := runGit(ctx, worktree, "update-ref", "--no-deref", ref, want, strings.Repeat("0", 40)); err != nil {
				return fmt.Errorf("restore exact portable repair guard %s: %w", ref, err)
			}
		} else if current != want {
			return fmt.Errorf("portable repair guard %s collides with a different object", ref)
		}
	}
	restoreHead := attempt.CandidateParent
	if attempt.PortableBundleStage == StageCommitted {
		restoreHead = attempt.CommitSHA
	}
	if !validGitCommitSHA(restoreHead) {
		return fmt.Errorf("portable repair restore head is invalid")
	}
	branch, err := runGit(ctx, worktree, "branch", "--show-current")
	if err != nil || strings.TrimSpace(branch) != attempt.Branch {
		return fmt.Errorf("portable repair worktree is not on its exact Hive branch")
	}
	if _, err := runGit(ctx, worktree, "reset", "--hard", restoreHead); err != nil {
		return fmt.Errorf("restore exact portable repair head: %w", err)
	}
	if _, err := runGit(ctx, worktree, "clean", "-fd", "--", "."); err != nil {
		return fmt.Errorf("clean restored portable repair worktree: %w", err)
	}
	attempt.Worktree = worktree
	if attempt.PortableBundleStage == StageValidated {
		if err := validateSealedCandidate(ctx, worktree, *attempt); err != nil {
			return fmt.Errorf("validate restored sealed repair candidate: %w", err)
		}
	} else if attempt.ModelBaseGuardRef != "" || attempt.CandidateGuardRef != "" {
		if err := verifySealedRepairCommit(ctx, worktree, attempt.CommitSHA, *attempt, repairCommitMessage(findingTitle, issue, repositoryID)); err != nil {
			return fmt.Errorf("validate restored committed repair: %w", err)
		}
	} else if recovered, ok, err := recoverCommittedRepair(ctx, worktree, attempt.ChangedFiles, findingTitle, issue, repositoryID); err != nil || !ok || recovered != attempt.CommitSHA {
		return fmt.Errorf("validate restored committed repair ownership: recovered=%s exact=%t: %w", recovered, ok, err)
	}
	return w.State.Put(*attempt)
}

func clearPortableRepairBundle(attempt *Attempt) {
	attempt.PortableBundlePath = ""
	attempt.PortableBundleSHA256 = ""
	attempt.PortableBundleBytes = 0
	attempt.PortableBundleStage = ""
	attempt.PortableBundleBinding = ""
	attempt.PortableBundleHeadsSHA256 = ""
}

func (s *Store) retirePortableRepairBundle(attempt *Attempt) error {
	if !hasPortableRepairBundle(*attempt) {
		return nil
	}
	if err := portableRepairBundleStateError(*attempt); err != nil {
		return err
	}
	path, err := s.portableRepairBundlePath(attempt.PortableBundlePath)
	if err != nil {
		return err
	}
	if err := ensurePrivatePortableBundleDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if _, _, err := readPortableRepairBundle(path, attempt.PortableBundleSHA256, attempt.PortableBundleBytes, true); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syncParentDirectory(path); err != nil {
		return err
	}
	clearPortableRepairBundle(attempt)
	return s.Put(*attempt)
}

func (s *Store) sweepUnreferencedPortableRepairBundles() error {
	root := filepath.Join(filepath.Dir(s.path), "portable-git")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := ensurePrivatePortableBundleDirectory(root); err != nil {
		return err
	}
	referenced := map[string]bool{}
	for _, attempt := range s.Snapshot().Attempts {
		if attempt != nil && attempt.PortableBundlePath != "" {
			referenced[filepath.Base(filepath.FromSlash(attempt.PortableBundlePath))] = true
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		relative := "portable-git/" + name
		if referenced[name] || !portableRepairBundlePathPattern.MatchString(relative) {
			continue
		}
		path, err := s.portableRepairBundlePath(relative)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !portableModeIsPrivate(info.Mode()) || info.Size() > portableRepairBundleMaxBytes {
			return fmt.Errorf("unreferenced portable repair bundle is not safe to remove")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return syncParentDirectory(root)
}

func portableModeIsPrivate(mode os.FileMode) bool {
	// Windows' os.FileMode permission bits are synthesized and do not describe
	// its ACL. The containing Hive state root is ACL-sealed by the hosted
	// controller; on POSIX the artifact and directory must additionally have no
	// group/other permission bits.
	return runtime.GOOS == "windows" || mode.Perm()&0o077 == 0
}
