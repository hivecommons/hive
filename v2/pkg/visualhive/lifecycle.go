package visualhive

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

const LifecycleSchema = "hive.visual-hive-lifecycle.v1"

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
	Repository            string          `json:"repository"`
	RepositoryID          string          `json:"repository_id,omitempty"`
	Fingerprint           string          `json:"fingerprint"`
	RepositoryFingerprint string          `json:"repository_fingerprint"`
	Status                LifecycleStatus `json:"status"`
	IssueKind             string          `json:"issue_kind"`
	Severity              string          `json:"severity"`
	OwningAgentHint       string          `json:"owning_agent_hint"`
	Title                 string          `json:"title"`
	Body                  string          `json:"body"`
	Labels                []string        `json:"labels"`
	AffectedContracts     []string        `json:"affected_contracts"`
	ValidationCommand     string          `json:"validation_command"`
	BeadID                string          `json:"bead_id,omitempty"`
	IssueNumber           int             `json:"issue_number,omitempty"`
	IssueURL              string          `json:"issue_url,omitempty"`
	Branch                string          `json:"branch,omitempty"`
	RepairCommitSHA       string          `json:"repair_commit_sha,omitempty"`
	PRNumber              int             `json:"pr_number,omitempty"`
	PRURL                 string          `json:"pr_url,omitempty"`
	MergeSHA              string          `json:"merge_sha,omitempty"`
	ValidationRunID       string          `json:"validation_run_id,omitempty"`
	ValidationRunURL      string          `json:"validation_run_url,omitempty"`
	LastCheckSummary      string          `json:"last_check_summary,omitempty"`
	LastCheckRuns         []CheckEvidence `json:"last_check_runs,omitempty"`
	FirstSeenAt           time.Time       `json:"first_seen_at"`
	LastSeenAt            time.Time       `json:"last_seen_at"`
	ResolvedAt            *time.Time      `json:"resolved_at,omitempty"`
	ClosedAt              *time.Time      `json:"closed_at,omitempty"`
	LastBundleID          string          `json:"last_bundle_id"`
	LastBundleDigest      string          `json:"last_bundle_digest"`
	LastWorkflowRunID     string          `json:"last_workflow_run_id,omitempty"`
	RepairAttempts        int             `json:"repair_attempts"`
	Recurrences           int             `json:"recurrences"`
}

type CheckEvidence struct {
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
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
}

type LifecycleAuditEntry struct {
	Timestamp             time.Time `json:"timestamp"`
	Action                string    `json:"action"`
	Allowed               bool      `json:"allowed"`
	Repository            string    `json:"repository,omitempty"`
	RepositoryFingerprint string    `json:"repository_fingerprint,omitempty"`
	BundleID              string    `json:"bundle_id,omitempty"`
	Detail                string    `json:"detail,omitempty"`
}

type ApplyLifecycleOptions struct {
	TargetRef         string
	VerificationRunID string
	VerificationURL   string
	MaxActiveIssues   int
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

type LifecycleStore struct {
	mu        sync.Mutex
	dir       string
	statePath string
	auditPath string
	state     LifecycleState
}

func NewLifecycleStore(dir string) (*LifecycleStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("lifecycle store directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create lifecycle store: %w", err)
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
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *LifecycleStore) ApplyBundle(bundle *ValidatedBundle, beadStore *beads.Store, options ApplyLifecycleOptions) (ApplyLifecycleResult, error) {
	if bundle == nil {
		return ApplyLifecycleResult{}, fmt.Errorf("validated Visual Hive bundle is required")
	}
	if beadStore == nil {
		return ApplyLifecycleResult{}, fmt.Errorf("persistent Hive bead store is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	manifest := bundle.Manifest
	result := ApplyLifecycleResult{BundleID: manifest.BundleID}
	if priorDigest, exists := s.state.ReplayKeys[manifest.ReplayProtection.Key]; exists {
		if priorDigest != manifest.OverallDigest {
			s.auditLocked(LifecycleAuditEntry{Action: "bundle_replay", Allowed: false, Repository: manifest.Source.Repository, BundleID: manifest.BundleID, Detail: "replay key reused with a different digest"})
			return result, fmt.Errorf("bundle replay key was reused with a different digest")
		}
		result.Idempotent = true
		s.auditLocked(LifecycleAuditEntry{Action: "bundle_replay", Allowed: true, Repository: manifest.Source.Repository, BundleID: manifest.BundleID, Detail: "idempotent retry"})
		return result, nil
	}
	backup := cloneLifecycleState(s.state)

	now := time.Now().UTC()
	targetRef := options.TargetRef
	if targetRef == "" {
		targetRef = "refs/heads/main"
	}
	beadInputs := make([]beads.BatchInput, 0, len(manifest.Observations))
	for _, observation := range manifest.Observations {
		if observation.State != "present" {
			continue
		}
		if existing := s.state.Findings[observation.RepositoryFingerprint]; existing == nil || existing.BeadID == "" {
			beadInputs = append(beadInputs, beadInputForObservation(manifest, observation))
		}
	}
	if len(beadInputs) > 0 {
		beadResult, err := beadStore.ImportBatch(beadInputs)
		if err != nil {
			return result, fmt.Errorf("persist lifecycle beads: %w", err)
		}
		result.BeadsCreated, result.BeadsSkipped = beadResult.Created, beadResult.Skipped
	}

	observedFingerprints := make(map[string]bool, len(manifest.Observations))
	publicationSet := selectIssuePublications(manifest.Observations, s.state.Findings, options.MaxActiveIssues)
	for _, observation := range manifest.Observations {
		observedFingerprints[observation.RepositoryFingerprint] = true
		finding := s.state.Findings[observation.RepositoryFingerprint]
		if observation.State == "absent" {
			if finding == nil {
				result.IgnoredAbsent++
				continue
			}
			if allowed, reason := resolutionAllowed(finding, manifest, targetRef); !allowed {
				result.IgnoredAbsent++
				s.auditLocked(LifecycleAuditEntry{Action: "resolve_finding", Allowed: false, Repository: manifest.Source.Repository, RepositoryFingerprint: observation.RepositoryFingerprint, BundleID: manifest.BundleID, Detail: reason})
				continue
			}
			updateFindingFromObservation(finding, manifest, observation)
			if finding.Status == StatusIssueClosed {
				result.Updated++
				result.FindingIDs = append(result.FindingIDs, observation.RepositoryFingerprint)
				continue
			}
			finding.Status = StatusResolved
			finding.ResolvedAt = &now
			finding.ClosedAt = nil
			setVerificationEvidence(finding, manifest, options)
			result.Resolved++
			result.Updated++
			if finding.IssueNumber > 0 {
				if s.enqueueLocked(outboxForFinding(OutboxCloseIssue, finding, manifest, now)) {
					result.OutboxCreated++
				}
			}
			s.auditLocked(LifecycleAuditEntry{Action: "resolve_finding", Allowed: true, Repository: finding.Repository, RepositoryFingerprint: finding.RepositoryFingerprint, BundleID: manifest.BundleID, Detail: "authoritative target-ref absence"})
			result.FindingIDs = append(result.FindingIDs, observation.RepositoryFingerprint)
			continue
		}

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
				result.Reopened++
				wasReopened = true
			}
		}
		updateFindingFromObservation(finding, manifest, observation)
		if bead := beadStore.FindByExternalRef(beadExternalRef(manifest.Source.Repository, observation.RepositoryFingerprint)); bead != nil {
			if finding.BeadID == "" {
				finding.BeadID = bead.ID
			}
			if wasReopened && (bead.Status == beads.StatusClosed || bead.Status == beads.StatusDone) {
				_ = beadStore.Update(bead.ID, func(value *beads.Bead) {
					value.Status = beads.StatusOpen
					value.ClosedAt = nil
				})
			}
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
			s.auditLocked(LifecycleAuditEntry{Action: "defer_open_issue", Allowed: true, Repository: finding.Repository, RepositoryFingerprint: finding.RepositoryFingerprint, BundleID: manifest.BundleID, Detail: "active issue work-in-progress limit reached"})
			result.FindingIDs = append(result.FindingIDs, observation.RepositoryFingerprint)
			continue
		}
		if s.enqueueLocked(outboxForFinding(action, finding, manifest, now)) {
			result.OutboxCreated++
		}
		s.auditLocked(LifecycleAuditEntry{Action: string(action), Allowed: true, Repository: finding.Repository, RepositoryFingerprint: finding.RepositoryFingerprint, BundleID: manifest.BundleID})
		result.FindingIDs = append(result.FindingIDs, observation.RepositoryFingerprint)
	}

	// A full authoritative bundle is an exhaustive inventory for the contracts
	// it says it evaluated. Resolve an existing finding omitted from that
	// inventory only when every affected contract was actually executed. This
	// lets a trusted target-branch run close findings without copying Hive's
	// private lifecycle database into the target workflow.
	if manifest.Scan.Scope == "full" && manifest.Scan.AuthoritativeForResolution && refsEquivalent(manifest.Source.Ref, targetRef) {
		keys := make([]string, 0, len(s.state.Findings))
		for key := range s.state.Findings {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			finding := s.state.Findings[key]
			if finding == nil || !strings.EqualFold(finding.Repository, manifest.Source.Repository) || observedFingerprints[key] || finding.Status == StatusIssueClosed || finding.Status == StatusResolved {
				continue
			}
			if allowed, reason := resolutionAllowed(finding, manifest, targetRef); !allowed {
				s.auditLocked(LifecycleAuditEntry{Action: "infer_absent_finding", Allowed: false, Repository: finding.Repository, RepositoryFingerprint: key, BundleID: manifest.BundleID, Detail: reason})
				continue
			}
			finding.Status = StatusResolved
			finding.ResolvedAt = &now
			finding.ClosedAt = nil
			finding.LastBundleID = manifest.BundleID
			finding.LastBundleDigest = manifest.OverallDigest
			finding.LastWorkflowRunID = manifest.Source.WorkflowRunID
			setVerificationEvidence(finding, manifest, options)
			result.Resolved++
			result.Updated++
			if finding.IssueNumber > 0 && s.enqueueLocked(outboxForFinding(OutboxCloseIssue, finding, manifest, now)) {
				result.OutboxCreated++
			}
			result.FindingIDs = append(result.FindingIDs, key)
			s.auditLocked(LifecycleAuditEntry{Action: "infer_absent_finding", Allowed: true, Repository: finding.Repository, RepositoryFingerprint: key, BundleID: manifest.BundleID, Detail: "omitted from exhaustive evaluated-contract inventory"})
		}
	}

	s.state.ReplayKeys[manifest.ReplayProtection.Key] = manifest.OverallDigest
	s.state.UpdatedAt = now
	s.sortStateLocked()
	if err := s.persistLocked(); err != nil {
		s.state = backup
		return ApplyLifecycleResult{}, err
	}
	sort.Strings(result.FindingIDs)
	return result, nil
}

func selectIssuePublications(observations []Observation, findings map[string]*FindingLifecycle, maxActive int) map[string]bool {
	selected := map[string]bool{}
	if maxActive <= 0 {
		for _, observation := range observations {
			if observation.State == "present" {
				selected[observation.RepositoryFingerprint] = true
			}
		}
		return selected
	}
	active := 0
	for _, finding := range findings {
		if finding != nil && finding.IssueNumber > 0 && finding.Status != StatusResolved && finding.Status != StatusIssueClosed {
			active++
		}
	}
	slots := maxActive - active
	if slots <= 0 {
		return selected
	}
	candidates := make([]Observation, 0, len(observations))
	for _, observation := range observations {
		if observation.State != "present" {
			continue
		}
		if finding := findings[observation.RepositoryFingerprint]; finding != nil && finding.IssueNumber > 0 {
			continue
		}
		candidates = append(candidates, observation)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if severityRank(left.Severity) != severityRank(right.Severity) {
			return severityRank(left.Severity) > severityRank(right.Severity)
		}
		if issueKindRank(left.IssueKind) != issueKindRank(right.IssueKind) {
			return issueKindRank(left.IssueKind) > issueKindRank(right.IssueKind)
		}
		return left.RepositoryFingerprint < right.RepositoryFingerprint
	})
	for _, observation := range candidates {
		if len(selected) >= slots {
			break
		}
		selected[observation.RepositoryFingerprint] = true
	}
	return selected
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
	case "visual_regression", "selector_contract_failure", "screenshot_diff", "mutation_survivor", "accessibility_failure", "console_error", "network_error", "security_failure":
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
	}
	s.state.UpdatedAt = time.Now().UTC()
	if err := s.persistLocked(); err != nil {
		s.state = backup
		return err
	}
	return nil
}

func (s *LifecycleStore) RecordAuthorization(repositoryFingerprint, action string, allowed bool, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	finding := s.state.Findings[repositoryFingerprint]
	entry := LifecycleAuditEntry{Action: "authorize_" + action, Allowed: allowed, RepositoryFingerprint: repositoryFingerprint, Detail: truncate(detail, 4096)}
	if finding != nil {
		entry.Repository = finding.Repository
		entry.BundleID = finding.LastBundleID
	}
	s.auditLocked(entry)
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

func (s *LifecycleStore) MarkRepairStarted(repositoryFingerprint, branch string) error {
	return s.updateFinding(repositoryFingerprint, "repair_started", func(finding *FindingLifecycle) error {
		if finding.Status != StatusIssueOpen && finding.Status != StatusFixQueued && finding.Status != StatusNeedsRevision {
			return fmt.Errorf("cannot start repair from %s", finding.Status)
		}
		if strings.TrimSpace(branch) == "" {
			return fmt.Errorf("repair branch is required")
		}
		finding.Branch, finding.Status = branch, StatusRepairRunning
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
		return nil
	})
}

func (s *LifecycleStore) MarkChecks(repositoryFingerprint, testedSHA string, allGreen bool) error {
	return s.MarkChecksWithEvidence(repositoryFingerprint, testedSHA, allGreen, "", nil)
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

func (s *LifecycleStore) MarkPostMergeVerifying(repositoryFingerprint, runID, runURL string) error {
	return s.updateFinding(repositoryFingerprint, "post_merge_verifying", func(finding *FindingLifecycle) error {
		if finding.Status != StatusMerged && finding.Status != StatusPostMergeVerifying {
			return fmt.Errorf("cannot start post-merge verification from %s", finding.Status)
		}
		finding.ValidationRunID, finding.ValidationRunURL, finding.Status = runID, runURL, StatusPostMergeVerifying
		return nil
	})
}

func (s *LifecycleStore) MarkIssueClosed(repositoryFingerprint string, beadStore *beads.Store) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	backup := cloneLifecycleState(s.state)
	finding := s.state.Findings[repositoryFingerprint]
	if finding == nil {
		return fmt.Errorf("finding %s not found", repositoryFingerprint)
	}
	if finding.Status != StatusResolved {
		s.auditLocked(LifecycleAuditEntry{Action: "issue_closed", Allowed: false, Repository: finding.Repository, RepositoryFingerprint: repositoryFingerprint, Detail: "finding is not resolved"})
		return fmt.Errorf("cannot close issue from %s", finding.Status)
	}
	now := time.Now().UTC()
	finding.Status, finding.ClosedAt = StatusIssueClosed, &now
	if finding.BeadID != "" {
		if err := beadStore.Close(finding.BeadID); err != nil {
			return err
		}
	}
	s.auditLocked(LifecycleAuditEntry{Action: "issue_closed", Allowed: true, Repository: finding.Repository, RepositoryFingerprint: repositoryFingerprint})
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
		s.auditLocked(LifecycleAuditEntry{Action: action, Allowed: false, Repository: finding.Repository, RepositoryFingerprint: repositoryFingerprint, Detail: err.Error()})
		return err
	}
	s.auditLocked(LifecycleAuditEntry{Action: action, Allowed: true, Repository: finding.Repository, RepositoryFingerprint: repositoryFingerprint})
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
	if s.state.SchemaVersion != LifecycleSchema {
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
	return nil
}

func (s *LifecycleStore) persistLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal lifecycle state: %w", err)
	}
	temporary := s.statePath + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write lifecycle state: %w", err)
	}
	if err := os.Rename(temporary, s.statePath); err != nil {
		return fmt.Errorf("publish lifecycle state: %w", err)
	}
	return nil
}

func (s *LifecycleStore) auditLocked(entry LifecycleAuditEntry) {
	entry.Timestamp = time.Now().UTC()
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	file, err := os.OpenFile(s.auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	_, _ = writer.Write(append(data, '\n'))
	_ = writer.Flush()
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
	finding.IssueKind = observation.IssueKind
	finding.Severity = observation.Severity
	finding.OwningAgentHint = observation.OwningAgentHint
	finding.Title = observation.Title
	finding.Body = observation.Body
	finding.Labels = append([]string(nil), observation.Labels...)
	finding.AffectedContracts = append([]string(nil), observation.AffectedContracts...)
	finding.ValidationCommand = observation.ValidationCommand
	finding.LastSeenAt = observedAt
	finding.LastBundleID = manifest.BundleID
	finding.LastBundleDigest = manifest.OverallDigest
	finding.LastWorkflowRunID = manifest.Source.WorkflowRunID
}

func beadInputForObservation(manifest Manifest, observation Observation) beads.BatchInput {
	return beads.BatchInput{
		SourceID:    observation.RepositoryFingerprint,
		Title:       observation.Title,
		Type:        beads.TypeBug,
		Status:      beads.StatusOpen,
		Priority:    priorityForSeverity(observation.Severity),
		Actor:       actorForHint(observation.OwningAgentHint),
		ExternalRef: beadExternalRef(manifest.Source.Repository, observation.RepositoryFingerprint),
		Metadata: map[string]interface{}{
			"visual_hive_fingerprint":            observation.Fingerprint,
			"visual_hive_repository_fingerprint": observation.RepositoryFingerprint,
			"visual_hive_bundle_id":              manifest.BundleID,
			"visual_hive_digest":                 manifest.OverallDigest,
			"visual_hive_repository":             manifest.Source.Repository,
			"visual_hive_commit":                 manifest.Source.CommitSHA,
		},
		Notes:     observation.Body,
		DependsOn: []string{},
	}
}

func outboxForFinding(action OutboxAction, finding *FindingLifecycle, manifest Manifest, now time.Time) OutboxEntry {
	id := digest([]byte(strings.Join([]string{string(action), finding.RepositoryFingerprint, manifest.BundleID}, "\x00")))
	return OutboxEntry{
		ID: id, Action: action, Repository: finding.Repository, RepositoryFingerprint: finding.RepositoryFingerprint,
		BundleID: manifest.BundleID, BundleDigest: manifest.OverallDigest, IssueNumber: finding.IssueNumber,
		Title: finding.Title, Body: finding.Body, Labels: append([]string(nil), finding.Labels...),
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

func resolutionAllowed(finding *FindingLifecycle, manifest Manifest, targetRef string) (bool, string) {
	if !manifest.Scan.AuthoritativeForResolution || manifest.Scan.Scope != "full" || !refsEquivalent(manifest.Source.Ref, targetRef) {
		return false, "absence was not from an authoritative target-ref scan"
	}
	if finding == nil || len(finding.AffectedContracts) == 0 {
		return false, "finding has no affected contract that can be proven evaluated"
	}
	evaluated := make(map[string]bool, len(manifest.Scan.EvaluatedContracts))
	for _, contract := range manifest.Scan.EvaluatedContracts {
		evaluated[contract] = true
	}
	for _, contract := range finding.AffectedContracts {
		if !evaluated[contract] {
			return false, fmt.Sprintf("affected contract %s was not evaluated", contract)
		}
	}
	if finding.MergeSHA != "" && (finding.Status == StatusMerged || finding.Status == StatusPostMergeVerifying) && finding.MergeSHA != manifest.Source.CommitSHA {
		return false, "target-branch verification did not run at the recorded merge SHA"
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
