package github

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/internal/visualhivepr"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

var exactVisualHiveSHA256 = regexp.MustCompile(`^[a-f0-9]{64}$`)

// VisualHivePullRequestProducerCommit is the only audited runtime-sidecar
// producer admitted by the exact PR lane. Updating it requires a deliberate
// proof review; ordinary trusted-bundle imports retain their independent pin.
const VisualHivePullRequestProducerCommit = "9f48408f9f090fc43fce0c7c6171771f05660444"

// VisualHivePullRequestBundleRequest carries only independently knowable
// repository, PR, workflow, App, and producer pins. The verifier discovers the
// unique exact run attempt and artifacts from live GitHub data; artifact-local
// digests are untrusted claims that it recomputes before sealing.
type VisualHivePullRequestBundleRequest struct {
	Repository                 string
	PullRequestNumber          int
	ExpectedBaseRepository     string
	ExpectedBaseRepositoryID   int64
	ExpectedBaseBranch         string
	ExpectedBaseSHA            string
	ExpectedHeadRepository     string
	ExpectedHeadRepositoryID   int64
	ExpectedHeadBranch         string
	ExpectedHeadSHA            string
	ExpectedWorkflowName       string
	ExpectedWorkflowPath       string
	ExpectedGitHubActionsAppID int64
	ExpectedProducerGitCommit  string
	DestinationDir             string
	MaxACMM                    int
}

// VerifiedVisualHivePullRequestBundle is an opaque capability returned only by
// FetchAndVerifyVisualHivePullRequestBundle. Its unexported receipt and seal
// prevent a locally parsed bundle or caller-built snapshot from asserting that
// live GitHub provenance was checked.
type VerifiedVisualHivePullRequestBundle struct {
	sealedReceipt      visualhivepr.SealedReceipt
	evidenceRootPath   string
	manifestPath       string
	sourceArtifactPath string
}

func (verified VerifiedVisualHivePullRequestBundle) Receipt() (visualhive.PullRequestCheckReceiptSnapshot, error) {
	identityBytes, receiptSHA256, replayKey, err := verified.sealedReceipt.Open()
	if err != nil {
		return visualhive.PullRequestCheckReceiptSnapshot{}, err
	}
	var identity visualhive.PullRequestCheckReceiptIdentity
	if err := json.Unmarshal(identityBytes, &identity); err != nil {
		return visualhive.PullRequestCheckReceiptSnapshot{}, fmt.Errorf("decode live GitHub verified PR receipt: %w", err)
	}
	if identity.SchemaVersion != visualhive.PullRequestCheckReceiptSchema ||
		identity.Authority != (visualhive.PullRequestCheckAuthority{CheckEvidenceOnly: true}) ||
		!exactVisualHiveSHA256.MatchString(receiptSHA256) || identity.ReplayKey != replayKey ||
		identity.ReplayKey != visualhive.PullRequestCheckReplayKey(identity) {
		return visualhive.PullRequestCheckReceiptSnapshot{}, fmt.Errorf("live GitHub verified PR receipt capability is invalid")
	}
	if digestBytes(identityBytes) != receiptSHA256 {
		return visualhive.PullRequestCheckReceiptSnapshot{}, fmt.Errorf("live GitHub verified PR receipt identity changed under its seal")
	}
	receipt := visualhive.PullRequestCheckReceiptSnapshot{Identity: identity, ReceiptSHA256: receiptSHA256}
	snapshotBytes, err := json.Marshal(receipt)
	if err != nil {
		return visualhive.PullRequestCheckReceiptSnapshot{}, fmt.Errorf("clone live GitHub verified PR receipt: %w", err)
	}
	var snapshot visualhive.PullRequestCheckReceiptSnapshot
	if err := json.Unmarshal(snapshotBytes, &snapshot); err != nil {
		return visualhive.PullRequestCheckReceiptSnapshot{}, fmt.Errorf("clone live GitHub verified PR receipt: %w", err)
	}
	return snapshot, nil
}

func (verified VerifiedVisualHivePullRequestBundle) EvidenceRoot() (string, error) {
	if _, err := verified.Receipt(); err != nil || verified.evidenceRootPath == "" {
		return "", fmt.Errorf("verified PR evidence root is unavailable")
	}
	return verified.evidenceRootPath, nil
}

func (verified VerifiedVisualHivePullRequestBundle) ApplyCheckEvidence(store *visualhive.LifecycleStore, repositoryFingerprint string) (visualhive.ApplyPullRequestCheckUpdateResult, error) {
	_, err := verified.Receipt()
	if err != nil {
		return visualhive.ApplyPullRequestCheckUpdateResult{}, err
	}
	if store == nil {
		return visualhive.ApplyPullRequestCheckUpdateResult{}, fmt.Errorf("Hive lifecycle store is required")
	}
	return store.ApplySealedPullRequestCheckReceipt(repositoryFingerprint, verified.sealedReceipt)
}

// FetchAndVerifyVisualHivePullRequestBundle is the only PR v3 admission path.
// It is intentionally separate from both the failed-review artifact fetcher
// and the trusted default-branch bundle fetcher.
func (c *Client) FetchAndVerifyVisualHivePullRequestBundle(ctx context.Context, request VisualHivePullRequestBundleRequest) (*visualhive.ValidatedBundle, VerifiedVisualHivePullRequestBundle, error) {
	owner, repo, err := splitFullRepository(request.Repository)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, err
	}
	if err := validateVisualHivePullRequestBundleRequest(request); err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, err
	}
	repository, _, err := c.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("verify PR bundle base repository: %w", err)
	}
	if repository.GetID() != request.ExpectedBaseRepositoryID || !strings.EqualFold(repository.GetFullName(), request.ExpectedBaseRepository) || repository.GetDefaultBranch() != request.ExpectedBaseBranch {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR bundle base repository identity mismatch")
	}
	baseBranch, _, err := c.client.Repositories.GetBranch(ctx, owner, repo, request.ExpectedBaseBranch, 0)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("verify exact trusted base branch revision: %w", err)
	}
	if baseBranch.GetName() != request.ExpectedBaseBranch || baseBranch.GetCommit() == nil || baseBranch.GetCommit().GetSHA() != request.ExpectedBaseSHA {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("trusted base branch revision drifted from the exact PR base SHA")
	}
	headOwner, headRepo, err := splitFullRepository(request.ExpectedHeadRepository)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, err
	}
	headRepository, _, err := c.client.Repositories.Get(ctx, headOwner, headRepo)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("verify PR bundle head repository: %w", err)
	}
	if headRepository.GetID() != request.ExpectedHeadRepositoryID || !strings.EqualFold(headRepository.GetFullName(), request.ExpectedHeadRepository) {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR bundle head repository identity mismatch")
	}
	pull, _, err := c.client.PullRequests.Get(ctx, owner, repo, request.PullRequestNumber)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("verify current PR bundle target: %w", err)
	}
	if err := validateExactOpenPullRequest(pull, request); err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, err
	}
	run, err := c.findExactPullRequestWorkflowRun(ctx, owner, repo, request)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, err
	}
	runID, runAttempt := run.GetID(), run.GetRunAttempt()
	workflowProof, err := c.verifyExactPullRequestWorkflowJobCheck(ctx, owner, repo, pull, run, request.ExpectedGitHubActionsAppID)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, err
	}
	definition, _, err := c.client.Actions.GetWorkflowByID(ctx, owner, repo, run.GetWorkflowID())
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("verify exact PR workflow definition: %w", err)
	}
	if definition == nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR workflow definition response is empty")
	}
	definitionPath := strictPullRequestWorkflowPath(definition.GetPath())
	if definition.GetID() != run.GetWorkflowID() || definition.GetName() != request.ExpectedWorkflowName || definition.GetState() != "active" || definitionPath != request.ExpectedWorkflowPath {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR workflow definition identity mismatch")
	}
	baseWorkflow, err := c.fetchRepositoryFileAtCommit(ctx, request.ExpectedBaseRepository, request.ExpectedWorkflowPath, request.ExpectedBaseSHA)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("fetch base PR workflow definition: %w", err)
	}
	workflowDefinitionSHA256 := digestBytes(baseWorkflow)
	// GitHub's run metadata does not expose an immutable blob identity for the
	// root workflow definition. Until it does, fail closed if the PR head alters
	// that file: the base-side bytes remain the sole authority pin, while the
	// untrusted head copy is consulted only as a drift rejection check.
	headWorkflow, err := c.fetchRepositoryFileAtCommit(ctx, request.ExpectedHeadRepository, request.ExpectedWorkflowPath, request.ExpectedHeadSHA)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("reject PR workflow-definition drift at exact head: %w", err)
	}
	if !bytes.Equal(headWorkflow, baseWorkflow) {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR head modified the trusted workflow definition used to admit its own artifacts")
	}
	headConfig, err := c.fetchRepositoryFileAtCommit(ctx, request.ExpectedHeadRepository, "visual-hive.config.yaml", request.ExpectedHeadSHA)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("fetch exact-head Visual Hive config: %w", err)
	}
	configSHA256 := digestBytes(headConfig)
	bundleArtifactName := fmt.Sprintf("visual-hive-pr-bundle-%d-%d", runID, runAttempt)
	sourceArtifactName := fmt.Sprintf("visual-hive-pr-source-%d-%d", runID, runAttempt)
	bundleArtifact, sourceArtifact, err := c.findExactPullRequestBundleArtifacts(ctx, owner, repo, runID, runAttempt)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, err
	}
	if err := validateExactPullRequestArtifactIdentity(bundleArtifact, runID, repository.GetID(), request.ExpectedHeadRepositoryID, request.ExpectedHeadBranch, request.ExpectedHeadSHA, "PR bundle"); err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, err
	}
	if err := validateExactPullRequestArtifactIdentity(sourceArtifact, runID, repository.GetID(), request.ExpectedHeadRepositoryID, request.ExpectedHeadBranch, request.ExpectedHeadSHA, "PR source"); err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, err
	}
	if err := os.MkdirAll(request.DestinationDir, 0o700); err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("create PR verifier destination: %w", err)
	}
	verificationDir, err := os.MkdirTemp(request.DestinationDir, ".visual-hive-pr-verification-")
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("create fresh PR verifier extraction root: %w", err)
	}
	manifestPath, err := c.downloadAndExtractVisualHiveArtifact(ctx, owner, repo, bundleArtifact.GetID(), verificationDir)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("download exact PR bundle artifact: %w", err)
	}
	bundle, err := visualhive.ParsePullRequestBundle(manifestPath, visualhive.PullRequestParseOptions{
		MaxACMM: request.MaxACMM, ExpectedProducerGitCommit: request.ExpectedProducerGitCommit,
		ExpectedRepository: request.Repository, ExpectedRepositoryID: strconv.FormatInt(request.ExpectedBaseRepositoryID, 10),
		ExpectedHeadSHA: request.ExpectedHeadSHA, ExpectedHeadRef: request.ExpectedHeadBranch, ExpectedWorkflowName: request.ExpectedWorkflowName,
		ExpectedWorkflowRunID: strconv.FormatInt(runID, 10), ExpectedWorkflowRunAttempt: strconv.Itoa(runAttempt),
		ExpectedSourceArtifactID: strconv.FormatInt(sourceArtifact.GetID(), 10),
	})
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("validate independently fetched PR bundle: %w", err)
	}
	if bundle.Manifest.Verdict != "ready" {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR bundle does not carry a ready deterministic verdict")
	}
	sourceArtifactPath, err := c.downloadAndExtractArtifact(ctx, owner, repo, sourceArtifact.GetID(), verificationDir, "source-artifact")
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("download exact PR source artifact: %w", err)
	}
	if err := bundle.VerifySourceArtifact(sourceArtifactPath); err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("verify complete content-addressed PR source artifact: %w", err)
	}
	if bundle.Validation.Trusted || bundle.Validation.Authoritative {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR source verification unexpectedly granted lifecycle authority")
	}
	binding, err := bundle.VerifyPullRequestSourceBinding(visualhive.PullRequestSourceExpectations{
		Repository: request.Repository, RepositoryID: strconv.FormatInt(request.ExpectedBaseRepositoryID, 10), PullRequest: request.PullRequestNumber,
		Base:         visualhive.PullRequestRevisionIdentity{Repository: request.ExpectedBaseRepository, RepositoryID: strconv.FormatInt(request.ExpectedBaseRepositoryID, 10), Ref: request.ExpectedBaseBranch, SHA: request.ExpectedBaseSHA},
		Head:         visualhive.PullRequestRevisionIdentity{Repository: request.ExpectedHeadRepository, RepositoryID: strconv.FormatInt(request.ExpectedHeadRepositoryID, 10), Ref: request.ExpectedHeadBranch, SHA: request.ExpectedHeadSHA},
		WorkflowName: request.ExpectedWorkflowName, WorkflowPath: request.ExpectedWorkflowPath, WorkflowRunID: strconv.FormatInt(runID, 10), WorkflowRunAttempt: strconv.Itoa(runAttempt),
		WorkflowDefinitionSHA256: workflowDefinitionSHA256, ProducerCommit: request.ExpectedProducerGitCommit,
		SourceArtifactName: sourceArtifactName, BundleArtifactName: bundleArtifactName, ConfigSHA256: configSHA256,
	})
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, err
	}
	if err := c.verifyExactHeadPullRequestBaselines(ctx, bundle, binding, request.ExpectedHeadRepository, request.ExpectedHeadSHA); err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, err
	}
	evidenceRoot, err := bundle.VerifiedEvidenceRoot()
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("resolve verified PR evidence root: %w", err)
	}
	manifestSHA256, err := bundle.VerifiedManifestSHA256()
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, err
	}
	identity := visualhive.PullRequestCheckReceiptIdentity{
		SchemaVersion: visualhive.PullRequestCheckReceiptSchema,
		Source:        binding,
		Workflow: visualhive.PullRequestCheckWorkflowIdentity{
			ID: strconv.FormatInt(run.GetWorkflowID(), 10), Name: request.ExpectedWorkflowName, Path: request.ExpectedWorkflowPath,
			Event: "pull_request", RunID: strconv.FormatInt(runID, 10), RunAttempt: strconv.Itoa(runAttempt),
			CheckSuiteID: strconv.FormatInt(workflowProof.CheckSuiteID, 10), JobID: strconv.FormatInt(workflowProof.JobID, 10),
			CheckRunID: strconv.FormatInt(workflowProof.CheckRunID, 10), AppID: strconv.FormatInt(workflowProof.AppID, 10), Definition: binding.Workflow.Definition,
		},
		SourceArtifact: visualhive.PullRequestCheckArtifactIdentity{ID: strconv.FormatInt(sourceArtifact.GetID(), 10), Name: sourceArtifactName},
		BundleArtifact: visualhive.PullRequestCheckArtifactIdentity{ID: strconv.FormatInt(bundleArtifact.GetID(), 10), Name: bundleArtifactName},
		Producer:       bundle.Manifest.Producer,
		Bundle: visualhive.PullRequestCheckBundleIdentity{
			SchemaVersion: bundle.Manifest.SchemaVersion, BundleID: bundle.Manifest.BundleID, OverallSHA256: bundle.Manifest.OverallDigest,
			ManifestSHA256: manifestSHA256, ArtifactIndex: *bundle.Manifest.ArtifactIndex,
		},
		Check:     visualhive.PullRequestCheckResultIdentity{State: "success", Conclusion: "success", Summary: "Visual Hive exact pull_request bundle verified", RunURL: run.GetHTMLURL()},
		Authority: visualhive.PullRequestCheckAuthority{CheckEvidenceOnly: true},
	}
	finalPull, _, err := c.client.PullRequests.Get(ctx, owner, repo, request.PullRequestNumber)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("reverify current PR after artifact downloads: %w", err)
	}
	if err := validateExactOpenPullRequest(finalPull, request); err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR changed while its v3 artifacts were verified: %w", err)
	}
	finalRun, err := c.findExactPullRequestWorkflowRun(ctx, owner, repo, request)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR workflow run selection changed while its v3 artifacts were verified: %w", err)
	}
	if finalRun.GetID() != runID || finalRun.GetRunAttempt() != runAttempt {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR workflow run selection changed while its v3 artifacts were verified")
	}
	finalWorkflowProof, err := c.verifyExactPullRequestWorkflowJobCheck(ctx, owner, repo, finalPull, finalRun, request.ExpectedGitHubActionsAppID)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR workflow job/check/App identity changed while its v3 artifacts were verified: %w", err)
	}
	if finalWorkflowProof != workflowProof {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR workflow job/check/App identity changed while its v3 artifacts were verified")
	}
	finalDefinition, _, err := c.client.Actions.GetWorkflowByID(ctx, owner, repo, finalRun.GetWorkflowID())
	if err != nil || finalDefinition == nil || finalDefinition.GetID() != definition.GetID() || finalDefinition.GetName() != definition.GetName() ||
		finalDefinition.GetState() != "active" || strictPullRequestWorkflowPath(finalDefinition.GetPath()) != request.ExpectedWorkflowPath {
		if err != nil {
			return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("reverify exact PR workflow definition after artifact downloads: %w", err)
		}
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR workflow definition changed while its v3 artifacts were verified")
	}
	finalBundleArtifact, finalSourceArtifact, err := c.findExactPullRequestBundleArtifacts(ctx, owner, repo, runID, runAttempt)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("reverify exact PR artifacts after downloads: %w", err)
	}
	if finalBundleArtifact.GetID() != bundleArtifact.GetID() || finalSourceArtifact.GetID() != sourceArtifact.GetID() {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR artifact selection changed while its bytes were verified")
	}
	if err := validateExactPullRequestArtifactIdentity(finalBundleArtifact, runID, repository.GetID(), request.ExpectedHeadRepositoryID, request.ExpectedHeadBranch, request.ExpectedHeadSHA, "PR bundle"); err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR bundle artifact changed while its bytes were verified: %w", err)
	}
	if err := validateExactPullRequestArtifactIdentity(finalSourceArtifact, runID, repository.GetID(), request.ExpectedHeadRepositoryID, request.ExpectedHeadBranch, request.ExpectedHeadSHA, "PR source"); err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR source artifact changed while its bytes were verified: %w", err)
	}
	finalRepository, _, err := c.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("reverify PR base repository after artifact downloads: %w", err)
	}
	if finalRepository.GetID() != request.ExpectedBaseRepositoryID || !strings.EqualFold(finalRepository.GetFullName(), request.ExpectedBaseRepository) || finalRepository.GetDefaultBranch() != request.ExpectedBaseBranch {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("PR base repository changed while its v3 artifacts were verified")
	}
	finalBaseBranch, _, err := c.client.Repositories.GetBranch(ctx, owner, repo, request.ExpectedBaseBranch, 0)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("reverify exact trusted base branch after artifact downloads: %w", err)
	}
	if finalBaseBranch.GetName() != request.ExpectedBaseBranch || finalBaseBranch.GetCommit() == nil || finalBaseBranch.GetCommit().GetSHA() != request.ExpectedBaseSHA {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("trusted base branch changed while its v3 artifacts were verified")
	}
	identity.Check.RunURL = finalRun.GetHTMLURL()
	identity.ReplayKey = visualhive.PullRequestCheckReplayKey(identity)
	identityBytes, err := json.Marshal(identity)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("marshal independently verified PR check receipt: %w", err)
	}
	receiptSHA256 := digestBytes(identityBytes)
	sealedReceipt, err := visualhivepr.SealVerifiedReceipt(identityBytes, receiptSHA256, identity.ReplayKey)
	if err != nil {
		return nil, VerifiedVisualHivePullRequestBundle{}, fmt.Errorf("freeze independently verified PR check receipt: %w", err)
	}
	return bundle, VerifiedVisualHivePullRequestBundle{
		sealedReceipt: sealedReceipt, evidenceRootPath: evidenceRoot, manifestPath: manifestPath, sourceArtifactPath: sourceArtifactPath,
	}, nil
}

func validateExactPullRequestArtifactIdentity(artifact *gh.Artifact, runID, repositoryID, headRepositoryID int64, headBranch, headSHA, label string) error {
	if artifact == nil || artifact.WorkflowRun == nil || artifact.WorkflowRun.GetID() != runID ||
		artifact.WorkflowRun.GetRepositoryID() != repositoryID || artifact.WorkflowRun.GetHeadRepositoryID() != headRepositoryID ||
		artifact.WorkflowRun.GetHeadBranch() != headBranch || artifact.WorkflowRun.GetHeadSHA() != headSHA {
		return fmt.Errorf("Visual Hive %s artifact is missing its exact workflow run, base/head repository, branch, or commit identity", label)
	}
	return nil
}

func validateVisualHivePullRequestBundleRequest(request VisualHivePullRequestBundleRequest) error {
	requiredStrings := []string{request.Repository, request.ExpectedBaseRepository, request.ExpectedBaseBranch, request.ExpectedHeadRepository, request.ExpectedHeadBranch,
		request.ExpectedWorkflowName, request.ExpectedWorkflowPath, request.DestinationDir}
	for _, value := range requiredStrings {
		if strings.TrimSpace(value) == "" || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("exact non-empty PR repository, branch, workflow, and destination identities are required")
		}
	}
	if !strings.EqualFold(request.Repository, request.ExpectedBaseRepository) || request.PullRequestNumber <= 0 || request.ExpectedBaseRepositoryID <= 0 || request.ExpectedHeadRepositoryID <= 0 ||
		request.ExpectedGitHubActionsAppID <= 0 {
		return fmt.Errorf("exact positive PR, repository, workflow, and GitHub Actions App identities are required")
	}
	if request.ExpectedProducerGitCommit != VisualHivePullRequestProducerCommit || !exactVisualHiveProducerCommit.MatchString(request.ExpectedBaseSHA) || !exactVisualHiveProducerCommit.MatchString(request.ExpectedHeadSHA) {
		return fmt.Errorf("exact 40-character producer, base, and head commits are required")
	}
	for _, value := range []string{request.ExpectedBaseBranch, request.ExpectedHeadBranch, request.ExpectedWorkflowPath} {
		lower := strings.ToLower(value)
		if strings.HasPrefix(lower, "refs/") || strings.Contains(lower, "refs/pull/") || strings.Contains(lower, "/merge") {
			return fmt.Errorf("merge refs and ref-shaped branch/workflow identities are forbidden in the PR bundle lane")
		}
	}
	cleanWorkflowPath := filepath.ToSlash(filepath.Clean(request.ExpectedWorkflowPath))
	if cleanWorkflowPath != request.ExpectedWorkflowPath || !strings.HasPrefix(cleanWorkflowPath, ".github/workflows/") || strings.Contains(cleanWorkflowPath, "..") {
		return fmt.Errorf("exact repository-relative PR workflow path is required")
	}
	return nil
}

// verifyExactHeadPullRequestBaselines proves that every sealed PNG is the
// exact tracked Git blob at the immutable PR head. SHA-1 is used here only for
// Git object identity compatibility; SHA-256 remains the evidence digest.
func (c *Client) verifyExactHeadPullRequestBaselines(ctx context.Context, bundle *visualhive.ValidatedBundle, binding visualhive.PullRequestSourceBinding, repository, commit string) error {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return err
	}
	for _, identity := range binding.Baselines.Files {
		sourcePath := strings.TrimPrefix(identity.Path, ".visual-hive/proof/pr/baselines/")
		if sourcePath == identity.Path || sourcePath == "" || sourcePath == "." || pathpkg.Clean(sourcePath) != sourcePath || strings.HasPrefix(sourcePath, "../") {
			return fmt.Errorf("sealed PR baseline %q has no canonical repository source path", identity.Path)
		}
		extractedPath, size, sha256Identity, err := bundle.VerifiedSourceFileIdentity(identity.Path)
		if err != nil {
			return fmt.Errorf("reverify sealed PR baseline %q: %w", identity.Path, err)
		}
		data, err := os.ReadFile(extractedPath)
		if err != nil {
			return fmt.Errorf("read sealed PR baseline %q: %w", identity.Path, err)
		}
		if size != identity.Bytes || int64(len(data)) != identity.Bytes || sha256Identity != identity.SHA256 || digestBytes(data) != identity.SHA256 {
			return fmt.Errorf("sealed PR baseline %q changed after source verification", identity.Path)
		}
		file, directory, _, err := c.client.Repositories.GetContents(ctx, owner, repo, sourcePath, &gh.RepositoryContentGetOptions{Ref: commit})
		if err != nil {
			return fmt.Errorf("verify exact-head tracked PR baseline %q: %w", sourcePath, err)
		}
		if file == nil || directory != nil || file.GetType() != "file" || file.GetPath() != sourcePath || int64(file.GetSize()) != identity.Bytes ||
			!exactVisualHiveProducerCommit.MatchString(file.GetSHA()) || file.GetSHA() != gitBlobSHA(data) {
			return fmt.Errorf("sealed PR baseline %q is not the exact tracked Git blob at head %s", sourcePath, commit)
		}
	}
	return nil
}

func gitBlobSHA(data []byte) string {
	hash := sha1.New() // Git's object format requires SHA-1 for this identity.
	_, _ = hash.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

type pullRequestWorkflowProof struct {
	CheckSuiteID int64
	JobID        int64
	CheckRunID   int64
	AppID        int64
}

func (c *Client) findExactPullRequestWorkflowRun(ctx context.Context, owner, repo string, request VisualHivePullRequestBundleRequest) (*gh.WorkflowRun, error) {
	options := &gh.ListWorkflowRunsOptions{
		Branch: request.ExpectedHeadBranch, Event: "pull_request", Status: "success", HeadSHA: request.ExpectedHeadSHA,
		ListOptions: gh.ListOptions{Page: 1, PerPage: 100},
	}
	seenPages := map[int]bool{}
	seenRuns := map[int64]bool{}
	declaredTotal := -1
	seen := 0
	var matched *gh.WorkflowRun
	for options.Page > 0 {
		if seenPages[options.Page] || len(seenPages) >= 10 {
			return nil, fmt.Errorf("PR workflow run discovery exceeded its bounded unique-page limit")
		}
		seenPages[options.Page] = true
		runs, response, err := c.client.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, pathpkg.Base(request.ExpectedWorkflowPath), options)
		if err != nil {
			return nil, fmt.Errorf("discover exact successful Visual Hive PR workflow run: %w", err)
		}
		if runs == nil || runs.TotalCount == nil || runs.GetTotalCount() < 0 || (declaredTotal >= 0 && runs.GetTotalCount() != declaredTotal) {
			return nil, fmt.Errorf("PR workflow run inventory is incomplete or unstable")
		}
		declaredTotal = runs.GetTotalCount()
		for _, candidate := range runs.WorkflowRuns {
			seen++
			if seen > 1000 || candidate == nil || candidate.GetID() <= 0 || seenRuns[candidate.GetID()] {
				return nil, fmt.Errorf("PR workflow run inventory is oversized or contains a missing/duplicate run identity")
			}
			seenRuns[candidate.GetID()] = true
			if validateExactPullRequestWorkflowRun(candidate, request, 0, 0) != nil {
				continue
			}
			if matched != nil {
				return nil, fmt.Errorf("multiple successful workflow runs match the exact PR head and workflow")
			}
			matched = candidate
		}
		if response == nil || response.NextPage <= options.Page {
			options.Page = 0
		} else {
			options.Page = response.NextPage
		}
	}
	if seen != declaredTotal {
		return nil, fmt.Errorf("PR workflow run pagination did not cover the declared inventory")
	}
	if matched == nil {
		return nil, fmt.Errorf("no unique successful workflow run matches the exact PR head and workflow")
	}
	detail, _, err := c.client.Actions.GetWorkflowRunByID(ctx, owner, repo, matched.GetID())
	if err != nil {
		return nil, fmt.Errorf("read discovered exact PR workflow run: %w", err)
	}
	if err := validateExactPullRequestWorkflowRun(detail, request, matched.GetID(), matched.GetRunAttempt()); err != nil {
		return nil, err
	}
	if detail.GetCheckSuiteID() != matched.GetCheckSuiteID() {
		return nil, fmt.Errorf("discovered PR workflow run changed check-suite identity")
	}
	return detail, nil
}

func (c *Client) verifyExactPullRequestWorkflowJobCheck(ctx context.Context, owner, repo string, pull *gh.PullRequest, run *gh.WorkflowRun, expectedAppID int64) (pullRequestWorkflowProof, error) {
	page := 1
	seenPages := map[int]bool{}
	seenJobs := map[int64]bool{}
	declaredTotal := -1
	seen := 0
	var matched *gh.WorkflowJob
	for page > 0 {
		if seenPages[page] || len(seenPages) >= 6 {
			return pullRequestWorkflowProof{}, fmt.Errorf("PR workflow job discovery exceeded its bounded unique-page limit")
		}
		seenPages[page] = true
		jobs, response, err := c.client.Actions.ListWorkflowJobsAttempt(ctx, owner, repo, run.GetID(), int64(run.GetRunAttempt()), &gh.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return pullRequestWorkflowProof{}, fmt.Errorf("list exact PR workflow-attempt jobs: %w", err)
		}
		if jobs == nil || jobs.TotalCount == nil || jobs.GetTotalCount() < 0 || (declaredTotal >= 0 && jobs.GetTotalCount() != declaredTotal) {
			return pullRequestWorkflowProof{}, fmt.Errorf("PR workflow job inventory is incomplete or unstable")
		}
		declaredTotal = jobs.GetTotalCount()
		for _, job := range jobs.Jobs {
			seen++
			if seen > 512 || job == nil || job.GetID() <= 0 || seenJobs[job.GetID()] {
				return pullRequestWorkflowProof{}, fmt.Errorf("PR workflow job inventory is oversized or contains a missing/duplicate identity")
			}
			seenJobs[job.GetID()] = true
			if job.GetName() != "visual-hive" {
				continue
			}
			if matched != nil || job.GetRunID() != run.GetID() || job.GetRunAttempt() != int64(run.GetRunAttempt()) ||
				job.GetHeadSHA() != run.GetHeadSHA() || job.GetHeadBranch() != run.GetHeadBranch() || job.GetWorkflowName() != run.GetName() ||
				job.GetStatus() != "completed" || job.GetConclusion() != "success" || !exactHTTPSURL(job.GetHTMLURL()) {
				return pullRequestWorkflowProof{}, fmt.Errorf("PR workflow aggregator job is duplicated or does not match the exact successful run attempt")
			}
			matched = job
		}
		if response == nil || response.NextPage <= page {
			page = 0
		} else {
			page = response.NextPage
		}
	}
	if seen != declaredTotal || matched == nil {
		return pullRequestWorkflowProof{}, fmt.Errorf("PR workflow job pagination did not yield one exact successful visual-hive job")
	}
	checkRunID, err := checkRunIDFromAPIURL(matched.GetCheckRunURL())
	if err != nil {
		return pullRequestWorkflowProof{}, err
	}
	check, _, err := c.client.Checks.GetCheckRun(ctx, owner, repo, checkRunID)
	if err != nil {
		return pullRequestWorkflowProof{}, fmt.Errorf("read exact PR workflow job Check Run: %w", err)
	}
	if check == nil {
		return pullRequestWorkflowProof{}, fmt.Errorf("PR workflow job Check Run response is empty")
	}
	associationPresent, associationMatches := pullRequestAssociationStatus(check.PullRequests, pull)
	if check.GetID() != checkRunID || check.GetName() != matched.GetName() || check.GetHeadSHA() != run.GetHeadSHA() ||
		check.GetStatus() != "completed" || check.GetConclusion() != "success" || check.GetCheckSuite().GetID() != run.GetCheckSuiteID() ||
		check.GetApp().GetID() != expectedAppID || (associationPresent && !associationMatches) {
		return pullRequestWorkflowProof{}, fmt.Errorf("PR workflow job Check Run is not the exact successful GitHub Actions App result")
	}
	return pullRequestWorkflowProof{CheckSuiteID: run.GetCheckSuiteID(), JobID: matched.GetID(), CheckRunID: checkRunID, AppID: expectedAppID}, nil
}

func checkRunIDFromAPIURL(value string) (int64, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return 0, fmt.Errorf("PR workflow job has an invalid Check Run URL")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "check-runs" {
		return 0, fmt.Errorf("PR workflow job has an unrecognized Check Run URL")
	}
	id, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("PR workflow job has an invalid Check Run ID")
	}
	return id, nil
}

func validateExactOpenPullRequest(pull *gh.PullRequest, request VisualHivePullRequestBundleRequest) error {
	if pull == nil || pull.GetNumber() != request.PullRequestNumber || pull.GetState() != "open" || pull.GetMerged() || pull.GetBase() == nil || pull.GetHead() == nil || pull.GetBase().GetRepo() == nil || pull.GetHead().GetRepo() == nil {
		return fmt.Errorf("PR bundle target is not the exact current open and unmerged pull request")
	}
	if !strings.EqualFold(pull.GetBase().GetRepo().GetFullName(), request.ExpectedBaseRepository) || pull.GetBase().GetRepo().GetID() != request.ExpectedBaseRepositoryID ||
		pull.GetBase().GetRef() != request.ExpectedBaseBranch || pull.GetBase().GetSHA() != request.ExpectedBaseSHA ||
		!strings.EqualFold(pull.GetHead().GetRepo().GetFullName(), request.ExpectedHeadRepository) || pull.GetHead().GetRepo().GetID() != request.ExpectedHeadRepositoryID ||
		pull.GetHead().GetRef() != request.ExpectedHeadBranch || pull.GetHead().GetSHA() != request.ExpectedHeadSHA {
		return fmt.Errorf("PR bundle base/head repository, branch, or commit drifted")
	}
	return nil
}

func validateExactPullRequestWorkflowRun(run *gh.WorkflowRun, request VisualHivePullRequestBundleRequest, expectedRunID int64, expectedRunAttempt int) error {
	if run == nil || run.GetID() <= 0 || run.GetRunAttempt() <= 0 || (expectedRunID > 0 && run.GetID() != expectedRunID) ||
		(expectedRunAttempt > 0 && run.GetRunAttempt() != expectedRunAttempt) || run.GetWorkflowID() <= 0 || run.GetCheckSuiteID() <= 0 ||
		run.GetEvent() != "pull_request" || run.GetStatus() != "completed" || run.GetConclusion() != "success" || run.GetName() != request.ExpectedWorkflowName ||
		strictPullRequestWorkflowPath(run.GetPath()) != request.ExpectedWorkflowPath || run.GetHeadSHA() != request.ExpectedHeadSHA || run.GetHeadBranch() != request.ExpectedHeadBranch ||
		run.GetRepository() == nil || run.GetHeadRepository() == nil || !exactHTTPSURL(run.GetHTMLURL()) {
		return fmt.Errorf("workflow run is not the exact successful pull_request run")
	}
	if strings.Contains(strings.ToLower(run.GetPath()), "refs/pull/") || strings.Contains(strings.ToLower(run.GetPath()), "/merge") ||
		run.GetRepository().GetID() != request.ExpectedBaseRepositoryID || !strings.EqualFold(run.GetRepository().GetFullName(), request.ExpectedBaseRepository) ||
		run.GetHeadRepository().GetID() != request.ExpectedHeadRepositoryID || !strings.EqualFold(run.GetHeadRepository().GetFullName(), request.ExpectedHeadRepository) {
		return fmt.Errorf("workflow run repository or workflow-definition identity drifted")
	}
	// GitHub may omit pull_requests entirely for successful runs from forks.
	// The independently fetched live PR and the top-level run repository/head
	// identities remain mandatory above. If GitHub does return an association,
	// treat it as additional evidence and require it to match exactly.
	if len(run.PullRequests) == 0 {
		return nil
	}
	if len(run.PullRequests) != 1 {
		return fmt.Errorf("workflow run references ambiguous pull requests")
	}
	association := run.PullRequests[0]
	if association != nil && association.GetNumber() == request.PullRequestNumber && association.GetBase() != nil && association.GetHead() != nil &&
		association.GetBase().GetRepo() != nil && association.GetHead().GetRepo() != nil &&
		association.GetBase().GetRepo().GetID() == request.ExpectedBaseRepositoryID && optionalExactRepositoryName(association.GetBase().GetRepo().GetFullName(), request.ExpectedBaseRepository) &&
		association.GetHead().GetRepo().GetID() == request.ExpectedHeadRepositoryID && optionalExactRepositoryName(association.GetHead().GetRepo().GetFullName(), request.ExpectedHeadRepository) &&
		association.GetBase().GetRef() == request.ExpectedBaseBranch && association.GetBase().GetSHA() == request.ExpectedBaseSHA &&
		association.GetHead().GetRef() == request.ExpectedHeadBranch && association.GetHead().GetSHA() == request.ExpectedHeadSHA {
		return nil
	}
	return fmt.Errorf("workflow run does not reference the exact PR base and head")
}

func optionalExactRepositoryName(actual, expected string) bool {
	return actual == "" || strings.EqualFold(actual, expected)
}

func exactHTTPSURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func strictPullRequestWorkflowPath(value string) string {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "@") || strings.HasPrefix(value, "/") {
		return ""
	}
	return filepath.ToSlash(value)
}

func (c *Client) fetchRepositoryFileAtCommit(ctx context.Context, repository, path, commit string) ([]byte, error) {
	owner, repo, err := splitFullRepository(repository)
	if err != nil {
		return nil, err
	}
	file, directory, _, err := c.client.Repositories.GetContents(ctx, owner, repo, path, &gh.RepositoryContentGetOptions{Ref: commit})
	if err != nil {
		return nil, err
	}
	if file == nil || directory != nil || file.GetType() != "file" || file.GetPath() != path || file.GetSHA() == "" {
		return nil, fmt.Errorf("repository content is not the exact ordinary file %q", path)
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, err
	}
	data := []byte(content)
	if len(data) == 0 || len(data) > 1<<20 || (file.GetSize() > 0 && file.GetSize() != len(data)) ||
		!exactVisualHiveProducerCommit.MatchString(file.GetSHA()) || file.GetSHA() != gitBlobSHA(data) {
		return nil, fmt.Errorf("repository content %q has an invalid immutable size or Git blob identity", path)
	}
	return data, nil
}

func (c *Client) findExactPullRequestBundleArtifacts(ctx context.Context, owner, repo string, runID int64, runAttempt int) (*gh.Artifact, *gh.Artifact, error) {
	bundleName := fmt.Sprintf("visual-hive-pr-bundle-%d-%d", runID, runAttempt)
	sourceName := fmt.Sprintf("visual-hive-pr-source-%d-%d", runID, runAttempt)
	page := 1
	seen := 0
	seenPages := map[int]bool{}
	seenArtifactIDs := map[int64]bool{}
	declaredTotal := int64(-1)
	var bundleArtifact, sourceArtifact *gh.Artifact
	for page > 0 {
		if seenPages[page] || len(seenPages) >= 10 {
			return nil, nil, fmt.Errorf("PR workflow artifact discovery exceeded its bounded unique-page limit")
		}
		seenPages[page] = true
		artifacts, response, err := c.client.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, runID, &gh.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, nil, fmt.Errorf("list exact PR workflow artifacts: %w", err)
		}
		if artifacts == nil || artifacts.TotalCount == nil || artifacts.GetTotalCount() < 0 || (declaredTotal >= 0 && artifacts.GetTotalCount() != declaredTotal) {
			return nil, nil, fmt.Errorf("PR workflow artifact inventory is incomplete or unstable")
		}
		declaredTotal = artifacts.GetTotalCount()
		for _, artifact := range artifacts.Artifacts {
			seen++
			if seen > 1000 || artifact == nil || artifact.GetID() <= 0 || seenArtifactIDs[artifact.GetID()] {
				return nil, nil, fmt.Errorf("PR workflow artifact inventory exceeds the verification bound or contains a missing/duplicate identity")
			}
			seenArtifactIDs[artifact.GetID()] = true
			switch artifact.GetName() {
			case bundleName:
				if bundleArtifact != nil || artifact.GetID() <= 0 {
					return nil, nil, fmt.Errorf("PR bundle artifact name is duplicated or has no live ID")
				}
				bundleArtifact = artifact
			case sourceName:
				if sourceArtifact != nil || artifact.GetID() <= 0 {
					return nil, nil, fmt.Errorf("PR source artifact name is duplicated or has no live ID")
				}
				sourceArtifact = artifact
			}
		}
		if response == nil || response.NextPage <= page {
			page = 0
		} else {
			page = response.NextPage
		}
	}
	if int64(seen) != declaredTotal {
		return nil, nil, fmt.Errorf("PR workflow artifact pagination did not cover the declared inventory")
	}
	if bundleArtifact == nil || sourceArtifact == nil || bundleArtifact.GetID() == sourceArtifact.GetID() || bundleArtifact.GetExpired() || sourceArtifact.GetExpired() ||
		bundleArtifact.GetSizeInBytes() <= 0 || bundleArtifact.GetSizeInBytes() > maxVisualHiveArtifactBytes ||
		sourceArtifact.GetSizeInBytes() <= 0 || sourceArtifact.GetSizeInBytes() > maxVisualHiveSourceDownloadBytes {
		return nil, nil, fmt.Errorf("exact distinct unexpired PR bundle and source artifacts are required")
	}
	return bundleArtifact, sourceArtifact, nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
