package integrated

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

const (
	SetupBaselineSchema             = "hive.setup-baseline.v1"
	setupBaselineFile               = "setup-baseline.json"
	SetupBaselinePending            = "pending"
	SetupBaselineDispatched         = "dispatched"
	SetupBaselineArtifactVerified   = "artifact_verified"
	SetupBaselineProposalPrepared   = "proposal_prepared"
	SetupBaselinePROpen             = "pr_open"
	SetupBaselineApproved           = "approved"
	SetupBaselineMerged             = "merged"
	SetupBaselineProductionVerified = "production_verified"
	setupBaselineOperation          = "setup-baseline"
	setupBaselineWorkflowOperation  = "setup-baseline-capture"
	setupBaselineArtifactSchema     = "hive.setup-baseline-artifact.v1"
)

type SetupBaselineCandidate struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type SetupBaselineIntent struct {
	SchemaVersion                string                     `json:"schema_version"`
	Phase                        string                     `json:"phase"`
	Repository                   string                     `json:"repository"`
	RepositoryID                 string                     `json:"repository_id"`
	DefaultBranch                string                     `json:"default_branch"`
	AuthorizerID                 int64                      `json:"authorizer_id"`
	SetupPRNumber                int                        `json:"setup_pr_number"`
	SetupPRURL                   string                     `json:"setup_pr_url"`
	SetupHeadSHA                 string                     `json:"setup_head_sha"`
	VisualHiveConfigDigest       string                     `json:"visual_hive_config_digest"`
	ScreenshotContractDigest     string                     `json:"screenshot_contract_digest"`
	InitialBaselineDigest        string                     `json:"initial_baseline_digest"`
	InitialBaselineCandidates    []SetupBaselineCandidate   `json:"initial_baseline_candidates,omitempty"`
	CaptureCorrelation           string                     `json:"capture_correlation,omitempty"`
	CaptureHeadSHA               string                     `json:"capture_head_sha,omitempty"`
	CaptureRunID                 int64                      `json:"capture_run_id,omitempty"`
	CaptureRunURL                string                     `json:"capture_run_url,omitempty"`
	ArtifactID                   int64                      `json:"artifact_id,omitempty"`
	ArtifactName                 string                     `json:"artifact_name,omitempty"`
	ArtifactRoot                 string                     `json:"artifact_root,omitempty"`
	CaptureCandidateDigest       string                     `json:"capture_candidate_digest,omitempty"`
	CaptureCandidates            []SetupBaselineCandidate   `json:"capture_candidates,omitempty"`
	CandidateDigest              string                     `json:"candidate_digest,omitempty"`
	Candidates                   []SetupBaselineCandidate   `json:"candidates,omitempty"`
	Branch                       string                     `json:"branch,omitempty"`
	CommitSHA                    string                     `json:"commit_sha,omitempty"`
	Marker                       string                     `json:"marker,omitempty"`
	PRNumber                     int                        `json:"pr_number,omitempty"`
	PRURL                        string                     `json:"pr_url,omitempty"`
	BaseSHA                      string                     `json:"base_sha,omitempty"`
	DiffDigest                   string                     `json:"diff_digest,omitempty"`
	ApprovalPlanDigest           string                     `json:"approval_plan_digest,omitempty"`
	ApprovalActorID              int64                      `json:"approval_actor_id,omitempty"`
	ApprovalReason               string                     `json:"approval_reason,omitempty"`
	MergeAttemptedAt             time.Time                  `json:"merge_attempted_at,omitempty"`
	MergeSHA                     string                     `json:"merge_sha,omitempty"`
	ProductionRunID              int64                      `json:"production_run_id,omitempty"`
	ProductionHeadSHA            string                     `json:"production_head_sha,omitempty"`
	ExistingHeadReverified       bool                       `json:"existing_head_reverified,omitempty"`
	PreviouslyApprovedDigest     string                     `json:"previously_approved_digest,omitempty"`
	PreviouslyApprovedCandidates []SetupBaselineCandidate   `json:"previously_approved_candidates,omitempty"`
	RetiredCaptureCorrelations   []string                   `json:"retired_capture_correlations,omitempty"`
	CreatedAt                    time.Time                  `json:"created_at"`
	UpdatedAt                    time.Time                  `json:"updated_at"`
	DispatchAttemptedAt          time.Time                  `json:"dispatch_attempted_at,omitempty"`
	DispatchAcknowledgedAt       time.Time                  `json:"dispatch_acknowledged_at,omitempty"`
	PendingAudit                 *setupBaselineAuditReceipt `json:"pending_audit,omitempty"`
}

func (s *Store) SaveSetupBaselineIntent(intent SetupBaselineIntent) error {
	intent.SchemaVersion = SetupBaselineSchema
	intent.UpdatedAt = time.Now().UTC()
	if err := validateSetupBaselineIntent(intent); err != nil {
		return err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(filepath.Join(s.dir, setupBaselineFile), append(data, '\n'))
}

func (s *Store) LoadSetupBaselineIntent() (SetupBaselineIntent, bool, error) {
	data, err := readOrdinaryStateFile(filepath.Join(s.dir, setupBaselineFile))
	if os.IsNotExist(err) {
		return SetupBaselineIntent{}, false, nil
	}
	if err != nil {
		return SetupBaselineIntent{}, false, err
	}
	var intent SetupBaselineIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return SetupBaselineIntent{}, false, fmt.Errorf("decode setup baseline intent: %w", err)
	}
	if err := validateSetupBaselineIntent(intent); err != nil {
		return SetupBaselineIntent{}, false, err
	}
	return intent, true, nil
}

func (s *Store) DeleteSetupBaselineIntent() error {
	return durableRemoveStateFile(filepath.Join(s.dir, setupBaselineFile))
}

func validateSetupBaselineIntent(intent SetupBaselineIntent) error {
	phases := map[string]bool{SetupBaselinePending: true, SetupBaselineDispatched: true, SetupBaselineArtifactVerified: true, SetupBaselineProposalPrepared: true, SetupBaselinePROpen: true, SetupBaselineApproved: true, SetupBaselineMerged: true, SetupBaselineProductionVerified: true}
	if intent.SchemaVersion != SetupBaselineSchema || !phases[intent.Phase] || strings.TrimSpace(intent.Repository) == "" || strings.TrimSpace(intent.RepositoryID) == "" ||
		strings.TrimSpace(intent.DefaultBranch) == "" || intent.AuthorizerID <= 0 || intent.SetupPRNumber <= 0 || strings.TrimSpace(intent.SetupPRURL) == "" ||
		!immutableCommit.MatchString(strings.ToLower(intent.SetupHeadSHA)) || len(intent.VisualHiveConfigDigest) != sha256.Size*2 || len(intent.ScreenshotContractDigest) != sha256.Size*2 || intent.CreatedAt.IsZero() || intent.UpdatedAt.IsZero() {
		return fmt.Errorf("setup baseline intent is incomplete or invalid")
	}
	if err := validateSetupBaselineAuditReceipt(intent.PendingAudit); err != nil {
		return err
	}
	if len(intent.PreviouslyApprovedCandidates) > 0 {
		digest, digestErr := setupBaselineCandidateDigest(intent.PreviouslyApprovedCandidates)
		if digestErr != nil || !strings.EqualFold(digest, intent.PreviouslyApprovedDigest) {
			return fmt.Errorf("setup baseline prior approved candidate binding is invalid")
		}
	} else if intent.PreviouslyApprovedDigest != "" {
		return fmt.Errorf("setup baseline prior approved digest has no candidate set")
	}
	if len(intent.RetiredCaptureCorrelations) > 32 {
		return fmt.Errorf("setup baseline retired capture history exceeds its bound")
	}
	seenRetired := map[string]bool{}
	for _, correlation := range intent.RetiredCaptureCorrelations {
		if !workflowDispatchCorrelationPattern.MatchString(correlation) || seenRetired[correlation] {
			return fmt.Errorf("setup baseline retired capture history is invalid")
		}
		seenRetired[correlation] = true
	}
	if digest, digestErr := setupBaselineCandidateDigest(intent.InitialBaselineCandidates); digestErr != nil || !strings.EqualFold(digest, intent.InitialBaselineDigest) {
		return fmt.Errorf("setup baseline initial trusted inventory binding is invalid")
	}
	if intent.Phase == SetupBaselinePending {
		return nil
	}
	if !workflowDispatchCorrelationPattern.MatchString(intent.CaptureCorrelation) || !immutableCommit.MatchString(strings.ToLower(intent.CaptureHeadSHA)) || intent.DispatchAttemptedAt.IsZero() {
		return fmt.Errorf("setup baseline dispatch binding is incomplete")
	}
	if intent.CaptureRunID < 0 || (intent.CaptureRunID > 0 && (strings.TrimSpace(intent.CaptureRunURL) == "" || intent.DispatchAcknowledgedAt.IsZero())) {
		return fmt.Errorf("setup baseline capture run binding is invalid")
	}
	if intent.Phase == SetupBaselineDispatched {
		return nil
	}
	if intent.CaptureRunID <= 0 || intent.ArtifactID <= 0 || strings.TrimSpace(intent.ArtifactName) == "" || strings.TrimSpace(intent.ArtifactRoot) == "" ||
		len(intent.CaptureCandidateDigest) != sha256.Size*2 || len(intent.CaptureCandidates) == 0 {
		return fmt.Errorf("setup baseline artifact evidence is incomplete")
	}
	if digest, err := setupBaselineCandidateDigest(intent.CaptureCandidates); err != nil || !strings.EqualFold(digest, intent.CaptureCandidateDigest) {
		return fmt.Errorf("setup baseline candidate digest is invalid")
	}
	if intent.Phase == SetupBaselineArtifactVerified {
		return nil
	}
	if intent.ExistingHeadReverified {
		if (intent.Phase != SetupBaselineMerged && intent.Phase != SetupBaselineProductionVerified) ||
			!immutableCommit.MatchString(strings.ToLower(intent.MergeSHA)) || intent.MergeAttemptedAt.IsZero() {
			return fmt.Errorf("setup baseline existing-head verification binding is incomplete")
		}
		if intent.Phase == SetupBaselineProductionVerified && (intent.ProductionRunID <= 0 || !immutableCommit.MatchString(strings.ToLower(intent.ProductionHeadSHA))) {
			return fmt.Errorf("setup baseline production verification is incomplete")
		}
		return nil
	}
	if len(intent.CandidateDigest) != sha256.Size*2 || len(intent.Candidates) == 0 {
		return fmt.Errorf("setup baseline review delta binding is incomplete")
	}
	if digest, err := setupBaselineCandidateDigest(intent.Candidates); err != nil || !strings.EqualFold(digest, intent.CandidateDigest) {
		return fmt.Errorf("setup baseline review delta digest is invalid")
	}
	if intent.Branch != managedOperationBranch(setupBaselineOperation, intent.RepositoryID) || !immutableCommit.MatchString(strings.ToLower(intent.CommitSHA)) ||
		intent.Marker != setupBaselineMarker(intent.Repository, intent.CaptureCorrelation) {
		return fmt.Errorf("setup baseline proposal binding is incomplete")
	}
	if intent.Phase == SetupBaselineProposalPrepared {
		if intent.PRNumber != 0 || intent.PRURL != "" || intent.BaseSHA != "" || intent.DiffDigest != "" {
			return fmt.Errorf("prepared setup baseline proposal contains premature pull request identity")
		}
		return nil
	}
	if intent.PRNumber <= 0 || strings.TrimSpace(intent.PRURL) == "" || !immutableCommit.MatchString(strings.ToLower(intent.BaseSHA)) || len(intent.DiffDigest) != sha256.Size*2 {
		return fmt.Errorf("setup baseline pull request binding is incomplete")
	}
	if intent.Phase == SetupBaselinePROpen {
		return nil
	}
	if len(intent.ApprovalPlanDigest) != sha256.Size*2 || intent.ApprovalActorID != intent.AuthorizerID || strings.TrimSpace(intent.ApprovalReason) == "" {
		return fmt.Errorf("setup baseline approval binding is incomplete")
	}
	if intent.Phase == SetupBaselineApproved {
		return nil
	}
	if !immutableCommit.MatchString(strings.ToLower(intent.MergeSHA)) || intent.MergeAttemptedAt.IsZero() {
		return fmt.Errorf("setup baseline merge binding is incomplete")
	}
	if intent.Phase == SetupBaselineMerged {
		return nil
	}
	if intent.ProductionRunID <= 0 || !immutableCommit.MatchString(strings.ToLower(intent.ProductionHeadSHA)) {
		return fmt.Errorf("setup baseline production verification is incomplete")
	}
	return nil
}

func ensureSetupBaselineIntent(store *Store, config Config, setupHead string, setupPRNumber int, setupPRURL string) (SetupBaselineIntent, error) {
	setupHead = strings.ToLower(strings.TrimSpace(setupHead))
	if setupHead == "" {
		setupHead = strings.ToLower(strings.TrimSpace(config.SetupHeadSHA))
	}
	intent, exists, err := store.LoadSetupBaselineIntent()
	if err != nil {
		return intent, err
	}
	if exists {
		intent, err = flushSetupBaselineAudit(store, intent)
		if err != nil {
			return intent, fmt.Errorf("finish pending setup baseline audit before reuse: %w", err)
		}
		exact := strings.EqualFold(intent.Repository, config.Repository) && intent.RepositoryID == config.RepositoryID && intent.AuthorizerID == config.SetupAuthorizationActorID &&
			intent.SetupPRNumber == setupPRNumber && intent.SetupPRURL == strings.TrimSpace(setupPRURL) && strings.EqualFold(intent.SetupHeadSHA, setupHead) &&
			strings.EqualFold(intent.VisualHiveConfigDigest, config.VisualHiveConfigDigest) && strings.EqualFold(intent.ScreenshotContractDigest, config.SetupBaselineContractDigest) &&
			strings.EqualFold(intent.InitialBaselineDigest, config.SetupBaselineInitialDigest) && reflect.DeepEqual(intent.InitialBaselineCandidates, config.SetupBaselineInitialCandidates)
		if exact {
			return intent, nil
		}
		if intent.Phase != SetupBaselineProductionVerified {
			return intent, fmt.Errorf("active setup baseline state belongs to a different setup PR/head, Visual Hive config, screenshot contract inventory, repository, or authorizer")
		}
		if err := store.AuditStrict(AuditEntry{Action: "setup_baseline_binding_invalidated", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("old_setup_pr=%d old_setup_head=%s old_visual=%s old_contracts=%s old_initial=%s new_setup_pr=%d new_setup_head=%s new_visual=%s new_contracts=%s new_initial=%s", intent.SetupPRNumber, intent.SetupHeadSHA, intent.VisualHiveConfigDigest, intent.ScreenshotContractDigest, intent.InitialBaselineDigest, setupPRNumber, setupHead, config.VisualHiveConfigDigest, config.SetupBaselineContractDigest, config.SetupBaselineInitialDigest)}); err != nil {
			return intent, err
		}
	}
	previouslyApprovedDigest := ""
	previouslyApprovedCandidates := []SetupBaselineCandidate(nil)
	if exists && intent.Phase == SetupBaselineProductionVerified {
		previouslyApprovedDigest, previouslyApprovedCandidates = intent.CandidateDigest, append([]SetupBaselineCandidate(nil), intent.Candidates...)
	}
	now := time.Now().UTC()
	intent = SetupBaselineIntent{
		SchemaVersion: SetupBaselineSchema, Phase: SetupBaselinePending, Repository: config.Repository, RepositoryID: config.RepositoryID,
		DefaultBranch: config.DefaultBranch, AuthorizerID: config.SetupAuthorizationActorID, SetupPRNumber: setupPRNumber, SetupPRURL: setupPRURL,
		SetupHeadSHA: setupHead, VisualHiveConfigDigest: strings.ToLower(config.VisualHiveConfigDigest), ScreenshotContractDigest: strings.ToLower(config.SetupBaselineContractDigest),
		InitialBaselineDigest: strings.ToLower(config.SetupBaselineInitialDigest), InitialBaselineCandidates: append([]SetupBaselineCandidate(nil), config.SetupBaselineInitialCandidates...),
		PreviouslyApprovedDigest: previouslyApprovedDigest, PreviouslyApprovedCandidates: previouslyApprovedCandidates, CreatedAt: now, UpdatedAt: now,
	}
	return saveSetupBaselineTransition(store, intent, "setup_baseline_pending", true, fmt.Sprintf("setup_pr=%d setup_head=%s", setupPRNumber, setupHead))
}

type setupBaselineArtifactManifest struct {
	SchemaVersion      string                   `json:"schema_version"`
	Repository         string                   `json:"repository"`
	RepositoryID       string                   `json:"repository_id"`
	CaptureCorrelation string                   `json:"capture_correlation"`
	CaptureHead        string                   `json:"capture_head"`
	WorkflowRunID      string                   `json:"workflow_run_id"`
	WorkflowName       string                   `json:"workflow_name"`
	WorkflowPath       string                   `json:"workflow_path"`
	Event              string                   `json:"event"`
	Platform           string                   `json:"platform"`
	Runner             string                   `json:"runner"`
	CandidateDigest    string                   `json:"candidate_digest"`
	FileCount          int                      `json:"file_count"`
	TotalBytes         int64                    `json:"total_bytes"`
	Files              []SetupBaselineCandidate `json:"files"`
}

func verifySetupBaselineArtifact(root string, intent SetupBaselineIntent) ([]SetupBaselineCandidate, string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(filepath.Join(root, "setup-baseline-manifest.json"))
	if err != nil {
		return nil, "", fmt.Errorf("read setup baseline manifest: %w", err)
	}
	var manifest setupBaselineArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, "", fmt.Errorf("parse setup baseline manifest: %w", err)
	}
	if manifest.SchemaVersion != setupBaselineArtifactSchema || manifest.Repository != intent.Repository || manifest.RepositoryID != intent.RepositoryID ||
		manifest.CaptureCorrelation != intent.CaptureCorrelation || !strings.EqualFold(manifest.CaptureHead, intent.CaptureHeadSHA) ||
		manifest.WorkflowRunID != strconv.FormatInt(intent.CaptureRunID, 10) || manifest.WorkflowName != visualHiveProductionWorkflowName ||
		manifest.WorkflowPath != visualHiveProductionWorkflowPath || manifest.Event != "workflow_dispatch" || manifest.Platform != "linux" || manifest.Runner != "ubuntu-latest" ||
		manifest.FileCount != len(manifest.Files) || manifest.FileCount < 1 || manifest.FileCount > 200 {
		return nil, "", fmt.Errorf("setup baseline manifest identity or bounded file count is invalid")
	}
	markerPath := filepath.Join(root, ".hive-extraction-complete")
	markerInfo, markerErr := os.Lstat(markerPath)
	marker, readMarkerErr := os.ReadFile(markerPath)
	if intent.ArtifactID <= 0 || markerErr != nil || readMarkerErr != nil || !markerInfo.Mode().IsRegular() || string(marker) != strconv.FormatInt(intent.ArtifactID, 10)+"\n" {
		return nil, "", fmt.Errorf("setup baseline artifact extraction completion marker is invalid")
	}
	expectedPaths := map[string]bool{"setup-baseline-manifest.json": true, ".hive-extraction-complete": true}
	var total int64
	for index, candidate := range manifest.Files {
		clean := filepath.ToSlash(filepath.Clean(candidate.Path))
		if clean != candidate.Path || !strings.HasPrefix(clean, ".visual-hive/snapshots/") || !strings.HasSuffix(strings.ToLower(clean), ".png") || strings.Contains(clean, "../") ||
			candidate.Bytes <= 8 || candidate.Bytes > 20<<20 || len(candidate.SHA256) != sha256.Size*2 || (index > 0 && manifest.Files[index-1].Path >= candidate.Path) {
			return nil, "", fmt.Errorf("setup baseline candidate %q is unsafe or non-canonical", candidate.Path)
		}
		absolute := filepath.Join(root, filepath.FromSlash(clean))
		info, err := os.Lstat(absolute)
		if err != nil || !info.Mode().IsRegular() || info.Size() != candidate.Bytes {
			return nil, "", fmt.Errorf("setup baseline candidate %q is missing or not the exact regular file", clean)
		}
		content, err := os.ReadFile(absolute)
		if err != nil || len(content) < 8 || !strings.EqualFold(hex.EncodeToString(content[:8]), "89504e470d0a1a0a") {
			return nil, "", fmt.Errorf("setup baseline candidate %q is not a PNG", clean)
		}
		digest := sha256.Sum256(content)
		if !strings.EqualFold(hex.EncodeToString(digest[:]), candidate.SHA256) {
			return nil, "", fmt.Errorf("setup baseline candidate %q digest changed", clean)
		}
		total += candidate.Bytes
		expectedPaths[clean] = true
	}
	if total != manifest.TotalBytes || total > 500<<20 {
		return nil, "", fmt.Errorf("setup baseline artifact total bytes are invalid")
	}
	digest, err := setupBaselineCandidateDigest(manifest.Files)
	if err != nil || !strings.EqualFold(digest, manifest.CandidateDigest) {
		return nil, "", fmt.Errorf("setup baseline artifact candidate digest is invalid")
	}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || !expectedPaths[filepath.ToSlash(relative)] {
			return fmt.Errorf("setup baseline artifact contains unexpected file %q", filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return append([]SetupBaselineCandidate(nil), manifest.Files...), strings.ToLower(digest), nil
}

func setupBaselineCandidateDigest(candidates []SetupBaselineCandidate) (string, error) {
	if candidates == nil {
		candidates = []SetupBaselineCandidate{}
	}
	data, err := json.Marshal(candidates)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func newSetupBaselineCorrelation() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func setupBaselineMarker(repository, correlation string) string {
	return fmt.Sprintf("<!-- hive-setup-baseline: %s:%s -->", strings.ToLower(strings.TrimSpace(repository)), strings.ToLower(strings.TrimSpace(correlation)))
}

func setupBaselineCandidatePaths(candidates []SetupBaselineCandidate) []string {
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		paths = append(paths, candidate.Path)
	}
	sort.Strings(paths)
	return paths
}

func createSetupBaselineProposal(ctx context.Context, store *Store, config Config, intent SetupBaselineIntent, client *hivegithub.Client) (SetupBaselineIntent, error) {
	if intent.Phase == SetupBaselineProposalPrepared {
		return publishPreparedSetupBaselineProposal(ctx, store, config, intent, client)
	}
	if intent.Phase != SetupBaselineArtifactVerified {
		return intent, fmt.Errorf("setup baseline proposal requires verified hosted candidates")
	}
	// ArtifactRoot is durable metadata, not durable evidence. The extracted
	// files may have been changed between process runs, so repeat the complete
	// manifest/file/path/digest verification at the last point before copying.
	liveCandidates, liveDigest, err := verifySetupBaselineArtifact(intent.ArtifactRoot, intent)
	if err != nil {
		return intent, fmt.Errorf("re-verify hosted setup baseline artifact before proposal: %w", err)
	}
	if !reflect.DeepEqual(liveCandidates, intent.CaptureCandidates) || !strings.EqualFold(liveDigest, intent.CaptureCandidateDigest) {
		return intent, fmt.Errorf("hosted setup baseline artifact candidates changed after durable verification")
	}
	alreadyPresent, err := canReusePreviouslyApprovedSetupBaseline(ctx, client, config, intent, intent.CaptureHeadSHA)
	if err != nil {
		return intent, fmt.Errorf("check recaptured setup baselines at exact default head: %w", err)
	}
	if alreadyPresent {
		intent.Phase, intent.ExistingHeadReverified = SetupBaselineMerged, true
		intent.MergeSHA, intent.MergeAttemptedAt = strings.ToLower(intent.CaptureHeadSHA), time.Now().UTC()
		return saveSetupBaselineTransition(store, intent, "setup_baseline_existing_head_reverified", true, fmt.Sprintf("run=%d artifact=%d head=%s candidates=%s", intent.CaptureRunID, intent.ArtifactID, intent.CaptureHeadSHA, intent.CandidateDigest))
	}
	defaultBranch, err := ensureCheckout(ctx, config.Repository, config.CheckoutDir)
	if err != nil {
		return intent, err
	}
	if defaultBranch != intent.DefaultBranch {
		return intent, fmt.Errorf("setup baseline default branch changed")
	}
	reviewCandidates, err := setupBaselineReviewDeltaAtCommit(ctx, client, config, intent, intent.CaptureHeadSHA)
	if err != nil {
		return intent, fmt.Errorf("derive exact hosted baseline review delta: %w", err)
	}
	if len(reviewCandidates) == 0 {
		intent.Phase, intent.ExistingHeadReverified = SetupBaselineMerged, true
		intent.MergeSHA, intent.MergeAttemptedAt = strings.ToLower(intent.CaptureHeadSHA), time.Now().UTC()
		return saveSetupBaselineTransition(store, intent, "setup_baseline_existing_head_reverified", true, fmt.Sprintf("run=%d artifact=%d head=%s capture_candidates=%s delta=none", intent.CaptureRunID, intent.ArtifactID, intent.CaptureHeadSHA, intent.CaptureCandidateDigest))
	}
	intent.Candidates = reviewCandidates
	intent.CandidateDigest, err = setupBaselineCandidateDigest(reviewCandidates)
	if err != nil {
		return intent, err
	}
	branch := managedOperationBranch(setupBaselineOperation, config.RepositoryID)
	if _, err := git(ctx, config.CheckoutDir, "switch", "-C", branch, intent.CaptureHeadSHA); err != nil {
		return intent, err
	}
	for _, candidate := range intent.Candidates {
		source := filepath.Join(intent.ArtifactRoot, filepath.FromSlash(candidate.Path))
		destination, err := ensureSetupBaselineDestination(config.CheckoutDir, candidate.Path)
		if err != nil {
			return intent, err
		}
		if info, err := os.Lstat(destination); err == nil && !info.Mode().IsRegular() {
			return intent, fmt.Errorf("baseline destination %s is not a regular file", candidate.Path)
		} else if err != nil && !os.IsNotExist(err) {
			return intent, err
		}
		input, err := os.Open(source)
		if err != nil {
			return intent, err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err == nil {
			_, err = io.Copy(output, input)
		}
		closeErr, inputCloseErr := outputClose(output), input.Close()
		if err != nil {
			return intent, err
		}
		if closeErr != nil {
			return intent, closeErr
		}
		if inputCloseErr != nil {
			return intent, inputCloseErr
		}
	}
	paths := setupBaselineCandidatePaths(intent.Candidates)
	if _, err := git(ctx, config.CheckoutDir, append([]string{"add", "-f", "--"}, paths...)...); err != nil {
		return intent, err
	}
	changed, err := git(ctx, config.CheckoutDir, "diff", "--cached", "--name-only")
	if err != nil || !equalPaths(strings.Fields(changed), paths) {
		return intent, fmt.Errorf("setup baseline proposal changed outside the exact hosted candidates")
	}
	if _, err := git(ctx, config.CheckoutDir, "diff", "--cached", "--check"); err != nil {
		return intent, fmt.Errorf("setup baseline proposal diff is invalid: %w", err)
	}
	if _, err := git(ctx, config.CheckoutDir, "-c", "user.name=Hive Setup", "-c", "user.email=hive-setup@users.noreply.github.com", "commit", "-m", "test(visual): review hosted Linux setup baselines", "-m", managedCommitTrailers(config.RepositoryID, setupBaselineOperation)); err != nil {
		return intent, err
	}
	head, err := git(ctx, config.CheckoutDir, "rev-parse", "HEAD")
	if err != nil {
		return intent, err
	}
	head = strings.ToLower(strings.TrimSpace(head))
	intent.Phase, intent.Branch, intent.CommitSHA = SetupBaselineProposalPrepared, branch, head
	intent.Marker = setupBaselineMarker(config.Repository, intent.CaptureCorrelation)
	intent, err = saveSetupBaselineTransition(store, intent, "setup_baseline_proposal_prepared", true, fmt.Sprintf("run=%d artifact=%d branch=%s head=%s candidates=%s", intent.CaptureRunID, intent.ArtifactID, intent.Branch, intent.CommitSHA, intent.CandidateDigest))
	if err != nil {
		return intent, err
	}
	return publishPreparedSetupBaselineProposal(ctx, store, config, intent, client)
}

func publishPreparedSetupBaselineProposal(ctx context.Context, store *Store, config Config, intent SetupBaselineIntent, client *hivegithub.Client) (SetupBaselineIntent, error) {
	if intent.Phase != SetupBaselineProposalPrepared || intent.Branch != managedOperationBranch(setupBaselineOperation, config.RepositoryID) ||
		!immutableCommit.MatchString(strings.ToLower(intent.CommitSHA)) || intent.Marker != setupBaselineMarker(config.Repository, intent.CaptureCorrelation) {
		return intent, fmt.Errorf("setup baseline proposal is not durably prepared for exact publication")
	}
	branch, head, marker := intent.Branch, strings.ToLower(intent.CommitSHA), intent.Marker
	paths := setupBaselineCandidatePaths(intent.Candidates)
	if err := authorizeSetup(store, integratedPolicy(config), config.Repository, automation.ActionSetupPush); err != nil {
		return intent, err
	}
	if err := pushManagedBranch(ctx, config.CheckoutDir, config.Repository, branch, config.RepositoryID, setupBaselineOperation, head); err != nil {
		return intent, err
	}
	body := fmt.Sprintf("%s\n\nHive captured %d PNG baselines on GitHub-hosted Linux at exact default head `%s`; this PR contains only the %d missing or changed candidates.\n\n- Capture run: %s\n- Artifact ID: `%d`\n- Full capture digest: `%s`\n- Review delta digest: `%s`\n\nHive will not remove the hold or merge until the recorded setup authorizer approves every exact binding through `hive approve-baseline`.\n", marker, len(intent.CaptureCandidates), intent.CaptureHeadSHA, len(paths), intent.CaptureRunURL, intent.ArtifactID, intent.CaptureCandidateDigest, intent.CandidateDigest)
	pull, err := client.UpsertHeldPullRequest(ctx, config.Repository, branch, head, config.DefaultBranch, "Review hosted Linux Visual Hive setup baselines", body, marker)
	if err != nil {
		return intent, err
	}
	snapshot, err := client.InspectManagedPullRequestExact(ctx, config.Repository, config.RepositoryID, pull.Number, marker, branch, head, config.DefaultBranch)
	if err != nil {
		return intent, err
	}
	if !strings.EqualFold(snapshot.BaseSHA, intent.CaptureHeadSHA) || !equalPaths(snapshot.ChangedFiles, paths) {
		return intent, fmt.Errorf("setup baseline PR does not target the exact capture head or candidate set")
	}
	intent.Phase = SetupBaselinePROpen
	intent.PRNumber, intent.PRURL, intent.BaseSHA, intent.DiffDigest = snapshot.Number, snapshot.URL, strings.ToLower(snapshot.BaseSHA), strings.ToLower(snapshot.DiffDigest)
	return saveSetupBaselineTransition(store, intent, "setup_baseline_pr_open", true, fmt.Sprintf("run=%d artifact=%d pr=%d head=%s candidates=%s", intent.CaptureRunID, intent.ArtifactID, intent.PRNumber, intent.CommitSHA, intent.CandidateDigest))
}

// ensureSetupBaselineDestination creates only ordinary directories beneath the
// state-owned checkout. It rejects symlinks/junctions/reparse-like paths by
// checking both Lstat metadata and the fully evaluated parent after each step.
func ensureSetupBaselineDestination(checkout, relative string) (string, error) {
	root, err := filepath.Abs(checkout)
	if err != nil {
		return "", err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("setup baseline checkout is not an ordinary directory")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve setup baseline checkout: %w", err)
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("setup baseline destination %q escapes the checkout", relative)
	}
	destination := filepath.Join(root, clean)
	parent := filepath.Dir(destination)
	relParent, err := filepath.Rel(root, parent)
	if err != nil || relParent == ".." || strings.HasPrefix(relParent, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("setup baseline destination %q escapes the checkout", relative)
	}
	cursor := root
	if relParent != "." {
		for _, component := range strings.Split(relParent, string(filepath.Separator)) {
			if component == "" || component == "." || component == ".." {
				return "", fmt.Errorf("setup baseline destination %q has an unsafe parent", relative)
			}
			cursor = filepath.Join(cursor, component)
			info, statErr := os.Lstat(cursor)
			if os.IsNotExist(statErr) {
				if mkdirErr := os.Mkdir(cursor, 0o755); mkdirErr != nil {
					return "", mkdirErr
				}
				info, statErr = os.Lstat(cursor)
			}
			if statErr != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("setup baseline destination parent %q is not an ordinary directory", cursor)
			}
			realCursor, evalErr := filepath.EvalSymlinks(cursor)
			if evalErr != nil {
				return "", fmt.Errorf("resolve setup baseline destination parent: %w", evalErr)
			}
			realRel, relErr := filepath.Rel(realRoot, realCursor)
			if relErr != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("setup baseline destination parent escapes the real checkout")
			}
			if filepath.Clean(realCursor) != filepath.Clean(cursor) {
				return "", fmt.Errorf("setup baseline destination parent %q resolves through a link or reparse point", cursor)
			}
		}
	}
	return destination, nil
}

func outputClose(file *os.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func setupBaselinePlanCommand(stateDir string, intent SetupBaselineIntent) string {
	return fmt.Sprintf("hive approve-baseline --state-dir %q --plan --json", strings.TrimSpace(stateDir))
}

func resetSetupBaselineCapture(intent SetupBaselineIntent, retireCorrelation string) SetupBaselineIntent {
	retired := append([]string(nil), intent.RetiredCaptureCorrelations...)
	retireCorrelation = strings.ToLower(strings.TrimSpace(retireCorrelation))
	if workflowDispatchCorrelationPattern.MatchString(retireCorrelation) && !containsExact(retired, retireCorrelation) {
		retired = append(retired, retireCorrelation)
		if len(retired) > 32 {
			retired = append([]string(nil), retired[len(retired)-32:]...)
		}
	}
	now := time.Now().UTC()
	return SetupBaselineIntent{
		SchemaVersion: SetupBaselineSchema, Phase: SetupBaselinePending, Repository: intent.Repository, RepositoryID: intent.RepositoryID,
		DefaultBranch: intent.DefaultBranch, AuthorizerID: intent.AuthorizerID, SetupPRNumber: intent.SetupPRNumber, SetupPRURL: intent.SetupPRURL,
		SetupHeadSHA: intent.SetupHeadSHA, VisualHiveConfigDigest: intent.VisualHiveConfigDigest, ScreenshotContractDigest: intent.ScreenshotContractDigest,
		InitialBaselineDigest: intent.InitialBaselineDigest, InitialBaselineCandidates: append([]SetupBaselineCandidate(nil), intent.InitialBaselineCandidates...),
		PreviouslyApprovedDigest:     intent.PreviouslyApprovedDigest,
		PreviouslyApprovedCandidates: append([]SetupBaselineCandidate(nil), intent.PreviouslyApprovedCandidates...),
		RetiredCaptureCorrelations:   retired, CreatedAt: intent.CreatedAt, UpdatedAt: now,
	}
}

func containsExact(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func revokeSetupBaselineDispatch(store *Store, config Config, dispatch WorkflowDispatchIntent) error {
	intent, exists, err := store.LoadSetupBaselineIntent()
	if err != nil {
		return err
	}
	if !exists || intent.Phase != SetupBaselineDispatched || intent.Repository != config.Repository || intent.RepositoryID != config.RepositoryID ||
		dispatch.Operation != setupBaselineWorkflowOperation || dispatch.CorrelationID != intent.CaptureCorrelation {
		return fmt.Errorf("ambiguous dispatch is not the exact active setup baseline capture")
	}
	reset := resetSetupBaselineCapture(intent, dispatch.CorrelationID)
	_, err = saveSetupBaselineTransition(store, reset, "setup_baseline_dispatch_revoked", true, fmt.Sprintf("correlation=%s request=%s", dispatch.CorrelationID, dispatch.RequestDigest))
	return err
}

// reconcileRequiredSetupBaselineBeforeRun repairs the only durable split that
// can occur between saving Config and saving setup-baseline.json. The required
// bit is written with Config before setup returns, so a crash in that narrow
// window can never silently skip hosted baseline review. Recovery records a
// fresh pending intent at the current installed default head and stops this
// run; the scheduler's next retry resumes the ordinary state machine.
func reconcileRequiredSetupBaselineBeforeRun(ctx context.Context, store *Store, config Config, client *hivegithub.Client) (SetupBaselineIntent, bool, error) {
	if rebind, exists, rebindErr := store.LoadSetupBaselineRebindIntent(); rebindErr != nil {
		return SetupBaselineIntent{}, true, fmt.Errorf("read setup baseline rebind recovery: %w", rebindErr)
	} else if exists {
		return rebind.Source, true, fmt.Errorf("setup baseline reconfiguration phase %s blocks production; retry %s", rebind.Phase, SetupBaselineRebindNextCommand(rebind))
	}
	intent, exists, err := store.LoadSetupBaselineIntent()
	if err != nil {
		return intent, true, err
	}
	if !exists && config.SetupBaselineRequired {
		currentHead, headErr := currentDefaultBranchHead(ctx, client, config)
		if headErr != nil {
			return intent, true, fmt.Errorf("recover required setup baseline checkpoint: %w", headErr)
		}
		intent, err = ensureSetupBaselineIntent(store, config, config.SetupHeadSHA, config.SetupPRNumber, config.SetupPRURL)
		if err != nil {
			return intent, true, fmt.Errorf("recover required setup baseline checkpoint: %w", err)
		}
		return intent, true, fmt.Errorf("recovered missing required setup baseline checkpoint at %s; run Hive again to start the exact hosted capture", currentHead)
	}
	return reconcileSetupBaselineBeforeRun(ctx, store, config, client)
}

func reconcileSetupBaselineBeforeRun(ctx context.Context, store *Store, config Config, client *hivegithub.Client) (SetupBaselineIntent, bool, error) {
	intent, exists, err := store.LoadSetupBaselineIntent()
	if err != nil || !exists {
		return intent, false, err
	}
	intent, err = flushSetupBaselineAudit(store, intent)
	if err != nil {
		return intent, true, fmt.Errorf("finish pending setup baseline audit before lifecycle progress: %w", err)
	}
	if intent.Repository != config.Repository || intent.RepositoryID != config.RepositoryID || intent.DefaultBranch != config.DefaultBranch || intent.AuthorizerID != config.SetupAuthorizationActorID ||
		intent.SetupPRNumber != config.SetupPRNumber || intent.SetupPRURL != config.SetupPRURL || !strings.EqualFold(intent.SetupHeadSHA, config.SetupHeadSHA) ||
		!strings.EqualFold(intent.VisualHiveConfigDigest, config.VisualHiveConfigDigest) || !strings.EqualFold(intent.ScreenshotContractDigest, config.SetupBaselineContractDigest) ||
		!strings.EqualFold(intent.InitialBaselineDigest, config.SetupBaselineInitialDigest) || !reflect.DeepEqual(intent.InitialBaselineCandidates, config.SetupBaselineInitialCandidates) {
		return intent, true, fmt.Errorf("setup baseline intent no longer matches the installed repository, setup PR/head, Visual Hive config, screenshot contracts, or setup authorizer")
	}
	currentHead, err := currentDefaultBranchHead(ctx, client, config)
	if err != nil {
		return intent, true, err
	}
	if intent.Phase == SetupBaselineMerged && !strings.EqualFold(intent.MergeSHA, currentHead) {
		reset, resetErr := resetSetupBaselineAfterMergedHeadDrift(ctx, store, config, client, intent, currentHead)
		if resetErr != nil {
			return intent, true, resetErr
		}
		intent = reset
	} else if intent.Phase != SetupBaselinePending && intent.Phase != SetupBaselineMerged && intent.Phase != SetupBaselineProductionVerified && !strings.EqualFold(intent.CaptureHeadSHA, currentHead) {
		reset, resetErr := resetSetupBaselineForDrift(ctx, store, config, client, intent, currentHead)
		if resetErr != nil {
			return intent, true, resetErr
		}
		intent = reset
	}
	if err := reconcileVerifiedSetupBaselineDispatch(store, intent); err != nil {
		return intent, true, err
	}
	if intent.Phase == SetupBaselineProductionVerified {
		return intent, false, nil
	}
	if intent.Phase == SetupBaselineMerged {
		if err := verifySetupBaselineCandidatesAtCommit(ctx, client, config, intent, currentHead); err != nil {
			return intent, true, err
		}
		workflow, err := dispatchAndWait(ctx, client, config)
		if err != nil {
			return intent, true, fmt.Errorf("verify merged setup baselines with full production workflow: %w", err)
		}
		if workflow.Conclusion != "success" || workflow.RepositoryTestOverall != 0 {
			return intent, true, fmt.Errorf("full production verification after baseline merge is not green")
		}
		bundle, _, err := client.FetchAndVerifyVisualHiveBundle(ctx, hivegithub.VisualHiveArtifactRequest{
			Repository: config.Repository, WorkflowRunID: workflow.RunID, ArtifactID: workflow.BundleArtifact, SourceArtifactID: workflow.EvidenceArtifact,
			DestinationDir: filepath.Join(config.StateDir, "setup-baseline", "production-verification"), TargetRef: config.DefaultBranch,
			MaxACMM: config.ACMMLevel, ExpectedWorkflowName: visualHiveProductionWorkflowName, ExpectedWorkflowPath: visualHiveProductionWorkflowPath, ExpectedEvent: visualHiveProductionWorkflowEvent,
			ExpectedRunName: workflowDispatchDisplayTitle(workflow.CorrelationID), ExpectedProducerGitCommit: config.VisualHiveRef, FetchSourceArtifact: true,
		})
		if err != nil {
			return intent, true, fmt.Errorf("validate post-baseline production bundle without lifecycle writes: %w", err)
		}
		if !postBaselineProductionValidationAccepted(bundle.Validation.Status, bundle.Validation.Trusted) {
			return intent, true, fmt.Errorf("post-baseline production bundle is not trusted valid evidence")
		}
		if _, err := requireLiveInstalledWorkflowHead(ctx, client, config, workflow.HeadSHA); err != nil {
			return intent, true, err
		}
		if err := consumeWorkflowDispatch(config.StateDir, workflow); err != nil {
			return intent, true, err
		}
		intent.Phase, intent.ProductionRunID, intent.ProductionHeadSHA = SetupBaselineProductionVerified, workflow.RunID, strings.ToLower(workflow.HeadSHA)
		intent, err = saveSetupBaselineTransition(store, intent, "setup_baseline_production_verified", true, fmt.Sprintf("run=%d head=%s", workflow.RunID, workflow.HeadSHA))
		if err != nil {
			return intent, true, err
		}
		return intent, true, fmt.Errorf("setup baseline production verification succeeded; run Hive again to permit activation and lifecycle writes")
	}
	if intent.Phase == SetupBaselineApproved {
		intent, merged, mergeErr := mergeApprovedSetupBaseline(ctx, store, config, intent, client)
		if mergeErr != nil {
			return intent, true, fmt.Errorf("complete durably approved setup baseline merge: %w", mergeErr)
		}
		if merged {
			return intent, true, fmt.Errorf("approved setup baselines merged at %s; run Hive again for exact production verification", intent.MergeSHA)
		}
		return intent, true, fmt.Errorf("setup baseline approval remains pending exact merge gates")
	}
	if intent.Phase == SetupBaselinePROpen {
		return intent, true, fmt.Errorf("setup baseline review PR %s requires exact approval; run %s", intent.PRURL, setupBaselinePlanCommand(config.StateDir, intent))
	}
	if intent.Phase == SetupBaselinePending {
		if dispatch, dispatchExists, dispatchErr := store.LoadWorkflowDispatchIntent(); dispatchErr != nil {
			return intent, true, dispatchErr
		} else if dispatchExists {
			if dispatch.Operation != setupBaselineWorkflowOperation || !containsExact(intent.RetiredCaptureCorrelations, dispatch.CorrelationID) {
				return intent, true, fmt.Errorf("a different or non-retired workflow dispatch blocks setup baseline capture")
			}
			if err := store.DeleteWorkflowDispatchIntent(); err != nil {
				return intent, true, fmt.Errorf("finish retiring revoked setup baseline dispatch: %w", err)
			}
		}
		dispatchIntent, err := newWorkflowDispatchIntentForOperation(config, "hive-visual-hive.yml", config.DefaultBranch, setupBaselineWorkflowOperation, "", time.Time{})
		if err != nil {
			return intent, true, err
		}
		intent.CaptureCorrelation, intent.CaptureHeadSHA = dispatchIntent.CorrelationID, currentHead
		intent.DispatchAttemptedAt, intent.Phase = dispatchIntent.PreparedAt, SetupBaselineDispatched
		if err := store.SaveSetupBaselineIntent(intent); err != nil {
			return intent, true, err
		}
		if err := store.SaveWorkflowDispatchIntent(dispatchIntent); err != nil {
			return intent, true, fmt.Errorf("persist exact setup baseline workflow intent: %w", err)
		}
	}
	if intent.Phase == SetupBaselineDispatched {
		owner, repo, _ := strings.Cut(config.Repository, "/")
		dispatchIntent, dispatchExists, dispatchErr := store.LoadWorkflowDispatchIntent()
		if dispatchErr != nil {
			return intent, true, dispatchErr
		}
		if !dispatchExists {
			dispatchIntent, err = newWorkflowDispatchIntentForOperation(config, "hive-visual-hive.yml", config.DefaultBranch, setupBaselineWorkflowOperation, intent.CaptureCorrelation, intent.DispatchAttemptedAt)
			if err != nil {
				return intent, true, err
			}
			if err := store.SaveWorkflowDispatchIntent(dispatchIntent); err != nil {
				return intent, true, fmt.Errorf("recover prepared setup baseline dispatch: %w", err)
			}
		}
		if validateErr := validateWorkflowDispatchOperationBinding(dispatchIntent, config, "hive-visual-hive.yml", config.DefaultBranch, setupBaselineWorkflowOperation); validateErr != nil {
			return intent, true, fmt.Errorf("setup baseline dispatch checkpoint mismatch: %w", validateErr)
		}
		if dispatchIntent.CorrelationID != intent.CaptureCorrelation {
			return intent, true, fmt.Errorf("setup baseline dispatch correlation changed")
		}
		run, liveDispatch, dispatchErr := dispatchAndWaitAttempt(ctx, client, store, config, owner, repo, "hive-visual-hive.yml", config.DefaultBranch, setupBaselineWorkflowOperation)
		if dispatchErr != nil {
			return intent, true, dispatchErr
		}
		intent.DispatchAcknowledgedAt, intent.CaptureRunID, intent.CaptureRunURL = liveDispatch.DispatchAcknowledgedAt, run.GetID(), run.GetHTMLURL()
		if err := store.SaveSetupBaselineIntent(intent); err != nil {
			return intent, true, err
		}
		if run.GetConclusion() != "success" || !strings.EqualFold(run.GetHeadSHA(), intent.CaptureHeadSHA) {
			reset := resetSetupBaselineCapture(intent, intent.CaptureCorrelation)
			reset, saveErr := saveSetupBaselineTransition(store, reset, "setup_baseline_capture_terminal_retry", false, fmt.Sprintf("run=%d correlation=%s conclusion=%s head=%s", run.GetID(), intent.CaptureCorrelation, run.GetConclusion(), run.GetHeadSHA()))
			if saveErr != nil {
				return intent, true, fmt.Errorf("setup baseline capture failed; persist restart state: %w", saveErr)
			}
			if discardErr := discardWorkflowDispatch(store, liveDispatch, run.GetID()); discardErr != nil {
				return reset, true, fmt.Errorf("setup baseline capture failed; retire exact terminal dispatch: %w", discardErr)
			}
			return reset, true, fmt.Errorf("setup baseline capture run %d was %s or stale; exact correlation retired, run Hive again for a fresh capture", run.GetID(), run.GetConclusion())
		}
		artifactName := "hive-setup-baselines-" + intent.CaptureCorrelation
		artifact, err := client.FindUniqueWorkflowRunArtifactByName(ctx, owner, repo, run.GetID(), artifactName)
		if err != nil || artifact.GetID() <= 0 {
			return intent, true, fmt.Errorf("find unique correlation-bound setup baseline artifact: %w", err)
		}
		artifactID := artifact.GetID()
		verified, err := client.FetchAndVerifySetupBaselineArtifact(ctx, hivegithub.SetupBaselineArtifactRequest{
			Repository: config.Repository, RepositoryID: config.RepositoryID, WorkflowRunID: run.GetID(), ArtifactID: artifactID,
			ArtifactName: artifactName, CaptureHeadSHA: intent.CaptureHeadSHA, DefaultBranch: config.DefaultBranch,
			ExpectedWorkflowName: visualHiveProductionWorkflowName, ExpectedWorkflowPath: visualHiveProductionWorkflowPath,
			ExpectedRunName: workflowDispatchDisplayTitle(intent.CaptureCorrelation), DestinationDir: filepath.Join(config.StateDir, "setup-baseline", "capture-artifacts"),
		})
		if err != nil {
			return intent, true, err
		}
		intent.CaptureRunID, intent.CaptureRunURL, intent.ArtifactID, intent.ArtifactName, intent.ArtifactRoot = run.GetID(), run.GetHTMLURL(), artifactID, artifactName, verified.ArtifactRoot
		intent.CaptureCandidates, intent.CaptureCandidateDigest, err = verifySetupBaselineArtifact(verified.ArtifactRoot, intent)
		if err != nil {
			return intent, true, err
		}
		intent.Phase = SetupBaselineArtifactVerified
		intent, err = saveSetupBaselineTransition(store, intent, "setup_baseline_artifact_verified", true, fmt.Sprintf("run=%d artifact=%d head=%s capture_candidates=%s", intent.CaptureRunID, intent.ArtifactID, intent.CaptureHeadSHA, intent.CaptureCandidateDigest))
		if err != nil {
			return intent, true, err
		}
		if err := discardWorkflowDispatch(store, liveDispatch, run.GetID()); err != nil {
			return intent, true, fmt.Errorf("consume exact setup baseline capture dispatch: %w", err)
		}
	}
	if intent.Phase == SetupBaselineArtifactVerified || intent.Phase == SetupBaselineProposalPrepared {
		intent, err = createSetupBaselineProposal(ctx, store, config, intent, client)
		if err != nil {
			return intent, true, err
		}
		if intent.Phase == SetupBaselineMerged && intent.ExistingHeadReverified {
			return intent, true, fmt.Errorf("recaptured setup baselines already match exact default head %s; run Hive again for full production verification", intent.MergeSHA)
		}
	}
	return intent, true, fmt.Errorf("setup baseline review PR %s is ready; run %s", intent.PRURL, setupBaselinePlanCommand(config.StateDir, intent))
}

func postBaselineProductionValidationAccepted(status string, trusted bool) bool {
	// This gate proves that the installed hosted workflow can produce a
	// structurally valid, provenance-trusted bundle. It must not require a
	// defect-free scan: a failed Visual Hive verdict is exactly the evidence
	// the governed lifecycle needs to create present findings. The bundle's
	// authoritative flag remains intact and continues to gate absence-based
	// resolution in the lifecycle importer.
	return status == "passed" && trusted
}

func reconcileVerifiedSetupBaselineDispatch(store *Store, intent SetupBaselineIntent) error {
	if intent.Phase == SetupBaselinePending || intent.Phase == SetupBaselineDispatched || intent.CaptureRunID <= 0 {
		return nil
	}
	dispatch, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil || !exists {
		return err
	}
	// A failed or interrupted production verifier deliberately retains its
	// exact checkpoint. Leave it for dispatchAndWait to resume both while the
	// baseline is being verified and after that verification has completed.
	if (intent.Phase == SetupBaselineMerged || intent.Phase == SetupBaselineProductionVerified) && dispatch.Operation == "production" {
		return nil
	}
	if dispatch.Operation != setupBaselineWorkflowOperation || dispatch.CorrelationID != intent.CaptureCorrelation || dispatch.RunID != intent.CaptureRunID {
		return fmt.Errorf("lingering setup baseline dispatch no longer matches the verified capture")
	}
	if err := discardWorkflowDispatch(store, dispatch, intent.CaptureRunID); err != nil {
		return fmt.Errorf("finish consuming verified setup baseline dispatch: %w", err)
	}
	return nil
}

func resetSetupBaselineAfterMergedHeadDrift(ctx context.Context, store *Store, config Config, client *hivegithub.Client, intent SetupBaselineIntent, currentHead string) (SetupBaselineIntent, error) {
	retirement, err := verifyAndRetireMergedSetupBaselineBranch(ctx, store, config, intent, client, "merged_head_drift")
	if err != nil {
		return intent, err
	}
	reset := resetSetupBaselineCapture(intent, intent.CaptureCorrelation)
	reset.PreviouslyApprovedDigest, reset.PreviouslyApprovedCandidates = intent.CandidateDigest, append([]SetupBaselineCandidate(nil), intent.Candidates...)
	return saveSetupBaselineTransition(store, reset, "setup_baseline_merged_head_drift_recapture", true, fmt.Sprintf("merged_head=%s current_head=%s %s", intent.MergeSHA, currentHead, retirement))
}

func resetSetupBaselineForDrift(ctx context.Context, store *Store, config Config, client *hivegithub.Client, intent SetupBaselineIntent, currentHead string) (SetupBaselineIntent, error) {
	if intent.Branch != "" {
		if _, _, err := client.CloseManagedPullRequestForCancellation(ctx, config.Repository, config.RepositoryID, intent.PRNumber, intent.PRURL, intent.Marker, intent.Branch, intent.DefaultBranch); err != nil {
			return intent, fmt.Errorf("close stale setup baseline PR before recapture: %w", err)
		}
		if _, _, err := client.DeleteSetupBaselineBranchDescendantExact(ctx, config.Repository, intent.Branch, intent.CommitSHA); err != nil {
			return intent, fmt.Errorf("retire stale setup baseline branch before recapture: %w", err)
		}
	}
	reset := resetSetupBaselineCapture(intent, intent.CaptureCorrelation)
	return saveSetupBaselineTransition(store, reset, "setup_baseline_drift_recapture", true, fmt.Sprintf("old_capture_head=%s current_head=%s", intent.CaptureHeadSHA, currentHead))
}

func verifySetupBaselineCandidatesAtCommit(ctx context.Context, client *hivegithub.Client, config Config, intent SetupBaselineIntent, commitSHA string) error {
	matched, err := setupBaselineCandidateTreeMatchesCommit(ctx, client, config, intent.CaptureCandidates, commitSHA)
	if err != nil {
		return err
	}
	if !matched {
		return fmt.Errorf("reviewed setup baseline candidates changed at current default head")
	}
	return nil
}

func canReusePreviouslyApprovedSetupBaseline(ctx context.Context, client *hivegithub.Client, config Config, intent SetupBaselineIntent, commitSHA string) (bool, error) {
	if len(intent.PreviouslyApprovedCandidates) == 0 || !strings.EqualFold(intent.PreviouslyApprovedDigest, intent.CandidateDigest) ||
		!reflect.DeepEqual(intent.PreviouslyApprovedCandidates, intent.Candidates) {
		return false, nil
	}
	return setupBaselineCandidateTreeMatchesCommit(ctx, client, config, intent.Candidates, commitSHA)
}

func setupBaselineCandidateTreeMatchesCommit(ctx context.Context, client *hivegithub.Client, config Config, candidates []SetupBaselineCandidate, commitSHA string) (bool, error) {
	delta, err := setupBaselineCandidateDeltaAtCommit(ctx, client, config, candidates, commitSHA)
	if err != nil {
		return false, err
	}
	return len(delta) == 0, nil
}

func setupBaselineCandidateDeltaAtCommit(ctx context.Context, client *hivegithub.Client, config Config, candidates []SetupBaselineCandidate, commitSHA string) ([]SetupBaselineCandidate, error) {
	owner, repo, _ := strings.Cut(config.Repository, "/")
	tree, _, err := client.GoGitHub().Git.GetTree(ctx, owner, repo, commitSHA, true)
	if err != nil {
		return nil, fmt.Errorf("read exact setup baseline tree at %s: %w", commitSHA, err)
	}
	if tree.GetTruncated() {
		return nil, fmt.Errorf("setup baseline tree at %s is truncated", commitSHA)
	}
	entries := map[string]*gh.TreeEntry{}
	for _, entry := range tree.Entries {
		entries[filepath.ToSlash(entry.GetPath())] = entry
	}
	delta := make([]SetupBaselineCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		entry, present := entries[candidate.Path]
		if !present {
			delta = append(delta, candidate)
			continue
		}
		if entry.GetType() != "blob" || (entry.GetMode() != "100644" && entry.GetMode() != "100755") {
			return nil, fmt.Errorf("existing setup baseline %s is not a regular Git blob", candidate.Path)
		}
		if entry.GetSize() <= 0 || entry.GetSize() > 20<<20 || int64(entry.GetSize()) != candidate.Bytes || !immutableCommit.MatchString(strings.ToLower(entry.GetSHA())) {
			delta = append(delta, candidate)
			continue
		}
		value, _, err := client.GoGitHub().Git.GetBlobRaw(ctx, owner, repo, entry.GetSHA())
		if err != nil {
			return nil, fmt.Errorf("read exact reviewed setup baseline blob %s at %s: %w", candidate.Path, entry.GetSHA(), err)
		}
		if int64(len(value)) > 20<<20 {
			return nil, fmt.Errorf("reviewed setup baseline %s exceeds the strict 20 MiB bound", candidate.Path)
		}
		digest := sha256.Sum256(value)
		if int64(len(value)) != candidate.Bytes || !strings.EqualFold(hex.EncodeToString(digest[:]), candidate.SHA256) {
			delta = append(delta, candidate)
		}
	}
	return delta, nil
}

func setupBaselineReviewDeltaAtCommit(ctx context.Context, client *hivegithub.Client, config Config, intent SetupBaselineIntent, commitSHA string) ([]SetupBaselineCandidate, error) {
	liveDelta, err := setupBaselineCandidateDeltaAtCommit(ctx, client, config, intent.CaptureCandidates, commitSHA)
	if err != nil {
		return nil, err
	}
	notLiveExact := map[string]bool{}
	for _, candidate := range liveDelta {
		notLiveExact[candidate.Path] = true
	}
	trusted := map[string]SetupBaselineCandidate{}
	for _, candidate := range append(append([]SetupBaselineCandidate(nil), intent.InitialBaselineCandidates...), intent.PreviouslyApprovedCandidates...) {
		trusted[candidate.Path] = candidate
	}
	review := make([]SetupBaselineCandidate, 0, len(intent.CaptureCandidates))
	for _, candidate := range intent.CaptureCandidates {
		bound, exists := trusted[candidate.Path]
		if exists && bound == candidate && !notLiveExact[candidate.Path] {
			continue
		}
		review = append(review, candidate)
	}
	return review, nil
}

func drainSetupBaselineForUninstall(ctx context.Context, store *Store, config Config, client *hivegithub.Client) error {
	if err := drainSetupBaselineRebindForUninstall(ctx, store, config, client); err != nil {
		return err
	}
	intent, exists, err := store.LoadSetupBaselineIntent()
	if err != nil || !exists {
		return err
	}
	intent, err = flushSetupBaselineAudit(store, intent)
	if err != nil {
		return fmt.Errorf("finish pending setup baseline audit before uninstall drain: %w", err)
	}
	if intent.Repository != config.Repository || intent.RepositoryID != config.RepositoryID || intent.AuthorizerID != config.SetupAuthorizationActorID {
		return fmt.Errorf("setup baseline state no longer matches installed repository authority")
	}
	detail := fmt.Sprintf("phase=%s run=%d artifact=%d pr=%d head=%s", intent.Phase, intent.CaptureRunID, intent.ArtifactID, intent.PRNumber, intent.CommitSHA)
	if intent.Phase == SetupBaselineMerged || intent.Phase == SetupBaselineProductionVerified {
		retirement, retireErr := verifyAndRetireMergedSetupBaselineBranch(ctx, store, config, intent, client, "uninstall")
		if retireErr != nil {
			return retireErr
		}
		detail += " " + retirement
	} else if intent.PRNumber > 0 && (intent.Phase == SetupBaselinePROpen || intent.Phase == SetupBaselineApproved) {
		snapshot, inspectErr := client.InspectManagedPullRequestExact(ctx, config.Repository, config.RepositoryID, intent.PRNumber, intent.Marker, intent.Branch, intent.CommitSHA, intent.DefaultBranch)
		if inspectErr != nil {
			return inspectErr
		}
		if snapshot.Merged {
			if intent.MergeAttemptedAt.IsZero() || !immutableCommit.MatchString(strings.ToLower(snapshot.MergeSHA)) ||
				!strings.EqualFold(snapshot.DiffDigest, intent.DiffDigest) || !equalPaths(snapshot.ChangedFiles, setupBaselineCandidatePaths(intent.Candidates)) {
				return fmt.Errorf("setup baseline PR merged without exact durable Hive merge authority; uninstall cannot guess its disposition")
			}
			retirement, retireErr := retireSetupBaselineBranchRef(ctx, store, config, intent, client, snapshot.MergeSHA, "uninstall_recovered_merge")
			if retireErr != nil {
				return retireErr
			}
			detail += " merged=" + snapshot.MergeSHA + " " + retirement
		} else {
			closed, found, closeErr := client.CloseManagedPullRequestForCancellation(ctx, config.Repository, config.RepositoryID, intent.PRNumber, intent.PRURL, intent.Marker, intent.Branch, intent.DefaultBranch)
			if closeErr != nil || !found {
				if closeErr == nil {
					closeErr = fmt.Errorf("recorded setup baseline PR disappeared")
				}
				return closeErr
			}
			deleted, retiredHead, deleteErr := client.DeleteSetupBaselineBranchDescendantExact(ctx, config.Repository, intent.Branch, intent.CommitSHA)
			if deleteErr != nil {
				return deleteErr
			}
			detail += fmt.Sprintf(" closed_pr=%d retired_head=%s branch_deleted=%t", closed.Number, retiredHead, deleted)
		}
	} else if intent.Branch != "" {
		closed, found, closeErr := client.CloseManagedPullRequestForCancellation(ctx, config.Repository, config.RepositoryID, intent.PRNumber, intent.PRURL, intent.Marker, intent.Branch, intent.DefaultBranch)
		if closeErr != nil {
			return closeErr
		}
		deleted, retiredHead, deleteErr := client.DeleteSetupBaselineBranchDescendantExact(ctx, config.Repository, intent.Branch, intent.CommitSHA)
		if deleteErr != nil {
			return deleteErr
		}
		detail += fmt.Sprintf(" discovered_pr=%t closed_pr=%d retired_head=%s branch_deleted=%t", found, closed.Number, retiredHead, deleted)
	}
	if _, err := saveSetupBaselineTransition(store, intent, "uninstall_setup_baseline_drained", true, detail); err != nil {
		return err
	}
	return store.DeleteSetupBaselineIntent()
}

func verifyAndRetireMergedSetupBaselineBranch(ctx context.Context, store *Store, config Config, intent SetupBaselineIntent, client *hivegithub.Client, purpose string) (string, error) {
	if intent.Phase != SetupBaselineMerged && intent.Phase != SetupBaselineProductionVerified {
		return "", fmt.Errorf("setup baseline merged branch retirement requires merged or production-verified state")
	}
	if intent.ExistingHeadReverified {
		if intent.PRNumber != 0 || intent.PRURL != "" || intent.Branch != "" || intent.CommitSHA != "" || !immutableCommit.MatchString(strings.ToLower(intent.MergeSHA)) || intent.MergeAttemptedAt.IsZero() {
			return "", fmt.Errorf("existing-head setup baseline merge has an invalid no-PR binding")
		}
		return fmt.Sprintf("existing_head_reverified=true merge=%s branch_deleted=false", intent.MergeSHA), nil
	}
	if client == nil || intent.PRNumber <= 0 || intent.Branch == "" || !immutableCommit.MatchString(strings.ToLower(intent.CommitSHA)) ||
		!immutableCommit.MatchString(strings.ToLower(intent.MergeSHA)) || intent.MergeAttemptedAt.IsZero() {
		return "", fmt.Errorf("merged setup baseline state lacks exact PR, branch, head, or merge evidence")
	}
	snapshot, err := client.InspectManagedPullRequestExact(ctx, config.Repository, config.RepositoryID, intent.PRNumber, intent.Marker, intent.Branch, intent.CommitSHA, intent.DefaultBranch)
	if err != nil {
		return "", fmt.Errorf("inspect exact merged setup baseline PR before %s retirement: %w", purpose, err)
	}
	if !snapshot.Merged || !strings.EqualFold(snapshot.MergeSHA, intent.MergeSHA) || !strings.EqualFold(snapshot.HeadSHA, intent.CommitSHA) ||
		!strings.EqualFold(snapshot.BaseSHA, intent.BaseSHA) || !strings.EqualFold(snapshot.DiffDigest, intent.DiffDigest) ||
		!equalPaths(snapshot.ChangedFiles, setupBaselineCandidatePaths(intent.Candidates)) {
		return "", fmt.Errorf("merged setup baseline PR no longer matches its exact durable merge, refs, diff, or candidate paths")
	}
	return retireSetupBaselineBranchRef(ctx, store, config, intent, client, snapshot.MergeSHA, purpose)
}

func retireSetupBaselineBranchRef(ctx context.Context, store *Store, config Config, intent SetupBaselineIntent, client *hivegithub.Client, mergeSHA, purpose string) (string, error) {
	detail := fmt.Sprintf("purpose=%s pr=%d branch=%s owned_head=%s merge=%s", purpose, intent.PRNumber, intent.Branch, intent.CommitSHA, mergeSHA)
	if err := store.AuditStrict(AuditEntry{Action: "authorize_setup_baseline_merged_branch_retirement", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return "", err
	}
	deleted, retiredHead, err := client.DeleteSetupBaselineBranchDescendantExact(ctx, config.Repository, intent.Branch, intent.CommitSHA)
	if err != nil {
		return "", fmt.Errorf("retire exact merged setup baseline branch for %s: %w", purpose, err)
	}
	return fmt.Sprintf("retired_head=%s branch_deleted=%t", retiredHead, deleted), nil
}

func drainSetupBaselineRebindForUninstall(ctx context.Context, store *Store, config Config, client *hivegithub.Client) error {
	rebind, exists, err := store.LoadSetupBaselineRebindIntent()
	if err != nil || !exists {
		return err
	}
	rebind, err = flushSetupBaselineRebindAudit(store, rebind)
	if err != nil {
		return fmt.Errorf("finish pending setup baseline rebind audit before uninstall drain: %w", err)
	}
	if rebind.Repository != config.Repository || rebind.RepositoryID != config.RepositoryID || rebind.Source.AuthorizerID != config.SetupAuthorizationActorID {
		return fmt.Errorf("setup baseline rebind state no longer matches installed repository authority")
	}
	if rebind.Phase == SetupBaselineRebindPrepared {
		if err := retireSetupBaselineRebindResources(ctx, store, client, &rebind); err != nil {
			return fmt.Errorf("retire exact setup baseline rebind resources before uninstall: %w", err)
		}
		rebind.Phase, rebind.ResourcesRetiredAt = SetupBaselineRebindResourcesRetired, time.Now().UTC()
		rebind, err = saveSetupBaselineRebindTransition(store, rebind, "uninstall_setup_baseline_rebind_resources_retired", true,
			fmt.Sprintf("source_phase=%s source_capture=%s", rebind.Source.Phase, rebind.Source.CaptureCorrelation))
		if err != nil {
			return err
		}
	}
	if active, activeExists, loadErr := store.LoadSetupBaselineIntent(); loadErr != nil {
		return loadErr
	} else if activeExists {
		if active.Repository != rebind.Source.Repository || active.RepositoryID != rebind.Source.RepositoryID || active.CreatedAt != rebind.Source.CreatedAt {
			return fmt.Errorf("active setup baseline intent changed after rebind cleanup was prepared")
		}
		if err := store.DeleteSetupBaselineIntent(); err != nil {
			return err
		}
	}
	if _, err := saveSetupBaselineRebindTransition(store, rebind, "uninstall_setup_baseline_rebind_drained", true,
		fmt.Sprintf("source_phase=%s source_capture=%s resources_retired_at=%s", rebind.Source.Phase, rebind.Source.CaptureCorrelation, rebind.ResourcesRetiredAt.UTC().Format(time.RFC3339Nano))); err != nil {
		return err
	}
	return store.DeleteSetupBaselineRebindIntent()
}

var errSetupBaselineTreeMismatch = errors.New("setup baseline tree does not match the approved candidate set")

func validateSetupBaselineTreeEntries(entries []*gh.TreeEntry, candidates []SetupBaselineCandidate) error {
	observedPaths := []string{}
	for _, entry := range entries {
		path := filepath.ToSlash(entry.GetPath())
		if !strings.HasPrefix(strings.ToLower(path), ".visual-hive/snapshots/") || !strings.HasSuffix(strings.ToLower(path), ".png") {
			continue
		}
		if entry.GetType() != "blob" || (entry.GetMode() != "100644" && entry.GetMode() != "100755") {
			return fmt.Errorf("setup baseline tree contains non-regular PNG entry %s", path)
		}
		observedPaths = append(observedPaths, path)
	}
	sort.Strings(observedPaths)
	expectedPaths := setupBaselineCandidatePaths(candidates)
	if !equalPaths(observedPaths, expectedPaths) {
		return errSetupBaselineTreeMismatch
	}
	return nil
}
