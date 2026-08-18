package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/integrated"
	"github.com/kubestellar/hive/pkg/visualhive"
	"github.com/kubestellar/hive/pkg/visualhive/normalservice"
)

func TestNormalVisualPullRequestVerifierAppliesOnlyExactSealedCheckEvidence(t *testing.T) {
	fingerprint := strings.Repeat("f", 64)
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	lifecycle := writeNormalVisualVerifierLifecycle(t, fingerprint, headSHA)
	snapshot := normalVisualVerifierSnapshot(t, baseSHA, headSHA)
	verified := &fakeNormalVisualVerifiedPullRequest{snapshot: snapshot}
	var fetched hivegithub.VisualHivePullRequestBundleRequest
	config := integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(),
		VisualHive: true, Automation: integrated.AutomationRepairPR, ACMMLevel: 5,
	}
	verifier := &normalVisualPullRequestVerifier{
		lifecycle:    lifecycle,
		loadConfig:   func() (integrated.Config, int, error) { return config, 4, nil },
		actionsAppID: func(context.Context) (int64, error) { return 15368, nil },
		fetch: func(_ context.Context, request hivegithub.VisualHivePullRequestBundleRequest) (normalVisualVerifiedPullRequest, error) {
			fetched = request
			return verified, nil
		},
	}
	receipt, err := verifier.VerifyPullRequest(context.Background(), normalservice.PullRequestVerdictRequest{
		IdempotencyKey: "workflow:order", Repository: "owner/repo", RepositoryFingerprint: fingerprint,
		PullRequestNumber: 7, PullRequestURL: "https://example.test/pr/7", HeadBranch: "hive/repair-proof", HeadSHA: headSHA, BaseBranch: "main", BaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if verified.applyCalls != 1 || verified.appliedFingerprint != fingerprint || receipt.HeadSHA != headSHA || receipt.Status != "success" || receipt.ReceiptSHA256 != snapshot.ReceiptSHA256 {
		t.Fatalf("exact check evidence was not applied once: verified=%+v receipt=%+v", verified, receipt)
	}
	if fetched.Repository != config.Repository || fetched.ExpectedBaseRepositoryID != 123 || fetched.ExpectedHeadRepositoryID != 123 ||
		fetched.ExpectedBaseSHA != baseSHA || fetched.ExpectedHeadSHA != headSHA || fetched.ExpectedHeadBranch != "hive/repair-proof" ||
		fetched.ExpectedWorkflowName != normalVisualPullRequestWorkflowName || fetched.ExpectedWorkflowPath != normalVisualPullRequestWorkflowPath ||
		fetched.ExpectedGitHubActionsAppID != 15368 || fetched.ExpectedProducerGitCommit != hivegithub.VisualHivePullRequestProducerCommit || fetched.MaxACMM != 4 ||
		fetched.DestinationDir != filepath.Join(config.StateDir, "visual-hive", "pr-verifier") {
		t.Fatalf("verifier fetch request lost independently knowable pins: %+v", fetched)
	}
	var persistedIdentity visualhive.PullRequestCheckReceiptIdentity
	if json.Unmarshal(receipt.Receipt, &persistedIdentity) != nil || persistedIdentity.ReplayKey != snapshot.Identity.ReplayKey {
		t.Fatalf("service receipt is not the canonical sealed identity: %s", receipt.Receipt)
	}
}

func TestNormalVisualPullRequestVerifierRejectsReceiptDriftBeforeLifecycleApply(t *testing.T) {
	fingerprint := strings.Repeat("f", 64)
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	lifecycle := writeNormalVisualVerifierLifecycle(t, fingerprint, headSHA)
	snapshot := normalVisualVerifierSnapshot(t, baseSHA, strings.Repeat("c", 40))
	verified := &fakeNormalVisualVerifiedPullRequest{snapshot: snapshot}
	config := integrated.Config{Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(), VisualHive: true, Automation: integrated.AutomationRepairPR, ACMMLevel: 5}
	verifier := &normalVisualPullRequestVerifier{
		lifecycle: lifecycle, loadConfig: func() (integrated.Config, int, error) { return config, 5, nil },
		actionsAppID: func(context.Context) (int64, error) { return 15368, nil },
		fetch: func(context.Context, hivegithub.VisualHivePullRequestBundleRequest) (normalVisualVerifiedPullRequest, error) {
			return verified, nil
		},
	}
	_, err := verifier.VerifyPullRequest(context.Background(), normalservice.PullRequestVerdictRequest{
		IdempotencyKey: "workflow:order", Repository: "owner/repo", RepositoryFingerprint: fingerprint,
		PullRequestNumber: 7, PullRequestURL: "https://example.test/pr/7", HeadBranch: "hive/repair-proof", HeadSHA: headSHA, BaseBranch: "main", BaseSHA: baseSHA,
	})
	if err == nil || !strings.Contains(err.Error(), "differs from the exact installed Worker PR") || verified.applyCalls != 0 {
		t.Fatalf("drifted receipt reached lifecycle apply: err=%v verified=%+v", err, verified)
	}
}

func TestNormalVisualPullRequestVerifierRecordsVerifiedFailedReviewAfterDefaultBranchAdvances(t *testing.T) {
	fingerprint := strings.Repeat("f", 64)
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	lifecycle := writeNormalVisualVerifierLifecycle(t, fingerprint, headSHA)
	config := integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(),
		VisualHive: true, Automation: integrated.AutomationRepairPR, ACMMLevel: 5,
	}
	request := normalservice.PullRequestVerdictRequest{
		IdempotencyKey: "workflow:order", Repository: config.Repository, RepositoryFingerprint: fingerprint,
		PullRequestNumber: 7, PullRequestURL: "https://example.test/pr/7",
		HeadBranch: "hive/repair-proof", HeadSHA: headSHA, BaseBranch: "main", BaseSHA: baseSHA,
	}
	var reviewRequest hivegithub.PullRequestArtifactRequest
	successFetches := 0
	verifier := &normalVisualPullRequestVerifier{
		lifecycle: lifecycle, loadConfig: func() (integrated.Config, int, error) { return config, 5, nil },
		actionsAppID: func(context.Context) (int64, error) { return 15368, nil },
		fetch: func(context.Context, hivegithub.VisualHivePullRequestBundleRequest) (normalVisualVerifiedPullRequest, error) {
			successFetches++
			return nil, errors.New("successful-bundle path rejected advanced default branch")
		},
		inspectGate: func(_ context.Context, repository string, number int) (hivegithub.PullRequestGate, error) {
			if repository != config.Repository || number != request.PullRequestNumber {
				t.Fatalf("gate inspection lost exact PR identity: repository=%s number=%d", repository, number)
			}
			return hivegithub.PullRequestGate{
				Number: number, URL: request.PullRequestURL, Open: true,
				BaseBranch: request.BaseBranch, BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA,
				VisualHiveCheckState: "failure", VisualHiveProvenanceVerified: true,
				VisualHiveWorkflowRunID: 77, VisualHiveWorkflowPath: normalVisualPullRequestWorkflowPath,
				VisualHiveWorkflowEvent: "pull_request",
				Checks: []hivegithub.CheckObservation{
					{Name: "repository-test-001", State: "success"},
					{Name: "visual-hive", State: "failure", URL: "https://example.test/run/77", ProvenanceVerified: true, WorkflowRunID: 77, WorkflowPath: normalVisualPullRequestWorkflowPath, WorkflowEvent: "pull_request"},
				},
			}, nil
		},
		fetchReview: func(_ context.Context, value hivegithub.PullRequestArtifactRequest) (hivegithub.VerifiedPullRequestArtifact, error) {
			reviewRequest = value
			return hivegithub.VerifiedPullRequestArtifact{
				RepositoryID: config.RepositoryID, PullRequestNumber: request.PullRequestNumber,
				WorkflowRunID: 77, WorkflowRunAttempt: 1, ArtifactID: 99, ArtifactName: "visual-hive-pr",
				CommitSHA: request.HeadSHA, HeadBranch: request.HeadBranch,
				WorkflowName: normalVisualPullRequestWorkflowName, WorkflowPath: normalVisualPullRequestWorkflowPath,
				Conclusion: "failure", RunURL: "https://example.test/run/77",
				ReviewSchemaVersion: visualhive.ReviewEvidenceSchemaVersion, ArtifactIndexSHA256: strings.Repeat("c", 64),
			}, nil
		},
	}
	receipt, err := verifier.VerifyPullRequest(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.HeadSHA != headSHA || receipt.Status != "failure" || !json.Valid(receipt.Receipt) {
		t.Fatalf("failed review did not return an exact red receipt: %+v", receipt)
	}
	if successFetches != 0 {
		t.Fatalf("verified red gate attempted the successful-bundle path after base drift: fetches=%d", successFetches)
	}
	digest := sha256.Sum256(receipt.Receipt)
	if receipt.ReceiptSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("failed review receipt digest = %s, want %s", receipt.ReceiptSHA256, hex.EncodeToString(digest[:]))
	}
	if reviewRequest.Repository != config.Repository || reviewRequest.PullRequestNumber != request.PullRequestNumber ||
		reviewRequest.ExpectedWorkflowRunID != 77 || reviewRequest.ExpectedHeadSHA != request.HeadSHA ||
		reviewRequest.ExpectedHeadBranch != request.HeadBranch || reviewRequest.ArtifactName != "visual-hive-pr" ||
		reviewRequest.DestinationDir != filepath.Join(config.StateDir, "visual-hive", "pr-review") {
		t.Fatalf("failed review fetch lost exact immutable identity: %+v", reviewRequest)
	}
	finding, exists := lifecycle.Finding(fingerprint)
	if !exists || finding.Status != visualhive.StatusNeedsRevision || finding.LastPullRequestCheckReceipt != nil ||
		len(finding.LastCheckRuns) != 2 || finding.LastCheckRuns[1].State != "failure" ||
		!finding.LastCheckRuns[1].ProvenanceVerified {
		t.Fatalf("failed review was reinterpreted or not durably recorded: exists=%t finding=%+v", exists, finding)
	}
}

func TestNormalVisualPullRequestVerifierProductionClientEnablesGatePreinspection(t *testing.T) {
	if (&normalVisualPullRequestVerifier{}).hasPullRequestGateSource() {
		t.Fatal("empty verifier unexpectedly exposes a pull-request gate source")
	}
	if !(&normalVisualPullRequestVerifier{github: &hivegithub.Client{}}).hasPullRequestGateSource() {
		t.Fatal("production GitHub client did not enable pull-request gate preinspection")
	}
	if !(&normalVisualPullRequestVerifier{
		inspectGate: func(context.Context, string, int) (hivegithub.PullRequestGate, error) {
			return hivegithub.PullRequestGate{}, nil
		},
	}).hasPullRequestGateSource() {
		t.Fatal("injected pull-request gate inspector did not enable preinspection")
	}
}

func TestNormalVisualPullRequestVerifierKeepsUnfinishedExactHeadPending(t *testing.T) {
	fingerprint := strings.Repeat("f", 64)
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	lifecycle := writeNormalVisualVerifierLifecycle(t, fingerprint, headSHA)
	config := integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(),
		VisualHive: true, Automation: integrated.AutomationRepairPR, ACMMLevel: 5,
	}
	reviewFetches := 0
	verifier := &normalVisualPullRequestVerifier{
		lifecycle: lifecycle, loadConfig: func() (integrated.Config, int, error) { return config, 5, nil },
		actionsAppID: func(context.Context) (int64, error) { return 15368, nil },
		fetch: func(context.Context, hivegithub.VisualHivePullRequestBundleRequest) (normalVisualVerifiedPullRequest, error) {
			t.Fatal("unfinished exact-head gate reached successful-bundle fetch")
			return nil, errors.New("unreachable")
		},
		inspectGate: func(context.Context, string, int) (hivegithub.PullRequestGate, error) {
			return hivegithub.PullRequestGate{
				Number: 7, URL: "https://example.test/pr/7", Open: true,
				BaseBranch: "main", BaseSHA: baseSHA, HeadSHA: headSHA, VisualHiveCheckState: "in_progress",
			}, nil
		},
		fetchReview: func(context.Context, hivegithub.PullRequestArtifactRequest) (hivegithub.VerifiedPullRequestArtifact, error) {
			reviewFetches++
			return hivegithub.VerifiedPullRequestArtifact{}, nil
		},
	}
	_, err := verifier.VerifyPullRequest(context.Background(), normalservice.PullRequestVerdictRequest{
		IdempotencyKey: "workflow:order", Repository: config.Repository, RepositoryFingerprint: fingerprint,
		PullRequestNumber: 7, PullRequestURL: "https://example.test/pr/7",
		HeadBranch: "hive/repair-proof", HeadSHA: headSHA, BaseBranch: "main", BaseSHA: baseSHA,
	})
	if !errors.Is(err, normalservice.ErrFinalVerdictPending) || reviewFetches != 0 {
		t.Fatalf("unfinished exact head = %v, review fetches=%d; want pending without review fetch", err, reviewFetches)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if finding.Status != visualhive.StatusPROpen {
		t.Fatalf("pending exact head changed lifecycle status: %+v", finding)
	}
}

func TestNormalVisualPullRequestStateObserverReusesExactReadOnlyManagedPRInspection(t *testing.T) {
	fingerprint := strings.Repeat("f", 64)
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	sealedReceipt := normalVisualVerifierSnapshot(t, baseSHA, headSHA)
	lifecycle := writeNormalVisualReadyVerifierLifecycle(t, fingerprint, headSHA, sealedReceipt)
	config := integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(),
		VisualHive: true, Automation: integrated.AutomationRepairPR, ACMMLevel: 5,
	}
	request := normalservice.PullRequestStateRequest{
		PullRequestVerdictRequest: normalservice.PullRequestVerdictRequest{
			IdempotencyKey: "workflow:order", Repository: config.Repository, RepositoryFingerprint: fingerprint,
			PullRequestNumber: 7, PullRequestURL: "https://example.test/pr/7", HeadBranch: "hive/repair-proof", HeadSHA: headSHA, BaseBranch: "main", BaseSHA: baseSHA,
		},
		VerdictReceiptSHA256: sealedReceipt.ReceiptSHA256,
	}
	state, merged, calls := "open", false, 0
	verifier := &normalVisualPullRequestVerifier{
		lifecycle: lifecycle, loadConfig: func() (integrated.Config, int, error) { return config, 5, nil },
		inspectState: func(_ context.Context, repository, repositoryID string, number int, marker, branch, head, base string) (hivegithub.ManagedPullRequestSnapshot, error) {
			calls++
			if repository != config.Repository || repositoryID != config.RepositoryID || number != request.PullRequestNumber ||
				marker != "<!-- hive-repair: "+fingerprint+" -->" || branch != request.HeadBranch || head != request.HeadSHA || base != request.BaseBranch {
				t.Fatalf("state inspection lost exact durable PR identity: repo=%s id=%s number=%d marker=%q branch=%s head=%s base=%s", repository, repositoryID, number, marker, branch, head, base)
			}
			return hivegithub.ManagedPullRequestSnapshot{
				Number: number, URL: request.PullRequestURL, State: state, Merged: merged,
				HeadBranch: branch, HeadSHA: head, BaseBranch: base,
			}, nil
		},
	}
	observation, err := verifier.ObservePullRequestState(context.Background(), request)
	if err != nil || !observation.Open || observation.Merged || observation.State != "open" {
		t.Fatalf("open exact PR observation = %+v err=%v", observation, err)
	}
	state, merged = "closed", true
	observation, err = verifier.ObservePullRequestState(context.Background(), request)
	if err != nil || observation.Open || !observation.Merged || observation.State != "closed" || calls != 2 {
		t.Fatalf("merged exact PR observation = %+v calls=%d err=%v", observation, calls, err)
	}
}

func TestNormalVisualPullRequestStateObserverRejectsNonReadyOrDriftedSealedLifecycle(t *testing.T) {
	fingerprint := strings.Repeat("f", 64)
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	valid := normalVisualVerifierSnapshot(t, baseSHA, headSHA)
	drifted := valid
	drifted.Identity.Source.Head.SHA = strings.Repeat("c", 40)
	driftedBytes, err := json.Marshal(drifted.Identity)
	if err != nil {
		t.Fatal(err)
	}
	driftedDigest := sha256.Sum256(driftedBytes)
	drifted.ReceiptSHA256 = hex.EncodeToString(driftedDigest[:])
	cases := []struct {
		name    string
		status  visualhive.LifecycleStatus
		receipt *visualhive.PullRequestCheckReceiptSnapshot
		digest  string
	}{
		{name: "not Ready", status: visualhive.StatusPROpen, receipt: &valid, digest: valid.ReceiptSHA256},
		{name: "pre-merge needs revision", status: visualhive.StatusNeedsRevision, receipt: &valid, digest: valid.ReceiptSHA256},
		{name: "missing receipt", status: visualhive.StatusReady, receipt: nil, digest: valid.ReceiptSHA256},
		{name: "ledger digest drift", status: visualhive.StatusReady, receipt: &valid, digest: strings.Repeat("0", 64)},
		{name: "sealed head drift", status: visualhive.StatusReady, receipt: &drifted, digest: drifted.ReceiptSHA256},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			lifecycle := writeNormalVisualVerifierLifecycleState(t, fingerprint, headSHA, test.status, test.receipt)
			config := integrated.Config{
				Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(),
				VisualHive: true, Automation: integrated.AutomationRepairPR, ACMMLevel: 5,
			}
			inspections := 0
			verifier := &normalVisualPullRequestVerifier{
				lifecycle: lifecycle, loadConfig: func() (integrated.Config, int, error) { return config, 5, nil },
				inspectState: func(context.Context, string, string, int, string, string, string, string) (hivegithub.ManagedPullRequestSnapshot, error) {
					inspections++
					return hivegithub.ManagedPullRequestSnapshot{}, nil
				},
			}
			_, observeErr := verifier.ObservePullRequestState(context.Background(), normalservice.PullRequestStateRequest{
				PullRequestVerdictRequest: normalservice.PullRequestVerdictRequest{
					IdempotencyKey: "workflow:order", Repository: config.Repository, RepositoryFingerprint: fingerprint,
					PullRequestNumber: 7, PullRequestURL: "https://example.test/pr/7", HeadBranch: "hive/repair-proof", HeadSHA: headSHA, BaseBranch: "main", BaseSHA: baseSHA,
				},
				VerdictReceiptSHA256: test.digest,
			})
			if observeErr == nil || !strings.Contains(observeErr.Error(), "Ready or exact merged-successor Worker PR and sealed receipt") || inspections != 0 {
				t.Fatalf("unsealed lifecycle reached live PR inspection: err=%v inspections=%d", observeErr, inspections)
			}
		})
	}
}

func TestNormalVisualPullRequestStateObserverAcceptsOnlyExactMergedLifecycleSuccessors(t *testing.T) {
	fingerprint := strings.Repeat("f", 64)
	baseSHA, headSHA, mergeSHA := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("d", 40)
	sealedReceipt := normalVisualVerifierSnapshot(t, baseSHA, headSHA)
	config := integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(),
		VisualHive: true, Automation: integrated.AutomationRepairPR, ACMMLevel: 5,
	}
	request := normalservice.PullRequestStateRequest{
		PullRequestVerdictRequest: normalservice.PullRequestVerdictRequest{
			IdempotencyKey: "workflow:order", Repository: config.Repository, RepositoryFingerprint: fingerprint,
			PullRequestNumber: 7, PullRequestURL: "https://example.test/pr/7", HeadBranch: "hive/repair-proof", HeadSHA: headSHA, BaseBranch: "main", BaseSHA: baseSHA,
		},
		VerdictReceiptSHA256: sealedReceipt.ReceiptSHA256,
	}
	for _, status := range []visualhive.LifecycleStatus{
		visualhive.StatusMerged, visualhive.StatusPostMergeVerifying, visualhive.StatusResolved, visualhive.StatusIssueClosed, visualhive.StatusNeedsRevision,
	} {
		t.Run(string(status), func(t *testing.T) {
			lifecycle := writeNormalVisualVerifierMergedLifecycleState(t, fingerprint, headSHA, status, sealedReceipt, mergeSHA)
			observedMerge := mergeSHA
			merged := true
			state := "closed"
			verifier := &normalVisualPullRequestVerifier{
				lifecycle: lifecycle, loadConfig: func() (integrated.Config, int, error) { return config, 5, nil },
				inspectState: func(context.Context, string, string, int, string, string, string, string) (hivegithub.ManagedPullRequestSnapshot, error) {
					return hivegithub.ManagedPullRequestSnapshot{
						Number: 7, URL: request.PullRequestURL, State: state, Merged: merged, MergeSHA: observedMerge,
						HeadBranch: request.HeadBranch, HeadSHA: request.HeadSHA, BaseBranch: request.BaseBranch,
					}, nil
				},
			}
			observation, err := verifier.ObservePullRequestState(context.Background(), request)
			if err != nil || observation.Open || !observation.Merged || observation.State != "closed" {
				t.Fatalf("exact merged successor observation = %+v err=%v", observation, err)
			}
			observedMerge = strings.Repeat("e", 40)
			if _, err := verifier.ObservePullRequestState(context.Background(), request); err == nil || !strings.Contains(err.Error(), "exact live Worker PR merge") {
				t.Fatalf("drifted successor merge was accepted: %v", err)
			}
			observedMerge, merged = mergeSHA, false
			if _, err := verifier.ObservePullRequestState(context.Background(), request); err == nil || !strings.Contains(err.Error(), "exact live Worker PR merge") {
				t.Fatalf("unmerged successor PR was accepted: %v", err)
			}
			merged, state = false, "open"
			if _, err := verifier.ObservePullRequestState(context.Background(), request); err == nil || !strings.Contains(err.Error(), "live open Worker PR") {
				t.Fatalf("open successor PR was accepted: %v", err)
			}
		})
	}
}

func TestNormalVisualPullRequestStateObserverRejectsLiveIdentityAndStateDrift(t *testing.T) {
	fingerprint := strings.Repeat("f", 64)
	baseSHA, headSHA := strings.Repeat("a", 40), strings.Repeat("b", 40)
	sealedReceipt := normalVisualVerifierSnapshot(t, baseSHA, headSHA)
	config := integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(),
		VisualHive: true, Automation: integrated.AutomationRepairPR, ACMMLevel: 5,
	}
	request := normalservice.PullRequestStateRequest{
		PullRequestVerdictRequest: normalservice.PullRequestVerdictRequest{
			IdempotencyKey: "workflow:order", Repository: config.Repository, RepositoryFingerprint: fingerprint,
			PullRequestNumber: 7, PullRequestURL: "https://example.test/pr/7", HeadBranch: "hive/repair-proof", HeadSHA: headSHA, BaseBranch: "main", BaseSHA: baseSHA,
		},
		VerdictReceiptSHA256: sealedReceipt.ReceiptSHA256,
	}
	for name, mutate := range map[string]func(*hivegithub.ManagedPullRequestSnapshot){
		"URL":           func(snapshot *hivegithub.ManagedPullRequestSnapshot) { snapshot.URL = "https://example.test/pr/8" },
		"head":          func(snapshot *hivegithub.ManagedPullRequestSnapshot) { snapshot.HeadSHA = strings.Repeat("c", 40) },
		"unknown state": func(snapshot *hivegithub.ManagedPullRequestSnapshot) { snapshot.State = "unknown" },
		"open merged":   func(snapshot *hivegithub.ManagedPullRequestSnapshot) { snapshot.Merged = true },
	} {
		t.Run(name, func(t *testing.T) {
			lifecycle := writeNormalVisualReadyVerifierLifecycle(t, fingerprint, headSHA, sealedReceipt)
			verifier := &normalVisualPullRequestVerifier{
				lifecycle: lifecycle, loadConfig: func() (integrated.Config, int, error) { return config, 5, nil },
				inspectState: func(context.Context, string, string, int, string, string, string, string) (hivegithub.ManagedPullRequestSnapshot, error) {
					snapshot := hivegithub.ManagedPullRequestSnapshot{
						Number: 7, URL: request.PullRequestURL, State: "open", HeadBranch: request.HeadBranch, HeadSHA: request.HeadSHA, BaseBranch: request.BaseBranch,
					}
					mutate(&snapshot)
					return snapshot, nil
				},
			}
			if _, err := verifier.ObservePullRequestState(context.Background(), request); err == nil {
				t.Fatal("drifted live Worker PR state was accepted")
			}
		})
	}
}

type fakeNormalVisualVerifiedPullRequest struct {
	snapshot           visualhive.PullRequestCheckReceiptSnapshot
	applyCalls         int
	appliedFingerprint string
}

func (verified *fakeNormalVisualVerifiedPullRequest) Receipt() (visualhive.PullRequestCheckReceiptSnapshot, error) {
	return verified.snapshot, nil
}

func (verified *fakeNormalVisualVerifiedPullRequest) ApplyCheckEvidence(_ *visualhive.LifecycleStore, fingerprint string) (visualhive.ApplyPullRequestCheckUpdateResult, error) {
	verified.applyCalls++
	verified.appliedFingerprint = fingerprint
	return visualhive.ApplyPullRequestCheckUpdateResult{ReceiptSHA256: verified.snapshot.ReceiptSHA256, ReplayKey: verified.snapshot.Identity.ReplayKey}, nil
}

func writeNormalVisualVerifierLifecycle(t *testing.T, fingerprint, headSHA string) *visualhive.LifecycleStore {
	return writeNormalVisualVerifierLifecycleState(t, fingerprint, headSHA, visualhive.StatusPROpen, nil)
}

func writeNormalVisualReadyVerifierLifecycle(t *testing.T, fingerprint, headSHA string, receipt visualhive.PullRequestCheckReceiptSnapshot) *visualhive.LifecycleStore {
	return writeNormalVisualVerifierLifecycleState(t, fingerprint, headSHA, visualhive.StatusReady, &receipt)
}

func writeNormalVisualVerifierLifecycleState(t *testing.T, fingerprint, headSHA string, status visualhive.LifecycleStatus, receipt *visualhive.PullRequestCheckReceiptSnapshot) *visualhive.LifecycleStore {
	return writeNormalVisualVerifierLifecycleStateWithMerge(t, fingerprint, headSHA, status, receipt, "")
}

func writeNormalVisualVerifierMergedLifecycleState(t *testing.T, fingerprint, headSHA string, status visualhive.LifecycleStatus, receipt visualhive.PullRequestCheckReceiptSnapshot, mergeSHA string) *visualhive.LifecycleStore {
	return writeNormalVisualVerifierLifecycleStateWithMerge(t, fingerprint, headSHA, status, &receipt, mergeSHA)
}

func writeNormalVisualVerifierLifecycleStateWithMerge(t *testing.T, fingerprint, headSHA string, status visualhive.LifecycleStatus, receipt *visualhive.PullRequestCheckReceiptSnapshot, mergeSHA string) *visualhive.LifecycleStore {
	t.Helper()
	dir := t.TempDir()
	state := visualhive.LifecycleState{
		SchemaVersion: visualhive.LifecycleSchema, UpdatedAt: time.Now().UTC(),
		Findings: map[string]*visualhive.FindingLifecycle{fingerprint: {
			Repository: "owner/repo", RepositoryID: "123", Fingerprint: "finding", RepositoryFingerprint: fingerprint,
			Status: status, Branch: "hive/repair-proof", RepairCommitSHA: headSHA, PRNumber: 7, PRURL: "https://example.test/pr/7",
			MergeSHA: mergeSHA, LastPullRequestCheckReceipt: receipt,
		}},
		ReplayKeys: map[string]string{}, Outbox: []*visualhive.OutboxEntry{},
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visual-hive-lifecycle.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return lifecycle
}

func normalVisualVerifierSnapshot(t *testing.T, baseSHA, headSHA string) visualhive.PullRequestCheckReceiptSnapshot {
	t.Helper()
	identity := visualhive.PullRequestCheckReceiptIdentity{
		SchemaVersion: visualhive.PullRequestCheckReceiptSchema,
		Source: visualhive.PullRequestSourceBinding{
			SchemaVersion: visualhive.PullRequestSourceBindingSchema, Repository: "owner/repo", RepositoryID: "123", PullRequest: 7,
			Base: visualhive.PullRequestRevisionIdentity{Repository: "owner/repo", RepositoryID: "123", Ref: "main", SHA: baseSHA},
			Head: visualhive.PullRequestRevisionIdentity{Repository: "owner/repo", RepositoryID: "123", Ref: "hive/repair-proof", SHA: headSHA},
		},
		Workflow:  visualhive.PullRequestCheckWorkflowIdentity{Name: normalVisualPullRequestWorkflowName, Path: normalVisualPullRequestWorkflowPath, Event: "pull_request", AppID: "15368"},
		Producer:  visualhive.Producer{Name: "visual-hive", Version: "test", GitCommit: hivegithub.VisualHivePullRequestProducerCommit},
		Check:     visualhive.PullRequestCheckResultIdentity{State: "success", Conclusion: "success"},
		Authority: visualhive.PullRequestCheckAuthority{CheckEvidenceOnly: true}, ReplayKey: strings.Repeat("e", 64),
	}
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return visualhive.PullRequestCheckReceiptSnapshot{Identity: identity, ReceiptSHA256: hex.EncodeToString(digest[:])}
}
