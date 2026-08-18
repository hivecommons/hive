package repair

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// RetryRequest binds an operator-authorized resume to the exact failed model
// ordinal and failure classification observed by the caller. This prevents a
// stale retry request from resuming a different failure after state changes.
type RetryRequest struct {
	RepositoryFingerprint string       `json:"repository_fingerprint"`
	ExpectedRecurrence    int          `json:"expected_recurrence"`
	ExpectedAttempt       int          `json:"expected_attempt"`
	ExpectedFailureClass  FailureClass `json:"expected_failure_class"`
	ExpectedFailureID     string       `json:"expected_failure_id"`
	Actor                 string       `json:"actor"`
	Reason                string       `json:"reason"`
}

type RetryAuditEntry struct {
	Timestamp             time.Time    `json:"timestamp"`
	Action                string       `json:"action"`
	Phase                 string       `json:"phase"`
	TransactionID         string       `json:"transaction_id"`
	Allowed               bool         `json:"allowed"`
	RepositoryFingerprint string       `json:"repository_fingerprint,omitempty"`
	ExpectedRecurrence    int          `json:"expected_recurrence"`
	ExpectedAttempt       int          `json:"expected_attempt,omitempty"`
	ExpectedFailureClass  FailureClass `json:"expected_failure_class,omitempty"`
	ExpectedFailureID     string       `json:"expected_failure_id,omitempty"`
	ResumeStage           Stage        `json:"resume_stage,omitempty"`
	Actor                 string       `json:"actor,omitempty"`
	Reason                string       `json:"reason,omitempty"`
	Detail                string       `json:"detail"`
}

// ResumableFailureError reports an infrastructure or patch-engine failure that
// is durably paused. ResumeRetry must authorize the exact checkpoint before a
// worker can continue; it never spends another model attempt by itself.
type ResumableFailureError struct {
	RepositoryFingerprint string
	Recurrence            int
	Attempt               int
	Class                 FailureClass
	FailureID             string
	Cause                 error
}

func (e *ResumableFailureError) Error() string {
	return fmt.Sprintf("repair %s recurrence %d attempt %d paused after %s failure %s: %v", e.RepositoryFingerprint, e.Recurrence, e.Attempt, e.Class, e.FailureID, e.Cause)
}

func (e *ResumableFailureError) Unwrap() error { return e.Cause }

func IsResumableFailureError(err error) bool {
	var resumable *ResumableFailureError
	return errors.As(err, &resumable)
}

// ResumeRetry atomically validates and resumes an exact failed checkpoint. It
// records every allow or deny decision in a flushed append-only audit before
// changing repair state. It does not increment Attempt or clear the durable
// failure classification, so status remains explainable after recovery.
func (s *Store) ResumeRetry(request RetryRequest) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return Attempt{}, s.poisoned
	}

	request.RepositoryFingerprint = strings.TrimSpace(request.RepositoryFingerprint)
	request.Actor = strings.TrimSpace(request.Actor)
	request.Reason = strings.TrimSpace(request.Reason)
	request.ExpectedFailureID = strings.TrimSpace(request.ExpectedFailureID)
	now := time.Now().UTC()
	transactionID := retryTransactionID(request, now)
	entry := RetryAuditEntry{
		Timestamp: now, Action: "resume_retry", TransactionID: transactionID, RepositoryFingerprint: request.RepositoryFingerprint,
		ExpectedRecurrence: request.ExpectedRecurrence, ExpectedAttempt: request.ExpectedAttempt, ExpectedFailureClass: request.ExpectedFailureClass,
		ExpectedFailureID: request.ExpectedFailureID, Actor: request.Actor, Reason: safeExcerpt(request.Reason),
	}

	deny := func(cause error) (Attempt, error) {
		entry.Allowed = false
		entry.Phase = "deny"
		entry.Detail = safeExcerpt(cause.Error())
		if auditErr := s.appendRetryAuditLocked(entry); auditErr != nil {
			return Attempt{}, fmt.Errorf("%v; persist denied retry audit: %w", cause, auditErr)
		}
		return Attempt{}, cause
	}

	if request.RepositoryFingerprint == "" || request.ExpectedRecurrence < 0 || request.ExpectedAttempt <= 0 || request.ExpectedFailureClass == "" || request.ExpectedFailureID == "" || request.Actor == "" || request.Reason == "" {
		return deny(fmt.Errorf("retry requires a repository fingerprint, non-negative expected recurrence, expected attempt, expected failure class, expected failure ID, actor, and reason"))
	}
	attempt := s.data.Attempts[request.RepositoryFingerprint]
	if attempt == nil {
		return deny(fmt.Errorf("repair checkpoint %q does not exist", request.RepositoryFingerprint))
	}
	if attempt.Recurrence != request.ExpectedRecurrence || attempt.Attempt != request.ExpectedAttempt || attempt.LastFailureClass != request.ExpectedFailureClass || attempt.LastFailureID != request.ExpectedFailureID {
		return deny(fmt.Errorf("retry checkpoint changed: current recurrence=%d attempt=%d class=%s failure_id=%s", attempt.Recurrence, attempt.Attempt, attempt.LastFailureClass, attempt.LastFailureID))
	}
	if err := validateRetryCheckpoint(*attempt); err != nil {
		return deny(err)
	}
	if attempt.RecoveredPatchAttempt > 0 {
		if attempt.ResumeStage != StageModelComplete || attempt.PreparationCleanupPending || !validRecoveredPatchProvenance(*attempt) || !recoveredPatchDigestMatches(*attempt) {
			return deny(fmt.Errorf("recovered patch retry checkpoint has invalid exact provenance"))
		}
		if !strings.Contains(safeExcerpt(request.Reason), recoveredProviderAttestation(*attempt)) {
			return deny(fmt.Errorf("recovered patch retry reason must attest the historical read-only Codex provider identity with %s", recoveredProviderAttestation(*attempt)))
		}
	}

	entry.Allowed = true
	entry.Phase = "intent"
	entry.ResumeStage = attempt.ResumeStage
	entry.TransactionID = retryTransactionID(request, now, attempt.ResumeStage)
	transactionID = entry.TransactionID
	entry.Detail = fmt.Sprintf("resume %s from %s without incrementing model attempt %d", attempt.ResumeStage, attempt.LastFailureClass, attempt.Attempt)
	if attempt.RecoveredPatchAttempt > 0 {
		entry.Detail += fmt.Sprintf("; authorize recovered source attempt %d patch %s provider %s replay %s", attempt.RecoveredPatchAttempt, attempt.RecoveredPatchSHA256, attempt.RecoveredProviderSHA256, attempt.PreparationReplayProof)
	}
	if err := s.appendRetryAuditLocked(entry); err != nil {
		return Attempt{}, fmt.Errorf("persist allowed retry audit: %w", err)
	}

	previous := cloneState(s.data)
	updated := cloneAttempt(*attempt)
	updated.Stage = updated.ResumeStage
	updated.RetryResumeStage = updated.ResumeStage
	updated.ResumeStage = ""
	updated.RetryAuthorizedAt = now
	updated.RetryActor = request.Actor
	updated.RetryReason = safeExcerpt(request.Reason)
	updated.RetryTransactionID = transactionID
	if updated.RecoveredPatchAttempt > 0 {
		updated.RecoveredPatchAuthorized = true
	}
	updated.UpdatedAt = now
	s.data.Attempts[request.RepositoryFingerprint] = &updated
	desired := cloneState(s.data)
	if err := validatePersistedRepairState(desired); err != nil {
		s.data = previous
		return Attempt{}, fmt.Errorf("validate resumed repair checkpoint: %w", err)
	}
	if err := s.persistLocked(); err != nil {
		if reconcileErr := s.reconcilePersistFailureLocked(err, previous, desired); reconcileErr != nil {
			return Attempt{}, fmt.Errorf("persist resumed repair checkpoint: %w", reconcileErr)
		}
	}
	entry.Timestamp = time.Now().UTC()
	entry.Phase = "commit"
	entry.Detail = fmt.Sprintf("resumed %s at model attempt %d", updated.Stage, updated.Attempt)
	if err := s.appendRetryAuditLocked(entry); err != nil {
		s.poisoned = fmt.Errorf("repair retry was applied but its commit audit is not durable: %w", err)
		return cloneAttempt(updated), s.poisoned
	}
	return cloneAttempt(updated), nil
}

func retryTransactionID(request RetryRequest, at time.Time, resumeStages ...Stage) string {
	resumeStage := Stage("")
	if len(resumeStages) > 0 {
		resumeStage = resumeStages[0]
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d", request.RepositoryFingerprint, request.ExpectedRecurrence, request.ExpectedAttempt, request.ExpectedFailureClass, request.ExpectedFailureID, resumeStage, request.Actor, safeExcerpt(strings.TrimSpace(request.Reason)), at.UnixNano())))
	return hex.EncodeToString(digest[:16])
}

// reconcileRetryAuditLocked completes the audit commit for a state transition
// that was durably renamed before the process stopped. An intent without a
// state transaction remains intentionally incomplete and is never promoted.
func (s *Store) reconcileRetryAuditLocked() error {
	commits := map[string]RetryAuditEntry{}
	intents := map[string]RetryAuditEntry{}
	data, err := os.ReadFile(s.auditPath)
	if err == nil {
		data, err = s.recoverTornRetryAuditLocked(data)
		if err != nil {
			return err
		}
		for lineNumber, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var entry RetryAuditEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				return fmt.Errorf("parse repair retry audit line %d: %w", lineNumber+1, err)
			}
			if err := validateRetryAuditEntry(entry); err != nil {
				return fmt.Errorf("validate repair retry audit line %d: %w", lineNumber+1, err)
			}
			if entry.Phase == "commit" && entry.TransactionID != "" {
				intent, exists := intents[entry.TransactionID]
				if !exists {
					return fmt.Errorf("repair retry commit transaction %s precedes or has no durable audit intent", entry.TransactionID)
				}
				if entry.Timestamp.Before(intent.Timestamp) || !sameRetryAuditTransaction(intent, entry) {
					return fmt.Errorf("repair retry commit transaction %s does not match its prior durable intent", entry.TransactionID)
				}
				if _, duplicate := commits[entry.TransactionID]; duplicate {
					return fmt.Errorf("repair retry transaction %s has duplicate commit records", entry.TransactionID)
				}
				commits[entry.TransactionID] = entry
			} else if entry.Phase == "intent" && entry.TransactionID != "" {
				if _, duplicate := intents[entry.TransactionID]; duplicate {
					return fmt.Errorf("repair retry transaction %s has duplicate intent records", entry.TransactionID)
				}
				intents[entry.TransactionID] = entry
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for transactionID, commit := range commits {
		intent, exists := intents[transactionID]
		if !exists {
			return fmt.Errorf("repair retry commit transaction %s has no durable audit intent", transactionID)
		}
		if !sameRetryAuditTransaction(intent, commit) {
			return fmt.Errorf("repair retry commit transaction %s does not match its durable intent", transactionID)
		}
	}
	for _, attempt := range s.data.Attempts {
		if attempt == nil || attempt.RetryTransactionID == "" {
			continue
		}
		entry, exists := intents[attempt.RetryTransactionID]
		if !exists {
			return fmt.Errorf("repair retry state transaction %s has no durable audit intent", attempt.RetryTransactionID)
		}
		if err := validateRetryStateTransaction(*attempt, entry); err != nil {
			return fmt.Errorf("repair retry state transaction %s is inconsistent: %w", attempt.RetryTransactionID, err)
		}
		if _, committed := commits[attempt.RetryTransactionID]; committed {
			continue
		}
		entry.Timestamp = time.Now().UTC()
		entry.Phase = "commit"
		entry.Detail = fmt.Sprintf("recovered durable retry transition at stage %s", attempt.Stage)
		if err := s.appendRetryAuditLocked(entry); err != nil {
			return fmt.Errorf("reconcile repair retry audit transaction %s: %w", attempt.RetryTransactionID, err)
		}
	}
	return nil
}

func validateRetryAuditEntry(entry RetryAuditEntry) error {
	if entry.Action != "resume_retry" || strings.TrimSpace(entry.TransactionID) == "" || entry.Timestamp.IsZero() {
		return fmt.Errorf("retry audit record is missing action, transaction ID, or timestamp")
	}
	switch entry.Phase {
	case "deny":
		if entry.Allowed {
			return fmt.Errorf("denied retry audit record is marked allowed")
		}
		return nil
	case "intent", "commit":
		if !entry.Allowed || strings.TrimSpace(entry.RepositoryFingerprint) == "" || entry.ExpectedRecurrence < 0 || entry.ExpectedAttempt <= 0 ||
			(entry.ExpectedFailureClass != FailureInfrastructure && entry.ExpectedFailureClass != FailurePatchEngine) ||
			strings.TrimSpace(entry.ExpectedFailureID) == "" || strings.TrimSpace(entry.Actor) == "" || strings.TrimSpace(entry.Reason) == "" ||
			(entry.ResumeStage != StagePreparing && entry.ResumeStage != StagePrepared && entry.ResumeStage != StageModelComplete && entry.ResumeStage != StagePROpen) {
			return fmt.Errorf("allowed retry audit record has incomplete or ineligible authority fields")
		}
		if entry.ExpectedFailureClass == FailurePatchEngine && entry.ResumeStage != StageModelComplete ||
			entry.ExpectedFailureClass == FailureInfrastructure && entry.ResumeStage == StageModelComplete {
			return fmt.Errorf("retry audit resume stage does not match its failure class")
		}
		if entry.Phase == "intent" {
			request := RetryRequest{
				RepositoryFingerprint: entry.RepositoryFingerprint, ExpectedRecurrence: entry.ExpectedRecurrence,
				ExpectedAttempt: entry.ExpectedAttempt, ExpectedFailureClass: entry.ExpectedFailureClass,
				ExpectedFailureID: entry.ExpectedFailureID, Actor: entry.Actor, Reason: entry.Reason,
			}
			if retryTransactionID(request, entry.Timestamp, entry.ResumeStage) != entry.TransactionID {
				return fmt.Errorf("retry intent transaction ID does not bind its immutable request")
			}
		}
		return nil
	default:
		return fmt.Errorf("retry audit record has unsupported phase %q", entry.Phase)
	}
}

func sameRetryAuditTransaction(intent, commit RetryAuditEntry) bool {
	return intent.TransactionID == commit.TransactionID && intent.Action == commit.Action && intent.Allowed == commit.Allowed &&
		intent.RepositoryFingerprint == commit.RepositoryFingerprint && intent.ExpectedRecurrence == commit.ExpectedRecurrence &&
		intent.ExpectedAttempt == commit.ExpectedAttempt && intent.ExpectedFailureClass == commit.ExpectedFailureClass &&
		intent.ExpectedFailureID == commit.ExpectedFailureID && intent.ResumeStage == commit.ResumeStage && intent.Actor == commit.Actor && intent.Reason == commit.Reason
}

func validateRetryStateTransaction(attempt Attempt, intent RetryAuditEntry) error {
	if attempt.RepositoryFingerprint != intent.RepositoryFingerprint || attempt.Recurrence != intent.ExpectedRecurrence ||
		attempt.Attempt != intent.ExpectedAttempt || attempt.LastFailureClass != intent.ExpectedFailureClass ||
		attempt.LastFailureID != intent.ExpectedFailureID || attempt.RetryActor != intent.Actor ||
		attempt.RetryReason != safeExcerpt(intent.Reason) || !attempt.RetryAuthorizedAt.Equal(intent.Timestamp) ||
		attempt.RetryTransactionID != intent.TransactionID || attempt.RetryResumeStage != intent.ResumeStage {
		return fmt.Errorf("persisted retry authority does not match the intent")
	}
	if attempt.Stage == StageFailed || attempt.ResumeStage != "" {
		return fmt.Errorf("persisted retry transaction did not leave the failed checkpoint")
	}
	if current, ok := retryStageRank(attempt.Stage); !ok {
		return fmt.Errorf("persisted retry transaction has unsupported stage %q", attempt.Stage)
	} else if resumed, ok := retryStageRank(intent.ResumeStage); !ok || current < resumed {
		return fmt.Errorf("persisted retry transaction regressed before its authorized resume stage")
	}
	if attempt.LastFailureClass == FailurePatchEngine && !attempt.AttemptCounted {
		return fmt.Errorf("patch-engine retry lost its counted model ordinal")
	}
	return nil
}

func retryStageRank(stage Stage) (int, bool) {
	switch stage {
	case StagePreparing:
		return 0, true
	case StagePrepared:
		return 1, true
	case StageModelRunning:
		return 2, true
	case StageModelComplete:
		return 3, true
	case StageNoChange, StageValidated:
		return 4, true
	case StageCommitted:
		return 5, true
	case StagePushed:
		return 6, true
	case StagePROpen:
		return 7, true
	default:
		return 0, false
	}
}

func (s *Store) recoverTornRetryAuditLocked(data []byte) ([]byte, error) {
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return data, nil
	}
	lastNewline := bytes.LastIndexByte(data, '\n')
	complete, fragment := []byte{}, data
	if lastNewline >= 0 {
		complete = append([]byte(nil), data[:lastNewline+1]...)
		fragment = data[lastNewline+1:]
	}
	digest := sha256.Sum256(fragment)
	quarantine := fmt.Sprintf("%s.torn-%s", s.auditPath, hex.EncodeToString(digest[:8]))
	if err := writeNewSynced(quarantine, fragment); err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("quarantine torn repair retry audit: %w", err)
		}
		existing, readErr := os.ReadFile(quarantine)
		if readErr != nil {
			return nil, fmt.Errorf("verify torn repair retry audit quarantine: %w", readErr)
		}
		if !bytes.Equal(existing, fragment) {
			return nil, fmt.Errorf("torn repair retry audit quarantine content changed")
		}
	}
	if err := replaceFileSynced(s.auditPath, complete); err != nil {
		return nil, fmt.Errorf("remove quarantined torn repair retry audit record: %w", err)
	}
	return complete, nil
}

func writeNewSynced(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncParentDirectory(path)
}

func replaceFileSynced(path string, data []byte) error {
	temporary := path + ".recover.tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := durableRename(temporary, path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func validateRetryCheckpoint(attempt Attempt) error {
	if attempt.Stage != StageFailed {
		return fmt.Errorf("repair attempt %d is not paused in failed state", attempt.Attempt)
	}
	if strings.TrimSpace(attempt.LastFailureID) == "" {
		return fmt.Errorf("repair failure checkpoint has no durable ID")
	}
	switch attempt.LastFailureClass {
	case FailureInfrastructure:
		// A migrated v1-v3 prepared checkpoint may already have spent its
		// lifecycle ordinal before provider health ran. It is still safe to
		// resume that exact ordinal; ResumeRetry never increments it again.
		if attempt.ResumeStage != StagePreparing && attempt.ResumeStage != StagePrepared && attempt.ResumeStage != StagePROpen {
			return fmt.Errorf("infrastructure retry checkpoint is inconsistent")
		}
		if attempt.ResumeStage == StagePROpen && (!attempt.AttemptCounted || !attempt.LifecyclePROpen || attempt.PRNumber <= 0 ||
			strings.TrimSpace(attempt.PRURL) == "" || strings.TrimSpace(attempt.CommitSHA) == "" || len(attempt.ChangedFiles) == 0) {
			return fmt.Errorf("PR refresh retry checkpoint is incomplete")
		}
	case FailurePatchEngine:
		if !attempt.AttemptCounted || attempt.ResumeStage != StageModelComplete {
			return fmt.Errorf("patch-engine retry checkpoint is inconsistent")
		}
	default:
		return fmt.Errorf("failure class %q is not eligible for same-attempt retry", attempt.LastFailureClass)
	}
	return nil
}

func (s *Store) appendRetryAuditLocked(entry RetryAuditEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(s.auditPath)
	created := os.IsNotExist(statErr)
	if statErr != nil && !created {
		return statErr
	}
	file, err := os.OpenFile(s.auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if created {
		return syncParentDirectory(s.auditPath)
	}
	return nil
}
