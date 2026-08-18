package repair

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	StateSchemaV1 = "hive.repair-worker-state.v1"
	StateSchemaV2 = "hive.repair-worker-state.v2"
	StateSchemaV3 = "hive.repair-worker-state.v3"
	StateSchemaV4 = "hive.repair-worker-state.v4"
	StateSchemaV5 = "hive.repair-worker-state.v5"
	StateSchema   = "hive.repair-worker-state.v6"
)

type Stage string

const (
	StagePreparing     Stage = "preparing"
	StagePrepared      Stage = "prepared"
	StageModelRunning  Stage = "model_running"
	StageModelComplete Stage = "model_complete"
	StageNoChange      Stage = "no_change"
	StageValidated     Stage = "validated"
	StageCommitted     Stage = "committed"
	StagePushed        Stage = "pushed"
	StagePROpen        Stage = "pr_open"
	StageFailed        Stage = "failed"
)

type FailureClass string

const (
	FailureInfrastructure FailureClass = "infrastructure"
	FailurePatchEngine    FailureClass = "patch_engine"
	FailureModel          FailureClass = "model"
)

type Attempt struct {
	Repository            string `json:"repository"`
	RepositoryFingerprint string `json:"repository_fingerprint"`
	Recurrence            int    `json:"recurrence,omitempty"`
	Attempt               int    `json:"attempt"`
	Branch                string `json:"branch"`
	Worktree              string `json:"worktree"`
	DiscardDirtyBranch    string `json:"discard_dirty_branch,omitempty"`
	Stage                 Stage  `json:"stage"`
	Provider              string `json:"provider"`
	LifecycleStarted      bool   `json:"lifecycle_started,omitempty"`
	AttemptCounted        bool   `json:"attempt_counted"`
	// SpecialistRevision* reserves one deterministic next-attempt ordinal and
	// deadline anchor before a failed exact-head PR artifact is dispatched to a
	// persistent specialist. It is consumed only by the Worker revision path.
	SpecialistRevisionAttempt   int       `json:"specialist_revision_attempt,omitempty"`
	SpecialistRevisionStartedAt time.Time `json:"specialist_revision_started_at,omitempty"`
	SpecialistRevisionBaseSHA   string    `json:"specialist_revision_base_sha,omitempty"`
	SpecialistRevisionBranch    string    `json:"specialist_revision_branch,omitempty"`
	SpecialistRevisionPRNumber  int       `json:"specialist_revision_pr_number,omitempty"`
	ModelInvocationID           string    `json:"model_invocation_id,omitempty"`
	ModelSummary                string    `json:"model_summary,omitempty"`
	PriorModelSummary           string    `json:"prior_model_summary,omitempty"`
	ModelPatch                  string    `json:"model_patch,omitempty"`
	// ModelBaseTree is the exact cumulative tree presented to a read-only
	// provider. CandidateTree is the immutable tree authorized after applying
	// that provider's bounded patch; commits are created from it, never by
	// staging a mutable post-validation worktree.
	ModelBaseTree         string `json:"model_base_tree,omitempty"`
	ModelBaseParent       string `json:"model_base_parent,omitempty"`
	ModelBaseGuardRef     string `json:"model_base_guard_ref,omitempty"`
	ModelBaseGuardCommit  string `json:"model_base_guard_commit,omitempty"`
	ModelBaseGuardBinding string `json:"model_base_guard_binding,omitempty"`
	ModelBaseGuardKind    string `json:"model_base_guard_kind,omitempty"`
	CandidateTree         string `json:"candidate_tree,omitempty"`
	CandidateParent       string `json:"candidate_parent,omitempty"`
	CandidateGuardRef     string `json:"candidate_guard_ref,omitempty"`
	CandidateGuardCommit  string `json:"candidate_guard_commit,omitempty"`
	CandidateGuardBinding string `json:"candidate_guard_binding,omitempty"`
	CandidateGuardKind    string `json:"candidate_guard_kind,omitempty"`
	// PortableBundle* binds the minimum Git object closure needed to resume a
	// validated or committed repair on a fresh hosted runner. The bundle lives
	// below the repair state directory; it never contains a source checkout or
	// mutable worktree.
	PortableBundlePath        string       `json:"portable_bundle_path,omitempty"`
	PortableBundleSHA256      string       `json:"portable_bundle_sha256,omitempty"`
	PortableBundleBytes       int64        `json:"portable_bundle_bytes,omitempty"`
	PortableBundleStage       Stage        `json:"portable_bundle_stage,omitempty"`
	PortableBundleBinding     string       `json:"portable_bundle_binding,omitempty"`
	PortableBundleHeadsSHA256 string       `json:"portable_bundle_heads_sha256,omitempty"`
	ToolSnapshotRef           string       `json:"tool_snapshot_ref,omitempty"`
	ToolSnapshotCommit        string       `json:"tool_snapshot_commit,omitempty"`
	ToolSnapshotHead          string       `json:"tool_snapshot_head,omitempty"`
	ToolSnapshotBranch        string       `json:"tool_snapshot_branch,omitempty"`
	ToolSnapshotPhase         string       `json:"tool_snapshot_phase,omitempty"`
	ToolSnapshotStartedAt     time.Time    `json:"tool_snapshot_started_at,omitempty"`
	ToolSnapshotDeadline      time.Time    `json:"tool_snapshot_deadline,omitempty"`
	RecoveredPatchAttempt     int          `json:"recovered_patch_attempt,omitempty"`
	RecoveredPatchSHA256      string       `json:"recovered_patch_sha256,omitempty"`
	RecoveredPatchAuthorized  bool         `json:"recovered_patch_authorized,omitempty"`
	RecoveredProviderSHA256   string       `json:"recovered_provider_sha256,omitempty"`
	PreparationRecoveryHead   string       `json:"preparation_recovery_head,omitempty"`
	PreparationReplayTree     string       `json:"preparation_replay_tree,omitempty"`
	PreparationReplayProof    string       `json:"preparation_replay_proof,omitempty"`
	PreparationCleanupPending bool         `json:"preparation_cleanup_pending,omitempty"`
	PreparationCleanupRef     string       `json:"preparation_cleanup_ref,omitempty"`
	PreparationCleanupCommit  string       `json:"preparation_cleanup_commit,omitempty"`
	LastFailureClass          FailureClass `json:"last_failure_class,omitempty"`
	LastFailureID             string       `json:"last_failure_id,omitempty"`
	LastFailure               string       `json:"last_failure,omitempty"`
	ResumeStage               Stage        `json:"resume_stage,omitempty"`
	RetryAuthorizedAt         time.Time    `json:"retry_authorized_at,omitempty"`
	RetryActor                string       `json:"retry_actor,omitempty"`
	RetryReason               string       `json:"retry_reason,omitempty"`
	RetryTransactionID        string       `json:"retry_transaction_id,omitempty"`
	RetryResumeStage          Stage        `json:"retry_resume_stage,omitempty"`
	CommitSHA                 string       `json:"commit_sha,omitempty"`
	PRNumber                  int          `json:"pr_number,omitempty"`
	PRURL                     string       `json:"pr_url,omitempty"`
	LifecyclePROpen           bool         `json:"lifecycle_pr_open,omitempty"`
	// LegacyOwnershipAdoption is set only while migrating a fully bound
	// pre-v5 PR-open checkpoint. It is consumed after one exact local refresh
	// creates an ownership-trailed head and is never enabled for new attempts.
	LegacyOwnershipAdoption  bool            `json:"legacy_ownership_adoption,omitempty"`
	LegacyUnsealedCheckpoint bool            `json:"legacy_unsealed_checkpoint,omitempty"`
	ChangedFiles             []string        `json:"changed_files,omitempty"`
	BaselineReview           *BaselineReview `json:"baseline_review,omitempty"`
	StartedAt                time.Time       `json:"started_at"`
	UpdatedAt                time.Time       `json:"updated_at"`
}

// CountedModelAttempts returns the bounded model budget represented by this
// checkpoint. An infrastructure failure may already have reserved the next
// ordinal without invoking a successful model run, so its uncounted ordinal is
// excluded until ensureAttemptCounted reconciles it.
func (a Attempt) CountedModelAttempts() int {
	count := a.Attempt
	if !a.AttemptCounted {
		count--
	}
	return max(count, 0)
}

type BaselineReviewStatus string

const (
	BaselineReviewCandidateReady BaselineReviewStatus = "candidate_ready"
	BaselineReviewProposalOpen   BaselineReviewStatus = "proposal_open"
	BaselineReviewApproved       BaselineReviewStatus = "approved"
	BaselineReviewRejected       BaselineReviewStatus = "rejected"
)

// BaselineCandidate is review evidence only. ActualPath points to a Hive-owned
// local copy; BaselinePath is always a repository-relative path under the
// configured baseline root. Neither field grants approval authority.
type BaselineCandidate struct {
	ContractID     string `json:"contract_id"`
	ScreenshotName string `json:"screenshot_name"`
	Route          string `json:"route,omitempty"`
	Viewport       string `json:"viewport,omitempty"`
	Platform       string `json:"platform"`
	ActualPath     string `json:"actual_path"`
	BaselinePath   string `json:"baseline_path"`
	SHA256         string `json:"sha256"`
	Bytes          int64  `json:"bytes"`
	Source         string `json:"source"`
}

type BaselineReview struct {
	Status            BaselineReviewStatus `json:"status"`
	Candidates        []BaselineCandidate  `json:"candidates"`
	RepairHeadSHA     string               `json:"repair_head_sha,omitempty"`
	SourceWorkflowRun int64                `json:"source_workflow_run_id,omitempty"`
	SourceArtifactID  int64                `json:"source_artifact_id,omitempty"`
	SourceRunURL      string               `json:"source_run_url,omitempty"`
	ProposalBranch    string               `json:"proposal_branch,omitempty"`
	ProposalCommitSHA string               `json:"proposal_commit_sha,omitempty"`
	ProposalPRNumber  int                  `json:"proposal_pr_number,omitempty"`
	ProposalPRURL     string               `json:"proposal_pr_url,omitempty"`
	ProposalMergeSHA  string               `json:"proposal_merge_sha,omitempty"`
	RejectionReason   string               `json:"rejection_reason,omitempty"`
}

type State struct {
	SchemaVersion string              `json:"schema_version"`
	Attempts      map[string]*Attempt `json:"attempts"`
}

type Store struct {
	mu             sync.Mutex
	path           string
	auditPath      string
	refJournalPath string
	data           State
	renameFile     func(string, string) error
	poisoned       error
}

func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("repair state directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	store := &Store{
		path:           filepath.Join(dir, "repair-worker-state.json"),
		auditPath:      filepath.Join(dir, "repair-retry-audit.jsonl"),
		refJournalPath: filepath.Join(dir, "repair-ref-journal.jsonl"),
		data:           State{SchemaVersion: StateSchema, Attempts: map[string]*Attempt{}},
		renameFile:     durableRename,
	}
	data, err := os.ReadFile(store.path)
	if err == nil {
		if err := json.Unmarshal(data, &store.data); err != nil {
			return nil, fmt.Errorf("parse repair worker state: %w", err)
		}
		migrated := false
		if store.data.SchemaVersion == StateSchemaV1 || store.data.SchemaVersion == StateSchemaV2 || store.data.SchemaVersion == StateSchemaV3 || store.data.SchemaVersion == StateSchemaV4 {
			for _, attempt := range store.data.Attempts {
				if attempt != nil && (attempt.LifecycleStarted || attempt.Stage != StagePrepared) {
					attempt.AttemptCounted = true
				}
				// v1-v3 did not persist the post-PR lifecycle checkpoint. It is
				// recoverable only when every exact PR binding was already durable;
				// integrated reconciliation additionally requires the lifecycle to
				// match this repository, branch, PR, and head before any mutation.
				if attempt != nil && attempt.Stage == StagePROpen && strings.TrimSpace(attempt.CommitSHA) != "" &&
					attempt.PRNumber > 0 && strings.TrimSpace(attempt.PRURL) != "" && strings.HasPrefix(strings.TrimSpace(attempt.Branch), "hive/repair-") {
					attempt.LifecyclePROpen = true
					attempt.LegacyOwnershipAdoption = true
				}
				if attempt != nil && (attempt.Stage == StageModelComplete || attempt.Stage == StageValidated) {
					attempt.LegacyUnsealedCheckpoint = true
				}
			}
			store.data.SchemaVersion = StateSchema
			migrated = true
		} else if store.data.SchemaVersion == StateSchemaV5 {
			// v5 already persisted exact attempt-counting semantics. In particular,
			// a model_running checkpoint can be intentionally uncounted until its
			// ambiguous invocation is reconciled; never apply the v1-v4 inference.
			for _, attempt := range store.data.Attempts {
				if attempt != nil && (attempt.Stage == StageModelComplete || attempt.Stage == StageValidated) {
					attempt.LegacyUnsealedCheckpoint = true
				}
			}
			store.data.SchemaVersion = StateSchema
			migrated = true
		} else if store.data.SchemaVersion != StateSchema {
			return nil, fmt.Errorf("unsupported repair state schema %q", store.data.SchemaVersion)
		}
		if store.data.Attempts == nil {
			store.data.Attempts = map[string]*Attempt{}
		}
		if err := validatePersistedRepairState(store.data); err != nil {
			return nil, fmt.Errorf("validate repair worker state: %w", err)
		}
		if migrated {
			if err := store.persistLocked(); err != nil {
				return nil, fmt.Errorf("migrate repair worker state: %w", err)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := store.reconcileRetryAuditLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Get(fingerprint string) (Attempt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	attempt := s.data.Attempts[fingerprint]
	if attempt == nil {
		return Attempt{}, false
	}
	return cloneAttempt(*attempt), true
}

// Err reports an in-process state-store poison condition. A store is poisoned
// when a failed atomic replacement cannot be reloaded and confirmed by a
// second durable rewrite; callers must stop before external side effects.
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.poisoned
}

func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.data)
}

func (s *Store) Put(attempt Attempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return s.poisoned
	}
	previous := cloneState(s.data)
	attempt.UpdatedAt = time.Now().UTC()
	copy := cloneAttempt(attempt)
	s.data.Attempts[attempt.RepositoryFingerprint] = &copy
	desired := cloneState(s.data)
	if err := validatePersistedRepairState(desired); err != nil {
		s.data = previous
		return fmt.Errorf("validate repair worker state update: %w", err)
	}
	if err := s.persistLocked(); err != nil {
		if reconcileErr := s.reconcilePersistFailureLocked(err, previous, desired); reconcileErr != nil {
			return reconcileErr
		}
	}
	return nil
}

func cloneAttempt(attempt Attempt) Attempt {
	attempt.ChangedFiles = append([]string(nil), attempt.ChangedFiles...)
	if attempt.BaselineReview != nil {
		review := *attempt.BaselineReview
		review.Candidates = append([]BaselineCandidate(nil), attempt.BaselineReview.Candidates...)
		attempt.BaselineReview = &review
	}
	return attempt
}

func cloneState(state State) State {
	result := State{SchemaVersion: state.SchemaVersion, Attempts: make(map[string]*Attempt, len(state.Attempts))}
	for key, attempt := range state.Attempts {
		if attempt != nil {
			copy := cloneAttempt(*attempt)
			result.Attempts[key] = &copy
		}
	}
	return result
}

func (s *Store) persistLocked() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	temporary := s.path + ".tmp"
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
	renameFile := s.renameFile
	if renameFile == nil {
		renameFile = durableRename
	}
	if err := renameFile(temporary, s.path); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}

func (s *Store) reconcilePersistFailureLocked(persistErr error, previous, desired State) error {
	if readErr := s.reloadPersistedStateLocked(); readErr != nil {
		s.poisoned = fmt.Errorf("repair worker state is poisoned after an indeterminate replacement (%v); reload failed: %w", persistErr, readErr)
		return s.poisoned
	}
	if !sameRepairState(s.data, previous) && !sameRepairState(s.data, desired) {
		s.poisoned = fmt.Errorf("repair worker state is poisoned after an indeterminate replacement: disk contains neither the prior nor requested state (%v)", persistErr)
		return s.poisoned
	}
	// The first replacement may have completed before reporting a directory
	// flush error, or may have left the prior file intact. Rewrite the requested
	// state and require a completely
	// successful durable replacement before allowing external side effects.
	s.data = cloneState(desired)
	if retryErr := s.persistLocked(); retryErr == nil {
		return nil
	} else if readErr := s.reloadPersistedStateLocked(); readErr != nil {
		s.poisoned = fmt.Errorf("repair worker state is poisoned after repeated indeterminate replacements (%v; %v); reload failed: %w", persistErr, retryErr, readErr)
	} else {
		s.poisoned = fmt.Errorf("repair worker state is poisoned after repeated indeterminate replacements: first=%v retry=%v", persistErr, retryErr)
	}
	return s.poisoned
}

func sameRepairState(left, right State) bool {
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftData) == string(rightData)
}

func (s *Store) reloadPersistedStateLocked() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var recovered State
	if err := json.Unmarshal(data, &recovered); err != nil {
		return err
	}
	if err := validatePersistedRepairState(recovered); err != nil {
		return err
	}
	s.data = recovered
	return nil
}

func validatePersistedRepairState(state State) error {
	if state.SchemaVersion != StateSchema {
		return fmt.Errorf("unexpected repair worker state schema %q", state.SchemaVersion)
	}
	if state.Attempts == nil {
		return fmt.Errorf("repair worker state has no attempts map")
	}
	for fingerprint, attempt := range state.Attempts {
		if strings.TrimSpace(fingerprint) == "" || attempt == nil || attempt.RepositoryFingerprint != fingerprint {
			return fmt.Errorf("repair worker state contains an invalid attempt key %q", fingerprint)
		}
		if !validToolSnapshot(*attempt) {
			return fmt.Errorf("repair worker state contains an invalid tool snapshot for %q", fingerprint)
		}
		if !validRecoveredPatchProvenance(*attempt) {
			return fmt.Errorf("repair worker state contains invalid recovered-patch provenance for %q", fingerprint)
		}
		if !validSpecialistRevisionReservation(*attempt) {
			return fmt.Errorf("repair worker state contains an invalid specialist revision reservation for %q", fingerprint)
		}
		if err := sealedTreeGuardStateError(*attempt); err != nil {
			return fmt.Errorf("repair worker state contains invalid sealed-tree guards for %q: %w", fingerprint, err)
		}
		if err := portableRepairBundleStateError(*attempt); err != nil {
			return fmt.Errorf("repair worker state contains invalid portable Git bundle for %q: %w", fingerprint, err)
		}
		if hasToolSnapshot(*attempt) && attempt.PreparationCleanupPending {
			return fmt.Errorf("repair worker state mixes tool and preparation-cleanup snapshots for %q", fingerprint)
		}
	}
	return nil
}

func validSpecialistRevisionReservation(attempt Attempt) bool {
	if attempt.SpecialistRevisionAttempt == 0 {
		return attempt.SpecialistRevisionStartedAt.IsZero() && attempt.SpecialistRevisionBaseSHA == "" &&
			attempt.SpecialistRevisionBranch == "" && attempt.SpecialistRevisionPRNumber == 0
	}
	return attempt.SpecialistRevisionAttempt > attempt.Attempt && !attempt.SpecialistRevisionStartedAt.IsZero() &&
		attempt.SpecialistRevisionStartedAt.Location() == time.UTC && validGitCommitSHA(attempt.SpecialistRevisionBaseSHA) &&
		strings.TrimSpace(attempt.SpecialistRevisionBranch) != "" && attempt.SpecialistRevisionPRNumber > 0 && attempt.Stage == StagePROpen
}
