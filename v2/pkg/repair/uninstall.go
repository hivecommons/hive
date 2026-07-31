package repair

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const StageCancelled Stage = "cancelled"

type uninstallRepairRef struct {
	kind    string
	ref     string
	commit  string
	binding string
}

// UninstallRefsRetired is true only after every durable local guard authority
// and its owned commit/binding fields have been cleared. Integrated uninstall
// must not treat StageCancelled as terminal without this proof.
func UninstallRefsRetired(attempt Attempt) bool {
	return !hasToolSnapshot(attempt) &&
		!hasPortableRepairBundle(attempt) &&
		attempt.RecoveredPatchAttempt == 0 && attempt.RecoveredPatchSHA256 == "" && !attempt.RecoveredPatchAuthorized && attempt.RecoveredProviderSHA256 == "" &&
		attempt.PreparationRecoveryHead == "" && attempt.PreparationReplayTree == "" && attempt.PreparationReplayProof == "" &&
		attempt.PreparationCleanupRef == "" && attempt.PreparationCleanupCommit == "" && !attempt.PreparationCleanupPending &&
		attempt.ModelBaseTree == "" && attempt.ModelBaseParent == "" && attempt.ModelBaseGuardRef == "" && attempt.ModelBaseGuardCommit == "" && attempt.ModelBaseGuardBinding == "" && attempt.ModelBaseGuardKind == "" &&
		attempt.CandidateTree == "" && attempt.CandidateParent == "" && attempt.CandidateGuardRef == "" && attempt.CandidateGuardCommit == "" && attempt.CandidateGuardBinding == "" && attempt.CandidateGuardKind == ""
}

// CancelForUninstall makes a repair checkpoint terminal only after every
// exact, journaled guard ref has completed the normal tombstone retirement
// protocol. Existing tombstone intents are resumed before any new authority is
// created, making a crash between atomic ref movement and journal completion
// retry-safe. An unjournaled or changed ref is never touched.
func (s *Store) CancelForUninstall(ctx context.Context, repositoryFingerprint string) error {
	attempt, exists := s.Get(repositoryFingerprint)
	if !exists {
		return fmt.Errorf("repair attempt %s not found", repositoryFingerprint)
	}
	if attempt.Stage == StageCancelled && UninstallRefsRetired(attempt) {
		return nil
	}
	if hasPortableRepairBundle(attempt) {
		if err := s.retirePortableRepairBundle(&attempt); err != nil {
			return fmt.Errorf("retire uninstall portable repair bundle: %w", err)
		}
	}
	refs, err := uninstallRepairRefs(attempt)
	if err != nil {
		return err
	}
	for _, guard := range refs {
		if err := s.retireJournaledUninstallRef(ctx, attempt, guard); err != nil {
			return fmt.Errorf("retire uninstall repair ref %s: %w", guard.ref, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return s.poisoned
	}
	current := s.data.Attempts[repositoryFingerprint]
	if current == nil {
		return fmt.Errorf("repair attempt %s disappeared during uninstall ref retirement", repositoryFingerprint)
	}
	currentRefs, err := uninstallRepairRefs(*current)
	if err != nil {
		return err
	}
	if current.Repository != attempt.Repository || current.Worktree != attempt.Worktree || !sameUninstallRepairRefs(refs, currentRefs) {
		return fmt.Errorf("repair attempt %s changed during uninstall ref retirement", repositoryFingerprint)
	}
	previous := cloneState(s.data)
	clearToolSnapshot(current)
	clearRecoveredPatchProvenance(current)
	clearSealedCandidate(current)
	current.Stage = StageCancelled
	current.UpdatedAt = time.Now().UTC()
	current.ResumeStage = ""
	current.RetryAuthorizedAt = time.Time{}
	current.RetryActor = ""
	current.RetryReason = ""
	current.RetryTransactionID = ""
	current.RetryResumeStage = ""
	if current.BaselineReview != nil && current.BaselineReview.Status != BaselineReviewApproved && current.BaselineReview.Status != BaselineReviewRejected {
		current.BaselineReview.Status = BaselineReviewRejected
		current.BaselineReview.RejectionReason = "cancelled by explicit Hive uninstall"
	}
	if !UninstallRefsRetired(*current) {
		s.data = previous
		return fmt.Errorf("repair attempt %s retained guard authority after uninstall cancellation", repositoryFingerprint)
	}
	desired := cloneState(s.data)
	if err := validatePersistedRepairState(desired); err != nil {
		s.data = previous
		return err
	}
	if err := s.persistLocked(); err != nil {
		if reconcileErr := s.reconcilePersistFailureLocked(err, previous, desired); reconcileErr != nil {
			return reconcileErr
		}
	}
	return nil
}

func uninstallRepairRefs(attempt Attempt) ([]uninstallRepairRef, error) {
	refs := make([]uninstallRepairRef, 0, 4)
	if hasToolSnapshot(attempt) {
		if !validToolSnapshot(attempt) {
			return nil, fmt.Errorf("repair attempt %s has invalid tool snapshot authority", attempt.RepositoryFingerprint)
		}
		refs = append(refs, uninstallRepairRef{
			kind: attempt.ToolSnapshotPhase, ref: attempt.ToolSnapshotRef, commit: attempt.ToolSnapshotCommit,
			binding: toolSnapshotBinding(attempt, attempt.ToolSnapshotPhase, attempt.ToolSnapshotHead, attempt.ToolSnapshotBranch),
		})
	}
	if attempt.PreparationCleanupRef != "" || attempt.PreparationCleanupCommit != "" || attempt.PreparationCleanupPending {
		if !validRecoveredPatchProvenance(attempt) || !attempt.PreparationCleanupPending {
			return nil, fmt.Errorf("repair attempt %s has invalid preparation cleanup authority", attempt.RepositoryFingerprint)
		}
		refs = append(refs, uninstallRepairRef{kind: "preparation_recovery", ref: attempt.PreparationCleanupRef, commit: attempt.PreparationCleanupCommit, binding: attempt.PreparationReplayProof})
	}
	if err := sealedTreeGuardStateError(attempt); err != nil {
		return nil, err
	}
	if attempt.ModelBaseGuardRef != "" {
		refs = append(refs, uninstallRepairRef{kind: attempt.ModelBaseGuardKind, ref: attempt.ModelBaseGuardRef, commit: attempt.ModelBaseGuardCommit, binding: attempt.ModelBaseGuardBinding})
	}
	if attempt.CandidateGuardRef != "" {
		refs = append(refs, uninstallRepairRef{kind: attempt.CandidateGuardKind, ref: attempt.CandidateGuardRef, commit: attempt.CandidateGuardCommit, binding: attempt.CandidateGuardBinding})
	}
	unique := make(map[string]uninstallRepairRef, len(refs))
	for _, guard := range refs {
		if !validUninstallRepairRef(guard) {
			return nil, fmt.Errorf("repair attempt %s has invalid uninstall ref %s", attempt.RepositoryFingerprint, guard.ref)
		}
		key := guard.kind + "\x00" + guard.ref + "\x00" + guard.commit + "\x00" + guard.binding
		unique[key] = guard
	}
	refs = refs[:0]
	for _, guard := range unique {
		refs = append(refs, guard)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ref < refs[j].ref })
	return refs, nil
}

func validUninstallRepairRef(guard uninstallRepairRef) bool {
	if !validGitCommitSHA(guard.commit) || !recoveryDigestPattern.MatchString(guard.binding) {
		return false
	}
	switch guard.kind {
	case toolSnapshotPreparation, toolSnapshotValidation:
		return toolSnapshotRefPattern.MatchString(guard.ref) && strings.Contains(guard.ref, "/"+guard.binding[:24]+"/")
	case "preparation_recovery":
		return preparationCleanupRefPattern.MatchString(guard.ref) && strings.Contains(guard.ref, "/"+guard.binding[:24]+"/")
	case sealedTreeModelBase, sealedTreeCandidate:
		return sealedTreeRefPattern.MatchString(guard.ref) && strings.Contains(guard.ref, "/"+guard.kind+"/"+guard.binding[:24]+"/")
	default:
		return false
	}
}

func (s *Store) retireJournaledUninstallRef(ctx context.Context, attempt Attempt, guard uninstallRepairRef) error {
	entries, err := s.repairRefJournalEntries()
	if err != nil {
		return err
	}
	matching := func(entry repairRefJournalEntry) bool {
		return entry.Kind == guard.kind && entry.Ref == guard.ref && entry.Commit == guard.commit && entry.Binding == guard.binding && entry.Repository == attempt.Repository
	}
	repositoryDir, commonDir, identity, err := resolveUninstallRepairRepository(ctx, attempt, guard, entries)
	if err != nil {
		return err
	}
	sawCurrentIntent := false
	originalConsumed := false
	tombstoneConsumed := map[string]bool{}
	for _, entry := range entries {
		if !matching(entry) {
			continue
		}
		if entry.Phase == "intent" && sameRepairCommonDir(entry.GitCommonDir, commonDir) && entry.RepositoryIdentity == identity {
			sawCurrentIntent = true
		}
		if entry.Phase == "consumed" {
			originalConsumed = true
		}
		if entry.Phase == "tombstone_consumed" {
			tombstoneConsumed[entry.TombstoneRef] = true
		}
	}
	if !sawCurrentIntent && !originalConsumed {
		return fmt.Errorf("repair ref is not bound to a journaled intent in the current repository identity")
	}
	if sawCurrentIntent && !originalConsumed {
		if err := removeJournaledBrokenRepairRef(ctx, repositoryDir, commonDir, guard.ref); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		if !matching(entry) || entry.Phase != "tombstone_intent" || tombstoneConsumed[entry.TombstoneRef] {
			continue
		}
		if !sameRepairCommonDir(entry.GitCommonDir, commonDir) || entry.RepositoryIdentity != identity {
			return fmt.Errorf("unconsumed repair ref tombstone belongs to a different repository identity")
		}
		if err := resumeRepairRefTombstone(ctx, repositoryDir, attempt.Repository, s, entry, originalConsumed); err != nil {
			return err
		}
		originalConsumed = true
		tombstoneConsumed[entry.TombstoneRef] = true
	}
	if originalConsumed {
		return nil
	}
	return retireRepairRef(ctx, repositoryDir, attempt.Repository, s, guard.kind, guard.ref, guard.commit, guard.binding)
}

// removeJournaledBrokenRepairRef recovers only a zero-byte loose Hive ref after
// the exact journal and repository identity have already been verified. Git's
// ordinary ref lock excludes concurrent Git writers; the file is revalidated
// after the lock is held. A crash after removal is safe because the existing
// journaled-intent recovery treats an absent original and tombstone as already
// consumed and records the durable completion on retry.
func removeJournaledBrokenRepairRef(ctx context.Context, repositoryDir, commonDir, ref string) error {
	if _, err := readRepairRef(ctx, repositoryDir, ref); err == nil {
		return nil
	} else {
		var broken *brokenRepairRefError
		if !errors.As(err, &broken) || broken.ref != ref {
			return err
		}
	}
	refPath := filepath.Join(commonDir, filepath.FromSlash(ref))
	relative, err := filepath.Rel(commonDir, refPath)
	if err != nil || filepath.IsAbs(relative) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("broken journaled repair ref path escapes the verified repository")
	}
	lockPath := refPath + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("lock broken journaled repair ref: %w", err)
	}
	lockClosed := false
	defer func() {
		if !lockClosed {
			_ = lock.Close()
		}
		_ = os.Remove(lockPath)
		_ = syncParentDirectory(lockPath)
	}()
	if err := lock.Sync(); err != nil {
		return fmt.Errorf("sync broken journaled repair ref lock: %w", err)
	}
	if err := lock.Close(); err != nil {
		return fmt.Errorf("close broken journaled repair ref lock: %w", err)
	}
	lockClosed = true
	info, err := os.Lstat(refPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != 0 {
		return fmt.Errorf("broken journaled repair ref is not an ordinary zero-byte loose ref")
	}
	file, err := os.Open(refPath)
	if err != nil {
		return fmt.Errorf("open broken journaled repair ref: %w", err)
	}
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, afterErr := os.Lstat(refPath)
	if statErr != nil || closeErr != nil || afterErr != nil || !os.SameFile(info, opened) || !os.SameFile(info, after) || after.Size() != 0 || !after.Mode().IsRegular() {
		return fmt.Errorf("broken journaled repair ref changed while it was inspected")
	}
	if err := os.Remove(refPath); err != nil {
		return fmt.Errorf("remove broken journaled repair ref: %w", err)
	}
	if err := syncParentDirectory(refPath); err != nil {
		return fmt.Errorf("sync broken journaled repair ref removal: %w", err)
	}
	return nil
}

// resolveUninstallRepairRepository normally uses the recorded repair worktree.
// If that disposable worktree's .git locator was truncated after Hive durably
// journaled an exact guard ref, uninstall may recover only through the single
// common repository identity already bound by that journal. The fallback never
// trusts a path from the damaged worktree and cannot cross repository identity.
func resolveUninstallRepairRepository(ctx context.Context, attempt Attempt, guard uninstallRepairRef, entries []repairRefJournalEntry) (string, string, string, error) {
	commonDir, commonErr := repairGitCommonDir(ctx, attempt.Worktree)
	if commonErr == nil {
		identity, identityErr := repairRepositoryIdentity(ctx, attempt.Worktree, attempt.Repository, commonDir)
		if identityErr == nil {
			return attempt.Worktree, commonDir, identity, nil
		}
		commonErr = identityErr
	}

	journalCommonDir := ""
	journalIdentity := ""
	sawIntent := false
	for _, entry := range entries {
		if entry.Kind != guard.kind || entry.Ref != guard.ref || entry.Commit != guard.commit || entry.Binding != guard.binding || entry.Repository != attempt.Repository {
			continue
		}
		if entry.Phase == "intent" {
			sawIntent = true
		}
		if journalCommonDir == "" {
			journalCommonDir, journalIdentity = entry.GitCommonDir, entry.RepositoryIdentity
			continue
		}
		if !sameRepairCommonDir(entry.GitCommonDir, journalCommonDir) || entry.RepositoryIdentity != journalIdentity {
			return "", "", "", fmt.Errorf("repair worktree Git metadata is unavailable and exact journal records span repository identities: %w", commonErr)
		}
	}
	if !sawIntent || journalCommonDir == "" || journalIdentity == "" {
		return "", "", "", fmt.Errorf("repair worktree Git metadata is unavailable without an exact journaled repository identity: %w", commonErr)
	}
	journalCommonDir = filepath.Clean(journalCommonDir)
	if filepath.Base(journalCommonDir) != ".git" {
		return "", "", "", fmt.Errorf("repair worktree Git metadata is unavailable and the journaled common directory is not an ordinary repository .git directory: %w", commonErr)
	}
	repositoryDir := filepath.Dir(journalCommonDir)
	control, err := captureRepositoryGitControl(repositoryDir, journalCommonDir)
	if err != nil {
		return "", "", "", fmt.Errorf("repair worktree Git metadata is unavailable and the journaled repository controls are unsafe: %w", err)
	}
	identity, err := repairRepositoryIdentity(ctx, repositoryDir, attempt.Repository, control.CommonDir.Path)
	if err != nil {
		return "", "", "", fmt.Errorf("repair worktree Git metadata is unavailable and the journaled repository identity cannot be verified: %w", err)
	}
	if identity != journalIdentity {
		return "", "", "", fmt.Errorf("repair worktree Git metadata is unavailable and the journaled repository identity changed")
	}
	return repositoryDir, control.CommonDir.Path, identity, nil
}

func sameUninstallRepairRefs(left, right []uninstallRepairRef) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
