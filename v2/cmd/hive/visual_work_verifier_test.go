package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
	"github.com/kubestellar/hive/v2/pkg/visualhive/normalservice"
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
