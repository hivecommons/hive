package integrated

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	HostedReleaseTransitionSchema = "hive.hosted-release-transition.v1"
	hostedReleaseTransitionFile   = "hosted-release-transition.json"
	hostedReleaseTransitionMax    = int64(6 << 20)
)

// HostedReleaseTransitionOperation identifies the one product operation whose
// immutable release identities are bound by a transition intent.
type HostedReleaseTransitionOperation string

const (
	HostedReleaseTransitionUpgrade  HostedReleaseTransitionOperation = "upgrade"
	HostedReleaseTransitionRollback HostedReleaseTransitionOperation = "rollback"
)

// HostedReleaseTransitionPhase is a monotonic crash-recovery checkpoint. A
// transition cannot skip a phase: every remote mutation is first bound to the
// exact evidence produced by the preceding phase.
type HostedReleaseTransitionPhase string

const (
	HostedReleaseTransitionPrepared                 HostedReleaseTransitionPhase = "prepared"
	HostedReleaseTransitionPreflightVerified        HostedReleaseTransitionPhase = "preflight_verified"
	HostedReleaseTransitionPausePrepared            HostedReleaseTransitionPhase = "pause_prepared"
	HostedReleaseTransitionPauseVerified            HostedReleaseTransitionPhase = "pause_verified"
	HostedReleaseTransitionSourceStatusPrepared     HostedReleaseTransitionPhase = "source_status_prepared"
	HostedReleaseTransitionSourceStatusVerified     HostedReleaseTransitionPhase = "source_status_verified"
	HostedReleaseTransitionSetupPrepared            HostedReleaseTransitionPhase = "setup_prepared"
	HostedReleaseTransitionPullRecorded             HostedReleaseTransitionPhase = "pull_recorded"
	HostedReleaseTransitionMerged                   HostedReleaseTransitionPhase = "merged"
	HostedReleaseTransitionTargetCheckpointPrepared HostedReleaseTransitionPhase = "target_checkpoint_prepared"
	HostedReleaseTransitionTargetCheckpointVerified HostedReleaseTransitionPhase = "target_checkpoint_verified"
	HostedReleaseTransitionStatusPrepared           HostedReleaseTransitionPhase = "status_prepared"
	HostedReleaseTransitionTargetVerified           HostedReleaseTransitionPhase = "target_verified"
	HostedReleaseTransitionPromoted                 HostedReleaseTransitionPhase = "promoted"
	HostedReleaseTransitionResumePrepared           HostedReleaseTransitionPhase = "resume_prepared"
	HostedReleaseTransitionResumeVerified           HostedReleaseTransitionPhase = "resume_verified"
	HostedReleaseTransitionPausePreserved           HostedReleaseTransitionPhase = "pause_preserved"
)

var (
	hostedTransitionRequestPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	hostedTransitionArtifactPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)
)

// HostedReleaseTransitionCandidateConfig is an unredacted durable Config
// snapshot. Unlike Config's public JSON form, it retains the complete managed
// preimage ledger needed for crash recovery and safe future uninstall.
type HostedReleaseTransitionCandidateConfig Config

// NewHostedReleaseTransitionCandidateConfig takes a complete Config snapshot
// and returns a deep, unredacted candidate plus its canonical policy digest.
func NewHostedReleaseTransitionCandidateConfig(config Config) (HostedReleaseTransitionCandidateConfig, string, error) {
	config.SchemaVersion = ConfigSchema
	type persistedConfig Config
	data, err := json.Marshal(persistedConfig(config))
	if err != nil {
		return HostedReleaseTransitionCandidateConfig{}, "", fmt.Errorf("encode hosted release transition candidate: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var candidate HostedReleaseTransitionCandidateConfig
	if err := decoder.Decode(&candidate); err != nil {
		return HostedReleaseTransitionCandidateConfig{}, "", fmt.Errorf("clone hosted release transition candidate: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return HostedReleaseTransitionCandidateConfig{}, "", err
	}
	digest, err := HostedReleaseTransitionConfigDigest(Config(candidate))
	if err != nil {
		return HostedReleaseTransitionCandidateConfig{}, "", err
	}
	return candidate, digest, nil
}

// Config returns a deep copy suitable for the one promotion Store.Save. The
// caller must use this sealed snapshot rather than regenerating policy after a
// setup branch or pull request has been published.
func (candidate HostedReleaseTransitionCandidateConfig) Config() (Config, error) {
	type persistedConfig Config
	data, err := json.Marshal(candidate)
	if err != nil {
		return Config{}, fmt.Errorf("encode sealed hosted release candidate: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result persistedConfig
	if err := decoder.Decode(&result); err != nil {
		return Config{}, fmt.Errorf("decode sealed hosted release candidate: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Config{}, err
	}
	return Config(result), nil
}

// HostedReleaseTransitionIntent is the durable authority boundary for one
// hosted upgrade or rollback. It retains only immutable identities and bounded
// provenance; downloaded artifacts and credentials never enter durable state.
type HostedReleaseTransitionIntent struct {
	SchemaVersion string                           `json:"schema_version"`
	Operation     HostedReleaseTransitionOperation `json:"operation"`
	Phase         HostedReleaseTransitionPhase     `json:"phase"`

	Repository    string `json:"repository"`
	RepositoryID  string `json:"repository_id"`
	DefaultBranch string `json:"default_branch"`

	ActiveRelease HostedReleaseIdentity `json:"active_release"`
	TargetRelease HostedReleaseIdentity `json:"target_release"`

	ActiveConfigDigest string `json:"active_config_sha256"`

	PreflightStatusRequestID string `json:"preflight_status_request_id"`
	PreflightRunID           int64  `json:"preflight_run_id,omitempty"`
	PreflightArtifactID      int64  `json:"preflight_artifact_id,omitempty"`
	PreflightArtifactName    string `json:"preflight_artifact_name,omitempty"`
	PreflightDefaultHeadSHA  string `json:"preflight_default_head_sha,omitempty"`
	PreflightStateHeadSHA    string `json:"preflight_state_head_sha,omitempty"`
	SourceWasPaused          bool   `json:"source_was_paused,omitempty"`

	PauseRequestID      string `json:"pause_request_id,omitempty"`
	PauseRunID          int64  `json:"pause_run_id,omitempty"`
	PauseArtifactID     int64  `json:"pause_artifact_id,omitempty"`
	PauseArtifactName   string `json:"pause_artifact_name,omitempty"`
	PauseDefaultHeadSHA string `json:"pause_default_head_sha,omitempty"`
	PauseStateHeadSHA   string `json:"pause_state_head_sha,omitempty"`

	SourceStatusRequestID      string `json:"source_status_request_id,omitempty"`
	SourceStatusRunID          int64  `json:"source_status_run_id,omitempty"`
	SourceStatusArtifactID     int64  `json:"source_status_artifact_id,omitempty"`
	SourceStatusArtifactName   string `json:"source_status_artifact_name,omitempty"`
	SourceStatusDefaultHeadSHA string `json:"source_status_default_head_sha,omitempty"`
	SourceStatusStateHeadSHA   string `json:"source_status_state_head_sha,omitempty"`
	SourceStatusPaused         bool   `json:"source_status_paused,omitempty"`
	SourceStatusQuiescent      bool   `json:"source_status_quiescent,omitempty"`

	SetupBranch                 string                                  `json:"setup_branch,omitempty"`
	SetupCommitSHA              string                                  `json:"setup_commit_sha,omitempty"`
	SetupBaseSHA                string                                  `json:"setup_base_sha,omitempty"`
	SetupDiffDigest             string                                  `json:"setup_diff_sha256,omitempty"`
	SetupCandidateDigest        string                                  `json:"setup_candidate_sha256,omitempty"`
	SetupMarker                 string                                  `json:"setup_marker,omitempty"`
	SetupChangedPaths           []string                                `json:"setup_changed_paths,omitempty"`
	SetupPRNumber               int                                     `json:"setup_pr_number,omitempty"`
	SetupPRURL                  string                                  `json:"setup_pr_url,omitempty"`
	SetupAuthorizationContext   string                                  `json:"setup_authorization_context,omitempty"`
	SetupAuthorizationStatusID  int64                                   `json:"setup_authorization_status_id,omitempty"`
	SetupAuthorizationCreatorID int64                                   `json:"setup_authorization_creator_id,omitempty"`
	MergeSHA                    string                                  `json:"merge_sha,omitempty"`
	CandidateConfig             *HostedReleaseTransitionCandidateConfig `json:"candidate_config,omitempty"`
	CandidateConfigDigest       string                                  `json:"candidate_config_sha256,omitempty"`

	TargetRequestID      string `json:"target_request_id,omitempty"`
	TargetRunID          int64  `json:"target_run_id,omitempty"`
	TargetArtifactID     int64  `json:"target_artifact_id,omitempty"`
	TargetArtifactName   string `json:"target_artifact_name,omitempty"`
	TargetDefaultHeadSHA string `json:"target_default_head_sha,omitempty"`
	TargetStateHeadSHA   string `json:"target_state_head_sha,omitempty"`
	TargetPaused         bool   `json:"target_paused,omitempty"`

	StatusRequestID      string `json:"status_request_id,omitempty"`
	StatusRunID          int64  `json:"status_run_id,omitempty"`
	StatusArtifactID     int64  `json:"status_artifact_id,omitempty"`
	StatusArtifactName   string `json:"status_artifact_name,omitempty"`
	StatusDefaultHeadSHA string `json:"status_default_head_sha,omitempty"`
	StatusStateHeadSHA   string `json:"status_state_head_sha,omitempty"`
	StatusPaused         bool   `json:"status_paused,omitempty"`
	StatusQuiescent      bool   `json:"status_quiescent,omitempty"`

	ResumeRequestID      string `json:"resume_request_id,omitempty"`
	ResumeRunID          int64  `json:"resume_run_id,omitempty"`
	ResumeArtifactID     int64  `json:"resume_artifact_id,omitempty"`
	ResumeArtifactName   string `json:"resume_artifact_name,omitempty"`
	ResumeDefaultHeadSHA string `json:"resume_default_head_sha,omitempty"`
	ResumeStateHeadSHA   string `json:"resume_state_head_sha,omitempty"`
	ResumeRunning        bool   `json:"resume_running,omitempty"`

	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	RecordSHA256 string    `json:"record_sha256"`
}

type persistedHostedReleaseTransitionIntent HostedReleaseTransitionIntent

// MarshalJSON preserves Config's normal disclosure boundary if an intent is
// ever included in diagnostics. Store persistence and record sealing
// deliberately use persistedHostedReleaseTransitionIntent instead.
func (intent HostedReleaseTransitionIntent) MarshalJSON() ([]byte, error) {
	public := persistedHostedReleaseTransitionIntent(intent)
	if public.CandidateConfig != nil {
		config := Config(*public.CandidateConfig)
		config.ManagedPathPreimages = redactManagedPathPreimages(config.ManagedPathPreimages)
		candidate := HostedReleaseTransitionCandidateConfig(config)
		public.CandidateConfig = &candidate
	}
	return json.Marshal(public)
}

// HostedReleaseTransitionConfigDigest returns the stable digest used to bind
// active and candidate durable policy. Only Store's writer-controlled UpdatedAt
// is excluded; pause state, setup identity, managed product policy, release
// identity, repository bindings, and complete preimages remain bound.
// Recovery must deterministically rebuild candidate Config from the exact
// active Config plus TargetRelease, regenerate its hosted workflow hash, and
// require this digest before performing any subsequent remote mutation.
func HostedReleaseTransitionConfigDigest(config Config) (string, error) {
	config.SchemaVersion = ConfigSchema
	config.UpdatedAt = time.Time{}
	type digestConfig Config
	data, err := json.Marshal(digestConfig(config))
	if err != nil {
		return "", fmt.Errorf("encode hosted release transition config: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) SaveHostedReleaseTransitionIntent(intent HostedReleaseTransitionIntent) error {
	intent = normalizeHostedReleaseTransitionIntent(intent)
	intent.SchemaVersion = HostedReleaseTransitionSchema
	now := time.Now().UTC()
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = now
	}
	intent.UpdatedAt = now
	intent.RecordSHA256 = ""
	seal, err := hostedReleaseTransitionSeal(intent)
	if err != nil {
		return err
	}
	intent.RecordSHA256 = seal
	if err := validateHostedReleaseTransitionIntent(intent); err != nil {
		return err
	}
	if err := s.validateHostedReleaseTransitionRepository(intent); err != nil {
		return err
	}

	current, exists, err := s.LoadHostedReleaseTransitionIntent()
	if err != nil {
		return fmt.Errorf("load existing hosted release transition before save: %w", err)
	}
	if !exists {
		if intent.Phase != HostedReleaseTransitionPrepared {
			return fmt.Errorf("hosted release transition must begin in %q", HostedReleaseTransitionPrepared)
		}
	} else if err := validateHostedReleaseTransitionAdvance(current, intent); err != nil {
		return err
	}

	data, err := json.MarshalIndent(persistedHostedReleaseTransitionIntent(intent), "", "  ")
	if err != nil {
		return err
	}
	if int64(len(data)+1) > hostedReleaseTransitionMax {
		return fmt.Errorf("hosted release transition exceeds %d bytes", hostedReleaseTransitionMax)
	}
	path := filepath.Join(s.dir, hostedReleaseTransitionFile)
	if err := rejectLinkedHostedTransitionDestination(path); err != nil {
		return err
	}
	return writeDurableStateFile(path, append(data, '\n'))
}

func (s *Store) LoadHostedReleaseTransitionIntent() (HostedReleaseTransitionIntent, bool, error) {
	path := filepath.Join(s.dir, hostedReleaseTransitionFile)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return HostedReleaseTransitionIntent{}, false, nil
	} else if err != nil {
		return HostedReleaseTransitionIntent{}, false, err
	}
	data, err := readBoundedOrdinaryFile(path, hostedReleaseTransitionMax)
	if err != nil {
		return HostedReleaseTransitionIntent{}, false, fmt.Errorf("read hosted release transition: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var intent HostedReleaseTransitionIntent
	if err := decoder.Decode(&intent); err != nil {
		return HostedReleaseTransitionIntent{}, false, fmt.Errorf("decode hosted release transition: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return HostedReleaseTransitionIntent{}, false, fmt.Errorf("decode hosted release transition: %w", err)
	}
	if err := validateHostedReleaseTransitionIntent(intent); err != nil {
		return HostedReleaseTransitionIntent{}, false, err
	}
	if err := s.validateHostedReleaseTransitionRepository(intent); err != nil {
		return HostedReleaseTransitionIntent{}, false, err
	}
	return intent, true, nil
}

// ClearHostedReleaseTransitionIntent durably removes only a fully resumed
// transaction whose target policy is now the durable Config. Earlier phases
// must recover forward or use the bounded pre-merge cancellation path.
func (s *Store) ClearHostedReleaseTransitionIntent() error {
	path := filepath.Join(s.dir, hostedReleaseTransitionFile)
	intent, exists, err := s.LoadHostedReleaseTransitionIntent()
	if err != nil {
		return err
	}
	if !exists {
		return durableRemoveStateFile(path)
	}
	validTerminal := !intent.SourceWasPaused && intent.Phase == HostedReleaseTransitionResumeVerified ||
		intent.SourceWasPaused && intent.Phase == HostedReleaseTransitionPausePreserved
	if !validTerminal {
		return fmt.Errorf("hosted release transition cannot be cleared before exact final pause-state verification")
	}
	if err := s.validateHostedReleaseTransitionTargetConfig(intent); err != nil {
		return err
	}
	return removeHostedReleaseTransitionFile(path)
}

// CancelHostedReleaseTransitionIntent is the distinct cleanup boundary for an
// exact setup proposal that orchestration proved closed and unmerged. It never
// removes a merged transition and never accepts promoted target policy.
func (s *Store) CancelHostedReleaseTransitionIntent() error {
	path := filepath.Join(s.dir, hostedReleaseTransitionFile)
	intent, exists, err := s.LoadHostedReleaseTransitionIntent()
	if err != nil {
		return err
	}
	if !exists {
		return durableRemoveStateFile(path)
	}
	if hostedTransitionPhaseIndex(intent.Phase) >= hostedTransitionPhaseIndex(HostedReleaseTransitionMerged) {
		return fmt.Errorf("merged hosted release transition cannot be cancelled")
	}
	if err := s.validateHostedReleaseTransitionActiveConfig(intent); err != nil {
		return err
	}
	return removeHostedReleaseTransitionFile(path)
}

func removeHostedReleaseTransitionFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return durableRemoveStateFile(path)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > hostedReleaseTransitionMax {
		return fmt.Errorf("hosted release transition is linked, non-regular, or exceeds %d bytes", hostedReleaseTransitionMax)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !sameFilesystemPath(resolved, path) {
		return fmt.Errorf("hosted release transition resolves through a link or reparse point")
	}
	return durableRemoveStateFile(path)
}

func (s *Store) validateHostedReleaseTransitionRepository(intent HostedReleaseTransitionIntent) error {
	config, err := s.Load()
	if err != nil {
		return fmt.Errorf("load installed policy for hosted release transition: %w", err)
	}
	if config.ExecutionMode != ExecutionHosted || !strings.EqualFold(config.Repository, intent.Repository) || config.RepositoryID != intent.RepositoryID || config.DefaultBranch != intent.DefaultBranch {
		return fmt.Errorf("hosted release transition does not match the installed hosted repository identity")
	}
	currentDigest, err := HostedReleaseTransitionConfigDigest(config)
	if err != nil {
		return err
	}
	currentRelease := hostedReleaseIdentity(config)
	active := equalHostedReleaseIdentity(currentRelease, intent.ActiveRelease) && currentDigest == intent.ActiveConfigDigest
	target := equalHostedReleaseIdentity(currentRelease, intent.TargetRelease) && currentDigest == intent.CandidateConfigDigest
	phase := hostedTransitionPhaseIndex(intent.Phase)
	if phase >= hostedTransitionPhaseIndex(HostedReleaseTransitionSetupPrepared) {
		if intent.CandidateConfig == nil {
			return fmt.Errorf("hosted release transition candidate Config is missing")
		}
		candidate, candidateErr := intent.CandidateConfig.Config()
		if candidateErr != nil {
			return candidateErr
		}
		if !sameFilesystemPath(candidate.StateDir, config.StateDir) || !sameFilesystemPath(candidate.CheckoutDir, config.CheckoutDir) ||
			!reflect.DeepEqual(candidate.ManagedPathPreimages, config.ManagedPathPreimages) || candidate.ManagedPreimagesVersion != config.ManagedPreimagesVersion {
			return fmt.Errorf("hosted release transition candidate changed state directories or the managed preimage ledger")
		}
		if target {
			target = samePromotedHostedTransitionConfig(config, candidate)
		}
	}
	allowTarget := phase >= hostedTransitionPhaseIndex(HostedReleaseTransitionPromoted)
	requireTarget := phase >= hostedTransitionPhaseIndex(HostedReleaseTransitionResumePrepared)
	if requireTarget && !target || !requireTarget && !active && (!allowTarget || !target) {
		return fmt.Errorf("hosted release transition no longer matches the active or verified target durable policy")
	}
	return nil
}

func (s *Store) validateHostedReleaseTransitionActiveConfig(intent HostedReleaseTransitionIntent) error {
	config, err := s.Load()
	if err != nil {
		return err
	}
	digest, err := HostedReleaseTransitionConfigDigest(config)
	if err != nil {
		return err
	}
	if !equalHostedReleaseIdentity(hostedReleaseIdentity(config), intent.ActiveRelease) || digest != intent.ActiveConfigDigest {
		return fmt.Errorf("hosted release transition cancellation requires the exact active durable policy")
	}
	return nil
}

func (s *Store) validateHostedReleaseTransitionTargetConfig(intent HostedReleaseTransitionIntent) error {
	config, err := s.Load()
	if err != nil {
		return err
	}
	digest, err := HostedReleaseTransitionConfigDigest(config)
	if err != nil {
		return err
	}
	if intent.CandidateConfig == nil {
		return fmt.Errorf("hosted release transition clear requires a sealed target Config")
	}
	candidate, candidateErr := intent.CandidateConfig.Config()
	if candidateErr != nil {
		return candidateErr
	}
	if !equalHostedReleaseIdentity(hostedReleaseIdentity(config), intent.TargetRelease) || digest != intent.CandidateConfigDigest || !samePromotedHostedTransitionConfig(config, candidate) {
		return fmt.Errorf("hosted release transition clear requires the exact promoted target durable policy")
	}
	return nil
}

func samePromotedHostedTransitionConfig(left, right Config) bool {
	left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}

func validateHostedReleaseTransitionIntent(intent HostedReleaseTransitionIntent) error {
	if intent.SchemaVersion != HostedReleaseTransitionSchema || !validHostedTransitionOperation(intent.Operation) || hostedTransitionPhaseIndex(intent.Phase) < 0 ||
		!validHostedTransitionRepository(intent.Repository) || !canonicalBoundedString(intent.RepositoryID, 256) || !validLegacyBranchName(intent.DefaultBranch) ||
		!canonicalHostedTransitionRelease(intent.ActiveRelease) || !canonicalHostedTransitionRelease(intent.TargetRelease) ||
		equalHostedReleaseIdentity(intent.ActiveRelease, intent.TargetRelease) || !validHostedTransitionDigest(intent.ActiveConfigDigest) ||
		!hostedTransitionRequestPattern.MatchString(intent.PreflightStatusRequestID) || intent.CreatedAt.IsZero() || intent.UpdatedAt.IsZero() ||
		intent.UpdatedAt.Before(intent.CreatedAt) || !validHostedTransitionDigest(intent.RecordSHA256) {
		return fmt.Errorf("hosted release transition is incomplete or invalid")
	}
	seal, err := hostedReleaseTransitionSeal(intent)
	if err != nil || seal != intent.RecordSHA256 {
		return fmt.Errorf("hosted release transition record digest is invalid")
	}

	phase := hostedTransitionPhaseIndex(intent.Phase)
	if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionPreflightVerified) {
		if !emptyHostedTransitionPreflightEvidence(intent) || intent.SourceWasPaused {
			return fmt.Errorf("prepared hosted release transition contains premature preflight evidence")
		}
	} else if !validHostedTransitionPreflightEvidence(intent) {
		return fmt.Errorf("hosted release transition preflight evidence is incomplete or invalid")
	}

	if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionPausePrepared) {
		if intent.PauseRequestID != "" || !emptyHostedTransitionPauseEvidence(intent) {
			return fmt.Errorf("hosted release transition contains premature pause evidence")
		}
	} else if !hostedTransitionRequestPattern.MatchString(intent.PauseRequestID) {
		return fmt.Errorf("hosted release transition pause request identity is incomplete or invalid")
	}
	if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionPauseVerified) {
		if !emptyHostedTransitionPauseEvidence(intent) {
			return fmt.Errorf("hosted release transition contains premature pause receipt evidence")
		}
	} else if !validHostedTransitionPauseEvidence(intent) {
		return fmt.Errorf("hosted release transition pause evidence is incomplete or invalid")
	}

	if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionSourceStatusPrepared) {
		if !emptyHostedTransitionSourceStatus(intent) {
			return fmt.Errorf("hosted release transition contains premature source status evidence")
		}
	} else {
		if !hostedTransitionRequestPattern.MatchString(intent.SourceStatusRequestID) {
			return fmt.Errorf("hosted release transition source status request is incomplete or invalid")
		}
		if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionSourceStatusVerified) {
			if !emptyHostedTransitionSourceStatusEvidence(intent) {
				return fmt.Errorf("hosted release transition contains premature source status receipt")
			}
		} else if !validHostedTransitionSourceStatusEvidence(intent) {
			return fmt.Errorf("hosted release transition source status evidence is incomplete, unpaused, or non-quiescent")
		}
	}

	if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionSetupPrepared) {
		if !emptyHostedTransitionSetup(intent) {
			return fmt.Errorf("hosted release transition contains premature setup evidence")
		}
	} else if err := validateHostedTransitionSetup(intent); err != nil {
		return err
	}

	if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionPullRecorded) {
		if !emptyHostedTransitionPull(intent) {
			return fmt.Errorf("hosted release transition contains premature pull evidence")
		}
	} else if !validHostedTransitionPull(intent) {
		return fmt.Errorf("hosted release transition pull evidence is incomplete or invalid")
	}

	if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionMerged) {
		if intent.MergeSHA != "" {
			return fmt.Errorf("hosted release transition contains premature merge evidence")
		}
	} else if !immutableCommit.MatchString(intent.MergeSHA) {
		return fmt.Errorf("hosted release transition merge evidence is incomplete or invalid")
	}

	if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionTargetCheckpointPrepared) {
		if !emptyHostedTransitionTarget(intent) {
			return fmt.Errorf("hosted release transition contains premature target checkpoint evidence")
		}
	} else {
		if !hostedTransitionRequestPattern.MatchString(intent.TargetRequestID) {
			return fmt.Errorf("hosted release transition target checkpoint request is incomplete or invalid")
		}
		if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionTargetCheckpointVerified) {
			if !emptyHostedTransitionTargetEvidence(intent) {
				return fmt.Errorf("hosted release transition contains premature target checkpoint receipt")
			}
		} else if !validHostedTransitionTargetEvidence(intent) {
			return fmt.Errorf("hosted release transition target checkpoint receipt is incomplete or invalid")
		}
	}

	if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionStatusPrepared) {
		if !emptyHostedTransitionStatus(intent) {
			return fmt.Errorf("hosted release transition contains premature status evidence")
		}
	} else {
		if !hostedTransitionRequestPattern.MatchString(intent.StatusRequestID) {
			return fmt.Errorf("hosted release transition status request is incomplete or invalid")
		}
		if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionTargetVerified) {
			if !emptyHostedTransitionStatusEvidence(intent) {
				return fmt.Errorf("hosted release transition contains premature target status receipt")
			}
		} else if !validHostedTransitionStatusEvidence(intent) {
			return fmt.Errorf("hosted release transition target status evidence is incomplete or invalid")
		}
	}

	if phase >= hostedTransitionPhaseIndex(HostedReleaseTransitionPreflightVerified) && intent.SourceWasPaused && (intent.Phase == HostedReleaseTransitionResumePrepared || intent.Phase == HostedReleaseTransitionResumeVerified) ||
		!intent.SourceWasPaused && intent.Phase == HostedReleaseTransitionPausePreserved {
		return fmt.Errorf("hosted release transition terminal pause behavior does not match its source pause state")
	}
	if intent.Phase == HostedReleaseTransitionPausePreserved {
		if !emptyHostedTransitionResume(intent) {
			return fmt.Errorf("pause-preserved hosted release transition contains resume evidence")
		}
	} else if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionResumePrepared) {
		if !emptyHostedTransitionResume(intent) {
			return fmt.Errorf("hosted release transition contains premature resume evidence")
		}
	} else {
		if !hostedTransitionRequestPattern.MatchString(intent.ResumeRequestID) {
			return fmt.Errorf("hosted release transition resume request is incomplete or invalid")
		}
		if phase < hostedTransitionPhaseIndex(HostedReleaseTransitionResumeVerified) {
			if !emptyHostedTransitionResumeEvidence(intent) {
				return fmt.Errorf("hosted release transition contains premature resume receipt")
			}
		} else if !validHostedTransitionResumeEvidence(intent) {
			return fmt.Errorf("hosted release transition resume receipt is incomplete or invalid")
		}
	}
	return nil
}

func validateHostedReleaseTransitionAdvance(current, next HostedReleaseTransitionIntent) error {
	currentIndex, nextIndex := hostedTransitionPhaseIndex(current.Phase), hostedTransitionPhaseIndex(next.Phase)
	validAdvance := nextIndex == currentIndex || nextIndex == currentIndex+1
	if current.Phase == HostedReleaseTransitionPromoted && current.SourceWasPaused {
		validAdvance = next.Phase == HostedReleaseTransitionPromoted || next.Phase == HostedReleaseTransitionPausePreserved
	}
	if current.Phase == HostedReleaseTransitionResumeVerified || current.Phase == HostedReleaseTransitionPausePreserved {
		validAdvance = next.Phase == current.Phase
	}
	if !validAdvance {
		return fmt.Errorf("hosted release transition cannot move from %q to %q", current.Phase, next.Phase)
	}
	if !sameHostedTransitionCore(current, next) {
		return fmt.Errorf("hosted release transition immutable identity changed")
	}
	if next.Phase == current.Phase {
		left, right := current, next
		left.UpdatedAt, right.UpdatedAt = time.Time{}, time.Time{}
		left.RecordSHA256, right.RecordSHA256 = "", ""
		if !reflect.DeepEqual(left, right) {
			return fmt.Errorf("hosted release transition phase %q cannot be rewritten with different evidence", current.Phase)
		}
		return nil
	}
	if currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionPreflightVerified) && !sameHostedTransitionPreflight(current, next) ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionPausePrepared) && current.PauseRequestID != next.PauseRequestID ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionPauseVerified) && !sameHostedTransitionPause(current, next) ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionSourceStatusPrepared) && current.SourceStatusRequestID != next.SourceStatusRequestID ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionSourceStatusVerified) && !sameHostedTransitionSourceStatus(current, next) ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionSetupPrepared) && !sameHostedTransitionSetup(current, next) ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionPullRecorded) && !sameHostedTransitionPull(current, next) ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionMerged) && current.MergeSHA != next.MergeSHA ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionTargetCheckpointPrepared) && current.TargetRequestID != next.TargetRequestID ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionTargetCheckpointVerified) && !sameHostedTransitionTargetEvidence(current, next) ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionStatusPrepared) && current.StatusRequestID != next.StatusRequestID ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionTargetVerified) && !sameHostedTransitionStatusEvidence(current, next) ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionResumePrepared) && current.ResumeRequestID != next.ResumeRequestID ||
		currentIndex >= hostedTransitionPhaseIndex(HostedReleaseTransitionResumeVerified) && !sameHostedTransitionResumeEvidence(current, next) {
		return fmt.Errorf("hosted release transition prior phase evidence changed")
	}
	return nil
}

func hostedReleaseTransitionSeal(intent HostedReleaseTransitionIntent) (string, error) {
	intent.RecordSHA256 = ""
	data, err := json.Marshal(persistedHostedReleaseTransitionIntent(intent))
	if err != nil {
		return "", fmt.Errorf("seal hosted release transition: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func normalizeHostedReleaseTransitionIntent(intent HostedReleaseTransitionIntent) HostedReleaseTransitionIntent {
	intent.Repository, intent.RepositoryID, intent.DefaultBranch = strings.TrimSpace(intent.Repository), strings.TrimSpace(intent.RepositoryID), strings.TrimSpace(intent.DefaultBranch)
	intent.ActiveRelease = normalizeHostedTransitionRelease(intent.ActiveRelease)
	intent.TargetRelease = normalizeHostedTransitionRelease(intent.TargetRelease)
	intent.ActiveConfigDigest = strings.ToLower(strings.TrimSpace(intent.ActiveConfigDigest))
	intent.CandidateConfigDigest = strings.ToLower(strings.TrimSpace(intent.CandidateConfigDigest))
	intent.PreflightStatusRequestID, intent.PreflightArtifactName = strings.TrimSpace(intent.PreflightStatusRequestID), strings.TrimSpace(intent.PreflightArtifactName)
	intent.PreflightDefaultHeadSHA, intent.PreflightStateHeadSHA = strings.ToLower(strings.TrimSpace(intent.PreflightDefaultHeadSHA)), strings.ToLower(strings.TrimSpace(intent.PreflightStateHeadSHA))
	intent.PauseRequestID, intent.PauseArtifactName = strings.TrimSpace(intent.PauseRequestID), strings.TrimSpace(intent.PauseArtifactName)
	intent.PauseDefaultHeadSHA, intent.PauseStateHeadSHA = strings.ToLower(strings.TrimSpace(intent.PauseDefaultHeadSHA)), strings.ToLower(strings.TrimSpace(intent.PauseStateHeadSHA))
	intent.SourceStatusRequestID, intent.SourceStatusArtifactName = strings.TrimSpace(intent.SourceStatusRequestID), strings.TrimSpace(intent.SourceStatusArtifactName)
	intent.SourceStatusDefaultHeadSHA, intent.SourceStatusStateHeadSHA = strings.ToLower(strings.TrimSpace(intent.SourceStatusDefaultHeadSHA)), strings.ToLower(strings.TrimSpace(intent.SourceStatusStateHeadSHA))
	intent.SetupBranch, intent.SetupPRURL, intent.SetupMarker = strings.TrimSpace(intent.SetupBranch), strings.TrimSpace(intent.SetupPRURL), strings.TrimSpace(intent.SetupMarker)
	intent.SetupCommitSHA, intent.SetupBaseSHA, intent.SetupDiffDigest = strings.ToLower(strings.TrimSpace(intent.SetupCommitSHA)), strings.ToLower(strings.TrimSpace(intent.SetupBaseSHA)), strings.ToLower(strings.TrimSpace(intent.SetupDiffDigest))
	intent.SetupCandidateDigest, intent.MergeSHA = strings.ToLower(strings.TrimSpace(intent.SetupCandidateDigest)), strings.ToLower(strings.TrimSpace(intent.MergeSHA))
	intent.SetupAuthorizationContext = strings.TrimSpace(intent.SetupAuthorizationContext)
	intent.TargetRequestID, intent.TargetArtifactName = strings.TrimSpace(intent.TargetRequestID), strings.TrimSpace(intent.TargetArtifactName)
	intent.TargetDefaultHeadSHA, intent.TargetStateHeadSHA = strings.ToLower(strings.TrimSpace(intent.TargetDefaultHeadSHA)), strings.ToLower(strings.TrimSpace(intent.TargetStateHeadSHA))
	intent.StatusRequestID, intent.StatusArtifactName = strings.TrimSpace(intent.StatusRequestID), strings.TrimSpace(intent.StatusArtifactName)
	intent.StatusDefaultHeadSHA, intent.StatusStateHeadSHA = strings.ToLower(strings.TrimSpace(intent.StatusDefaultHeadSHA)), strings.ToLower(strings.TrimSpace(intent.StatusStateHeadSHA))
	intent.ResumeRequestID, intent.ResumeArtifactName = strings.TrimSpace(intent.ResumeRequestID), strings.TrimSpace(intent.ResumeArtifactName)
	intent.ResumeDefaultHeadSHA, intent.ResumeStateHeadSHA = strings.ToLower(strings.TrimSpace(intent.ResumeDefaultHeadSHA)), strings.ToLower(strings.TrimSpace(intent.ResumeStateHeadSHA))
	intent.CreatedAt, intent.UpdatedAt = intent.CreatedAt.UTC(), intent.UpdatedAt.UTC()
	return intent
}

func normalizeHostedTransitionRelease(identity HostedReleaseIdentity) HostedReleaseIdentity {
	identity.Version = strings.TrimSpace(identity.Version)
	identity.HiveCommit = strings.ToLower(strings.TrimSpace(identity.HiveCommit))
	identity.VisualHiveCommit = strings.ToLower(strings.TrimSpace(identity.VisualHiveCommit))
	identity.DistributionManifestSHA256 = strings.ToLower(strings.TrimSpace(identity.DistributionManifestSHA256))
	return identity
}

func canonicalHostedTransitionRelease(identity HostedReleaseIdentity) bool {
	return reflect.DeepEqual(identity, normalizeHostedTransitionRelease(identity)) && validHostedReleaseIdentity(identity)
}

func validHostedTransitionOperation(operation HostedReleaseTransitionOperation) bool {
	return operation == HostedReleaseTransitionUpgrade || operation == HostedReleaseTransitionRollback
}

// HostedReleaseTransitionSetupMarker is the exact operation- and
// release-specific pull-request ownership marker retained by the intent.
func HostedReleaseTransitionSetupMarker(operation HostedReleaseTransitionOperation, repositoryID string, active, target HostedReleaseIdentity) string {
	active, target = normalizeHostedTransitionRelease(active), normalizeHostedTransitionRelease(target)
	material := strings.Join([]string{
		string(operation), strings.TrimSpace(repositoryID),
		active.Version, active.HiveCommit, active.VisualHiveCommit, active.DistributionManifestSHA256, strconv.Itoa(active.HostedControllerProtocol),
		target.Version, target.HiveCommit, target.VisualHiveCommit, target.DistributionManifestSHA256, strconv.Itoa(target.HostedControllerProtocol),
	}, "\x00")
	digest := sha256.Sum256([]byte(material))
	return fmt.Sprintf("<!-- hive-hosted-release-transition: %s:%s:%s -->", operation, strings.TrimSpace(repositoryID), hex.EncodeToString(digest[:]))
}

func hostedTransitionPhaseIndex(phase HostedReleaseTransitionPhase) int {
	for index, candidate := range []HostedReleaseTransitionPhase{
		HostedReleaseTransitionPrepared,
		HostedReleaseTransitionPreflightVerified,
		HostedReleaseTransitionPausePrepared,
		HostedReleaseTransitionPauseVerified,
		HostedReleaseTransitionSourceStatusPrepared,
		HostedReleaseTransitionSourceStatusVerified,
		HostedReleaseTransitionSetupPrepared,
		HostedReleaseTransitionPullRecorded,
		HostedReleaseTransitionMerged,
		HostedReleaseTransitionTargetCheckpointPrepared,
		HostedReleaseTransitionTargetCheckpointVerified,
		HostedReleaseTransitionStatusPrepared,
		HostedReleaseTransitionTargetVerified,
		HostedReleaseTransitionPromoted,
		HostedReleaseTransitionResumePrepared,
		HostedReleaseTransitionResumeVerified,
		HostedReleaseTransitionPausePreserved,
	} {
		if phase == candidate {
			return index
		}
	}
	return -1
}

func validHostedTransitionDigest(value string) bool {
	return hostedDigestPattern.MatchString(value)
}

func canonicalBoundedString(value string, maximum int) bool {
	return value == strings.TrimSpace(value) && value != "" && len(value) <= maximum && !strings.ContainsAny(value, "\x00\r\n")
}

func validHostedTransitionRepository(value string) bool {
	owner, repository, ok := strings.Cut(value, "/")
	return ok && !strings.Contains(repository, "/") && canonicalBoundedString(owner, 128) && canonicalBoundedString(repository, 128)
}

func validHostedTransitionPreflightEvidence(intent HostedReleaseTransitionIntent) bool {
	return intent.PreflightRunID > 0 && intent.PreflightArtifactID > 0 &&
		hostedTransitionArtifactPattern.MatchString(intent.PreflightArtifactName) && immutableCommit.MatchString(intent.PreflightDefaultHeadSHA) &&
		immutableCommit.MatchString(intent.PreflightStateHeadSHA)
}

func emptyHostedTransitionPreflightEvidence(intent HostedReleaseTransitionIntent) bool {
	return intent.PreflightRunID == 0 && intent.PreflightArtifactID == 0 && intent.PreflightArtifactName == "" && intent.PreflightDefaultHeadSHA == "" && intent.PreflightStateHeadSHA == ""
}

func validHostedTransitionPauseEvidence(intent HostedReleaseTransitionIntent) bool {
	return intent.PauseRunID > 0 && intent.PauseArtifactID > 0 &&
		hostedTransitionArtifactPattern.MatchString(intent.PauseArtifactName) && immutableCommit.MatchString(intent.PauseDefaultHeadSHA) &&
		immutableCommit.MatchString(intent.PauseStateHeadSHA)
}

func emptyHostedTransitionPauseEvidence(intent HostedReleaseTransitionIntent) bool {
	return intent.PauseRunID == 0 && intent.PauseArtifactID == 0 && intent.PauseArtifactName == "" && intent.PauseDefaultHeadSHA == "" && intent.PauseStateHeadSHA == ""
}

func validHostedTransitionSourceStatusEvidence(intent HostedReleaseTransitionIntent) bool {
	return intent.SourceStatusRunID > 0 && intent.SourceStatusArtifactID > 0 &&
		hostedTransitionArtifactPattern.MatchString(intent.SourceStatusArtifactName) && immutableCommit.MatchString(intent.SourceStatusDefaultHeadSHA) &&
		immutableCommit.MatchString(intent.SourceStatusStateHeadSHA) && intent.SourceStatusPaused && intent.SourceStatusQuiescent
}

func emptyHostedTransitionSourceStatus(intent HostedReleaseTransitionIntent) bool {
	return intent.SourceStatusRequestID == "" && emptyHostedTransitionSourceStatusEvidence(intent)
}

func emptyHostedTransitionSourceStatusEvidence(intent HostedReleaseTransitionIntent) bool {
	return intent.SourceStatusRunID == 0 && intent.SourceStatusArtifactID == 0 && intent.SourceStatusArtifactName == "" &&
		intent.SourceStatusDefaultHeadSHA == "" && intent.SourceStatusStateHeadSHA == "" && !intent.SourceStatusPaused && !intent.SourceStatusQuiescent
}

func validateHostedTransitionSetup(intent HostedReleaseTransitionIntent) error {
	if intent.SetupBranch != managedOperationBranch(string(intent.Operation), intent.RepositoryID) || !immutableCommit.MatchString(intent.SetupCommitSHA) ||
		!immutableCommit.MatchString(intent.SetupBaseSHA) || intent.SetupBaseSHA != intent.SourceStatusDefaultHeadSHA || !validHostedTransitionDigest(intent.SetupDiffDigest) ||
		!validHostedTransitionDigest(intent.SetupCandidateDigest) || intent.SetupCandidateDigest != intent.CandidateConfigDigest ||
		intent.SetupMarker != HostedReleaseTransitionSetupMarker(intent.Operation, intent.RepositoryID, intent.ActiveRelease, intent.TargetRelease) {
		return fmt.Errorf("hosted release transition setup evidence is incomplete or invalid")
	}
	if intent.CandidateConfig == nil {
		return fmt.Errorf("hosted release transition candidate Config is missing")
	}
	candidate, err := intent.CandidateConfig.Config()
	if err != nil {
		return err
	}
	if err := validateHostedTransitionCandidateConfig(intent, candidate); err != nil {
		return err
	}
	managed := managedSetupFilesForConfig(candidate)
	if len(intent.SetupChangedPaths) == 0 || len(intent.SetupChangedPaths) > len(managed) || !exactPathOrder(intent.SetupChangedPaths, sortedUniquePaths(intent.SetupChangedPaths)) {
		return fmt.Errorf("hosted release transition changed-path binding is empty, duplicate, unsorted, or unbounded")
	}
	allowed := make(map[string]bool, len(managed))
	for _, relative := range managed {
		allowed[relative] = true
	}
	for _, relative := range intent.SetupChangedPaths {
		if !allowed[relative] {
			return fmt.Errorf("hosted release transition changed-path binding contains unmanaged path %s", relative)
		}
	}
	return nil
}

func validateHostedTransitionCandidateConfig(intent HostedReleaseTransitionIntent, candidate Config) error {
	if candidate.SchemaVersion != ConfigSchema || candidate.Repository != intent.Repository || candidate.RepositoryID != intent.RepositoryID ||
		candidate.DefaultBranch != intent.DefaultBranch || candidate.ExecutionMode != ExecutionHosted ||
		!equalHostedReleaseIdentity(hostedReleaseIdentity(candidate), intent.TargetRelease) || candidate.PreviousHostedRelease == nil ||
		!equalHostedReleaseIdentity(*candidate.PreviousHostedRelease, intent.ActiveRelease) || candidate.SetupBranch != intent.SetupBranch ||
		candidate.SetupHeadSHA != intent.SetupCommitSHA || candidate.SetupPRNumber != 0 || candidate.SetupPRURL != "" ||
		!filepath.IsAbs(candidate.StateDir) || !filepath.IsAbs(candidate.CheckoutDir) || !sameFilesystemPath(filepath.Clean(candidate.StateDir), candidate.StateDir) ||
		!sameFilesystemPath(filepath.Clean(candidate.CheckoutDir), candidate.CheckoutDir) {
		return fmt.Errorf("hosted release transition candidate Config identity is incomplete or invalid")
	}
	if err := validateManagedPathPreimages(candidate.ManagedPreimagesVersion, candidate.ManagedPathPreimages, managedSetupFilesForConfig(candidate)); err != nil {
		return fmt.Errorf("hosted release transition candidate managed preimage ledger is invalid: %w", err)
	}
	digest, err := HostedReleaseTransitionConfigDigest(candidate)
	if err != nil || digest != intent.CandidateConfigDigest || digest == intent.ActiveConfigDigest {
		return fmt.Errorf("hosted release transition candidate Config digest is invalid")
	}
	controller, err := GenerateHostedControllerWorkflow(hostedWorkflowConfig(candidate))
	if err != nil {
		return fmt.Errorf("generate exact candidate hosted workflow: %w", err)
	}
	workflowDigest := sha256.Sum256([]byte(controller))
	if hex.EncodeToString(workflowDigest[:]) != candidate.HostedWorkflowSHA256 {
		return fmt.Errorf("hosted release transition candidate workflow digest is invalid")
	}
	return nil
}

func emptyHostedTransitionSetup(intent HostedReleaseTransitionIntent) bool {
	return intent.SetupBranch == "" && intent.SetupCommitSHA == "" && intent.SetupBaseSHA == "" && intent.SetupDiffDigest == "" && intent.SetupCandidateDigest == "" &&
		intent.SetupMarker == "" && len(intent.SetupChangedPaths) == 0 && intent.CandidateConfig == nil && intent.CandidateConfigDigest == "" &&
		emptyHostedTransitionPull(intent) && intent.MergeSHA == ""
}

func validHostedTransitionPull(intent HostedReleaseTransitionIntent) bool {
	if intent.SetupPRNumber <= 0 || len(intent.SetupPRURL) > 2048 || !validSetupAuthorizationContext(intent.SetupAuthorizationContext) ||
		intent.SetupAuthorizationStatusID <= 0 || intent.SetupAuthorizationCreatorID <= 0 {
		return false
	}
	parsed, err := url.Parse(intent.SetupPRURL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	wantPath := "/" + intent.Repository + "/pull/" + strconv.Itoa(intent.SetupPRNumber)
	if !strings.EqualFold(strings.TrimSuffix(parsed.EscapedPath(), "/"), wantPath) {
		return false
	}
	if intent.CandidateConfig == nil {
		return false
	}
	candidate, err := intent.CandidateConfig.Config()
	writer, ok := setupAuthorizationWriter(candidate)
	return err == nil && candidate.SetupAuthorizationActorID > 0 && ok && intent.SetupAuthorizationCreatorID == writer.ID
}

func emptyHostedTransitionPull(intent HostedReleaseTransitionIntent) bool {
	return intent.SetupPRNumber == 0 && intent.SetupPRURL == "" && intent.SetupAuthorizationContext == "" &&
		intent.SetupAuthorizationStatusID == 0 && intent.SetupAuthorizationCreatorID == 0
}

func validHostedTransitionTargetEvidence(intent HostedReleaseTransitionIntent) bool {
	return intent.TargetRunID > 0 && intent.TargetArtifactID > 0 &&
		hostedTransitionArtifactPattern.MatchString(intent.TargetArtifactName) && immutableCommit.MatchString(intent.TargetDefaultHeadSHA) &&
		immutableCommit.MatchString(intent.TargetStateHeadSHA) && intent.TargetPaused
}

func emptyHostedTransitionTarget(intent HostedReleaseTransitionIntent) bool {
	return intent.TargetRequestID == "" && emptyHostedTransitionTargetEvidence(intent)
}

func emptyHostedTransitionTargetEvidence(intent HostedReleaseTransitionIntent) bool {
	return intent.TargetRunID == 0 && intent.TargetArtifactID == 0 && intent.TargetArtifactName == "" && intent.TargetDefaultHeadSHA == "" && intent.TargetStateHeadSHA == "" && !intent.TargetPaused
}

func validHostedTransitionStatusEvidence(intent HostedReleaseTransitionIntent) bool {
	return intent.StatusRunID > 0 && intent.StatusArtifactID > 0 &&
		hostedTransitionArtifactPattern.MatchString(intent.StatusArtifactName) && immutableCommit.MatchString(intent.StatusDefaultHeadSHA) &&
		immutableCommit.MatchString(intent.StatusStateHeadSHA) && intent.StatusPaused && intent.StatusQuiescent
}

func emptyHostedTransitionStatus(intent HostedReleaseTransitionIntent) bool {
	return intent.StatusRequestID == "" && emptyHostedTransitionStatusEvidence(intent)

}

func emptyHostedTransitionStatusEvidence(intent HostedReleaseTransitionIntent) bool {
	return intent.StatusRunID == 0 && intent.StatusArtifactID == 0 && intent.StatusArtifactName == "" && intent.StatusDefaultHeadSHA == "" && intent.StatusStateHeadSHA == "" && !intent.StatusPaused && !intent.StatusQuiescent
}

func validHostedTransitionResumeEvidence(intent HostedReleaseTransitionIntent) bool {
	return intent.ResumeRunID > 0 && intent.ResumeArtifactID > 0 &&
		hostedTransitionArtifactPattern.MatchString(intent.ResumeArtifactName) && immutableCommit.MatchString(intent.ResumeDefaultHeadSHA) &&
		immutableCommit.MatchString(intent.ResumeStateHeadSHA) && intent.ResumeRunning
}

func emptyHostedTransitionResume(intent HostedReleaseTransitionIntent) bool {
	return intent.ResumeRequestID == "" && emptyHostedTransitionResumeEvidence(intent)
}

func emptyHostedTransitionResumeEvidence(intent HostedReleaseTransitionIntent) bool {
	return intent.ResumeRunID == 0 && intent.ResumeArtifactID == 0 && intent.ResumeArtifactName == "" && intent.ResumeDefaultHeadSHA == "" && intent.ResumeStateHeadSHA == "" && !intent.ResumeRunning
}

func sameHostedTransitionCore(left, right HostedReleaseTransitionIntent) bool {
	return left.SchemaVersion == right.SchemaVersion && left.Operation == right.Operation && left.Repository == right.Repository && left.RepositoryID == right.RepositoryID &&
		left.DefaultBranch == right.DefaultBranch && equalHostedReleaseIdentity(left.ActiveRelease, right.ActiveRelease) && equalHostedReleaseIdentity(left.TargetRelease, right.TargetRelease) &&
		left.ActiveConfigDigest == right.ActiveConfigDigest && left.PreflightStatusRequestID == right.PreflightStatusRequestID && left.CreatedAt.Equal(right.CreatedAt)
}

func sameHostedTransitionPreflight(left, right HostedReleaseTransitionIntent) bool {
	return left.PreflightRunID == right.PreflightRunID && left.PreflightArtifactID == right.PreflightArtifactID && left.PreflightArtifactName == right.PreflightArtifactName &&
		left.PreflightDefaultHeadSHA == right.PreflightDefaultHeadSHA && left.PreflightStateHeadSHA == right.PreflightStateHeadSHA && left.SourceWasPaused == right.SourceWasPaused
}

func sameHostedTransitionPause(left, right HostedReleaseTransitionIntent) bool {
	return left.PauseRequestID == right.PauseRequestID && left.PauseRunID == right.PauseRunID && left.PauseArtifactID == right.PauseArtifactID &&
		left.PauseArtifactName == right.PauseArtifactName && left.PauseDefaultHeadSHA == right.PauseDefaultHeadSHA && left.PauseStateHeadSHA == right.PauseStateHeadSHA
}

func sameHostedTransitionSetup(left, right HostedReleaseTransitionIntent) bool {
	return left.SetupBranch == right.SetupBranch && left.SetupCommitSHA == right.SetupCommitSHA && left.SetupBaseSHA == right.SetupBaseSHA &&
		left.SetupDiffDigest == right.SetupDiffDigest && left.SetupCandidateDigest == right.SetupCandidateDigest && left.SetupMarker == right.SetupMarker &&
		exactPathOrder(left.SetupChangedPaths, right.SetupChangedPaths) && left.CandidateConfigDigest == right.CandidateConfigDigest && reflect.DeepEqual(left.CandidateConfig, right.CandidateConfig)
}

func sameHostedTransitionSourceStatus(left, right HostedReleaseTransitionIntent) bool {
	return left.SourceStatusRunID == right.SourceStatusRunID && left.SourceStatusArtifactID == right.SourceStatusArtifactID && left.SourceStatusArtifactName == right.SourceStatusArtifactName &&
		left.SourceStatusDefaultHeadSHA == right.SourceStatusDefaultHeadSHA && left.SourceStatusStateHeadSHA == right.SourceStatusStateHeadSHA &&
		left.SourceStatusPaused == right.SourceStatusPaused && left.SourceStatusQuiescent == right.SourceStatusQuiescent
}

func sameHostedTransitionPull(left, right HostedReleaseTransitionIntent) bool {
	return left.SetupPRNumber == right.SetupPRNumber && left.SetupPRURL == right.SetupPRURL && left.SetupAuthorizationContext == right.SetupAuthorizationContext &&
		left.SetupAuthorizationStatusID == right.SetupAuthorizationStatusID && left.SetupAuthorizationCreatorID == right.SetupAuthorizationCreatorID
}

func sameHostedTransitionTargetEvidence(left, right HostedReleaseTransitionIntent) bool {
	return left.TargetRunID == right.TargetRunID && left.TargetArtifactID == right.TargetArtifactID && left.TargetArtifactName == right.TargetArtifactName &&
		left.TargetDefaultHeadSHA == right.TargetDefaultHeadSHA && left.TargetStateHeadSHA == right.TargetStateHeadSHA && left.TargetPaused == right.TargetPaused
}

func sameHostedTransitionStatusEvidence(left, right HostedReleaseTransitionIntent) bool {
	return left.StatusRunID == right.StatusRunID && left.StatusArtifactID == right.StatusArtifactID && left.StatusArtifactName == right.StatusArtifactName &&
		left.StatusDefaultHeadSHA == right.StatusDefaultHeadSHA && left.StatusStateHeadSHA == right.StatusStateHeadSHA &&
		left.StatusPaused == right.StatusPaused && left.StatusQuiescent == right.StatusQuiescent
}

func sameHostedTransitionResumeEvidence(left, right HostedReleaseTransitionIntent) bool {
	return left.ResumeRunID == right.ResumeRunID && left.ResumeArtifactID == right.ResumeArtifactID && left.ResumeArtifactName == right.ResumeArtifactName &&
		left.ResumeDefaultHeadSHA == right.ResumeDefaultHeadSHA && left.ResumeStateHeadSHA == right.ResumeStateHeadSHA && left.ResumeRunning == right.ResumeRunning
}

func rejectLinkedHostedTransitionDestination(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > hostedReleaseTransitionMax {
		return fmt.Errorf("hosted release transition destination is linked, non-regular, or exceeds %d bytes", hostedReleaseTransitionMax)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !sameFilesystemPath(resolved, path) {
		return fmt.Errorf("hosted release transition destination resolves through a link or reparse point")
	}
	return nil
}
