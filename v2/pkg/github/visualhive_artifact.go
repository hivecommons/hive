package github

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const (
	maxVisualHiveArtifactBytes       = 110 << 20
	maxVisualHiveArchiveFiles        = 1024
	maxVisualHiveSourceArtifactBytes = int64(1 << 30)
	maxVisualHiveSourceDownloadBytes = int64(1100 << 20)
	maxVisualHiveSourceArchiveFiles  = 5000
)

var exactVisualHiveProducerCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

type VisualHiveArtifactRequest struct {
	Repository                string
	WorkflowRunID             int64
	ArtifactID                int64
	SourceArtifactID          int64
	FetchSourceArtifact       bool
	DestinationDir            string
	TargetRef                 string
	MaxACMM                   int
	ExpectedProducerGitCommit string
	ExpectedWorkflowName      string
	ExpectedWorkflowPath      string
	ExpectedEvent             string
	ExpectedRunName           string
	AllowFailedWorkflowRun    bool
}

type VerifiedVisualHiveArtifact struct {
	RepositoryID        string `json:"repository_id"`
	WorkflowRunID       string `json:"workflow_run_id"`
	WorkflowRunAttempt  string `json:"workflow_run_attempt"`
	ArtifactID          string `json:"artifact_id"`
	SourceArtifactID    string `json:"source_artifact_id"`
	ArtifactName        string `json:"artifact_name"`
	CommitSHA           string `json:"commit_sha"`
	HeadBranch          string `json:"head_branch"`
	Event               string `json:"event"`
	WorkflowName        string `json:"workflow_name"`
	WorkflowRunName     string `json:"workflow_run_name"`
	WorkflowPath        string `json:"workflow_path"`
	RunURL              string `json:"run_url"`
	ManifestPath        string `json:"manifest_path"`
	SourceArtifactPath  string `json:"source_artifact_path"`
	EvidenceRootPath    string `json:"evidence_root_path"`
	BundleSchemaVersion string `json:"bundle_schema_version"`
	BundleSHA256        string `json:"bundle_sha256"`
	ManifestSHA256      string `json:"manifest_sha256"`
	ArtifactIndexSHA256 string `json:"artifact_index_sha256,omitempty"`
	seal                *verifiedVisualHiveArtifactSeal
}

type PullRequestArtifactRequest struct {
	Repository            string
	PullRequestNumber     int
	ExpectedWorkflowRunID int64
	ExpectedHeadSHA       string
	ExpectedHeadBranch    string
	ExpectedWorkflowName  string
	ExpectedWorkflowPath  string
	ArtifactName          string
	DestinationDir        string
}

// VerifiedPullRequestArtifact is deliberately review-only evidence. A failed
// pull_request run can never resolve a finding or satisfy a merge gate, but its
// exact-head screenshots may be presented through the separate baseline review
// path after Hive independently verifies the run and artifact provenance.
type VerifiedPullRequestArtifact struct {
	RepositoryID        string `json:"repository_id"`
	PullRequestNumber   int    `json:"pull_request_number"`
	WorkflowRunID       int64  `json:"workflow_run_id"`
	WorkflowRunAttempt  int    `json:"workflow_run_attempt"`
	ArtifactID          int64  `json:"artifact_id"`
	ArtifactName        string `json:"artifact_name"`
	CommitSHA           string `json:"commit_sha"`
	HeadBranch          string `json:"head_branch"`
	WorkflowName        string `json:"workflow_name"`
	WorkflowPath        string `json:"workflow_path"`
	Conclusion          string `json:"conclusion"`
	RunURL              string `json:"run_url"`
	ArtifactRoot        string `json:"artifact_root"`
	EvidenceRootPath    string `json:"evidence_root_path"`
	ReviewSchemaVersion string `json:"review_schema_version"`
	ArtifactIndexSHA256 string `json:"artifact_index_sha256"`
}

// FetchAndVerifyPullRequestArtifact retrieves a failed exact-head PR artifact
// for human review. It intentionally accepts only completed pull_request runs
// with conclusion=failure; successful gating evidence continues through
// FetchAndVerifyVisualHiveBundle instead.
func (c *Client) FetchAndVerifyPullRequestArtifact(ctx context.Context, request PullRequestArtifactRequest) (VerifiedPullRequestArtifact, error) {
	owner, repo, err := splitFullRepository(request.Repository)
	if err != nil {
		return VerifiedPullRequestArtifact{}, err
	}
	if request.PullRequestNumber <= 0 || request.ExpectedWorkflowRunID <= 0 || strings.TrimSpace(request.ExpectedHeadSHA) == "" || strings.TrimSpace(request.ExpectedHeadBranch) == "" ||
		strings.TrimSpace(request.ExpectedWorkflowName) == "" || strings.TrimSpace(request.ExpectedWorkflowPath) == "" || strings.TrimSpace(request.ArtifactName) == "" || strings.TrimSpace(request.DestinationDir) == "" {
		return VerifiedPullRequestArtifact{}, fmt.Errorf("exact PR number, workflow run, head, branch, workflow name/path, artifact name, and destination are required")
	}
	repository, _, err := c.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return VerifiedPullRequestArtifact{}, fmt.Errorf("verify PR artifact repository: %w", err)
	}
	pull, _, err := c.client.PullRequests.Get(ctx, owner, repo, request.PullRequestNumber)
	if err != nil {
		return VerifiedPullRequestArtifact{}, fmt.Errorf("verify current PR artifact target: %w", err)
	}
	if pull.GetState() != "open" || pull.GetMerged() || pull.GetNumber() != request.PullRequestNumber || pull.GetHead().GetSHA() != request.ExpectedHeadSHA || pull.GetHead().GetRef() != request.ExpectedHeadBranch {
		return VerifiedPullRequestArtifact{}, fmt.Errorf("PR artifact target is not the exact current open pull request head")
	}
	runs, _, err := c.client.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, &gh.ListWorkflowRunsOptions{
		Event: "pull_request", Status: "completed", HeadSHA: request.ExpectedHeadSHA, ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return VerifiedPullRequestArtifact{}, fmt.Errorf("list exact-head PR workflow runs: %w", err)
	}
	var selected *gh.WorkflowRun
	for _, run := range runs.WorkflowRuns {
		if run.GetID() != request.ExpectedWorkflowRunID || run.GetHeadSHA() != request.ExpectedHeadSHA || run.GetHeadBranch() != request.ExpectedHeadBranch || run.GetEvent() != "pull_request" ||
			run.GetStatus() != "completed" || run.GetConclusion() != "failure" || run.GetName() != request.ExpectedWorkflowName || run.GetRunAttempt() <= 0 ||
			!workflowPathMatches(run.GetPath(), request.ExpectedWorkflowPath) || !workflowRunReferencesPullRequest(run, request.PullRequestNumber) {
			continue
		}
		selected = run
		break
	}
	if selected == nil {
		return VerifiedPullRequestArtifact{}, fmt.Errorf("no completed failed PR run matches exact head %s and workflow %s", request.ExpectedHeadSHA, request.ExpectedWorkflowPath)
	}
	artifact, err := c.findRunArtifactByName(ctx, owner, repo, selected.GetID(), request.ArtifactName)
	if err != nil {
		return VerifiedPullRequestArtifact{}, err
	}
	if artifact.GetExpired() || artifact.GetSizeInBytes() <= 0 || artifact.GetSizeInBytes() > maxVisualHiveArtifactBytes {
		return VerifiedPullRequestArtifact{}, fmt.Errorf("PR review artifact is expired or has an invalid size")
	}
	if artifact.WorkflowRun != nil {
		if artifact.WorkflowRun.GetID() != 0 && artifact.WorkflowRun.GetID() != selected.GetID() {
			return VerifiedPullRequestArtifact{}, fmt.Errorf("PR review artifact workflow run mismatch")
		}
		if artifact.WorkflowRun.GetRepositoryID() != 0 && artifact.WorkflowRun.GetRepositoryID() != repository.GetID() {
			return VerifiedPullRequestArtifact{}, fmt.Errorf("PR review artifact repository mismatch")
		}
		if artifact.WorkflowRun.GetHeadSHA() != "" && artifact.WorkflowRun.GetHeadSHA() != request.ExpectedHeadSHA {
			return VerifiedPullRequestArtifact{}, fmt.Errorf("PR review artifact commit mismatch")
		}
	}
	root, err := c.downloadAndExtractArtifact(ctx, owner, repo, artifact.GetID(), request.DestinationDir, "pr-artifact")
	if err != nil {
		return VerifiedPullRequestArtifact{}, err
	}
	review, err := visualhive.VerifyReviewEvidenceArtifact(root, artifact.GetID())
	if err != nil {
		return VerifiedPullRequestArtifact{}, fmt.Errorf("verify complete PR review evidence: %w", err)
	}
	return VerifiedPullRequestArtifact{
		RepositoryID: strconv.FormatInt(repository.GetID(), 10), PullRequestNumber: request.PullRequestNumber,
		WorkflowRunID: selected.GetID(), WorkflowRunAttempt: selected.GetRunAttempt(), ArtifactID: artifact.GetID(), ArtifactName: artifact.GetName(),
		CommitSHA: selected.GetHeadSHA(), HeadBranch: selected.GetHeadBranch(), WorkflowName: selected.GetName(), WorkflowPath: selected.GetPath(),
		Conclusion: selected.GetConclusion(), RunURL: selected.GetHTMLURL(), ArtifactRoot: root, EvidenceRootPath: review.EvidenceRoot,
		ReviewSchemaVersion: review.SchemaVersion, ArtifactIndexSHA256: review.ArtifactIndexSHA256,
	}, nil
}

func workflowRunReferencesPullRequest(run *gh.WorkflowRun, number int) bool {
	if run == nil || number <= 0 {
		return false
	}
	for _, pull := range run.PullRequests {
		if pull != nil && pull.GetNumber() == number {
			return true
		}
	}
	return false
}

// FetchAndVerifyVisualHiveBundle downloads the artifact through Hive's GitHub
// client and binds the extracted manifest to authoritative repository and run
// metadata before allowing the bundle validator to mark provenance verified.
func (c *Client) FetchAndVerifyVisualHiveBundle(ctx context.Context, request VisualHiveArtifactRequest) (*visualhive.ValidatedBundle, VerifiedVisualHiveArtifact, error) {
	owner, repo, err := splitFullRepository(request.Repository)
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, err
	}
	if request.WorkflowRunID <= 0 || request.ArtifactID <= 0 || strings.TrimSpace(request.DestinationDir) == "" {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("workflow run id, artifact id, and destination directory are required")
	}
	if !request.FetchSourceArtifact {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("trusted Visual Hive ingestion requires full source artifact fetch and content verification")
	}
	expectedProducerCommit := strings.TrimSpace(request.ExpectedProducerGitCommit)
	if !exactVisualHiveProducerCommit.MatchString(expectedProducerCommit) {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("trusted Visual Hive ingestion requires an exact 40-character expected producer commit")
	}
	expectedWorkflowName := strings.TrimSpace(request.ExpectedWorkflowName)
	expectedWorkflowPath := strings.TrimSpace(request.ExpectedWorkflowPath)
	expectedEvent := strings.TrimSpace(request.ExpectedEvent)
	if expectedWorkflowName == "" || expectedWorkflowPath == "" || expectedEvent == "" {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("trusted Visual Hive ingestion requires an exact approved workflow name and path plus event")
	}
	repository, _, err := c.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("verify Visual Hive repository: %w", err)
	}
	run, _, err := c.client.Actions.GetWorkflowRunByID(ctx, owner, repo, request.WorkflowRunID)
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("verify Visual Hive workflow run: %w", err)
	}
	allowedConclusion := run.GetConclusion() == "success" || (request.AllowFailedWorkflowRun && run.GetConclusion() == "failure")
	if run.GetID() != request.WorkflowRunID || run.GetStatus() != "completed" || !allowedConclusion || run.GetEvent() == "pull_request" || run.GetEvent() == "pull_request_target" || strings.TrimSpace(run.GetHeadSHA()) == "" {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive workflow run is not an allowed completed non-PR run")
	}
	if run.GetEvent() != expectedEvent {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive workflow event does not match the exact approved event")
	}
	if targetRef := strings.TrimPrefix(strings.TrimSpace(request.TargetRef), "refs/heads/"); targetRef != "" && run.GetHeadBranch() != targetRef {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive workflow run branch %q does not match target branch %q", run.GetHeadBranch(), targetRef)
	}
	if run.GetWorkflowID() <= 0 {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive workflow run did not identify its workflow definition")
	}
	definition, _, err := c.client.Actions.GetWorkflowByID(ctx, owner, repo, run.GetWorkflowID())
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("verify Visual Hive workflow definition: %w", err)
	}
	definitionPath := strings.TrimPrefix(strings.TrimSpace(strings.Split(definition.GetPath(), "@")[0]), "/")
	runPath := strings.TrimPrefix(strings.TrimSpace(strings.Split(run.GetPath(), "@")[0]), "/")
	if definition.GetID() != run.GetWorkflowID() || strings.TrimSpace(definition.GetName()) == "" || definitionPath == "" || definitionPath != runPath {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive workflow definition does not match the exact workflow run")
	}
	if definition.GetName() != expectedWorkflowName {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive workflow definition name mismatch")
	}
	if !workflowPathMatches(definition.GetPath(), expectedWorkflowPath) {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive workflow definition path mismatch")
	}
	if request.ExpectedRunName != "" {
		if run.GetDisplayTitle() != request.ExpectedRunName {
			return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive correlated workflow run name mismatch")
		}
		runtimeName := strings.TrimSpace(run.GetName())
		if runtimeName != request.ExpectedRunName && runtimeName != expectedWorkflowName {
			return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive workflow runtime name mismatch")
		}
	} else if run.GetName() != expectedWorkflowName {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive workflow runtime name mismatch")
	}
	artifact, err := c.findRunArtifact(ctx, owner, repo, request.WorkflowRunID, request.ArtifactID)
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, err
	}
	if artifact.GetExpired() || artifact.GetSizeInBytes() <= 0 || artifact.GetSizeInBytes() > maxVisualHiveArtifactBytes {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive artifact is expired or has an invalid size")
	}
	sourceArtifactID := request.SourceArtifactID
	if sourceArtifactID <= 0 {
		sourceArtifactID = request.ArtifactID
	}
	sourceArtifact := artifact
	if sourceArtifactID != request.ArtifactID {
		sourceArtifact, err = c.findRunArtifact(ctx, owner, repo, request.WorkflowRunID, sourceArtifactID)
		if err != nil {
			return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("verify Visual Hive source artifact: %w", err)
		}
		if sourceArtifact.GetExpired() || sourceArtifact.GetSizeInBytes() <= 0 || sourceArtifact.GetSizeInBytes() > maxVisualHiveSourceDownloadBytes {
			return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive source artifact is expired or has an invalid size")
		}
	}
	if err := validateVisualHiveArtifactIdentity(artifact, request.WorkflowRunID, repository.GetID(), run.GetHeadSHA(), "bundle"); err != nil {
		return nil, VerifiedVisualHiveArtifact{}, err
	}
	if err := validateVisualHiveArtifactIdentity(sourceArtifact, request.WorkflowRunID, repository.GetID(), run.GetHeadSHA(), "source"); err != nil {
		return nil, VerifiedVisualHiveArtifact{}, err
	}
	manifestPath, err := c.downloadAndExtractVisualHiveArtifact(ctx, owner, repo, request.ArtifactID, request.DestinationDir)
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, err
	}
	verified := VerifiedVisualHiveArtifact{
		RepositoryID: strconv.FormatInt(repository.GetID(), 10), WorkflowRunID: strconv.FormatInt(request.WorkflowRunID, 10),
		WorkflowRunAttempt: strconv.Itoa(run.GetRunAttempt()),
		ArtifactID:         strconv.FormatInt(request.ArtifactID, 10), ArtifactName: artifact.GetName(), CommitSHA: run.GetHeadSHA(),
		SourceArtifactID: strconv.FormatInt(sourceArtifactID, 10),
		HeadBranch:       run.GetHeadBranch(), Event: run.GetEvent(), WorkflowName: definition.GetName(), WorkflowRunName: run.GetName(),
		WorkflowPath: definitionPath, RunURL: run.GetHTMLURL(), ManifestPath: manifestPath,
	}
	bundle, err := visualhive.ValidateBundle(manifestPath, visualhive.ValidationOptions{
		MaxACMM: request.MaxACMM, VerifiedProvenance: true, ExpectedRepository: request.Repository,
		ExpectedProducerGitCommit: expectedProducerCommit,
		ExpectedRepositoryID:      verified.RepositoryID, ExpectedWorkflowRunID: verified.WorkflowRunID,
		ExpectedWorkflowRunAttempt: verified.WorkflowRunAttempt,
	})
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("validate independently fetched Visual Hive bundle: %w", err)
	}
	manifest := bundle.Manifest
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("read verified Visual Hive manifest identity: %w", err)
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	verified.BundleSchemaVersion = manifest.SchemaVersion
	verified.BundleSHA256 = manifest.OverallDigest
	verified.ManifestSHA256 = hex.EncodeToString(manifestDigest[:])
	if manifest.ArtifactIndex != nil {
		verified.ArtifactIndexSHA256 = manifest.ArtifactIndex.SHA256
	}
	if manifest.Source.CommitSHA != verified.CommitSHA || manifest.Source.Event != verified.Event || manifest.Source.WorkflowArtifactID != verified.SourceArtifactID {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive manifest does not match independently fetched workflow metadata")
	}
	if strings.TrimPrefix(manifest.Source.Ref, "refs/heads/") != verified.HeadBranch {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive manifest ref does not match independently fetched workflow branch")
	}
	if manifest.Source.WorkflowName != "" && verified.WorkflowName != "" && manifest.Source.WorkflowName != verified.WorkflowName {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive manifest workflow name mismatch")
	}
	sourceArtifactPath, err := c.downloadAndExtractArtifact(ctx, owner, repo, sourceArtifactID, request.DestinationDir, "source-artifact")
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("download verified Visual Hive source artifact: %w", err)
	}
	verified.SourceArtifactPath = sourceArtifactPath
	if err := bundle.VerifySourceArtifact(sourceArtifactPath); err != nil {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("verify content-addressed Visual Hive source artifact: %w", err)
	}
	if !bundle.Validation.Trusted {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive source artifact verification did not establish trusted ingestion")
	}
	evidenceRootPath, err := bundle.VerifiedEvidenceRoot()
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("resolve verified Visual Hive evidence root: %w", err)
	}
	verified.EvidenceRootPath = evidenceRootPath
	if err := sealVerifiedVisualHiveArtifact(&verified, bundle); err != nil {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("seal verified Visual Hive intake: %w", err)
	}
	return bundle, verified, nil
}

func validateVisualHiveArtifactIdentity(artifact *gh.Artifact, runID, repositoryID int64, headSHA, label string) error {
	if artifact.WorkflowRun == nil {
		return nil
	}
	if artifact.WorkflowRun.GetID() != 0 && artifact.WorkflowRun.GetID() != runID {
		return fmt.Errorf("Visual Hive %s artifact workflow run mismatch", label)
	}
	if artifact.WorkflowRun.GetRepositoryID() != 0 && artifact.WorkflowRun.GetRepositoryID() != repositoryID {
		return fmt.Errorf("Visual Hive %s artifact repository mismatch", label)
	}
	if artifact.WorkflowRun.GetHeadSHA() != "" && artifact.WorkflowRun.GetHeadSHA() != headSHA {
		return fmt.Errorf("Visual Hive %s artifact commit mismatch", label)
	}
	return nil
}

func (c *Client) findRunArtifact(ctx context.Context, owner, repo string, runID, artifactID int64) (*gh.Artifact, error) {
	page := 1
	for page > 0 {
		artifacts, response, err := c.client.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, runID, &gh.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, fmt.Errorf("list Visual Hive workflow artifacts: %w", err)
		}
		for _, artifact := range artifacts.Artifacts {
			if artifact.GetID() == artifactID {
				return artifact, nil
			}
		}
		page = response.NextPage
	}
	return nil, fmt.Errorf("Visual Hive artifact %d does not belong to workflow run %d", artifactID, runID)
}

func (c *Client) findRunArtifactByName(ctx context.Context, owner, repo string, runID int64, name string) (*gh.Artifact, error) {
	page := 1
	var matched *gh.Artifact
	for page > 0 {
		artifacts, response, err := c.client.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, runID, &gh.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, fmt.Errorf("list PR workflow artifacts: %w", err)
		}
		for _, artifact := range artifacts.Artifacts {
			if artifact.GetName() != name {
				continue
			}
			if matched != nil {
				return nil, fmt.Errorf("multiple PR workflow artifacts are named %q", name)
			}
			matched = artifact
		}
		page = response.NextPage
	}
	if matched == nil {
		return nil, fmt.Errorf("PR workflow run %d has no artifact named %q", runID, name)
	}
	return matched, nil
}

func workflowPathMatches(actual, expected string) bool {
	actual = strings.ReplaceAll(strings.TrimSpace(strings.Split(actual, "@")[0]), "\\", "/")
	expected = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(expected), "\\", "/"), "./")
	return actual == expected || strings.HasSuffix(actual, "/"+expected)
}

func (c *Client) downloadAndExtractVisualHiveArtifact(ctx context.Context, owner, repo string, artifactID int64, destinationDir string) (string, error) {
	root, err := c.downloadAndExtractArtifact(ctx, owner, repo, artifactID, destinationDir, "artifact")
	if err != nil {
		return "", err
	}
	return findVisualHiveManifest(root)
}

func (c *Client) downloadAndExtractArtifact(ctx context.Context, owner, repo string, artifactID int64, destinationDir, prefix string) (string, error) {
	maxDownloadBytes, maxExtractedBytes, maxFiles := int64(maxVisualHiveArtifactBytes), int64(maxVisualHiveArtifactBytes), maxVisualHiveArchiveFiles
	if prefix == "source-artifact" {
		maxDownloadBytes, maxExtractedBytes, maxFiles = maxVisualHiveSourceDownloadBytes, maxVisualHiveSourceArtifactBytes, maxVisualHiveSourceArchiveFiles
	}
	if err := os.MkdirAll(destinationDir, 0o700); err != nil {
		return "", fmt.Errorf("create Visual Hive artifact directory: %w", err)
	}
	if !validArtifactExtractionPrefix(prefix) {
		return "", fmt.Errorf("invalid Visual Hive artifact extraction prefix")
	}
	finalDir := filepath.Join(destinationDir, fmt.Sprintf("%s-%d", prefix, artifactID))
	completePath := filepath.Join(finalDir, ".hive-extraction-complete")
	if info, err := os.Lstat(completePath); err == nil && info.Mode().IsRegular() {
		return finalDir, nil
	}
	if prefix == "artifact" {
		if _, err := findVisualHiveManifest(finalDir); err == nil {
			return finalDir, nil
		}
	}
	if _, err := os.Stat(finalDir); err == nil {
		return "", fmt.Errorf("refusing incomplete existing Visual Hive artifact directory %q", finalDir)
	}
	downloadURL, _, err := c.client.Actions.DownloadArtifact(ctx, owner, repo, artifactID, 3)
	if err != nil {
		return "", fmt.Errorf("request Visual Hive artifact download: %w", err)
	}
	if downloadURL.Scheme != "https" && !isLoopbackDownload(downloadURL.Hostname()) {
		return "", fmt.Errorf("refusing non-HTTPS Visual Hive artifact download URL")
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL.String(), nil)
	if err != nil {
		return "", err
	}
	// DownloadArtifact returns a short-lived signed object URL. Sending the
	// GitHub API Authorization header to that storage host both leaks authority
	// across a trust boundary and is rejected by GitHub's artifact backend.
	downloadTimeout := 2 * time.Minute
	if prefix == "source-artifact" {
		downloadTimeout = 10 * time.Minute
	}
	downloadClient := &http.Client{Timeout: downloadTimeout}
	response, err := downloadClient.Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("download Visual Hive artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download Visual Hive artifact: HTTP %d", response.StatusCode)
	}
	temporaryFile, err := os.CreateTemp(destinationDir, ".visual-hive-artifact-*.zip")
	if err != nil {
		return "", err
	}
	temporaryZip := temporaryFile.Name()
	defer os.Remove(temporaryZip)
	written, copyErr := io.Copy(temporaryFile, io.LimitReader(response.Body, maxDownloadBytes+1))
	closeErr := temporaryFile.Close()
	if copyErr != nil || closeErr != nil || written > maxDownloadBytes {
		return "", fmt.Errorf("Visual Hive artifact download exceeded limits or failed")
	}
	temporaryDir, err := os.MkdirTemp(destinationDir, ".visual-hive-extract-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporaryDir)
	if err := extractVisualHiveZipWithLimits(temporaryZip, temporaryDir, maxExtractedBytes, maxFiles); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(temporaryDir, ".hive-extraction-complete"), []byte(strconv.FormatInt(artifactID, 10)+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("mark Visual Hive artifact extraction complete: %w", err)
	}
	if err := os.Rename(temporaryDir, finalDir); err != nil {
		return "", fmt.Errorf("publish Visual Hive artifact: %w", err)
	}
	return finalDir, nil
}

func validArtifactExtractionPrefix(prefix string) bool {
	switch prefix {
	case "artifact", "source-artifact", "pr-artifact", "setup-baseline-artifact":
		return true
	default:
		return false
	}
}

func isLoopbackDownload(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func extractVisualHiveZip(zipPath, destination string) error {
	return extractVisualHiveZipWithLimits(zipPath, destination, maxVisualHiveArtifactBytes, maxVisualHiveArchiveFiles)
}

func extractVisualHiveZipWithLimits(zipPath, destination string, maxBytes int64, maxFiles int) error {
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open Visual Hive artifact zip: %w", err)
	}
	defer archive.Close()
	if len(archive.File) == 0 || len(archive.File) > maxFiles*4+100 {
		return fmt.Errorf("Visual Hive artifact zip has an invalid file count")
	}
	var total uint64
	files := 0
	for _, entry := range archive.File {
		clean := filepath.Clean(filepath.FromSlash(entry.Name))
		if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || entry.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe Visual Hive artifact zip entry %q", entry.Name)
		}
		if entry.UncompressedSize64 > uint64(maxBytes) || total > uint64(maxBytes)-entry.UncompressedSize64 {
			return fmt.Errorf("Visual Hive artifact uncompressed size exceeds limit")
		}
		total += entry.UncompressedSize64
		target := filepath.Join(destination, clean)
		relative, err := filepath.Rel(destination, target)
		if err != nil || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
			return fmt.Errorf("unsafe Visual Hive artifact extraction path")
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		files++
		if files > maxFiles {
			return fmt.Errorf("Visual Hive artifact zip has an invalid file count")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		reader, err := entry.Open()
		if err != nil {
			return err
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			reader.Close()
			return err
		}
		written, copyErr := io.Copy(file, io.LimitReader(reader, int64(entry.UncompressedSize64)+1))
		closeFileErr := file.Close()
		closeReaderErr := reader.Close()
		if copyErr != nil || closeFileErr != nil || closeReaderErr != nil || written != int64(entry.UncompressedSize64) {
			return fmt.Errorf("extract Visual Hive artifact entry %q", entry.Name)
		}
	}
	return nil
}

func findVisualHiveManifest(root string) (string, error) {
	var matches []string
	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "manifest.json" {
			return nil
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		defer file.Close()
		var identity struct {
			SchemaVersion string `json:"schemaVersion"`
		}
		if err := json.NewDecoder(io.LimitReader(file, 2<<20)).Decode(&identity); err == nil && (identity.SchemaVersion == visualhive.ManifestSchema || identity.SchemaVersion == visualhive.ManifestSchemaV3) {
			matches = append(matches, filePath)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected exactly one Visual Hive bundle manifest, found %d", len(matches))
	}
	return matches[0], nil
}
