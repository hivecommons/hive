package visualhive

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/internal/visualhivepr"
)

const (
	LifecycleSchemaV1 = "hive.visual-hive-lifecycle.v1"
	LifecycleSchema   = "hive.visual-hive-lifecycle.v2"
)

type LifecycleStatus string

const (
	StatusDetected           LifecycleStatus = "detected"
	StatusIssueOpen          LifecycleStatus = "issue_open"
	StatusFixQueued          LifecycleStatus = "fix_queued"
	StatusRepairRunning      LifecycleStatus = "repair_running"
	StatusPROpen             LifecycleStatus = "pr_open"
	StatusChecksRunning      LifecycleStatus = "checks_running"
	StatusNeedsRevision      LifecycleStatus = "needs_revision"
	StatusReady              LifecycleStatus = "ready"
	StatusMerged             LifecycleStatus = "merged"
	StatusPostMergeVerifying LifecycleStatus = "post_merge_verifying"
	StatusResolved           LifecycleStatus = "resolved"
	StatusIssueClosed        LifecycleStatus = "issue_closed"
)

type OutboxAction string

const (
	OutboxOpenIssue   OutboxAction = "open_issue"
	OutboxUpdateIssue OutboxAction = "update_issue"
	OutboxReopenIssue OutboxAction = "reopen_issue"
	OutboxCloseIssue  OutboxAction = "close_issue"
)

type FindingLifecycle struct {
	Repository                     string                           `json:"repository"`
	RepositoryID                   string                           `json:"repository_id,omitempty"`
	Fingerprint                    string                           `json:"fingerprint"`
	RepositoryFingerprint          string                           `json:"repository_fingerprint"`
	PublicationRole                string                           `json:"publication_role,omitempty"`
	RootCauseKey                   string                           `json:"root_cause_key,omitempty"`
	BlockedByRootKeys              []string                         `json:"blocked_by_root_keys,omitempty"`
	PublicationFingerprint         string                           `json:"publication_fingerprint,omitempty"`
	PendingMarkerMigrationFrom     string                           `json:"pending_marker_migration_from,omitempty"`
	Status                         LifecycleStatus                  `json:"status"`
	IssueKind                      string                           `json:"issue_kind"`
	Severity                       string                           `json:"severity"`
	OwningAgentHint                string                           `json:"owning_agent_hint"`
	Title                          string                           `json:"title"`
	Body                           string                           `json:"body"`
	Labels                         []string                         `json:"labels"`
	AffectedContracts              []string                         `json:"affected_contracts"`
	ValidationCommand              string                           `json:"validation_command"`
	HumanReviewRequired            bool                             `json:"human_review_required"`
	ObservationHumanReviewRequired bool                             `json:"observation_human_review_required"`
	ManualReviewKind               string                           `json:"manual_review_kind,omitempty"`
	ManualReviewReason             string                           `json:"manual_review_reason,omitempty"`
	BeadID                         string                           `json:"bead_id,omitempty"`
	IssueNumber                    int                              `json:"issue_number,omitempty"`
	IssueURL                       string                           `json:"issue_url,omitempty"`
	IssueWriterID                  int64                            `json:"issue_writer_id,omitempty"`
	IssueWriterLogin               string                           `json:"issue_writer_login,omitempty"`
	Branch                         string                           `json:"branch,omitempty"`
	RepairCommitSHA                string                           `json:"repair_commit_sha,omitempty"`
	PRNumber                       int                              `json:"pr_number,omitempty"`
	PRURL                          string                           `json:"pr_url,omitempty"`
	MergeSHA                       string                           `json:"merge_sha,omitempty"`
	ValidationRunID                string                           `json:"validation_run_id,omitempty"`
	ValidationRunURL               string                           `json:"validation_run_url,omitempty"`
	LastCheckSummary               string                           `json:"last_check_summary,omitempty"`
	LastCheckRuns                  []CheckEvidence                  `json:"last_check_runs,omitempty"`
	LastPullRequestCheckReceipt    *PullRequestCheckReceiptSnapshot `json:"last_pull_request_check_receipt,omitempty"`
	FirstSeenAt                    time.Time                        `json:"first_seen_at"`
	LastSeenAt                     time.Time                        `json:"last_seen_at"`
	ResolvedAt                     *time.Time                       `json:"resolved_at,omitempty"`
	ClosedAt                       *time.Time                       `json:"closed_at,omitempty"`
	LastBundleID                   string                           `json:"last_bundle_id"`
	LastBundleDigest               string                           `json:"last_bundle_digest"`
	LastWorkflowRunID              string                           `json:"last_workflow_run_id,omitempty"`
	RepairAttempts                 int                              `json:"repair_attempts"`
	Recurrences                    int                              `json:"recurrences"`
	PendingIssueAction             OutboxAction                     `json:"pending_issue_action,omitempty"`
}

type CheckEvidence struct {
	Name                     string `json:"name"`
	State                    string `json:"state"`
	URL                      string `json:"url,omitempty"`
	ProvenanceVerified       bool   `json:"provenance_verified,omitempty"`
	WorkflowID               int64  `json:"workflow_id,omitempty"`
	WorkflowRunID            int64  `json:"workflow_run_id,omitempty"`
	WorkflowRunAttempt       int    `json:"workflow_run_attempt,omitempty"`
	WorkflowPath             string `json:"workflow_path,omitempty"`
	WorkflowEvent            string `json:"workflow_event,omitempty"`
	WorkflowDefinitionSHA256 string `json:"workflow_definition_sha256,omitempty"`
	PullRequestNumber        int    `json:"pull_request_number,omitempty"`
	CommitSHA                string `json:"commit_sha,omitempty"`
	SourceArtifactID         int64  `json:"source_artifact_id,omitempty"`
	SourceArtifactName       string `json:"source_artifact_name,omitempty"`
	BundleArtifactID         int64  `json:"bundle_artifact_id,omitempty"`
	BundleArtifactName       string `json:"bundle_artifact_name,omitempty"`
	ReceiptReplayKey         string `json:"receipt_replay_key,omitempty"`
	ReceiptSHA256            string `json:"receipt_sha256,omitempty"`
}

type OutboxEntry struct {
	ID                    string            `json:"id"`
	Action                OutboxAction      `json:"action"`
	Repository            string            `json:"repository"`
	RepositoryFingerprint string            `json:"repository_fingerprint"`
	BundleID              string            `json:"bundle_id"`
	BundleDigest          string            `json:"bundle_digest"`
	IssueNumber           int               `json:"issue_number,omitempty"`
	Title                 string            `json:"title,omitempty"`
	Body                  string            `json:"body,omitempty"`
	Labels                []string          `json:"labels,omitempty"`
	Evidence              map[string]string `json:"evidence,omitempty"`
	PreviousMarker        string            `json:"previous_marker,omitempty"`
	Attempts              int               `json:"attempts"`
	LastError             string            `json:"last_error,omitempty"`
	CreatedAt             time.Time         `json:"created_at"`
	CompletedAt           *time.Time        `json:"completed_at,omitempty"`
}

type LifecycleState struct {
	SchemaVersion string                       `json:"schema_version"`
	UpdatedAt     time.Time                    `json:"updated_at"`
	Findings      map[string]*FindingLifecycle `json:"findings"`
	ReplayKeys    map[string]string            `json:"replay_keys"`
	Outbox        []*OutboxEntry               `json:"outbox"`
	LastImport    *VisualImportRunReceipt      `json:"last_import,omitempty"`
}

type VisualImportRunReceipt struct {
	PacketID            string    `json:"packet_id"`
	PacketDigest        string    `json:"packet_digest"`
	ReplayKey           string    `json:"replay_key"`
	Repository          string    `json:"repository"`
	RepositoryID        string    `json:"repository_id"`
	BaseSHA             string    `json:"base_sha"`
	WorkflowName        string    `json:"workflow_name"`
	WorkflowEvent       string    `json:"workflow_event"`
	WorkflowRunID       string    `json:"workflow_run_id"`
	WorkflowRunAttempt  string    `json:"workflow_run_attempt"`
	WorkflowArtifactID  string    `json:"workflow_artifact_id"`
	ProducerName        string    `json:"producer_name"`
	ProducerGitCommit   string    `json:"producer_git_commit"`
	ArtifactIndexSHA256 string    `json:"artifact_index_sha256,omitempty"`
	Verdict             string    `json:"verdict"`
	Observations        int       `json:"observations"`
	ImportedAt          time.Time `json:"imported_at"`
}

type LifecycleAuditEntry struct {
	ReceiptID             string    `json:"receipt_id"`
	Timestamp             time.Time `json:"timestamp"`
	Action                string    `json:"action"`
	Allowed               bool      `json:"allowed"`
	Repository            string    `json:"repository,omitempty"`
	RepositoryFingerprint string    `json:"repository_fingerprint,omitempty"`
	BundleID              string    `json:"bundle_id,omitempty"`
	Detail                string    `json:"detail,omitempty"`
}

type ApplyLifecycleOptions struct {
	TargetRef                        string
	CurrentTargetCommitSHA           string
	VerificationRunID                string
	VerificationURL                  string
	VerificationCommitSHA            string
	VerifiedMergeAncestorFingerprint string
	VerifiedMergeAncestorSHA         string
	MaxActiveIssues                  int
	PreferRepairable                 bool
	DisableIssuePublication          bool
}

type ApplyLifecycleResult struct {
	BundleID      string   `json:"bundle_id"`
	Idempotent    bool     `json:"idempotent"`
	Created       int      `json:"created"`
	Updated       int      `json:"updated"`
	Resolved      int      `json:"resolved"`
	Reopened      int      `json:"reopened"`
	IgnoredAbsent int      `json:"ignored_absent"`
	OutboxCreated int      `json:"outbox_created"`
	BeadsCreated  int      `json:"beads_created"`
	BeadsSkipped  int      `json:"beads_skipped"`
	Deferred      int      `json:"deferred"`
	FindingIDs    []string `json:"finding_ids"`
}

type ApplyPullRequestCheckUpdateResult struct {
	ReceiptSHA256 string `json:"receipt_sha256"`
	ReplayKey     string `json:"replay_key"`
	Idempotent    bool   `json:"idempotent"`
}

type LifecycleStore struct {
	mu            sync.Mutex
	dir           string
	statePath     string
	auditPath     string
	state         LifecycleState
	auditReceipts map[string]bool
}

// LifecycleBeadSink is the narrow existing-bead-store surface used while
// applying verified Visual Hive observations. A single *beads.Store remains
// the compatibility adapter; the normal service supplies a role-aware router
// backed by its already-open per-role stores.
type LifecycleBeadSink interface {
	ImportBatch([]beads.BatchInput) (beads.BatchResult, error)
	FindByExternalRef(string) *beads.Bead
	Update(string, func(*beads.Bead)) error
	Close(string) error
}

func NewLifecycleStore(dir string) (*LifecycleStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("lifecycle store directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create lifecycle store: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("protect lifecycle store: %w", err)
	}
	store := &LifecycleStore{
		dir:       dir,
		statePath: filepath.Join(dir, "visual-hive-lifecycle.json"),
		auditPath: filepath.Join(dir, "visual-hive-lifecycle-audit.jsonl"),
		state: LifecycleState{
			SchemaVersion: LifecycleSchema,
			Findings:      map[string]*FindingLifecycle{},
			ReplayKeys:    map[string]string{},
			Outbox:        []*OutboxEntry{},
		},
		auditReceipts: map[string]bool{},
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	if err := store.loadAuditReceipts(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *LifecycleStore) ApplyBundle(bundle *ValidatedBundle, beadStore LifecycleBeadSink, options ApplyLifecycleOptions) (ApplyLifecycleResult, error) {
	return s.applyBundle(bundle, beadStore, options, false)
}

func (s *LifecycleStore) applyBundle(bundle *ValidatedBundle, beadStore LifecycleBeadSink, options ApplyLifecycleOptions, controllerOwned bool) (ApplyLifecycleResult, error) {
	if bundle == nil {
		return ApplyLifecycleResult{}, fmt.Errorf("validated Visual Hive bundle is required")
	}
	if bundle.validationProfile == bundleValidationPullRequestLocal {
		return ApplyLifecycleResult{}, fmt.Errorf("pull_request bundles are check evidence only and cannot enter the authoritative lifecycle bundle path")
	}
	if !bundle.Validation.Trusted {
		return ApplyLifecycleResult{}, fmt.Errorf("trusted Visual Hive bundle validation is required before lifecycle application")
	}
	if beadStore == nil {
		return ApplyLifecycleResult{}, fmt.Errorf("persistent Hive bead store is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	manifest := bundle.Manifest
	validatedAuthoritative := bundle.Validation.Authoritative
	importInputs, err := buildObservationImportInputs(manifest, bundle.artifactIndex)
	if err != nil {
		return ApplyLifecycleResult{}, fmt.Errorf("build verified Visual Hive import plan: %w", err)
	}
	if !controllerOwned {
		for fingerprint, input := range importInputs {
			input.Status = beads.StatusOpen
			input.Metadata["visual_hive_controller_owned"] = false
			delete(input.Metadata, "visual_hive_admission_state")
			importInputs[fingerprint] = input
		}
	} else {
		identities := controllerImportIdentityMap(manifest, s.state.Findings)
		rekeyControllerImportInputs(manifest.Source.Repository, importInputs, identities)
		for key, input := range controllerAbsenceImportInputs(manifest, s.state.Findings, identities, options, validatedAuthoritative) {
			importInputs[key] = input
		}
	}
	result := ApplyLifecycleResult{BundleID: manifest.BundleID}
	if priorDigest, exists := s.state.ReplayKeys[manifest.ReplayProtection.Key]; exists {
		if priorDigest != manifest.OverallDigest {
			if err := s.auditLockedStrict(LifecycleAuditEntry{Action: "bundle_replay", Allowed: false, Repository: manifest.Source.Repository, BundleID: manifest.BundleID, Detail: "replay key reused with a different digest"}); err != nil {
				return result, err
			}
			return result, fmt.Errorf("bundle replay key was reused with a different digest")
		}
		result.Idempotent = true
		if controllerOwned {
			return s.projectControllerReplayLocked(manifest, importInputs, beadStore, result)
		}
		return result, nil
	}
	backup := cloneLifecycleState(s.state)
	holdControllerBead := func(finding *FindingLifecycle) error {
		if !controllerOwned || finding == nil {
			return nil
		}
		bead := beadStore.FindByExternalRef(beadExternalRef(finding.Repository, finding.RepositoryFingerprint))
		if bead == nil {
			return nil
		}
		if finding.BeadID == "" {
			finding.BeadID = bead.ID
		}
		return beadStore.Update(bead.ID, func(value *beads.Bead) {
			value.Status = beads.StatusBlocked
			value.ClosedAt = nil
			if value.Metadata == nil {
				value.Metadata = map[string]interface{}{}
			}
			value.Metadata["visual_hive_controller_owned"] = true
			value.Metadata["visual_hive_admission_state"] = "pending"
			delete(value.Metadata, "visual_hive_admission_decision_json")
			delete(value.Metadata, "visual_hive_dispatch_envelope_json")
		})
	}
	audit := func(entry LifecycleAuditEntry) error {
		if err := s.auditLockedStrict(entry); err != nil {
			s.state = backup
			return err
		}
		return nil
	}

	now := time.Now().UTC()
	targetRef := options.TargetRef
	if targetRef == "" {
		targetRef = "refs/heads/main"
	}
	canonicalOwners := make(map[string]*FindingLifecycle)
	newLegacyMigrations := make(map[string]*FindingLifecycle)
	ambiguousLegacyCanonicals := make(map[string][]legacyCanonicalMatch)
	for _, observation := range manifest.Observations {
		if observation.State != "present" || observation.PublicationRole != "canonical" || s.state.Findings[observation.RepositoryFingerprint] != nil {
			continue
		}
		if owner := exactMigratedCanonicalOwner(manifest.Source.Repository, s.state.Findings, observation); owner != nil {
			canonicalOwners[observation.RepositoryFingerprint] = owner
			continue
		}
		legacyMatches := exactLegacyCanonicalMigrationMatches(manifest.Source.Repository, s.state.Findings, observation)
		if len(legacyMatches) != 1 || legacyMatches[0].key != legacyMatches[0].finding.RepositoryFingerprint {
			if len(legacyMatches) == 0 {
				continue
			}
			ambiguousLegacyCanonicals[observation.RepositoryFingerprint] = legacyMatches
			if err := audit(LifecycleAuditEntry{Action: "migrate_legacy_canonical", Allowed: false, Repository: manifest.Source.Repository, RepositoryFingerprint: observation.RepositoryFingerprint, BundleID: manifest.BundleID, Detail: "ambiguous exact legacy canonical ownership; no existing issue was adopted"}); err != nil {
				return result, err
			}
			continue
		}
		legacy := legacyMatches[0].finding
		canonicalOwners[observation.RepositoryFingerprint] = legacy
		newLegacyMigrations[observation.RepositoryFingerprint] = legacy
	}
	beadInputs := make([]beads.BatchInput, 0, len(importInputs))
	if controllerOwned {
		for _, input := range importInputs {
			if beadStore.FindByExternalRef(input.ExternalRef) == nil {
				beadInputs = append(beadInputs, input)
			}
		}
	} else {
		for _, observation := range manifest.Observations {
			if observation.State != "present" || canonicalOwners[observation.RepositoryFingerprint] != nil {
				continue
			}
			if existing := s.state.Findings[observation.RepositoryFingerprint]; existing == nil || existing.BeadID == "" {
				if input, exists := importInputs[observation.RepositoryFingerprint]; exists {
					beadInputs = append(beadInputs, input)
				}
			}
		}
	}
	sort.Slice(beadInputs, func(i, j int) bool { return beadInputs[i].ExternalRef < beadInputs[j].ExternalRef })
	if len(beadInputs) > 0 {
		beadResult, err := beadStore.ImportBatch(beadInputs)
		if err != nil {
			return result, fmt.Errorf("persist lifecycle beads: %w", err)
		}
		result.BeadsCreated, result.BeadsSkipped = beadResult.Created, beadResult.Skipped
	}
	for _, observation := range manifest.Observations {
		legacy := newLegacyMigrations[observation.RepositoryFingerprint]
		if legacy == nil {
			continue
		}
		legacy.PendingMarkerMigrationFrom = lifecycleMarker(legacy.RepositoryFingerprint)
		attachCanonicalPublicationIdentity(legacy, manifest.Source.Repository, observation)
		if err := audit(LifecycleAuditEntry{Action: "migrate_legacy_canonical", Allowed: true, Repository: legacy.Repository, RepositoryFingerprint: legacy.RepositoryFingerprint, BundleID: manifest.BundleID, Detail: fmt.Sprintf("preserved issue #%d and existing repair identity for exact source fingerprint and issue kind", legacy.IssueNumber)}); err != nil {
			return result, err
		}
	}

	observedFingerprints := make(map[string]bool, len(manifest.Observations))
	presentRoots := make(map[string]bool)
	for _, observation := range manifest.Observations {
		if observation.State == "present" && observation.RootCauseKey != "" {
			presentRoots[observation.RootCauseKey] = true
		}
	}
	coveredRoots := coveredPublicationRoots(manifest.Source.Repository, manifest.Observations, s.state.Findings)
	pendingIssueReservations := pendingOpenIssueReservationCount(manifest.Source.Repository, s.state.Findings, s.state.Outbox)
	publicationSet := selectIssuePublications(manifest.Source.Repository, manifest.Observations, s.state.Findings, pendingIssueReservations, options.MaxActiveIssues, options.PreferRepairable)
	for _, observation := range manifest.Observations {
		observedFingerprints[observation.RepositoryFingerprint] = true
		for _, legacy := range ambiguousLegacyCanonicals[observation.RepositoryFingerprint] {
			observedFingerprints[legacy.key] = true
		}
		finding := s.state.Findings[observation.RepositoryFingerprint]
		canonicalOwner := canonicalOwners[observation.RepositoryFingerprint]
		if finding == nil && canonicalOwner != nil {
			finding = canonicalOwner
			observedFingerprints[finding.RepositoryFingerprint] = true
		}
		if observation.State == "absent" {
			if finding == nil {
				result.IgnoredAbsent++
				continue
			}
			if allowed, reason := s.rootResolutionAllowedLocked(finding, manifest, validatedAuthoritative, targetRef, options, presentRoots); !allowed {
				result.IgnoredAbsent++
				if err := audit(LifecycleAuditEntry{Action: "resolve_finding", Allowed: false, Repository: manifest.Source.Repository, RepositoryFingerprint: observation.RepositoryFingerprint, BundleID: manifest.BundleID, Detail: reason}); err != nil {
					return result, err
				}
				continue
			}
			if err := holdControllerBead(finding); err != nil {
				s.state = backup
				return result, fmt.Errorf("hold resolved Visual Hive bead for admission: %w", err)
			}
			updateFindingFromObservation(finding, manifest, observation)
			if finding.Status == StatusIssueClosed {
				result.Updated++
				result.FindingIDs = append(result.FindingIDs, observation.RepositoryFingerprint)
				continue
			}
			finding.Status = StatusResolved
			finding.PendingIssueAction = ""
			if finding.IssueNumber > 0 {
				finding.PendingIssueAction = OutboxCloseIssue
			}
			finding.ResolvedAt = &now
			finding.ClosedAt = nil
			setVerificationEvidence(finding, manifest, options)
			result.Resolved++
			result.Updated++
			if finding.IssueNumber > 0 && !options.DisableIssuePublication {
				if s.enqueueLocked(outboxForFinding(OutboxCloseIssue, finding, manifest, now)) {
					result.OutboxCreated++
				}
			}
			if err := audit(LifecycleAuditEntry{Action: "resolve_finding", Allowed: true, Repository: finding.Repository, RepositoryFingerprint: finding.RepositoryFingerprint, BundleID: manifest.BundleID, Detail: "authoritative target-ref absence"}); err != nil {
				return result, err
			}
			result.FindingIDs = append(result.FindingIDs, observation.RepositoryFingerprint)
			continue
		}

		deferredReason := publicationDeferralReason(observation, coveredRoots)
		preserveActiveOwner := deferredReason != "" && finding != nil && finding.IssueNumber > 0 && finding.RootCauseKey == observation.RootCauseKey &&
			finding.PublicationFingerprint == publicationFingerprint(finding.Repository, finding.RootCauseKey, finding.RepositoryFingerprint)
		wasReopened := false
		if finding == nil {
			firstSeenAt, _ := time.Parse(time.RFC3339Nano, observation.FirstSeenAt)
			finding = &FindingLifecycle{
				Repository: manifest.Source.Repository, RepositoryID: manifest.Source.RepositoryID,
				Fingerprint: observation.Fingerprint, RepositoryFingerprint: observation.RepositoryFingerprint,
				Status: StatusDetected, FirstSeenAt: firstSeenAt,
			}
			s.state.Findings[observation.RepositoryFingerprint] = finding
			result.Created++
		} else {
			result.Updated++
			if finding.Status == StatusIssueClosed || finding.Status == StatusResolved {
				finding.Status = StatusDetected
				finding.ResolvedAt = nil
				finding.ClosedAt = nil
				finding.Recurrences++
				resetRepairCycle(finding)
				result.Reopened++
				wasReopened = true
			}
		}
		if !preserveActiveOwner {
			if canonicalOwner != nil {
				updateFindingPublicationFromObservation(finding, manifest, observation)
			} else {
				legacyIssueIdentity := finding.IssueNumber > 0 && finding.RootCauseKey == "" && observation.RootCauseKey != ""
				legacyPublicationFingerprint := finding.PublicationFingerprint
				updateFindingFromObservation(finding, manifest, observation)
				if legacyIssueIdentity {
					// Publication metadata must never silently rebind a legacy observation
					// issue to a new root marker. It remains independently managed until a
					// user explicitly reconciles it.
					finding.PublicationRole, finding.RootCauseKey, finding.BlockedByRootKeys = "", "", nil
					finding.PublicationFingerprint = legacyPublicationFingerprint
				}
			}
		}
		beadFingerprint := observation.RepositoryFingerprint
		if canonicalOwner != nil {
			beadFingerprint = finding.RepositoryFingerprint
		}
		if bead := beadStore.FindByExternalRef(beadExternalRef(manifest.Source.Repository, beadFingerprint)); bead != nil {
			if finding.BeadID == "" || controllerOwned {
				finding.BeadID = bead.ID
			}
			if wasReopened && (bead.Status == beads.StatusClosed || bead.Status == beads.StatusDone) {
				if err := beadStore.Update(bead.ID, func(value *beads.Bead) {
					if controllerOwned {
						value.Status = beads.StatusBlocked
					} else {
						value.Status = beads.StatusOpen
					}
					value.ClosedAt = nil
				}); err != nil {
					s.state = backup
					return result, fmt.Errorf("reopen Visual Hive bead: %w", err)
				}
			}
			if controllerOwned {
				if err := beadStore.Update(bead.ID, func(value *beads.Bead) {
					value.Status = beads.StatusBlocked
					value.ClosedAt = nil
					if value.Metadata == nil {
						value.Metadata = map[string]interface{}{}
					}
					value.Metadata["visual_hive_controller_owned"] = true
					value.Metadata["visual_hive_admission_state"] = "pending"
					delete(value.Metadata, "visual_hive_admission_decision_json")
					delete(value.Metadata, "visual_hive_dispatch_envelope_json")
				}); err != nil {
					s.state = backup
					return result, fmt.Errorf("hold updated Visual Hive bead for admission: %w", err)
				}
			}
		}
		if deferredReason != "" {
			result.Deferred++
			if err := audit(LifecycleAuditEntry{Action: "defer_open_issue", Allowed: true, Repository: finding.Repository, RepositoryFingerprint: finding.RepositoryFingerprint, BundleID: manifest.BundleID, Detail: deferredReason}); err != nil {
				return result, err
			}
			result.FindingIDs = append(result.FindingIDs, observation.RepositoryFingerprint)
			continue
		}
		action := OutboxOpenIssue
		if finding.IssueNumber > 0 {
			if wasReopened {
				action = OutboxReopenIssue
			} else {
				action = OutboxUpdateIssue
			}
		} else if !publicationSet[observation.RepositoryFingerprint] {
			result.Deferred++
			if err := audit(LifecycleAuditEntry{Action: "defer_open_issue", Allowed: true, Repository: finding.Repository, RepositoryFingerprint: finding.RepositoryFingerprint, BundleID: manifest.BundleID, Detail: "active issue work-in-progress limit reached"}); err != nil {
				return result, err
			}
			result.FindingIDs = append(result.FindingIDs, observation.RepositoryFingerprint)
			continue
		}
		finding.PendingIssueAction = action
		if publicationSet[observation.RepositoryFingerprint] && observation.RootCauseKey != "" {
			if owner := publicationOwner(manifest.Source.Repository, s.state.Findings, observation); owner != nil && owner.RepositoryFingerprint != finding.RepositoryFingerprint {
				ownerReopened := owner.Status == StatusIssueClosed || owner.Status == StatusResolved
				if ownerReopened {
					owner.Status = StatusDetected
					owner.ResolvedAt, owner.ClosedAt = nil, nil
					owner.Recurrences++
					resetRepairCycle(owner)
					if owner.BeadID != "" {
						if err := beadStore.Update(owner.BeadID, func(value *beads.Bead) {
							if controllerOwned {
								value.Status = beads.StatusBlocked
							} else {
								value.Status = beads.StatusOpen
							}
							value.ClosedAt = nil
						}); err != nil {
							s.state = backup
							return result, fmt.Errorf("reopen Visual Hive publication owner bead: %w", err)
						}
					}
					result.Reopened++
				}
				updateFindingPublicationFromObservation(owner, manifest, observation)
				ownerAction := OutboxUpdateIssue
				if ownerReopened {
					ownerAction = OutboxReopenIssue
				}
				owner.PendingIssueAction = ownerAction
				if !options.DisableIssuePublication && s.enqueueLocked(outboxForFinding(ownerAction, owner, manifest, now)) {
					result.OutboxCreated++
				}
				result.Updated++
				result.FindingIDs = append(result.FindingIDs, owner.RepositoryFingerprint)
				if err := audit(LifecycleAuditEntry{Action: string(ownerAction), Allowed: true, Repository: owner.Repository, RepositoryFingerprint: owner.RepositoryFingerprint, BundleID: manifest.BundleID, Detail: "updated the existing exact-root publication owner"}); err != nil {
					return result, err
				}
				result.FindingIDs = append(result.FindingIDs, observation.RepositoryFingerprint)
				continue
			}
		}
		if !options.DisableIssuePublication && s.enqueueLocked(outboxForFinding(action, finding, manifest, now)) {
			result.OutboxCreated++
		}
		if err := audit(LifecycleAuditEntry{Action: string(action), Allowed: true, Repository: finding.Repository, RepositoryFingerprint: finding.RepositoryFingerprint, BundleID: manifest.BundleID}); err != nil {
			return result, err
		}
		result.FindingIDs = append(result.FindingIDs, observation.RepositoryFingerprint)
	}

	// A full authoritative bundle is an exhaustive inventory for the contracts
	// it says it evaluated. Resolve an existing finding omitted from that
	// inventory only when every affected contract was actually executed. This
	// lets a trusted target-branch run close findings without copying Hive's
	// private lifecycle database into the target workflow.
	if validatedAuthoritative && manifest.Scan.Scope == "full" && refsEquivalent(manifest.Source.Ref, targetRef) {
		keys := make([]string, 0, len(s.state.Findings))
		for key := range s.state.Findings {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			finding := s.state.Findings[key]
			if finding == nil || !strings.EqualFold(finding.Repository, manifest.Source.Repository) || observedFingerprints[key] || finding.Status == StatusIssueClosed {
				continue
			}
			if allowed, reason := s.rootResolutionAllowedLocked(finding, manifest, validatedAuthoritative, targetRef, options, presentRoots); !allowed {
				if err := audit(LifecycleAuditEntry{Action: "infer_absent_finding", Allowed: false, Repository: finding.Repository, RepositoryFingerprint: key, BundleID: manifest.BundleID, Detail: reason}); err != nil {
					return result, err
				}
				continue
			}
			if err := holdControllerBead(finding); err != nil {
				s.state = backup
				return result, fmt.Errorf("hold inferred-absence Visual Hive bead for admission: %w", err)
			}
			finding.Status = StatusResolved
			finding.PendingIssueAction = ""
			if finding.IssueNumber > 0 {
				finding.PendingIssueAction = OutboxCloseIssue
			}
			finding.ResolvedAt = &now
			finding.ClosedAt = nil
			finding.LastBundleID = manifest.BundleID
			finding.LastBundleDigest = manifest.OverallDigest
			finding.LastWorkflowRunID = manifest.Source.WorkflowRunID
			setVerificationEvidence(finding, manifest, options)
			result.Resolved++
			result.Updated++
			if finding.IssueNumber > 0 && !options.DisableIssuePublication && s.enqueueLocked(outboxForFinding(OutboxCloseIssue, finding, manifest, now)) {
				result.OutboxCreated++
			}
			result.FindingIDs = append(result.FindingIDs, key)
			if err := audit(LifecycleAuditEntry{Action: "infer_absent_finding", Allowed: true, Repository: finding.Repository, RepositoryFingerprint: key, BundleID: manifest.BundleID, Detail: "omitted from exhaustive evaluated-contract inventory"}); err != nil {
				return result, err
			}
		}
	}

	s.state.ReplayKeys[manifest.ReplayProtection.Key] = manifest.OverallDigest
	s.state.LastImport = visualImportRunReceipt(manifest, now)
	s.state.UpdatedAt = now
	s.sortStateLocked()
	if err := s.persistLocked(); err != nil {
		s.state = backup
		return ApplyLifecycleResult{}, err
	}
	sort.Strings(result.FindingIDs)
	return result, nil
}

// ApplyImportPlan accepts only a sealed plan built from complete private
// verification and applies its canonical manifest through the same lifecycle,
// replay, outbox, and recovery code as the compatibility path.
func (s *LifecycleStore) ApplyImportPlan(plan VerifiedImportPlan, beadStore LifecycleBeadSink, options ApplyLifecycleOptions) (ApplyLifecycleResult, error) {
	if err := plan.validate(); err != nil {
		return ApplyLifecycleResult{}, err
	}
	return s.applyBundle(plan.seal.bundle, beadStore, options, true)
}

// ResolveImportPlanWorks preserves the verified observation identity while
// mapping source work onto an existing durable canonical publication owner.
// The mapping is derived only from the sealed plan and this lifecycle store.
func (s *LifecycleStore) ResolveImportPlanWorks(plan VerifiedImportPlan, options ApplyLifecycleOptions) ([]AdmittedVisualWork, error) {
	if err := plan.validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	works := plan.Works()
	manifest := plan.seal.bundle.Manifest
	identities := controllerImportIdentityMap(manifest, s.state.Findings)
	observed := make(map[string]bool, len(works))
	for index := range works {
		original := works[index].RepositoryFingerprint
		observed[original] = true
		if works[index].ObservationFingerprint == "" {
			works[index].ObservationFingerprint = original
		}
		owner := identities[original]
		if owner == "" || owner == original {
			continue
		}
		works[index].RepositoryFingerprint = owner
		works[index].SourceExternalRef = beadExternalRef(works[index].Packet.Repository, owner)
		works[index].Bead.SourceID = owner
		works[index].Bead.ExternalRef = works[index].SourceExternalRef
		if works[index].Bead.Metadata == nil {
			works[index].Bead.Metadata = map[string]interface{}{}
		}
		works[index].Bead.Metadata["visual_hive_observation_repository_fingerprint"] = original
		works[index].Bead.Metadata["visual_hive_repository_fingerprint"] = owner
	}
	for index := range works {
		finding := s.state.Findings[works[index].RepositoryFingerprint]
		if works[index].ObservationState == "absent" && finding != nil && works[index].Bead.ExternalRef == "" {
			works[index].Bead = controllerFindingBatchInput(manifest, finding, works[index].Role, works[index].RoutingReason, works[index].RoutingAllowed, works[index].ObservationFingerprint)
		}
	}
	presentRoots := map[string]bool{}
	for _, observation := range manifest.Observations {
		if observation.State == "present" && observation.RootCauseKey != "" {
			presentRoots[observation.RootCauseKey] = true
		}
	}
	if plan.seal.bundle.Validation.Authoritative && manifest.Scan.Scope == "full" && refsEquivalent(manifest.Source.Ref, options.TargetRef) {
		keys := make([]string, 0, len(s.state.Findings))
		for key := range s.state.Findings {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			finding := s.state.Findings[key]
			if finding == nil || observed[key] || !strings.EqualFold(finding.Repository, manifest.Source.Repository) {
				continue
			}
			samePacketRecovery := finding.LastBundleID == plan.packet.PacketID && finding.LastBundleDigest == plan.packet.PacketDigest &&
				(finding.Status == StatusResolved || finding.Status == StatusIssueClosed)
			if !samePacketRecovery {
				if finding.Status == StatusIssueClosed {
					continue
				}
				if allowed, _ := s.rootResolutionAllowedLocked(finding, manifest, true, options.TargetRef, options, presentRoots); !allowed {
					continue
				}
			}
			route := ResolveSpecialist(SpecialistRoutingInput{IssueKind: finding.IssueKind, OwningAgentHint: finding.OwningAgentHint,
				GroundedRepairScope: strings.TrimSpace(finding.ValidationCommand) != "" && validAffectedContracts(finding.AffectedContracts), BaselineChangesForbidden: true})
			validation := []string{}
			if command := strings.TrimSpace(finding.ValidationCommand); command != "" {
				validation = append(validation, command)
			}
			work := AdmittedVisualWork{
				SourceExternalRef: beadExternalRef(finding.Repository, finding.RepositoryFingerprint), Packet: plan.packet,
				FindingFingerprint: finding.Fingerprint, ObservationFingerprint: finding.RepositoryFingerprint, RepositoryFingerprint: finding.RepositoryFingerprint,
				ObservationState: "absent", PublicationRole: finding.PublicationRole, RootCauseKey: finding.RootCauseKey,
				BlockedByRootKeys: append([]string(nil), finding.BlockedByRootKeys...), IssueKind: finding.IssueKind, Severity: finding.Severity,
				Title: finding.Title, Body: finding.Body, Labels: append([]string(nil), finding.Labels...), Role: route.Role,
				RoutingReason: "persisted lifecycle owner omitted from exhaustive verified inventory: " + route.Reason, RoutingAllowed: route.DispatchAllowed,
				AffectedContracts: append([]string(nil), finding.AffectedContracts...), KnowledgeKeywordState: "unavailable_no_verified_facts",
				ValidationCommands: validation, ReproductionCommands: append([]string(nil), validation...), ReproductionSource: "persisted_verified_observation_validation_command",
				Authority: VisualWorkAuthority{ProposalOnly: true},
			}
			work.Bead = controllerFindingBatchInput(manifest, finding, work.Role, work.RoutingReason, work.RoutingAllowed, work.ObservationFingerprint)
			works = append(works, work)
		}
	}
	for index := range works {
		for dependencyIndex, dependency := range works[index].Bead.DependsOn {
			if owner := identities[dependency]; owner != "" {
				works[index].Bead.DependsOn[dependencyIndex] = owner
			}
		}
		sort.Strings(works[index].Bead.DependsOn)
	}
	sort.Slice(works, func(i, j int) bool { return works[i].RepositoryFingerprint < works[j].RepositoryFingerprint })
	return works, nil
}

func controllerImportIdentityMap(manifest Manifest, findings map[string]*FindingLifecycle) map[string]string {
	identities := make(map[string]string, len(manifest.Observations))
	for _, observation := range manifest.Observations {
		identity := observation.RepositoryFingerprint
		if observation.PublicationRole == "canonical" {
			if owner := exactMigratedCanonicalOwner(manifest.Source.Repository, findings, observation); owner != nil {
				identity = owner.RepositoryFingerprint
			} else if matches := exactLegacyCanonicalMigrationMatches(manifest.Source.Repository, findings, observation); len(matches) == 1 {
				identity = matches[0].finding.RepositoryFingerprint
			}
		}
		identities[observation.RepositoryFingerprint] = identity
	}
	return identities
}

func controllerAbsenceImportInputs(manifest Manifest, findings map[string]*FindingLifecycle, identities map[string]string, options ApplyLifecycleOptions, authoritative bool) map[string]beads.BatchInput {
	inputs := map[string]beads.BatchInput{}
	observed := make(map[string]bool, len(manifest.Observations))
	presentRoots := map[string]bool{}
	for _, observation := range manifest.Observations {
		observed[observation.RepositoryFingerprint] = true
		if observation.State == "present" && observation.RootCauseKey != "" {
			presentRoots[observation.RootCauseKey] = true
		}
		if observation.State != "absent" {
			continue
		}
		identity := identities[observation.RepositoryFingerprint]
		if identity == "" {
			identity = observation.RepositoryFingerprint
		}
		finding := findings[identity]
		if finding == nil {
			continue
		}
		route := ResolveSpecialist(SpecialistRoutingInput{IssueKind: observation.IssueKind, OwningAgentHint: observation.OwningAgentHint,
			GroundedRepairScope: strings.TrimSpace(observation.ValidationCommand) != "" && validAffectedContracts(observation.AffectedContracts), BaselineChangesForbidden: true})
		inputs[observation.RepositoryFingerprint] = controllerFindingBatchInput(manifest, finding, route.Role, route.Reason, route.DispatchAllowed, observation.RepositoryFingerprint)
	}
	if !authoritative || manifest.Scan.Scope != "full" || !refsEquivalent(manifest.Source.Ref, options.TargetRef) {
		return inputs
	}
	keys := make([]string, 0, len(findings))
	for key := range findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	store := &LifecycleStore{state: LifecycleState{Findings: findings}}
	for _, key := range keys {
		finding := findings[key]
		if finding == nil || observed[key] || !strings.EqualFold(finding.Repository, manifest.Source.Repository) {
			continue
		}
		samePacketRecovery := finding.LastBundleID == manifest.BundleID && finding.LastBundleDigest == manifest.OverallDigest &&
			(finding.Status == StatusResolved || finding.Status == StatusIssueClosed)
		if !samePacketRecovery {
			if finding.Status == StatusIssueClosed {
				continue
			}
			if allowed, _ := store.rootResolutionAllowedLocked(finding, manifest, authoritative, options.TargetRef, options, presentRoots); !allowed {
				continue
			}
		}
		route := ResolveSpecialist(SpecialistRoutingInput{IssueKind: finding.IssueKind, OwningAgentHint: finding.OwningAgentHint,
			GroundedRepairScope: strings.TrimSpace(finding.ValidationCommand) != "" && validAffectedContracts(finding.AffectedContracts), BaselineChangesForbidden: true})
		inputs[key] = controllerFindingBatchInput(manifest, finding, route.Role, "persisted lifecycle owner omitted from exhaustive verified inventory: "+route.Reason, route.DispatchAllowed, key)
	}
	return inputs
}

func controllerFindingBatchInput(manifest Manifest, finding *FindingLifecycle, role, routingReason string, routingAllowed bool, observationFingerprint string) beads.BatchInput {
	validation := []string{}
	if command := strings.TrimSpace(finding.ValidationCommand); command != "" {
		validation = append(validation, command)
	}
	return beads.BatchInput{
		SourceID: finding.RepositoryFingerprint, Title: finding.Title, Type: beadTypeForObservation(finding.IssueKind), Status: beads.StatusBlocked,
		Priority: priorityForSeverity(finding.Severity), Actor: role, ExternalRef: beadExternalRef(finding.Repository, finding.RepositoryFingerprint), Notes: finding.Body,
		Metadata: map[string]interface{}{
			"visual_hive_controller_owned": true, "visual_hive_admission_state": "pending", "visual_hive_tombstone": true,
			"visual_hive_fingerprint": finding.Fingerprint, "visual_hive_repository_fingerprint": finding.RepositoryFingerprint,
			"visual_hive_observation_repository_fingerprint": observationFingerprint, "visual_hive_packet_id": manifest.BundleID,
			"visual_hive_packet_digest": manifest.OverallDigest, "visual_hive_replay_key": manifest.ReplayProtection.Key,
			"visual_hive_repository": finding.Repository, "visual_hive_repository_id": manifest.Source.RepositoryID,
			"visual_hive_source_ref": manifest.Source.Ref, "visual_hive_base_sha": manifest.Source.CommitSHA,
			"visual_hive_workflow_name": manifest.Source.WorkflowName, "visual_hive_workflow_event": manifest.Source.Event,
			"visual_hive_workflow_run_id": manifest.Source.WorkflowRunID, "visual_hive_workflow_run_attempt": manifest.Source.WorkflowRunAttempt,
			"visual_hive_workflow_artifact_id": manifest.Source.WorkflowArtifactID, "visual_hive_producer": manifest.Producer.Name,
			"visual_hive_producer_version": manifest.Producer.Version, "visual_hive_producer_git_commit": manifest.Producer.GitCommit,
			"visual_hive_issue_kind": finding.IssueKind, "visual_hive_severity": finding.Severity,
			"visual_hive_publication_role": finding.PublicationRole, "visual_hive_root_cause_key": finding.RootCauseKey,
			"visual_hive_blocked_by_root_keys": append([]string(nil), finding.BlockedByRootKeys...),
			"visual_hive_affected_contracts":   append([]string(nil), finding.AffectedContracts...), "visual_hive_validation_commands": validation,
			"visual_hive_route_role": role, "visual_hive_route_reason": routingReason, "visual_hive_route_allowed": routingAllowed,
			"hive_proposal_only": true, "hive_github_write_allowed": false, "hive_merge_allowed": false,
			"hive_baseline_changes_allowed": false, "hive_baseline_approval_allowed": false,
		},
	}
}

func rekeyControllerImportInputs(repository string, inputs map[string]beads.BatchInput, identities map[string]string) {
	for observationFingerprint, input := range inputs {
		owner := identities[observationFingerprint]
		if owner == "" || owner == observationFingerprint {
			continue
		}
		input = cloneBatchInput(input)
		input.SourceID = owner
		input.ExternalRef = beadExternalRef(repository, owner)
		input.Metadata["visual_hive_observation_repository_fingerprint"] = observationFingerprint
		input.Metadata["visual_hive_repository_fingerprint"] = owner
		for index, dependency := range input.DependsOn {
			if dependencyOwner := identities[dependency]; dependencyOwner != "" {
				input.DependsOn[index] = dependencyOwner
			}
		}
		sort.Strings(input.DependsOn)
		inputs[observationFingerprint] = input
	}
}

func (s *LifecycleStore) projectControllerReplayLocked(manifest Manifest, inputs map[string]beads.BatchInput, beadStore LifecycleBeadSink, result ApplyLifecycleResult) (ApplyLifecycleResult, error) {
	ordered := make([]beads.BatchInput, 0, len(inputs))
	createdRefs := make([]string, 0, len(inputs))
	identities := controllerImportIdentityMap(manifest, s.state.Findings)
	eligibleInputs := map[string]beads.BatchInput{}
	for observationFingerprint, input := range inputs {
		identity := identities[observationFingerprint]
		if identity == "" {
			identity = observationFingerprint
		}
		finding := s.state.Findings[identity]
		if finding == nil || finding.LastBundleID != manifest.BundleID || finding.LastBundleDigest != manifest.OverallDigest {
			continue
		}
		eligibleInputs[observationFingerprint] = input
		ordered = append(ordered, input)
		if beadStore.FindByExternalRef(input.ExternalRef) == nil {
			createdRefs = append(createdRefs, input.ExternalRef)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ExternalRef < ordered[j].ExternalRef })
	if len(ordered) == 0 {
		return result, nil
	}
	rollbackSink, rollbackAvailable := beadStore.(interface{ RollbackImportedExternalRefs([]string) error })
	if len(createdRefs) > 0 && !rollbackAvailable {
		return result, fmt.Errorf("normal role bead sink does not support transactional replay projection rollback")
	}
	imported, err := beadStore.ImportBatch(ordered)
	if err != nil {
		return result, fmt.Errorf("project replayed Visual Hive work into normal role stores: %w", err)
	}
	rollback := func(cause error) (ApplyLifecycleResult, error) {
		if len(createdRefs) > 0 {
			if rollbackErr := rollbackSink.RollbackImportedExternalRefs(createdRefs); rollbackErr != nil {
				return result, fmt.Errorf("%w; replay projection rollback failed: %v", cause, rollbackErr)
			}
		}
		return result, cause
	}
	backup := cloneLifecycleState(s.state)
	changed := false
	for observationFingerprint, input := range eligibleInputs {
		bead := beadStore.FindByExternalRef(input.ExternalRef)
		if bead == nil {
			s.state = backup
			return rollback(fmt.Errorf("replayed Visual Hive bead %s was not durably projected", input.ExternalRef))
		}
		identity := identities[observationFingerprint]
		if identity == "" {
			identity = observationFingerprint
		}
		if finding := s.state.Findings[identity]; finding != nil && finding.BeadID != bead.ID {
			finding.BeadID = bead.ID
			changed = true
		}
	}
	if changed {
		s.state.UpdatedAt = time.Now().UTC()
		if err := s.persistLocked(); err != nil {
			s.state = backup
			return rollback(fmt.Errorf("persist replayed Visual Hive normal-store projection: %w", err))
		}
	}
	result.BeadsCreated, result.BeadsSkipped = imported.Created, imported.Skipped
	return result, nil
}

// RecordControllerImportHold durably records a fail-closed intake routing
// failure in the existing lifecycle audit ledger. It does not create work,
// consume the packet replay key, or introduce another queue.
func (s *LifecycleStore) RecordControllerImportHold(packet VerifiedPacketIdentity, detail string) error {
	if strings.TrimSpace(packet.PacketID) == "" || strings.TrimSpace(packet.PacketDigest) == "" || strings.TrimSpace(packet.Repository) == "" || strings.TrimSpace(detail) == "" {
		return fmt.Errorf("verified packet identity and hold detail are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auditLockedStrict(LifecycleAuditEntry{
		Action: "controller_import_hold", Allowed: false, Repository: packet.Repository,
		BundleID: packet.PacketID, Detail: truncate(detail, 4096),
	})
}

func visualImportRunReceipt(manifest Manifest, importedAt time.Time) *VisualImportRunReceipt {
	receipt := &VisualImportRunReceipt{
		PacketID: manifest.BundleID, PacketDigest: manifest.OverallDigest, ReplayKey: manifest.ReplayProtection.Key,
		Repository: manifest.Source.Repository, RepositoryID: manifest.Source.RepositoryID, BaseSHA: manifest.Source.CommitSHA,
		WorkflowName: manifest.Source.WorkflowName, WorkflowEvent: manifest.Source.Event, WorkflowRunID: manifest.Source.WorkflowRunID,
		WorkflowRunAttempt: manifest.Source.WorkflowRunAttempt, WorkflowArtifactID: manifest.Source.WorkflowArtifactID,
		ProducerName: manifest.Producer.Name, ProducerGitCommit: manifest.Producer.GitCommit,
		Verdict: manifest.Verdict, Observations: len(manifest.Observations), ImportedAt: importedAt.UTC(),
	}
	if manifest.ArtifactIndex != nil {
		receipt.ArtifactIndexSHA256 = manifest.ArtifactIndex.SHA256
	}
	return receipt
}

func selectIssuePublications(repository string, observations []Observation, findings map[string]*FindingLifecycle, pendingIssueReservations, maxActive int, preferRepairable bool) map[string]bool {
	selected := map[string]bool{}
	coveredRoots := coveredPublicationRoots(repository, observations, findings)
	explicitPublication := false
	for _, observation := range observations {
		if observation.PublicationRole != "" {
			explicitPublication = true
			break
		}
	}
	activeIssues := map[string]bool{}
	for _, finding := range findings {
		if finding != nil && finding.IssueNumber > 0 && finding.Status != StatusResolved && finding.Status != StatusIssueClosed && (!explicitPublication || finding.RootCauseKey != "") {
			activeIssues[fmt.Sprintf("%s#%d", strings.ToLower(strings.TrimSpace(finding.Repository)), finding.IssueNumber)] = true
		}
	}
	candidates := make([]Observation, 0, len(observations))
	for _, observation := range observations {
		if observation.State != "present" {
			continue
		}
		if publicationDeferralReason(observation, coveredRoots) != "" {
			continue
		}
		if publicationOwner(repository, findings, observation) != nil {
			selected[observation.RepositoryFingerprint] = true
			continue
		}
		candidates = append(candidates, observation)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if preferRepairable {
			leftHuman, rightHuman := observationNeedsHumanReview(left), observationNeedsHumanReview(right)
			if leftHuman != rightHuman {
				return !leftHuman
			}
			if repairSignalRank(left) != repairSignalRank(right) {
				return repairSignalRank(left) > repairSignalRank(right)
			}
		}
		if severityRank(left.Severity) != severityRank(right.Severity) {
			return severityRank(left.Severity) > severityRank(right.Severity)
		}
		if issueKindRank(left.IssueKind) != issueKindRank(right.IssueKind) {
			return issueKindRank(left.IssueKind) > issueKindRank(right.IssueKind)
		}
		return left.RepositoryFingerprint < right.RepositoryFingerprint
	})
	slots := len(candidates)
	if maxActive > 0 {
		slots = maxActive - len(activeIssues) - pendingIssueReservations
		if slots < 0 {
			slots = 0
		}
	}
	created := 0
	for _, observation := range candidates {
		if created >= slots {
			break
		}
		selected[observation.RepositoryFingerprint] = true
		created++
	}
	return selected
}

func pendingOpenIssueReservationCount(repository string, findings map[string]*FindingLifecycle, outbox []*OutboxEntry) int {
	repository = strings.ToLower(strings.TrimSpace(repository))
	reservations := map[string]struct{}{}
	for _, entry := range outbox {
		if entry == nil || entry.Action != OutboxOpenIssue || entry.CompletedAt != nil ||
			strings.ToLower(strings.TrimSpace(entry.Repository)) != repository || strings.TrimSpace(entry.RepositoryFingerprint) == "" {
			continue
		}
		finding := findings[entry.RepositoryFingerprint]
		if finding == nil || finding.IssueNumber != 0 || finding.Status == StatusResolved || finding.Status == StatusIssueClosed ||
			entry.BundleDigest == "" || entry.BundleDigest != finding.LastBundleDigest {
			continue
		}
		identity := strings.TrimSpace(finding.PublicationFingerprint)
		if identity == "" {
			identity = entry.RepositoryFingerprint
		}
		reservations[identity] = struct{}{}
	}
	return len(reservations)
}

func coveredPublicationRoots(repository string, observations []Observation, findings map[string]*FindingLifecycle) map[string]bool {
	covered := map[string]bool{}
	present := map[string]bool{}
	absent := map[string]bool{}
	for _, observation := range observations {
		if observation.RootCauseKey == "" {
			continue
		}
		if observation.State == "present" {
			present[observation.RootCauseKey] = true
			if observation.PublicationRole == "canonical" {
				covered[observation.RootCauseKey] = true
			}
		} else if observation.State == "absent" {
			absent[observation.RootCauseKey] = true
		}
	}
	for _, finding := range findings {
		if finding != nil && finding.RootCauseKey != "" && finding.IssueNumber > 0 && finding.Status != StatusResolved && finding.Status != StatusIssueClosed &&
			(repository == "" || strings.EqualFold(strings.TrimSpace(finding.Repository), strings.TrimSpace(repository))) &&
			finding.PublicationFingerprint == publicationFingerprint(finding.Repository, finding.RootCauseKey, finding.RepositoryFingerprint) &&
			(!absent[finding.RootCauseKey] || present[finding.RootCauseKey]) {
			covered[finding.RootCauseKey] = true
		}
	}
	return covered
}

func publicationDeferralReason(observation Observation, coveredRoots map[string]bool) string {
	if observation.State != "present" || observation.RootCauseKey == "" {
		return ""
	}
	switch observation.PublicationRole {
	case "derivative":
		if coveredRoots[observation.RootCauseKey] {
			return fmt.Sprintf("deferred derivative observation: exact root %q has a canonical observation or active owner", observation.RootCauseKey)
		}
	case "aggregate":
		if len(observation.BlockedByRootKeys) == 0 {
			return "" // Invalid/unrecognized linkage fails open at publication.
		}
		for _, root := range observation.BlockedByRootKeys {
			if !coveredRoots[root] {
				return "" // Any uncovered dependency makes the aggregate independently actionable.
			}
		}
		return "deferred aggregate observation: every exact blocked root has a canonical observation or active owner"
	}
	return ""
}

func publicationOwner(repository string, findings map[string]*FindingLifecycle, observation Observation) *FindingLifecycle {
	if observation.RootCauseKey == "" {
		return findings[observation.RepositoryFingerprint]
	}
	var owner *FindingLifecycle
	for _, finding := range findings {
		if finding == nil || finding.IssueNumber <= 0 || finding.RootCauseKey != observation.RootCauseKey ||
			(repository != "" && !strings.EqualFold(strings.TrimSpace(finding.Repository), strings.TrimSpace(repository))) ||
			finding.PublicationFingerprint != publicationFingerprint(finding.Repository, finding.RootCauseKey, finding.RepositoryFingerprint) {
			continue
		}
		if owner == nil || finding.IssueNumber < owner.IssueNumber || (finding.IssueNumber == owner.IssueNumber && finding.RepositoryFingerprint < owner.RepositoryFingerprint) {
			owner = finding
		}
	}
	return owner
}

type legacyCanonicalMatch struct {
	key     string
	finding *FindingLifecycle
}

func exactLegacyCanonicalMigrationMatches(repository string, findings map[string]*FindingLifecycle, observation Observation) []legacyCanonicalMatch {
	if observation.State != "present" || observation.PublicationRole != "canonical" || observation.RootCauseKey == "" || !legacyCanonicalIssueKind(observation.IssueKind) {
		return nil
	}
	expectedLegacyFingerprint := digest([]byte(strings.ToLower(strings.TrimSpace(repository)) + "\x00" + observation.Fingerprint))
	matches := make([]legacyCanonicalMatch, 0, 1)
	for key, finding := range findings {
		if finding == nil || finding.IssueNumber <= 0 || strings.TrimSpace(finding.IssueURL) == "" || finding.Status == StatusResolved || finding.Status == StatusIssueClosed ||
			!strings.EqualFold(strings.TrimSpace(finding.Repository), strings.TrimSpace(repository)) ||
			finding.PublicationRole != "" || finding.RootCauseKey != "" || len(finding.BlockedByRootKeys) != 0 ||
			(finding.PublicationFingerprint != "" && finding.PublicationFingerprint != finding.RepositoryFingerprint) ||
			finding.RepositoryFingerprint != expectedLegacyFingerprint || finding.Fingerprint != observation.Fingerprint || finding.IssueKind != observation.IssueKind {
			continue
		}
		matches = append(matches, legacyCanonicalMatch{key: key, finding: finding})
	}
	sort.Slice(matches, func(left, right int) bool { return matches[left].key < matches[right].key })
	return matches
}

func legacyCanonicalIssueKind(issueKind string) bool {
	switch issueKind {
	case "missing_visual_coverage", "weak_visual_test", "external_repo_onboarding":
		return false
	default:
		return true
	}
}

func exactMigratedCanonicalOwner(repository string, findings map[string]*FindingLifecycle, observation Observation) *FindingLifecycle {
	expectedLegacyFingerprint := digest([]byte(strings.ToLower(strings.TrimSpace(repository)) + "\x00" + observation.Fingerprint))
	var owner *FindingLifecycle
	for key, finding := range findings {
		if finding == nil || key != expectedLegacyFingerprint || finding.RepositoryFingerprint != expectedLegacyFingerprint || finding.IssueNumber <= 0 ||
			!strings.EqualFold(strings.TrimSpace(finding.Repository), strings.TrimSpace(repository)) ||
			finding.PublicationRole != "canonical" || finding.RootCauseKey != observation.RootCauseKey || finding.Fingerprint != observation.Fingerprint || finding.IssueKind != observation.IssueKind ||
			finding.PublicationFingerprint != publicationFingerprint(repository, observation.RootCauseKey, observation.RepositoryFingerprint) {
			continue
		}
		if owner != nil {
			return nil
		}
		owner = finding
	}
	return owner
}

func attachCanonicalPublicationIdentity(finding *FindingLifecycle, repository string, observation Observation) {
	finding.PublicationRole = observation.PublicationRole
	finding.RootCauseKey = observation.RootCauseKey
	finding.BlockedByRootKeys = append([]string(nil), observation.BlockedByRootKeys...)
	finding.PublicationFingerprint = publicationFingerprint(repository, observation.RootCauseKey, observation.RepositoryFingerprint)
}

func publicationFingerprint(repository, rootCauseKey, legacyFingerprint string) string {
	if rootCauseKey == "" {
		return legacyFingerprint
	}
	return digest([]byte(strings.ToLower(strings.TrimSpace(repository)) + "\x00" + rootCauseKey))
}

func observationNeedsHumanReview(observation Observation) bool {
	title := strings.ToLower(strings.TrimSpace(observation.Title))
	if strings.Contains(title, "missing baseline") || strings.Contains(title, "missing_baseline") {
		return true
	}
	for _, label := range observation.Labels {
		normalized := strings.ToLower(strings.TrimSpace(label))
		if normalized == "stale-baseline" || normalized == "baseline-review" || normalized == "visual-hive/baseline-review" {
			return true
		}
	}
	return false
}

func repairSignalRank(observation Observation) int {
	if observationNeedsHumanReview(observation) {
		return 0
	}
	title := strings.ToLower(strings.TrimSpace(observation.Title))
	if strings.Contains(title, "console_error") || strings.Contains(title, "console error") || strings.Contains(title, "network_error") || strings.Contains(title, "network error") {
		return 6
	}
	if strings.Contains(title, "failed deterministic validation") || strings.Contains(title, "contract_result") || strings.Contains(title, "contract result") {
		return 5
	}
	if observation.IssueKind == "missing_visual_coverage" && strings.Contains(title, "storybook-discovery:") {
		return 4
	}
	if observation.IssueKind == "missing_visual_coverage" {
		for _, contract := range observation.AffectedContracts {
			if strings.TrimSpace(contract) != "" {
				return 3
			}
		}
		// Generic coverage/readiness gaps without an exact affected contract
		// remain useful advisory issues, but they do not outrank a bounded,
		// repository-specific repair candidate in repair modes.
	}
	return issueKindRank(observation.IssueKind)
}

func severityRank(value string) int {
	switch value {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	default:
		return 1
	}
}

func issueKindRank(value string) int {
	switch value {
	case RepositoryTestFailureKind:
		// A failing repository plan blocks every repair PR check and therefore
		// must consume a bounded WIP slot before non-blocking quality findings.
		return 5
	case "visual_regression", "selector_contract_failure", "screenshot_diff", "mutation_survivor", "test_adequacy_gap", "accessibility_failure", "console_error", "network_error", "security_failure":
		return 4
	case "workflow_safety", "provider_governance":
		return 3
	case "weak_visual_test", "missing_visual_coverage":
		return 2
	default:
		return 1
	}
}

func (s *LifecycleStore) PendingOutbox() []OutboxEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]OutboxEntry, 0)
	for _, entry := range s.state.Outbox {
		if entry.CompletedAt == nil {
			entries = append(entries, *entry)
		}
	}
	return entries
}

// QueueAdmittedIssueAction creates the existing Hive-owned issue outbox intent
// only after the normal Governor has durably admitted the controller bead.
// Replays are idempotent by the existing content-addressed outbox ID.
func (s *LifecycleStore) QueueAdmittedIssueAction(repositoryFingerprint string, packet VerifiedPacketIdentity) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	finding := s.state.Findings[repositoryFingerprint]
	if finding == nil {
		return false, fmt.Errorf("finding %s is not present", repositoryFingerprint)
	}
	if finding.LastBundleDigest != packet.PacketDigest || finding.LastBundleID != packet.PacketID {
		return false, fmt.Errorf("finding does not match the admitted verified packet")
	}
	action := finding.PendingIssueAction
	if action == "" {
		return false, nil
	}
	if action != OutboxOpenIssue && finding.IssueNumber <= 0 {
		return false, fmt.Errorf("%s action requires an existing issue identity", action)
	}
	manifest := Manifest{
		BundleID: packet.PacketID, OverallDigest: packet.PacketDigest,
		Source: Source{CommitSHA: packet.BaseSHA, WorkflowRunID: packet.WorkflowRunID},
	}
	now := time.Now().UTC()
	entry := outboxForFinding(action, finding, manifest, now)
	backup := cloneLifecycleState(s.state)
	if !s.enqueueLocked(entry) {
		return false, nil
	}
	if err := s.auditLockedStrict(LifecycleAuditEntry{
		Action: "queue_admitted_issue", Allowed: true, Repository: finding.Repository,
		RepositoryFingerprint: repositoryFingerprint, BundleID: packet.PacketID,
		Detail: "normal Governor admission durably preceded issue intent",
	}); err != nil {
		s.state = backup
		return false, err
	}
	s.state.UpdatedAt = now
	s.sortStateLocked()
	if err := s.persistLocked(); err != nil {
		s.state = backup
		return false, err
	}
	return true, nil
}

// CancelPendingIssuePublication durably consumes issue side effects when an
// operator downgrades the installation to advisory authority. Findings and
// beads remain intact; no GitHub mutation is attempted or implied.
func (s *LifecycleStore) CancelPendingIssuePublication(reason string) (int, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return 0, fmt.Errorf("outbox cancellation reason is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	backup := cloneLifecycleState(s.state)
	now := time.Now().UTC()
	cancelled := 0
	for _, entry := range s.state.Outbox {
		if entry == nil || entry.CompletedAt != nil {
			continue
		}
		entry.CompletedAt = &now
		entry.LastError = "cancelled without publication: " + truncate(reason, 1900)
		if err := s.auditLockedStrict(LifecycleAuditEntry{
			Action: "cancel_issue_publication", Allowed: true, Repository: entry.Repository,
			RepositoryFingerprint: entry.RepositoryFingerprint, BundleID: entry.BundleID,
			Detail: truncate(reason, 4096),
		}); err != nil {
			s.state = backup
			return 0, err
		}
		cancelled++
	}
	if cancelled == 0 {
		return 0, nil
	}
	s.state.UpdatedAt = now
	if err := s.persistLocked(); err != nil {
		s.state = backup
		return 0, err
	}
	return cancelled, nil
}

func (s *LifecycleStore) Snapshot() LifecycleState {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(s.state)
	if err != nil {
		return LifecycleState{SchemaVersion: LifecycleSchema, Findings: map[string]*FindingLifecycle{}, ReplayKeys: map[string]string{}, Outbox: []*OutboxEntry{}}
	}
	var snapshot LifecycleState
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return LifecycleState{SchemaVersion: LifecycleSchema, Findings: map[string]*FindingLifecycle{}, ReplayKeys: map[string]string{}, Outbox: []*OutboxEntry{}}
	}
	return snapshot
}

func (s *LifecycleStore) MarkOutboxAttempt(id string, actionErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	backup := cloneLifecycleState(s.state)
	entry := s.findOutboxLocked(id)
	if entry == nil {
		return fmt.Errorf("outbox entry %s not found", id)
	}
	entry.Attempts++
	if actionErr != nil {
		entry.LastError = truncate(actionErr.Error(), 2048)
	} else {
		now := time.Now().UTC()
		entry.LastError = ""
		entry.CompletedAt = &now
		if finding := s.state.Findings[entry.RepositoryFingerprint]; finding != nil && finding.PendingIssueAction == entry.Action {
			finding.PendingIssueAction = ""
		}
	}
	detail := fmt.Sprintf("outbox=%s action=%s attempt=%d completed=%t", entry.ID, entry.Action, entry.Attempts, actionErr == nil)
	if actionErr != nil {
		detail += " error=" + entry.LastError
	}
	if err := s.auditLockedStrict(LifecycleAuditEntry{Action: "outbox_" + string(entry.Action), Allowed: actionErr == nil, Repository: entry.Repository, RepositoryFingerprint: entry.RepositoryFingerprint, BundleID: entry.BundleID, Detail: detail}); err != nil {
		s.state = backup
		return err
	}
	s.state.UpdatedAt = time.Now().UTC()
	if err := s.persistLocked(); err != nil {
		s.state = backup
		return err
	}
	return nil
}

func (s *LifecycleStore) RecordAuthorization(repositoryFingerprint, action string, allowed bool, detail string) {
	_ = s.RecordAuthorizationStrict(repositoryFingerprint, action, allowed, detail)
}

func (s *LifecycleStore) RecordAuthorizationStrict(repositoryFingerprint, action string, allowed bool, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	finding := s.state.Findings[repositoryFingerprint]
	entry := LifecycleAuditEntry{Action: "authorize_" + action, Allowed: allowed, RepositoryFingerprint: repositoryFingerprint, Detail: truncate(detail, 4096)}
	if finding != nil {
		entry.Repository = finding.Repository
		entry.BundleID = finding.LastBundleID
	}
	return s.auditLockedStrict(entry)
}

// BindIssueWriter durably records the immutable author identity used to own a
// lifecycle issue before any remote issue mutation. Authentication may rotate,
// but issue discovery, update, closure, and uninstall remain bound to this
// original GitHub user identity.
func (s *LifecycleStore) BindIssueWriter(repositoryFingerprint string, writerID int64, writerLogin string) error {
	writerLogin = strings.TrimSpace(writerLogin)
	if writerID <= 0 || writerLogin == "" {
		return fmt.Errorf("immutable lifecycle issue writer ID and login are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	backup := cloneLifecycleState(s.state)
	finding := s.state.Findings[repositoryFingerprint]
	if finding == nil {
		return fmt.Errorf("finding %s not found", repositoryFingerprint)
	}
	if finding.IssueWriterID > 0 || strings.TrimSpace(finding.IssueWriterLogin) != "" {
		if finding.IssueWriterID != writerID || !strings.EqualFold(strings.TrimSpace(finding.IssueWriterLogin), writerLogin) {
			return fmt.Errorf("finding %s is already bound to immutable lifecycle issue writer %s/%d", repositoryFingerprint, finding.IssueWriterLogin, finding.IssueWriterID)
		}
		return nil
	}
	finding.IssueWriterID, finding.IssueWriterLogin = writerID, writerLogin
	if err := s.auditLockedStrict(LifecycleAuditEntry{Action: "bind_issue_writer", Allowed: true, Repository: finding.Repository, RepositoryFingerprint: repositoryFingerprint, BundleID: finding.LastBundleID, Detail: fmt.Sprintf("writer=%s/%d", writerLogin, writerID)}); err != nil {
		s.state = backup
		return err
	}
	s.state.UpdatedAt = time.Now().UTC()
	if err := s.persistLocked(); err != nil {
		s.state = backup
		return err
	}
	return nil
}

func (s *LifecycleStore) MarkIssueOpened(repositoryFingerprint string, number int, issueURL string) error {
	return s.updateFinding(repositoryFingerprint, "issue_opened", func(finding *FindingLifecycle) error {
		if number <= 0 || strings.TrimSpace(issueURL) == "" {
			return fmt.Errorf("GitHub issue number and URL are required")
		}
		finding.IssueNumber, finding.IssueURL = number, issueURL
		if finding.Status == StatusDetected || finding.Status == StatusResolved || finding.Status == StatusIssueClosed {
			finding.Status = StatusIssueOpen
		}
		return nil
	})
}

func (s *LifecycleStore) MarkIssueMarkerMigrated(repositoryFingerprint, previousMarker, marker string, number int, issueURL string) error {
	return s.updateFinding(repositoryFingerprint, "issue_marker_migrated", func(finding *FindingLifecycle) error {
		if number <= 0 || number != finding.IssueNumber || strings.TrimSpace(issueURL) == "" {
			return fmt.Errorf("migrated GitHub issue identity does not match the persisted lifecycle issue")
		}
		expectedMarker := lifecycleMarker(finding.PublicationFingerprint)
		if marker != expectedMarker {
			return fmt.Errorf("migrated GitHub issue marker does not match the stable publication identity")
		}
		if finding.PendingMarkerMigrationFrom == "" {
			if previousMarker != lifecycleMarker(finding.RepositoryFingerprint) {
				return fmt.Errorf("completed marker migration does not match the preserved legacy identity")
			}
			finding.IssueURL = issueURL
			return nil
		}
		if previousMarker != finding.PendingMarkerMigrationFrom || previousMarker == marker {
			return fmt.Errorf("marker migration does not match the pending exact legacy marker")
		}
		finding.PendingMarkerMigrationFrom = ""
		finding.IssueURL = issueURL
		return nil
	})
}

// RebindResolvedIssue records GitHub's canonical exact-marker issue after a
// duplicate-issue reconciliation without reopening the already verified
// finding. The identity is persisted before the local closed transition so a
// crash can safely retry the remote close against the canonical issue.
func (s *LifecycleStore) RebindResolvedIssue(repositoryFingerprint string, number int, issueURL string) error {
	return s.updateFinding(repositoryFingerprint, "resolved_issue_rebound", func(finding *FindingLifecycle) error {
		if finding.Status != StatusResolved {
			return fmt.Errorf("cannot rebind issue identity from %s", finding.Status)
		}
		if number <= 0 || strings.TrimSpace(issueURL) == "" {
			return fmt.Errorf("canonical GitHub issue number and URL are required")
		}
		finding.IssueNumber, finding.IssueURL = number, issueURL
		return nil
	})
}

func (s *LifecycleStore) MarkRepairStarted(repositoryFingerprint, branch string) error {
	return s.updateFinding(repositoryFingerprint, "repair_started", func(finding *FindingLifecycle) error {
		if finding.Status == StatusRepairRunning && strings.TrimSpace(finding.Branch) == strings.TrimSpace(branch) {
			return nil
		}
		if finding.Status != StatusIssueOpen && finding.Status != StatusFixQueued && finding.Status != StatusNeedsRevision && finding.Status != StatusRepairRunning {
			return fmt.Errorf("cannot start repair from %s", finding.Status)
		}
		if strings.TrimSpace(branch) == "" {
			return fmt.Errorf("repair branch is required")
		}
		finding.Branch, finding.Status = branch, StatusRepairRunning
		finding.MergeSHA, finding.ValidationRunID, finding.ValidationRunURL = "", "", ""
		finding.RepairAttempts++
		return nil
	})
}

// MarkRepairRetry spends one bounded attempt while preserving the exact open
// branch and PR. A retry must never create a second active repair PR merely
// because a model returned no incremental patch.
func (s *LifecycleStore) MarkRepairRetry(repositoryFingerprint, branch string) error {
	return s.updateFinding(repositoryFingerprint, "repair_retried", func(finding *FindingLifecycle) error {
		branch = strings.TrimSpace(branch)
		if (finding.Status != StatusRepairRunning && finding.Status != StatusNeedsRevision) || finding.PRNumber <= 0 {
			return fmt.Errorf("cannot retry open repair from %s", finding.Status)
		}
		if branch == "" || strings.TrimSpace(finding.Branch) != branch {
			return fmt.Errorf("retry must preserve the exact repair branch")
		}
		finding.Status = StatusRepairRunning
		finding.RepairAttempts++
		return nil
	})
}

func (s *LifecycleStore) MarkPROpen(repositoryFingerprint, commitSHA string, number int, prURL string) error {
	return s.updateFinding(repositoryFingerprint, "pr_opened", func(finding *FindingLifecycle) error {
		if finding.Status != StatusRepairRunning && finding.Status != StatusNeedsRevision {
			return fmt.Errorf("cannot open PR from %s", finding.Status)
		}
		if strings.TrimSpace(commitSHA) == "" || number <= 0 || strings.TrimSpace(prURL) == "" {
			return fmt.Errorf("repair commit, PR number, and PR URL are required")
		}
		finding.RepairCommitSHA, finding.PRNumber, finding.PRURL, finding.Status = commitSHA, number, prURL, StatusPROpen
		finding.LastCheckSummary, finding.LastCheckRuns, finding.LastPullRequestCheckReceipt = "", nil, nil
		return nil
	})
}

func (s *LifecycleStore) MarkManualReviewRequired(repositoryFingerprint, kind, reason string) error {
	return s.updateFinding(repositoryFingerprint, "manual_review_required", func(finding *FindingLifecycle) error {
		if finding.Status != StatusIssueOpen && finding.Status != StatusFixQueued && finding.Status != StatusRepairRunning && finding.Status != StatusPROpen && finding.Status != StatusChecksRunning && finding.Status != StatusNeedsRevision && finding.Status != StatusReady {
			return fmt.Errorf("cannot require manual review from %s", finding.Status)
		}
		kind, reason = strings.TrimSpace(kind), strings.TrimSpace(reason)
		if kind == "" || reason == "" {
			return fmt.Errorf("manual review kind and reason are required")
		}
		finding.ManualReviewKind, finding.ManualReviewReason, finding.HumanReviewRequired = kind, truncate(reason, 4096), true
		return nil
	})
}

func (s *LifecycleStore) MarkManualReviewComplete(repositoryFingerprint, kind string) error {
	return s.updateFinding(repositoryFingerprint, "manual_review_completed", func(finding *FindingLifecycle) error {
		if strings.TrimSpace(kind) == "" || finding.ManualReviewKind != strings.TrimSpace(kind) {
			return fmt.Errorf("manual review kind does not match pending review")
		}
		finding.ManualReviewKind, finding.ManualReviewReason = "", ""
		finding.HumanReviewRequired = finding.ObservationHumanReviewRequired
		return nil
	})
}

func (s *LifecycleStore) MarkPRHeadUpdated(repositoryFingerprint, commitSHA string) error {
	return s.updateFinding(repositoryFingerprint, "pr_head_updated", func(finding *FindingLifecycle) error {
		if finding.Status != StatusPROpen && finding.Status != StatusChecksRunning && finding.Status != StatusNeedsRevision && finding.Status != StatusReady {
			return fmt.Errorf("cannot update PR head from %s", finding.Status)
		}
		if strings.TrimSpace(commitSHA) == "" || finding.PRNumber <= 0 {
			return fmt.Errorf("updated PR head and persisted PR are required")
		}
		finding.RepairCommitSHA, finding.Status = strings.TrimSpace(commitSHA), StatusPROpen
		finding.LastCheckSummary = ""
		finding.LastCheckRuns = nil
		finding.LastPullRequestCheckReceipt = nil
		return nil
	})
}

func (s *LifecycleStore) MarkChecks(repositoryFingerprint, testedSHA string, allGreen bool) error {
	return s.MarkChecksWithEvidence(repositoryFingerprint, testedSHA, allGreen, "", nil)
}

// ApplySealedPullRequestCheckReceipt is the replay-aware check update seam.
// Its parameter is an internal opaque capability minted only after live GitHub
// verification; a persisted or caller-built snapshot cannot be supplied here.
func (s *LifecycleStore) ApplySealedPullRequestCheckReceipt(repositoryFingerprint string, sealed visualhivepr.SealedReceipt) (ApplyPullRequestCheckUpdateResult, error) {
	identityBytes, receiptSHA256, sealedReplayKey, err := sealed.Open()
	if err != nil {
		return ApplyPullRequestCheckUpdateResult{}, err
	}
	var identity PullRequestCheckReceiptIdentity
	if err := json.Unmarshal(identityBytes, &identity); err != nil {
		return ApplyPullRequestCheckUpdateResult{}, fmt.Errorf("decode sealed PR check receipt identity: %w", err)
	}
	snapshot := PullRequestCheckReceiptSnapshot{Identity: identity, ReceiptSHA256: receiptSHA256}
	if err := validatePullRequestCheckIdentity(snapshot.Identity); err != nil {
		return ApplyPullRequestCheckUpdateResult{}, err
	}
	if snapshot.Identity.ReplayKey != sealedReplayKey {
		return ApplyPullRequestCheckUpdateResult{}, fmt.Errorf("sealed PR check receipt replay identity does not match its content")
	}
	stableIdentity, err := clonePullRequestCheckIdentity(snapshot.Identity)
	if err != nil {
		return ApplyPullRequestCheckUpdateResult{}, err
	}
	snapshot.Identity = stableIdentity
	repositoryFingerprint = strings.TrimSpace(repositoryFingerprint)
	if repositoryFingerprint == "" {
		return ApplyPullRequestCheckUpdateResult{}, fmt.Errorf("repository fingerprint is required for PR check receipt application")
	}
	identity = snapshot.Identity
	result := ApplyPullRequestCheckUpdateResult{ReceiptSHA256: snapshot.ReceiptSHA256, ReplayKey: identity.ReplayKey}
	replayStateKey := pullRequestCheckReplayNamespace + repositoryFingerprint + ":" + identity.ReplayKey

	s.mu.Lock()
	defer s.mu.Unlock()

	// Replay intentionally precedes finding/status checks. The lifecycle may
	// already be Ready on an exact retry, while a conflicting re-seal for the
	// same run/artifact tuple must fail regardless of its current status.
	if prior, exists := s.state.ReplayKeys[replayStateKey]; exists {
		if prior != snapshot.ReceiptSHA256 {
			if auditErr := s.auditLockedStrict(LifecycleAuditEntry{
				Action: "pull_request_check_receipt_replay", Allowed: false, Repository: identity.Source.Repository,
				RepositoryFingerprint: repositoryFingerprint, BundleID: identity.Bundle.BundleID,
				Detail: "PR check receipt replay key reused with a different sealed receipt",
			}); auditErr != nil {
				return result, auditErr
			}
			return result, fmt.Errorf("PR check receipt replay key was reused with a different sealed receipt")
		}
		result.Idempotent = true
		if auditErr := s.auditLockedStrict(LifecycleAuditEntry{
			Action: "pull_request_check_receipt_replay", Allowed: true, Repository: identity.Source.Repository,
			RepositoryFingerprint: repositoryFingerprint, BundleID: identity.Bundle.BundleID, Detail: "idempotent sealed receipt retry",
		}); auditErr != nil {
			return result, auditErr
		}
		return result, nil
	}

	finding := s.state.Findings[repositoryFingerprint]
	if finding == nil {
		return result, fmt.Errorf("finding %s not found", repositoryFingerprint)
	}
	headBranch := identity.Source.Head.Ref
	if !strings.EqualFold(finding.Repository, identity.Source.Repository) || finding.RepositoryID != identity.Source.RepositoryID ||
		finding.PRNumber != identity.Source.PullRequest || finding.RepairCommitSHA != identity.Source.Head.SHA || finding.Branch != headBranch {
		detail := "receipt repository, pull request, branch, or repair SHA does not match the exact finding"
		if auditErr := s.auditLockedStrict(LifecycleAuditEntry{
			Action: "pull_request_check_receipt_applied", Allowed: false, Repository: finding.Repository,
			RepositoryFingerprint: repositoryFingerprint, BundleID: identity.Bundle.BundleID, Detail: detail,
		}); auditErr != nil {
			return result, auditErr
		}
		return result, fmt.Errorf("%s", detail)
	}
	if finding.Status != StatusPROpen && finding.Status != StatusChecksRunning && finding.Status != StatusNeedsRevision {
		detail := fmt.Sprintf("cannot apply PR check receipt from %s", finding.Status)
		if auditErr := s.auditLockedStrict(LifecycleAuditEntry{
			Action: "pull_request_check_receipt_applied", Allowed: false, Repository: finding.Repository,
			RepositoryFingerprint: repositoryFingerprint, BundleID: identity.Bundle.BundleID, Detail: detail,
		}); auditErr != nil {
			return result, auditErr
		}
		return result, fmt.Errorf("%s", detail)
	}
	if identity.Authority != (PullRequestCheckAuthority{CheckEvidenceOnly: true}) {
		return result, fmt.Errorf("PR receipt authority is not restricted to check evidence")
	}

	workflowID, _ := strconv.ParseInt(identity.Workflow.ID, 10, 64)
	workflowRunID, _ := strconv.ParseInt(identity.Workflow.RunID, 10, 64)
	workflowRunAttempt, _ := strconv.Atoi(identity.Workflow.RunAttempt)
	sourceArtifactID, _ := strconv.ParseInt(identity.SourceArtifact.ID, 10, 64)
	bundleArtifactID, _ := strconv.ParseInt(identity.BundleArtifact.ID, 10, 64)
	backup := cloneLifecycleState(s.state)
	finding.Status = StatusReady
	finding.LastCheckSummary = identity.Check.Summary
	finding.LastCheckRuns = []CheckEvidence{{
		Name: identity.Workflow.Name, State: identity.Check.State, URL: identity.Check.RunURL, ProvenanceVerified: true,
		WorkflowID: workflowID, WorkflowRunID: workflowRunID, WorkflowRunAttempt: workflowRunAttempt,
		WorkflowPath: identity.Workflow.Path, WorkflowEvent: identity.Workflow.Event, WorkflowDefinitionSHA256: identity.Workflow.Definition.SHA256,
		PullRequestNumber: identity.Source.PullRequest, CommitSHA: identity.Source.Head.SHA,
		SourceArtifactID: sourceArtifactID, SourceArtifactName: identity.SourceArtifact.Name,
		BundleArtifactID: bundleArtifactID, BundleArtifactName: identity.BundleArtifact.Name,
		ReceiptReplayKey: identity.ReplayKey, ReceiptSHA256: snapshot.ReceiptSHA256,
	}}
	storedSnapshot := snapshot
	finding.LastPullRequestCheckReceipt = &storedSnapshot
	s.state.ReplayKeys[replayStateKey] = snapshot.ReceiptSHA256
	s.state.UpdatedAt = time.Now().UTC()
	s.sortStateLocked()
	if auditErr := s.auditLockedStrict(LifecycleAuditEntry{
		Action: "pull_request_check_receipt_applied", Allowed: true, Repository: finding.Repository,
		RepositoryFingerprint: repositoryFingerprint, BundleID: identity.Bundle.BundleID,
		Detail: fmt.Sprintf("status=%s pr=%d repair=%s run=%s attempt=%s receipt=%s", finding.Status, finding.PRNumber, finding.RepairCommitSHA, identity.Workflow.RunID, identity.Workflow.RunAttempt, snapshot.ReceiptSHA256),
	}); auditErr != nil {
		s.state = backup
		return result, auditErr
	}
	if err := s.persistLocked(); err != nil {
		s.state = backup
		return result, err
	}
	return result, nil
}

func (s *LifecycleStore) MarkChecksWithEvidence(repositoryFingerprint, testedSHA string, allGreen bool, summary string, runs []CheckEvidence) error {
	return s.updateFinding(repositoryFingerprint, "checks_evaluated", func(finding *FindingLifecycle) error {
		if finding.Status != StatusPROpen && finding.Status != StatusChecksRunning && finding.Status != StatusNeedsRevision {
			return fmt.Errorf("cannot evaluate checks from %s", finding.Status)
		}
		if testedSHA == "" || testedSHA != finding.RepairCommitSHA {
			return fmt.Errorf("checks do not apply to the exact repair SHA")
		}
		if allGreen {
			finding.Status = StatusReady
		} else {
			finding.Status = StatusNeedsRevision
		}
		finding.LastCheckSummary = strings.TrimSpace(summary)
		finding.LastCheckRuns = append([]CheckEvidence(nil), runs...)
		return nil
	})
}

func (s *LifecycleStore) MarkMerged(repositoryFingerprint, mergeSHA string) error {
	return s.updateFinding(repositoryFingerprint, "pr_merged", func(finding *FindingLifecycle) error {
		if finding.Status != StatusReady {
			return fmt.Errorf("cannot merge from %s", finding.Status)
		}
		if strings.TrimSpace(mergeSHA) == "" {
			return fmt.Errorf("merge SHA is required")
		}
		finding.MergeSHA, finding.Status = mergeSHA, StatusMerged
		return nil
	})
}

// MarkRecoveredMerge records the exact merge for lifecycle state written by a
// previous Hive version that closed a repaired finding before reconciling its
// externally reviewed PR. The finding stays resolved/closed; this method only
// repairs the durable chain and clears a no-longer-actionable merge hold.
func (s *LifecycleStore) MarkRecoveredMerge(repositoryFingerprint, mergeSHA string) error {
	return s.updateFinding(repositoryFingerprint, "external_merge_recovered", func(finding *FindingLifecycle) error {
		if finding.Status != StatusResolved && finding.Status != StatusIssueClosed {
			return fmt.Errorf("cannot recover merge from %s", finding.Status)
		}
		if finding.PRNumber <= 0 || strings.TrimSpace(finding.RepairCommitSHA) == "" || strings.TrimSpace(mergeSHA) == "" {
			return fmt.Errorf("recovered merge requires a repair PR, exact repair commit, and merge SHA")
		}
		if finding.MergeSHA != "" && finding.MergeSHA != strings.TrimSpace(mergeSHA) {
			return fmt.Errorf("recovered merge SHA conflicts with persisted merge %s", finding.MergeSHA)
		}
		finding.MergeSHA = strings.TrimSpace(mergeSHA)
		finding.ManualReviewKind, finding.ManualReviewReason, finding.HumanReviewRequired = "", "", false
		return nil
	})
}

func (s *LifecycleStore) MarkPostMergeVerifying(repositoryFingerprint, runID, runURL string) error {
	return s.updateFinding(repositoryFingerprint, "post_merge_verifying", func(finding *FindingLifecycle) error {
		if finding.Status != StatusMerged && finding.Status != StatusPostMergeVerifying {
			return fmt.Errorf("cannot start post-merge verification from %s", finding.Status)
		}
		finding.ValidationRunID, finding.ValidationRunURL, finding.Status = runID, runURL, StatusPostMergeVerifying
		return nil
	})
}

// ResetStalePostMergeVerification restores the durable merge state after Hive
// proves that a bound verification run is no longer the live default head. The
// reset is exact-run bound so it cannot undo a different verification, and any
// close prepared by that stale run is completed locally without reaching
// GitHub. A subsequent exact-head run can then start from StatusMerged.
func (s *LifecycleStore) ResetStalePostMergeVerification(repositoryFingerprint, runID, commitSHA string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	backup := cloneLifecycleState(s.state)
	finding := s.state.Findings[repositoryFingerprint]
	if finding == nil {
		return fmt.Errorf("finding %s not found", repositoryFingerprint)
	}
	runID = strings.TrimSpace(runID)
	commitSHA = strings.TrimSpace(commitSHA)
	if runID == "" || commitSHA == "" || strings.TrimSpace(finding.MergeSHA) == "" {
		return fmt.Errorf("stale post-merge reset requires an exact run, commit, and recorded merge")
	}
	if finding.Status != StatusPostMergeVerifying && finding.Status != StatusResolved {
		return fmt.Errorf("cannot reset stale post-merge verification from %s", finding.Status)
	}
	if finding.ValidationRunID != runID {
		return fmt.Errorf("stale post-merge run %s does not match recorded verification %s", runID, finding.ValidationRunID)
	}
	if finding.Status == StatusResolved && finding.LastWorkflowRunID != runID {
		return fmt.Errorf("resolved finding was not produced by stale post-merge run %s", runID)
	}

	now := time.Now().UTC()
	matchingCloses := 0
	for _, entry := range s.state.Outbox {
		if entry == nil || entry.CompletedAt != nil || entry.Action != OutboxCloseIssue || entry.RepositoryFingerprint != repositoryFingerprint || entry.BundleDigest != finding.LastBundleDigest {
			continue
		}
		if entry.Evidence["workflow_run_id"] != runID || !strings.EqualFold(strings.TrimSpace(entry.Evidence["commit_sha"]), commitSHA) {
			continue
		}
		matchingCloses++
		entry.CompletedAt = &now
		entry.LastError = ""
	}
	if finding.Status == StatusResolved && matchingCloses != 1 {
		s.state = backup
		return fmt.Errorf("resolved stale post-merge run must match exactly one pending close; matched %d", matchingCloses)
	}
	if matchingCloses > 1 {
		s.state = backup
		return fmt.Errorf("stale post-merge run matched multiple pending closes")
	}
	finding.Status = StatusMerged
	finding.ResolvedAt = nil
	finding.ClosedAt = nil
	finding.ValidationRunID = ""
	finding.ValidationRunURL = ""
	if err := s.auditLockedStrict(LifecycleAuditEntry{
		Action: "reset_stale_post_merge_verification", Allowed: true, Repository: finding.Repository,
		RepositoryFingerprint: repositoryFingerprint, BundleID: finding.LastBundleID,
		Detail: fmt.Sprintf("discarded stale verification run %s at %s", runID, commitSHA),
	}); err != nil {
		s.state = backup
		return err
	}
	s.state.UpdatedAt = now
	if err := s.persistLocked(); err != nil {
		s.state = backup
		return err
	}
	return nil
}

func (s *LifecycleStore) MarkPostMergeFailed(repositoryFingerprint, summary string) error {
	return s.updateFinding(repositoryFingerprint, "post_merge_failed", func(finding *FindingLifecycle) error {
		if finding.Status != StatusPostMergeVerifying {
			return fmt.Errorf("cannot fail post-merge verification from %s", finding.Status)
		}
		if strings.TrimSpace(finding.ValidationRunID) == "" || finding.LastWorkflowRunID != finding.ValidationRunID {
			return fmt.Errorf("post-merge failure requires evidence from the recorded verification run")
		}
		finding.Status = StatusNeedsRevision
		finding.LastCheckSummary = strings.TrimSpace(summary)
		return nil
	})
}

func (s *LifecycleStore) MarkIssueClosed(repositoryFingerprint string, beadStore LifecycleBeadSink) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	backup := cloneLifecycleState(s.state)
	finding := s.state.Findings[repositoryFingerprint]
	if finding == nil {
		return fmt.Errorf("finding %s not found", repositoryFingerprint)
	}
	if finding.Status != StatusResolved {
		if err := s.auditLockedStrict(LifecycleAuditEntry{Action: "issue_closed", Allowed: false, Repository: finding.Repository, RepositoryFingerprint: repositoryFingerprint, BundleID: finding.LastBundleID, Detail: "finding is not resolved"}); err != nil {
			return err
		}
		return fmt.Errorf("cannot close issue from %s", finding.Status)
	}
	now := time.Now().UTC()
	finding.Status, finding.ClosedAt = StatusIssueClosed, &now
	finding.ManualReviewKind, finding.ManualReviewReason, finding.HumanReviewRequired = "", "", false
	if err := s.auditLockedStrict(LifecycleAuditEntry{Action: "issue_closed", Allowed: true, Repository: finding.Repository, RepositoryFingerprint: repositoryFingerprint, BundleID: finding.LastBundleID, Detail: fmt.Sprintf("issue=%d verification_run=%s merge=%s", finding.IssueNumber, finding.ValidationRunID, finding.MergeSHA)}); err != nil {
		s.state = backup
		return err
	}
	if finding.BeadID != "" {
		if err := beadStore.Close(finding.BeadID); err != nil {
			s.state = backup
			return err
		}
	}
	s.state.UpdatedAt = now
	if err := s.persistLocked(); err != nil {
		s.state = backup
		return err
	}
	return nil
}

func (s *LifecycleStore) Finding(repositoryFingerprint string) (FindingLifecycle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	finding := s.state.Findings[repositoryFingerprint]
	if finding == nil {
		return FindingLifecycle{}, false
	}
	return *finding, true
}

func (s *LifecycleStore) updateFinding(repositoryFingerprint, action string, update func(*FindingLifecycle) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	backup := cloneLifecycleState(s.state)
	finding := s.state.Findings[repositoryFingerprint]
	if finding == nil {
		return fmt.Errorf("finding %s not found", repositoryFingerprint)
	}
	if err := update(finding); err != nil {
		if auditErr := s.auditLockedStrict(LifecycleAuditEntry{Action: action, Allowed: false, Repository: finding.Repository, RepositoryFingerprint: repositoryFingerprint, BundleID: finding.LastBundleID, Detail: err.Error()}); auditErr != nil {
			return auditErr
		}
		return err
	}
	detail := fmt.Sprintf("status=%s issue=%d pr=%d repair=%s merge=%s validation_run=%s bundle_digest=%s", finding.Status, finding.IssueNumber, finding.PRNumber, finding.RepairCommitSHA, finding.MergeSHA, finding.ValidationRunID, finding.LastBundleDigest)
	if err := s.auditLockedStrict(LifecycleAuditEntry{Action: action, Allowed: true, Repository: finding.Repository, RepositoryFingerprint: repositoryFingerprint, BundleID: finding.LastBundleID, Detail: detail}); err != nil {
		s.state = backup
		return err
	}
	s.state.UpdatedAt = time.Now().UTC()
	if err := s.persistLocked(); err != nil {
		s.state = backup
		return err
	}
	return nil
}

func (s *LifecycleStore) enqueueLocked(entry OutboxEntry) bool {
	for _, existing := range s.state.Outbox {
		if existing.ID == entry.ID {
			return false
		}
	}
	s.state.Outbox = append(s.state.Outbox, &entry)
	return true
}

func (s *LifecycleStore) findOutboxLocked(id string) *OutboxEntry {
	for _, entry := range s.state.Outbox {
		if entry.ID == id {
			return entry
		}
	}
	return nil
}

func (s *LifecycleStore) load() error {
	data, err := os.ReadFile(s.statePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read lifecycle state: %w", err)
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return fmt.Errorf("parse lifecycle state: %w", err)
	}
	migrated := false
	if s.state.SchemaVersion == LifecycleSchemaV1 {
		s.state.SchemaVersion = LifecycleSchema
		for _, finding := range s.state.Findings {
			if finding != nil {
				finding.ObservationHumanReviewRequired = finding.HumanReviewRequired
			}
		}
		migrated = true
	} else if s.state.SchemaVersion != LifecycleSchema {
		return fmt.Errorf("unsupported lifecycle state schema %q", s.state.SchemaVersion)
	}
	if s.state.Findings == nil {
		s.state.Findings = map[string]*FindingLifecycle{}
	}
	if s.state.ReplayKeys == nil {
		s.state.ReplayKeys = map[string]string{}
	}
	if s.state.Outbox == nil {
		s.state.Outbox = []*OutboxEntry{}
	}
	for _, finding := range s.state.Findings {
		if finding != nil && (finding.IssueWriterID > 0) != (strings.TrimSpace(finding.IssueWriterLogin) != "") {
			return fmt.Errorf("lifecycle finding %s has a partial immutable issue writer identity", finding.RepositoryFingerprint)
		}
		if finding != nil && finding.Status == StatusIssueClosed && (finding.HumanReviewRequired || finding.ManualReviewKind != "" || finding.ManualReviewReason != "") {
			finding.HumanReviewRequired, finding.ManualReviewKind, finding.ManualReviewReason = false, "", ""
			migrated = true
		}
	}
	if migrated {
		if err := s.persistLocked(); err != nil {
			return fmt.Errorf("migrate lifecycle state: %w", err)
		}
	}
	return nil
}

func (s *LifecycleStore) persistLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lifecycle state: %w", err)
	}
	temporary := s.statePath + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open lifecycle state transaction: %w", err)
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("write lifecycle state: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync lifecycle state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close lifecycle state transaction: %w", err)
	}
	if err := durableReplaceLifecycleFile(temporary, s.statePath); err != nil {
		return fmt.Errorf("publish lifecycle state: %w", err)
	}
	removeTemporary = false
	return nil
}

func (s *LifecycleStore) auditLockedStrict(entry LifecycleAuditEntry) error {
	entry.Timestamp = time.Now().UTC()
	entry.ReceiptID = lifecycleAuditReceipt(entry)
	if s.auditReceipts[entry.ReceiptID] {
		return nil
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal lifecycle audit receipt: %w", err)
	}
	info, statErr := os.Lstat(s.auditPath)
	if statErr == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("Visual Hive lifecycle audit path is linked or non-regular")
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect lifecycle audit path: %w", statErr)
	}
	created := errors.Is(statErr, os.ErrNotExist)
	file, err := os.OpenFile(s.auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open lifecycle audit: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("write lifecycle audit receipt: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync lifecycle audit receipt: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close lifecycle audit receipt: %w", err)
	}
	if created {
		if err := syncLifecycleParentDirectory(s.auditPath); err != nil {
			return err
		}
	}
	s.auditReceipts[entry.ReceiptID] = true
	return nil
}

func (s *LifecycleStore) auditLocked(entry LifecycleAuditEntry) {
	_ = s.auditLockedStrict(entry)
}

func lifecycleAuditReceipt(entry LifecycleAuditEntry) string {
	entry.Timestamp = time.Time{}
	entry.ReceiptID = ""
	data, _ := json.Marshal(entry)
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest[:])
}

func (s *LifecycleStore) loadAuditReceipts() error {
	file, err := os.Open(s.auditPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open lifecycle audit receipts: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect lifecycle audit receipts: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Visual Hive lifecycle audit path is linked or non-regular")
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		var entry LifecycleAuditEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return fmt.Errorf("parse lifecycle audit receipt line %d: %w", line, err)
		}
		receipt := strings.TrimSpace(entry.ReceiptID)
		if receipt == "" {
			receipt = lifecycleAuditReceipt(entry)
		}
		if len(receipt) != sha256.Size*2 {
			return fmt.Errorf("lifecycle audit receipt line %d has invalid identity", line)
		}
		s.auditReceipts[receipt] = true
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read lifecycle audit receipts: %w", err)
	}
	return nil
}

func (s *LifecycleStore) sortStateLocked() {
	sort.SliceStable(s.state.Outbox, func(i, j int) bool {
		return s.state.Outbox[i].CreatedAt.Before(s.state.Outbox[j].CreatedAt)
	})
}

func updateFindingFromObservation(finding *FindingLifecycle, manifest Manifest, observation Observation) {
	observedAt, _ := time.Parse(time.RFC3339Nano, observation.ObservedAt)
	finding.Repository = manifest.Source.Repository
	finding.RepositoryID = manifest.Source.RepositoryID
	finding.Fingerprint = observation.Fingerprint
	finding.RepositoryFingerprint = observation.RepositoryFingerprint
	finding.PublicationRole = observation.PublicationRole
	finding.RootCauseKey = observation.RootCauseKey
	finding.BlockedByRootKeys = append([]string(nil), observation.BlockedByRootKeys...)
	finding.PublicationFingerprint = publicationFingerprint(manifest.Source.Repository, observation.RootCauseKey, observation.RepositoryFingerprint)
	finding.IssueKind = observation.IssueKind
	finding.Severity = observation.Severity
	finding.OwningAgentHint = observation.OwningAgentHint
	finding.Title = observation.Title
	finding.Body = observation.Body
	finding.Labels = append([]string(nil), observation.Labels...)
	finding.AffectedContracts = append([]string(nil), observation.AffectedContracts...)
	finding.ValidationCommand = observation.ValidationCommand
	finding.ObservationHumanReviewRequired = observationNeedsHumanReview(observation)
	finding.HumanReviewRequired = finding.ObservationHumanReviewRequired || finding.ManualReviewKind != ""
	finding.LastSeenAt = observedAt
	finding.LastBundleID = manifest.BundleID
	finding.LastBundleDigest = manifest.OverallDigest
	finding.LastWorkflowRunID = manifest.Source.WorkflowRunID
}

func updateFindingPublicationFromObservation(finding *FindingLifecycle, manifest Manifest, observation Observation) {
	identity := finding.RepositoryFingerprint
	beadID := finding.BeadID
	issueNumber, issueURL := finding.IssueNumber, finding.IssueURL
	firstSeenAt := finding.FirstSeenAt
	updateFindingFromObservation(finding, manifest, observation)
	// A root owner is a durable lifecycle identity. New canonical wording or a
	// changed producer fingerprint updates that exact issue; it never creates a
	// second marker or rewires the owner's repair history.
	finding.RepositoryFingerprint = identity
	finding.BeadID = beadID
	finding.IssueNumber, finding.IssueURL = issueNumber, issueURL
	finding.FirstSeenAt = firstSeenAt
}

func resetRepairCycle(finding *FindingLifecycle) {
	finding.Branch = ""
	finding.RepairCommitSHA = ""
	finding.PRNumber = 0
	finding.PRURL = ""
	finding.MergeSHA = ""
	finding.ValidationRunID = ""
	finding.ValidationRunURL = ""
	finding.LastCheckSummary = ""
	finding.LastCheckRuns = nil
	finding.LastPullRequestCheckReceipt = nil
	finding.ManualReviewKind = ""
	finding.ManualReviewReason = ""
	finding.HumanReviewRequired = false
	finding.RepairAttempts = 0
}

func outboxForFinding(action OutboxAction, finding *FindingLifecycle, manifest Manifest, now time.Time) OutboxEntry {
	id := digest([]byte(strings.Join([]string{string(action), finding.RepositoryFingerprint, manifest.BundleID}, "\x00")))
	return OutboxEntry{
		ID: id, Action: action, Repository: finding.Repository, RepositoryFingerprint: finding.RepositoryFingerprint,
		BundleID: manifest.BundleID, BundleDigest: manifest.OverallDigest, IssueNumber: finding.IssueNumber,
		Title: finding.Title, Body: finding.Body, Labels: append([]string(nil), finding.Labels...),
		PreviousMarker: finding.PendingMarkerMigrationFrom,
		Evidence: map[string]string{
			"bundle_id": manifest.BundleID, "bundle_digest": manifest.OverallDigest,
			"commit_sha": manifest.Source.CommitSHA, "workflow_run_id": manifest.Source.WorkflowRunID,
			"validation_command": finding.ValidationCommand,
		},
		CreatedAt: now,
	}
}

func beadExternalRef(repository, repositoryFingerprint string) string {
	return fmt.Sprintf("visual-hive://%s/%s", strings.ToLower(strings.TrimSpace(repository)), repositoryFingerprint)
}

func priorityForSeverity(severity string) beads.Priority {
	switch severity {
	case "critical":
		return beads.PriorityCritical
	case "high":
		return beads.PriorityHigh
	case "medium":
		return beads.PriorityMedium
	default:
		return beads.PriorityLow
	}
}

func actorForHint(hint string) string {
	parts := strings.Split(hint, "/")
	actor := parts[len(parts)-1]
	if actor == "test-creator" || actor == "test-maintainer" || actor == "mutation" {
		return "quality"
	}
	if actor == "ci" {
		return "ci-maintainer"
	}
	if actor == "" {
		return "quality"
	}
	return actor
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func refsEquivalent(left, right string) bool {
	normalize := func(value string) string {
		return strings.TrimPrefix(strings.TrimSpace(value), "refs/heads/")
	}
	return normalize(left) != "" && normalize(left) == normalize(right)
}

func (s *LifecycleStore) rootResolutionAllowedLocked(finding *FindingLifecycle, manifest Manifest, validatedAuthoritative bool, targetRef string, options ApplyLifecycleOptions, presentRoots map[string]bool) (bool, string) {
	resolutionSubject := s.publicationResolutionSubjectLocked(finding)
	allowed, reason := resolutionAllowed(resolutionSubject, manifest, validatedAuthoritative, targetRef, options)
	if !allowed || finding == nil || finding.RootCauseKey == "" {
		return allowed, reason
	}
	if presentRoots[finding.RootCauseKey] {
		return false, fmt.Sprintf("root %q still has present evidence", finding.RootCauseKey)
	}
	for _, related := range s.state.Findings {
		if related == nil || related == finding || related.RootCauseKey != finding.RootCauseKey || !strings.EqualFold(related.Repository, finding.Repository) || related.Status == StatusResolved || related.Status == StatusIssueClosed {
			continue
		}
		if relatedAllowed, relatedReason := resolutionAllowed(s.publicationResolutionSubjectLocked(related), manifest, validatedAuthoritative, targetRef, options); !relatedAllowed {
			return false, fmt.Sprintf("root %q is not authoritatively absent: %s", finding.RootCauseKey, relatedReason)
		}
	}
	return true, ""
}

// A deferred derivative is part of its canonical publication rather than an
// independent issue. Some derivative handoff observations intentionally have
// no affected contract of their own. Resolve them from the canonical owner's
// exact contract and post-merge verification proof; otherwise the metadata-only
// derivative can permanently prevent the canonical issue from closing after
// the root disappears from an exhaustive scan.
func (s *LifecycleStore) publicationResolutionSubjectLocked(finding *FindingLifecycle) *FindingLifecycle {
	if finding == nil || finding.IssueNumber > 0 || len(finding.AffectedContracts) != 0 || finding.PublicationRole != "derivative" || finding.PublicationFingerprint == "" || finding.RootCauseKey == "" {
		return finding
	}
	owner := s.state.Findings[finding.PublicationFingerprint]
	if owner == nil || owner == finding || owner.PublicationRole != "canonical" || owner.RootCauseKey != finding.RootCauseKey || !strings.EqualFold(owner.Repository, finding.Repository) || owner.IssueNumber <= 0 {
		return finding
	}
	return owner
}

func resolutionAllowed(finding *FindingLifecycle, manifest Manifest, validatedAuthoritative bool, targetRef string, options ApplyLifecycleOptions) (bool, string) {
	if !validatedAuthoritative || !manifest.Scan.AuthoritativeForResolution || manifest.Scan.Scope != "full" || !refsEquivalent(manifest.Source.Ref, targetRef) {
		return false, "absence was not from an authoritative target-ref scan"
	}
	if options.CurrentTargetCommitSHA != "" && !strings.EqualFold(options.CurrentTargetCommitSHA, manifest.Source.CommitSHA) {
		return false, "absence came from a stale target-branch commit"
	}
	if finding == nil {
		return false, "finding has no affected contract that can be proven evaluated"
	}
	affectedContracts := finding.AffectedContracts
	// Test-adequacy findings created before testing-layer resolution scopes were
	// introduced are repository-level Unit gaps. Preserve their fingerprint and
	// require the exact Unit layer inventory before accepting inferred absence.
	if len(affectedContracts) == 0 && finding.IssueKind == "test_adequacy_gap" {
		affectedContracts = []string{"testing-layer:2"}
	}
	// Repository-level workflow and provider findings intentionally have no UI
	// contract. Their deterministic audit artifacts are explicit evaluation
	// scopes so a complete target-branch scan can prove resolution safely.
	if len(affectedContracts) == 0 && finding.IssueKind == "workflow_safety" {
		affectedContracts = []string{"workflow-safety"}
	}
	if len(affectedContracts) == 0 && finding.IssueKind == "provider_governance" {
		affectedContracts = []string{"provider-governance"}
	}
	if len(affectedContracts) == 0 {
		return false, "finding has no affected contract that can be proven evaluated"
	}
	evaluated := make(map[string]bool, len(manifest.Scan.EvaluatedContracts))
	for _, contract := range manifest.Scan.EvaluatedContracts {
		evaluated[contract] = true
	}
	for _, contract := range affectedContracts {
		if !evaluated[contract] {
			return false, fmt.Sprintf("affected contract %s was not evaluated", contract)
		}
	}
	hasActiveRepair := finding.Branch != "" || finding.RepairCommitSHA != "" || finding.PRNumber > 0 || finding.MergeSHA != ""
	if hasActiveRepair && finding.Status != StatusResolved && finding.Status != StatusIssueClosed {
		if finding.MergeSHA == "" {
			return false, "repaired finding has no recorded merge"
		}
		if finding.Status != StatusPostMergeVerifying {
			return false, "repaired finding is not in post-merge verification"
		}
		verificationRunID := strings.TrimSpace(options.VerificationRunID)
		if verificationRunID == "" {
			verificationRunID = strings.TrimSpace(manifest.Source.WorkflowRunID)
		}
		if finding.ValidationRunID == "" || verificationRunID != finding.ValidationRunID || manifest.Source.WorkflowRunID != finding.ValidationRunID {
			return false, "absence did not come from the recorded post-merge verification run"
		}
		if options.VerificationCommitSHA != "" && options.VerificationCommitSHA != manifest.Source.CommitSHA {
			return false, "verification commit does not match the bundle source commit"
		}
	}
	if finding.MergeSHA != "" && finding.Status == StatusPostMergeVerifying && finding.MergeSHA != manifest.Source.CommitSHA {
		verifiedDescendant := options.VerifiedMergeAncestorFingerprint == finding.RepositoryFingerprint &&
			options.VerifiedMergeAncestorSHA == finding.MergeSHA &&
			options.VerificationCommitSHA == manifest.Source.CommitSHA
		if !verifiedDescendant {
			return false, "target-branch verification did not run at the recorded merge SHA or a verified non-conflicting descendant"
		}
	}
	return true, ""
}

func setVerificationEvidence(finding *FindingLifecycle, manifest Manifest, options ApplyLifecycleOptions) {
	if options.VerificationRunID != "" {
		finding.ValidationRunID = options.VerificationRunID
	} else if manifest.Source.WorkflowRunID != "" {
		finding.ValidationRunID = manifest.Source.WorkflowRunID
	}
	if options.VerificationURL != "" {
		finding.ValidationRunURL = options.VerificationURL
	}
}

func cloneLifecycleState(state LifecycleState) LifecycleState {
	data, err := json.Marshal(state)
	if err != nil {
		return state
	}
	var cloned LifecycleState
	if err := json.Unmarshal(data, &cloned); err != nil {
		return state
	}
	return cloned
}
