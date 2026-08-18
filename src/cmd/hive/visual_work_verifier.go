package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/integrated"
	"github.com/kubestellar/hive/pkg/visualhive"
	"github.com/kubestellar/hive/pkg/visualhive/normalservice"
)

const (
	normalVisualPullRequestWorkflowName = "Visual Hive PR"
	normalVisualPullRequestWorkflowPath = ".github/workflows/visual-hive-pr.yml"
)

type normalVisualVerifiedPullRequest interface {
	Receipt() (visualhive.PullRequestCheckReceiptSnapshot, error)
	ApplyCheckEvidence(*visualhive.LifecycleStore, string) (visualhive.ApplyPullRequestCheckUpdateResult, error)
}

type normalVisualPullRequestFetcher func(context.Context, hivegithub.VisualHivePullRequestBundleRequest) (normalVisualVerifiedPullRequest, error)
type normalVisualPullRequestStateInspector func(context.Context, string, string, int, string, string, string, string) (hivegithub.ManagedPullRequestSnapshot, error)
type normalVisualPullRequestGateInspector func(context.Context, string, int) (hivegithub.PullRequestGate, error)
type normalVisualPullRequestReviewFetcher func(context.Context, hivegithub.PullRequestArtifactRequest) (hivegithub.VerifiedPullRequestArtifact, error)

// normalVisualPullRequestVerifier is the sole post-Worker verdict adapter. It
// discovers and verifies the exact GitHub run/artifacts through pkg/github,
// applies only the verifier's opaque check-evidence capability to the existing
// lifecycle, and returns the canonical sealed identity bytes to the service
// ledger. It has no merge, baseline, issue, or repository-write path.
type normalVisualPullRequestVerifier struct {
	github       *hivegithub.Client
	lifecycle    *visualhive.LifecycleStore
	loadConfig   func() (integrated.Config, int, error)
	fetch        normalVisualPullRequestFetcher
	inspectState normalVisualPullRequestStateInspector
	inspectGate  normalVisualPullRequestGateInspector
	fetchReview  normalVisualPullRequestReviewFetcher
	actionsAppID func(context.Context) (int64, error)
}

func (verifier *normalVisualPullRequestVerifier) VerifyPullRequest(ctx context.Context, request normalservice.PullRequestVerdictRequest) (normalservice.PullRequestVerdictReceipt, error) {
	if verifier == nil || verifier.lifecycle == nil || verifier.loadConfig == nil {
		return normalservice.PullRequestVerdictReceipt{}, errors.New("normal Visual Hive exact-head verifier is not configured")
	}
	current, normalACMM, err := verifier.loadConfig()
	if err != nil {
		return normalservice.PullRequestVerdictReceipt{}, err
	}
	repositoryID, maxACMM, err := validateNormalVisualPullRequestRequest(current, normalACMM, request)
	if err != nil {
		return normalservice.PullRequestVerdictReceipt{}, err
	}
	finding, exists := verifier.lifecycle.Finding(request.RepositoryFingerprint)
	if !exists || !strings.EqualFold(finding.Repository, current.Repository) || finding.RepositoryID != current.RepositoryID ||
		finding.PRNumber != request.PullRequestNumber || finding.PRURL != request.PullRequestURL || finding.Branch != request.HeadBranch || !strings.EqualFold(finding.RepairCommitSHA, request.HeadSHA) ||
		(finding.Status != visualhive.StatusPROpen && finding.Status != visualhive.StatusChecksRunning && finding.Status != visualhive.StatusNeedsRevision && finding.Status != visualhive.StatusReady) {
		return normalservice.PullRequestVerdictReceipt{}, errors.New("exact-head verifier request does not match the existing Worker PR lifecycle")
	}
	appID, err := verifier.resolveActionsAppID(ctx)
	if err != nil || appID <= 0 {
		if err == nil {
			err = errors.New("GitHub Actions App identity is unavailable")
		}
		return normalservice.PullRequestVerdictReceipt{}, err
	}
	// Production uses the verifier's GitHub client directly, while focused
	// tests may inject an inspector. Either source must take the red/pending
	// path before the success-bundle verifier binds the live default branch.
	if verifier.hasPullRequestGateSource() {
		gate, gateErr := verifier.inspectPullRequestGate(ctx, current.Repository, request.PullRequestNumber)
		if gateErr != nil {
			return normalservice.PullRequestVerdictReceipt{}, gateErr
		}
		if strings.ToLower(strings.TrimSpace(gate.VisualHiveCheckState)) != "success" {
			return verifier.verifyFailedOrPendingPullRequestGate(ctx, request, current, repositoryID, gate)
		}
	}
	verified, err := verifier.fetchExact(ctx, hivegithub.VisualHivePullRequestBundleRequest{
		Repository: current.Repository, PullRequestNumber: request.PullRequestNumber,
		ExpectedBaseRepository: current.Repository, ExpectedBaseRepositoryID: repositoryID,
		ExpectedBaseBranch: request.BaseBranch, ExpectedBaseSHA: request.BaseSHA,
		ExpectedHeadRepository: current.Repository, ExpectedHeadRepositoryID: repositoryID,
		ExpectedHeadBranch: request.HeadBranch, ExpectedHeadSHA: request.HeadSHA,
		ExpectedWorkflowName: normalVisualPullRequestWorkflowName, ExpectedWorkflowPath: normalVisualPullRequestWorkflowPath,
		ExpectedGitHubActionsAppID: appID, ExpectedProducerGitCommit: hivegithub.VisualHivePullRequestProducerCommit,
		DestinationDir: filepath.Join(current.StateDir, "visual-hive", "pr-verifier"), MaxACMM: maxACMM,
	})
	if err != nil {
		if errors.Is(err, hivegithub.ErrNoSuccessfulVisualHivePullRequestRun) {
			return verifier.verifyFailedOrPendingPullRequest(ctx, request, current, repositoryID)
		}
		return normalservice.PullRequestVerdictReceipt{}, err
	}
	snapshot, err := verified.Receipt()
	if err != nil {
		return normalservice.PullRequestVerdictReceipt{}, err
	}
	if err := validateNormalVisualPullRequestReceipt(snapshot, request, current, repositoryID, appID); err != nil {
		return normalservice.PullRequestVerdictReceipt{}, err
	}
	receiptBytes, err := json.Marshal(snapshot.Identity)
	if err != nil {
		return normalservice.PullRequestVerdictReceipt{}, err
	}
	digest := sha256.Sum256(receiptBytes)
	if snapshot.ReceiptSHA256 != hex.EncodeToString(digest[:]) {
		return normalservice.PullRequestVerdictReceipt{}, errors.New("sealed PR receipt digest differs from its canonical identity bytes")
	}
	applied, err := verified.ApplyCheckEvidence(verifier.lifecycle, request.RepositoryFingerprint)
	if err != nil {
		return normalservice.PullRequestVerdictReceipt{}, err
	}
	if applied.ReceiptSHA256 != snapshot.ReceiptSHA256 || applied.ReplayKey != snapshot.Identity.ReplayKey {
		return normalservice.PullRequestVerdictReceipt{}, errors.New("lifecycle applied different PR check-evidence identity")
	}
	return normalservice.PullRequestVerdictReceipt{
		HeadSHA: snapshot.Identity.Source.Head.SHA, Status: snapshot.Identity.Check.Conclusion,
		Receipt: receiptBytes, ReceiptSHA256: snapshot.ReceiptSHA256,
	}, nil
}

func (verifier *normalVisualPullRequestVerifier) hasPullRequestGateSource() bool {
	return verifier != nil && (verifier.inspectGate != nil || verifier.github != nil)
}

type normalVisualFailedPullRequestReceiptIdentity struct {
	SchemaVersion       string `json:"schema_version"`
	Repository          string `json:"repository"`
	RepositoryID        string `json:"repository_id"`
	PullRequestNumber   int    `json:"pull_request_number"`
	PullRequestURL      string `json:"pull_request_url"`
	BaseBranch          string `json:"base_branch"`
	BaseSHA             string `json:"base_sha"`
	HeadBranch          string `json:"head_branch"`
	HeadSHA             string `json:"head_sha"`
	WorkflowRunID       int64  `json:"workflow_run_id"`
	WorkflowRunAttempt  int    `json:"workflow_run_attempt"`
	WorkflowName        string `json:"workflow_name"`
	WorkflowPath        string `json:"workflow_path"`
	WorkflowEvent       string `json:"workflow_event"`
	Conclusion          string `json:"conclusion"`
	RunURL              string `json:"run_url"`
	ArtifactID          int64  `json:"artifact_id"`
	ArtifactName        string `json:"artifact_name"`
	ArtifactIndexSHA256 string `json:"artifact_index_sha256"`
	Authority           string `json:"authority"`
}

// verifyFailedOrPendingPullRequest handles only the state excluded from the
// successful bundle verifier. A pending run remains pending. A completed red
// run must first pass the separate review-artifact verifier before its exact
// check evidence can move the Worker lifecycle to needs_revision. The receipt
// is durable observation only: it cannot complete, consume, merge, or resolve
// the repair.
func (verifier *normalVisualPullRequestVerifier) verifyFailedOrPendingPullRequest(
	ctx context.Context,
	request normalservice.PullRequestVerdictRequest,
	current integrated.Config,
	repositoryID int64,
) (normalservice.PullRequestVerdictReceipt, error) {
	gate, err := verifier.inspectPullRequestGate(ctx, current.Repository, request.PullRequestNumber)
	if err != nil {
		return normalservice.PullRequestVerdictReceipt{}, err
	}
	return verifier.verifyFailedOrPendingPullRequestGate(ctx, request, current, repositoryID, gate)
}

func (verifier *normalVisualPullRequestVerifier) verifyFailedOrPendingPullRequestGate(
	ctx context.Context,
	request normalservice.PullRequestVerdictRequest,
	current integrated.Config,
	repositoryID int64,
	gate hivegithub.PullRequestGate,
) (normalservice.PullRequestVerdictReceipt, error) {
	if gate.Number != request.PullRequestNumber || gate.URL != request.PullRequestURL || !gate.Open || gate.Merged ||
		gate.BaseBranch != request.BaseBranch || !strings.EqualFold(gate.BaseSHA, request.BaseSHA) ||
		!strings.EqualFold(gate.HeadSHA, request.HeadSHA) {
		return normalservice.PullRequestVerdictReceipt{}, errors.New("exact-head failed-verdict gate differs from the existing Worker PR")
	}
	state := strings.ToLower(strings.TrimSpace(gate.VisualHiveCheckState))
	switch state {
	case "", "pending", "queued", "in_progress", "requested", "waiting":
		return normalservice.PullRequestVerdictReceipt{}, normalservice.ErrFinalVerdictPending
	case "failure":
		// Continue below only for one independently provenance-verified failed
		// pull_request workflow bound to the exact head.
	default:
		return normalservice.PullRequestVerdictReceipt{}, fmt.Errorf("exact-head Visual Hive PR check ended in unsupported state %q", state)
	}
	if !gate.VisualHiveProvenanceVerified || gate.VisualHiveWorkflowRunID <= 0 ||
		gate.VisualHiveWorkflowPath != normalVisualPullRequestWorkflowPath ||
		gate.VisualHiveWorkflowEvent != "pull_request" {
		return normalservice.PullRequestVerdictReceipt{}, errors.New("failed exact-head Visual Hive PR check lacks trusted workflow provenance")
	}
	review, err := verifier.fetchPullRequestReview(ctx, hivegithub.PullRequestArtifactRequest{
		Repository: current.Repository, PullRequestNumber: request.PullRequestNumber,
		ExpectedWorkflowRunID: gate.VisualHiveWorkflowRunID, ExpectedHeadSHA: request.HeadSHA,
		ExpectedHeadBranch: request.HeadBranch, ExpectedWorkflowName: normalVisualPullRequestWorkflowName,
		ExpectedWorkflowPath: normalVisualPullRequestWorkflowPath, ArtifactName: "visual-hive-pr",
		DestinationDir: filepath.Join(current.StateDir, "visual-hive", "pr-review"),
	})
	if err != nil {
		return normalservice.PullRequestVerdictReceipt{}, err
	}
	if review.RepositoryID != strconv.FormatInt(repositoryID, 10) || review.PullRequestNumber != request.PullRequestNumber ||
		review.CommitSHA != request.HeadSHA || review.HeadBranch != request.HeadBranch ||
		review.WorkflowRunID != gate.VisualHiveWorkflowRunID || review.WorkflowRunAttempt <= 0 ||
		review.WorkflowName != normalVisualPullRequestWorkflowName ||
		review.WorkflowPath != normalVisualPullRequestWorkflowPath || review.Conclusion != "failure" ||
		review.ArtifactName != "visual-hive-pr" || review.ArtifactID <= 0 ||
		review.ReviewSchemaVersion != visualhive.ReviewEvidenceSchemaVersion ||
		len(review.ArtifactIndexSHA256) != sha256.Size*2 {
		return normalservice.PullRequestVerdictReceipt{}, errors.New("verified failed review artifact differs from the exact Worker PR gate")
	}
	if _, err := hex.DecodeString(review.ArtifactIndexSHA256); err != nil {
		return normalservice.PullRequestVerdictReceipt{}, errors.New("verified failed review artifact has an invalid content index digest")
	}
	checks := normalVisualCheckEvidence(gate.Checks)
	if err := verifier.lifecycle.MarkChecksWithEvidence(
		request.RepositoryFingerprint,
		request.HeadSHA,
		false,
		normalVisualCheckSummary(gate.Checks),
		checks,
	); err != nil {
		return normalservice.PullRequestVerdictReceipt{}, err
	}
	identity := normalVisualFailedPullRequestReceiptIdentity{
		SchemaVersion: "hive.normal-visual-pr-failure.v1",
		Repository:    current.Repository, RepositoryID: strconv.FormatInt(repositoryID, 10),
		PullRequestNumber: request.PullRequestNumber, PullRequestURL: request.PullRequestURL,
		BaseBranch: request.BaseBranch, BaseSHA: strings.ToLower(request.BaseSHA),
		HeadBranch: request.HeadBranch, HeadSHA: strings.ToLower(request.HeadSHA),
		WorkflowRunID: review.WorkflowRunID, WorkflowRunAttempt: review.WorkflowRunAttempt,
		WorkflowName: review.WorkflowName, WorkflowPath: review.WorkflowPath, WorkflowEvent: "pull_request",
		Conclusion: review.Conclusion, RunURL: review.RunURL,
		ArtifactID: review.ArtifactID, ArtifactName: review.ArtifactName,
		ArtifactIndexSHA256: strings.ToLower(review.ArtifactIndexSHA256),
		Authority:           "check-evidence-only; no completion, consume, merge, or resolution authority",
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return normalservice.PullRequestVerdictReceipt{}, err
	}
	digest := sha256.Sum256(encoded)
	return normalservice.PullRequestVerdictReceipt{
		HeadSHA: request.HeadSHA, Status: review.Conclusion, Receipt: encoded,
		ReceiptSHA256: hex.EncodeToString(digest[:]),
	}, nil
}

func normalVisualCheckEvidence(checks []hivegithub.CheckObservation) []visualhive.CheckEvidence {
	result := make([]visualhive.CheckEvidence, 0, len(checks))
	for _, check := range checks {
		result = append(result, visualhive.CheckEvidence{
			Name: check.Name, State: check.State, URL: check.URL,
			ProvenanceVerified: check.ProvenanceVerified, WorkflowRunID: check.WorkflowRunID,
			WorkflowPath: check.WorkflowPath, WorkflowEvent: check.WorkflowEvent,
		})
	}
	return result
}

func normalVisualCheckSummary(checks []hivegithub.CheckObservation) string {
	parts := make([]string, 0, len(checks))
	for _, check := range checks {
		parts = append(parts, fmt.Sprintf("%s=%s", check.Name, check.State))
	}
	if len(parts) == 0 {
		return "no hosted checks observed"
	}
	return strings.Join(parts, ", ")
}

// ObservePullRequestState reuses Hive's existing exact managed-PR inspector as
// a read-only repository-wide WIP gate. It cannot merge, close, relabel, or
// otherwise mutate the Worker PR.
func (verifier *normalVisualPullRequestVerifier) ObservePullRequestState(ctx context.Context, request normalservice.PullRequestStateRequest) (normalservice.PullRequestStateObservation, error) {
	if verifier == nil || verifier.lifecycle == nil || verifier.loadConfig == nil {
		return normalservice.PullRequestStateObservation{}, errors.New("normal Visual Hive exact PR state observer is not configured")
	}
	current, normalACMM, err := verifier.loadConfig()
	if err != nil {
		return normalservice.PullRequestStateObservation{}, err
	}
	if _, _, err := validateNormalVisualPullRequestRequest(current, normalACMM, request.PullRequestVerdictRequest); err != nil {
		return normalservice.PullRequestStateObservation{}, err
	}
	finding, exists := verifier.lifecycle.Finding(request.RepositoryFingerprint)
	mergedSuccessor, matches := findingMatchesPullRequestState(finding, current, request)
	if !exists || !matches {
		return normalservice.PullRequestStateObservation{}, errors.New("PR state request does not match the existing Ready or exact merged-successor Worker PR and sealed receipt")
	}
	marker := "<!-- hive-repair: " + request.RepositoryFingerprint + " -->"
	snapshot, err := verifier.inspectPullRequestState(ctx, current.Repository, current.RepositoryID, request.PullRequestNumber, marker, request.HeadBranch, request.HeadSHA, request.BaseBranch)
	if err != nil {
		return normalservice.PullRequestStateObservation{}, err
	}
	if snapshot.Number != request.PullRequestNumber || snapshot.URL != request.PullRequestURL || snapshot.HeadBranch != request.HeadBranch ||
		!strings.EqualFold(snapshot.HeadSHA, request.HeadSHA) || snapshot.BaseBranch != request.BaseBranch {
		return normalservice.PullRequestStateObservation{}, errors.New("live Worker PR state differs from its exact durable identity")
	}
	state := strings.ToLower(strings.TrimSpace(snapshot.State))
	switch state {
	case "open":
		if snapshot.Merged {
			return normalservice.PullRequestStateObservation{}, errors.New("live Worker PR is simultaneously open and merged")
		}
		if mergedSuccessor {
			return normalservice.PullRequestStateObservation{}, errors.New("merged-successor lifecycle points to a live open Worker PR")
		}
		return normalservice.PullRequestStateObservation{State: state, Open: true}, nil
	case "closed":
		if mergedSuccessor && (!snapshot.Merged || !strings.EqualFold(strings.TrimSpace(snapshot.MergeSHA), strings.TrimSpace(finding.MergeSHA))) {
			return normalservice.PullRequestStateObservation{}, errors.New("merged-successor lifecycle differs from the exact live Worker PR merge")
		}
		return normalservice.PullRequestStateObservation{State: state, Merged: snapshot.Merged}, nil
	default:
		return normalservice.PullRequestStateObservation{}, errors.New("live Worker PR returned an unsupported state")
	}
}

func findingMatchesPullRequestState(finding visualhive.FindingLifecycle, current integrated.Config, request normalservice.PullRequestStateRequest) (bool, bool) {
	receipt := finding.LastPullRequestCheckReceipt
	mergedSuccessor := false
	switch finding.Status {
	case visualhive.StatusReady:
	case visualhive.StatusMerged, visualhive.StatusPostMergeVerifying, visualhive.StatusResolved, visualhive.StatusIssueClosed, visualhive.StatusNeedsRevision:
		mergedSuccessor = exactNormalVisualGitCommit(finding.MergeSHA)
		if !mergedSuccessor {
			return false, false
		}
	default:
		return false, false
	}
	if receipt == nil || request.VerdictReceiptSHA256 == "" ||
		!strings.EqualFold(finding.Repository, current.Repository) || finding.RepositoryID != current.RepositoryID ||
		finding.PRNumber != request.PullRequestNumber || finding.PRURL != request.PullRequestURL || finding.Branch != request.HeadBranch ||
		!strings.EqualFold(finding.RepairCommitSHA, request.HeadSHA) || receipt.ReceiptSHA256 != request.VerdictReceiptSHA256 {
		return false, false
	}
	identity := receipt.Identity
	if identity.SchemaVersion != visualhive.PullRequestCheckReceiptSchema || identity.Authority != (visualhive.PullRequestCheckAuthority{CheckEvidenceOnly: true}) ||
		!strings.EqualFold(identity.Source.Repository, current.Repository) || identity.Source.RepositoryID != current.RepositoryID || identity.Source.PullRequest != request.PullRequestNumber ||
		!strings.EqualFold(identity.Source.Base.Repository, current.Repository) || identity.Source.Base.RepositoryID != current.RepositoryID ||
		identity.Source.Base.Ref != request.BaseBranch || identity.Source.Base.SHA != request.BaseSHA ||
		!strings.EqualFold(identity.Source.Head.Repository, current.Repository) || identity.Source.Head.RepositoryID != current.RepositoryID ||
		identity.Source.Head.Ref != request.HeadBranch || identity.Source.Head.SHA != request.HeadSHA ||
		identity.Workflow.Name != normalVisualPullRequestWorkflowName || identity.Workflow.Path != normalVisualPullRequestWorkflowPath || identity.Workflow.Event != "pull_request" ||
		identity.Producer.GitCommit != hivegithub.VisualHivePullRequestProducerCommit || identity.Check.State != "success" || identity.Check.Conclusion != "success" || identity.ReplayKey == "" {
		return false, false
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return false, false
	}
	digest := sha256.Sum256(encoded)
	return mergedSuccessor, receipt.ReceiptSHA256 == hex.EncodeToString(digest[:])
}

func (verifier *normalVisualPullRequestVerifier) inspectPullRequestState(ctx context.Context, repository, repositoryID string, number int, marker, branch, headSHA, base string) (hivegithub.ManagedPullRequestSnapshot, error) {
	if verifier.inspectState != nil {
		return verifier.inspectState(ctx, repository, repositoryID, number, marker, branch, headSHA, base)
	}
	if verifier.github == nil {
		return hivegithub.ManagedPullRequestSnapshot{}, errors.New("normal Visual Hive GitHub client is unavailable")
	}
	return verifier.github.InspectManagedPullRequestExact(ctx, repository, repositoryID, number, marker, branch, headSHA, base)
}

func (verifier *normalVisualPullRequestVerifier) inspectPullRequestGate(ctx context.Context, repository string, number int) (hivegithub.PullRequestGate, error) {
	if verifier.inspectGate != nil {
		return verifier.inspectGate(ctx, repository, number)
	}
	if verifier.github == nil {
		return hivegithub.PullRequestGate{}, errors.New("normal Visual Hive GitHub client is unavailable")
	}
	return verifier.github.InspectPullRequestGate(ctx, repository, number)
}

func (verifier *normalVisualPullRequestVerifier) fetchPullRequestReview(ctx context.Context, request hivegithub.PullRequestArtifactRequest) (hivegithub.VerifiedPullRequestArtifact, error) {
	if verifier.fetchReview != nil {
		return verifier.fetchReview(ctx, request)
	}
	if verifier.github == nil {
		return hivegithub.VerifiedPullRequestArtifact{}, errors.New("normal Visual Hive GitHub client is unavailable")
	}
	return verifier.github.FetchAndVerifyPullRequestArtifact(ctx, request)
}

func (verifier *normalVisualPullRequestVerifier) resolveActionsAppID(ctx context.Context) (int64, error) {
	if verifier.actionsAppID != nil {
		return verifier.actionsAppID(ctx)
	}
	if verifier.github == nil {
		return 0, errors.New("normal Visual Hive GitHub client is unavailable")
	}
	return verifier.github.ExpectedGitHubAppID(ctx, "github-actions")
}

func (verifier *normalVisualPullRequestVerifier) fetchExact(ctx context.Context, request hivegithub.VisualHivePullRequestBundleRequest) (normalVisualVerifiedPullRequest, error) {
	if verifier.fetch != nil {
		return verifier.fetch(ctx, request)
	}
	if verifier.github == nil {
		return nil, errors.New("normal Visual Hive GitHub client is unavailable")
	}
	_, verified, err := verifier.github.FetchAndVerifyVisualHivePullRequestBundle(ctx, request)
	if err != nil {
		return nil, err
	}
	return verified, nil
}

func validateNormalVisualPullRequestRequest(current integrated.Config, normalACMM int, request normalservice.PullRequestVerdictRequest) (int64, int, error) {
	repositoryID, err := strconv.ParseInt(current.RepositoryID, 10, 64)
	if err != nil || repositoryID <= 0 || strconv.FormatInt(repositoryID, 10) != current.RepositoryID {
		return 0, 0, errors.New("installed repository has no exact positive numeric GitHub identity")
	}
	if !current.VisualHive || current.Paused || (current.Automation != integrated.AutomationRepairPR && current.Automation != integrated.AutomationAutoMerge) ||
		!strings.EqualFold(request.Repository, current.Repository) || request.RepositoryFingerprint != strings.ToLower(strings.TrimSpace(request.RepositoryFingerprint)) ||
		len(request.RepositoryFingerprint) != sha256.Size*2 || request.PullRequestNumber <= 0 || strings.TrimSpace(request.PullRequestURL) == "" || request.PullRequestURL != strings.TrimSpace(request.PullRequestURL) || strings.TrimSpace(request.IdempotencyKey) == "" ||
		request.BaseBranch != current.DefaultBranch || request.BaseBranch != strings.TrimSpace(request.BaseBranch) || request.HeadBranch == "" || request.HeadBranch != strings.TrimSpace(request.HeadBranch) ||
		!exactNormalVisualGitCommit(request.BaseSHA) || !exactNormalVisualGitCommit(request.HeadSHA) || strings.TrimSpace(current.StateDir) == "" {
		return 0, 0, errors.New("normal Visual Hive verdict request lacks exact installed repository, PR, branch, base, head, or policy identity")
	}
	if _, err := hex.DecodeString(request.RepositoryFingerprint); err != nil {
		return 0, 0, errors.New("normal Visual Hive verdict request has an invalid repository fingerprint")
	}
	maxACMM := current.ACMMLevel
	if normalACMM < maxACMM {
		maxACMM = normalACMM
	}
	if maxACMM <= 0 {
		return 0, 0, errors.New("normal Visual Hive verdict request has no current ACMM authority")
	}
	return repositoryID, maxACMM, nil
}

func validateNormalVisualPullRequestReceipt(snapshot visualhive.PullRequestCheckReceiptSnapshot, request normalservice.PullRequestVerdictRequest, current integrated.Config, repositoryID, appID int64) error {
	identity := snapshot.Identity
	wantedAuthority := visualhive.PullRequestCheckAuthority{CheckEvidenceOnly: true}
	if snapshot.ReceiptSHA256 != strings.ToLower(strings.TrimSpace(snapshot.ReceiptSHA256)) || len(snapshot.ReceiptSHA256) != sha256.Size*2 ||
		identity.SchemaVersion != visualhive.PullRequestCheckReceiptSchema || identity.Authority != wantedAuthority ||
		!strings.EqualFold(identity.Source.Repository, current.Repository) || identity.Source.RepositoryID != strconv.FormatInt(repositoryID, 10) ||
		identity.Source.PullRequest != request.PullRequestNumber || !strings.EqualFold(identity.Source.Base.Repository, current.Repository) || identity.Source.Base.RepositoryID != strconv.FormatInt(repositoryID, 10) ||
		identity.Source.Base.Ref != request.BaseBranch || identity.Source.Base.SHA != request.BaseSHA || !strings.EqualFold(identity.Source.Head.Repository, current.Repository) ||
		identity.Source.Head.RepositoryID != strconv.FormatInt(repositoryID, 10) || identity.Source.Head.Ref != request.HeadBranch || identity.Source.Head.SHA != request.HeadSHA ||
		identity.Workflow.Name != normalVisualPullRequestWorkflowName || identity.Workflow.Path != normalVisualPullRequestWorkflowPath || identity.Workflow.Event != "pull_request" ||
		identity.Workflow.AppID != strconv.FormatInt(appID, 10) || identity.Producer.GitCommit != hivegithub.VisualHivePullRequestProducerCommit ||
		identity.Check.State != "success" || identity.Check.Conclusion != "success" || identity.ReplayKey == "" {
		return errors.New("sealed PR receipt differs from the exact installed Worker PR request")
	}
	if _, err := hex.DecodeString(snapshot.ReceiptSHA256); err != nil {
		return errors.New("sealed PR receipt has an invalid digest")
	}
	return nil
}

func exactNormalVisualGitCommit(value string) bool {
	if len(value) != 40 || value != strings.ToLower(strings.TrimSpace(value)) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
