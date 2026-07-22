package repair

import (
	"context"
	"fmt"
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
	commonDir, err := repairGitCommonDir(ctx, attempt.Worktree)
	if err != nil {
		return err
	}
	identity, err := repairRepositoryIdentity(ctx, attempt.Worktree, attempt.Repository, commonDir)
	if err != nil {
		return err
	}
	entries, err := s.repairRefJournalEntries()
	if err != nil {
		return err
	}
	matching := func(entry repairRefJournalEntry) bool {
		return entry.Kind == guard.kind && entry.Ref == guard.ref && entry.Commit == guard.commit && entry.Binding == guard.binding && entry.Repository == attempt.Repository
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
	for _, entry := range entries {
		if !matching(entry) || entry.Phase != "tombstone_intent" || tombstoneConsumed[entry.TombstoneRef] {
			continue
		}
		if !sameRepairCommonDir(entry.GitCommonDir, commonDir) || entry.RepositoryIdentity != identity {
			return fmt.Errorf("unconsumed repair ref tombstone belongs to a different repository identity")
		}
		if err := resumeRepairRefTombstone(ctx, attempt.Worktree, attempt.Repository, s, entry, originalConsumed); err != nil {
			return err
		}
		originalConsumed = true
		tombstoneConsumed[entry.TombstoneRef] = true
	}
	if originalConsumed {
		return nil
	}
	return retireRepairRef(ctx, attempt.Worktree, attempt.Repository, s, guard.kind, guard.ref, guard.commit, guard.binding)
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
