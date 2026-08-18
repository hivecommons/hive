package visualhive

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/internal/visualhivepr"
)

func TestApplyPullRequestCheckUpdateReplayAndConflict(t *testing.T) {
	update := testPullRequestCheckUpdate(t)
	identity := update.Identity
	lifecycle, fingerprint := testPullRequestCheckLifecycle(t, identity)

	first, err := lifecycle.ApplySealedPullRequestCheckReceipt(fingerprint, testSealedPullRequestCheckUpdate(t, update))
	if err != nil {
		t.Fatal(err)
	}
	if first.Idempotent || first.ReceiptSHA256 != update.ReceiptSHA256 || first.ReplayKey != identity.ReplayKey {
		t.Fatalf("unexpected first check-update result: %+v", first)
	}
	state := lifecycle.Snapshot()
	finding := state.Findings[fingerprint]
	if finding == nil || finding.Status != StatusReady || finding.LastCheckSummary != identity.Check.Summary ||
		finding.LastPullRequestCheckReceipt == nil || finding.LastPullRequestCheckReceipt.ReceiptSHA256 != update.ReceiptSHA256 ||
		len(finding.LastCheckRuns) != 1 || finding.LastCheckRuns[0].ReceiptSHA256 != update.ReceiptSHA256 ||
		finding.LastCheckRuns[0].WorkflowRunAttempt != 2 || len(state.ReplayKeys) != 1 || len(state.Outbox) != 0 {
		t.Fatalf("external receipt snapshot was not persisted as exact check evidence: finding=%+v state=%+v", finding, state)
	}

	reloaded, err := NewLifecycleStore(lifecycle.dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reloaded.ApplySealedPullRequestCheckReceipt(fingerprint, testSealedPullRequestCheckUpdate(t, update))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Idempotent {
		t.Fatalf("exact retry after Ready was not a no-op: %+v", second)
	}
	lifecycle = reloaded

	conflict := update
	conflict.Identity.Check.Summary = "same transport identity, different externally sealed summary"
	conflict.Identity.Bundle.ManifestSHA256 = strings.Repeat("e", 64)
	conflict.ReceiptSHA256 = testExternalReceiptDigest(t, conflict.Identity)
	if conflict.Identity.ReplayKey != identity.ReplayKey || conflict.ReceiptSHA256 == update.ReceiptSHA256 {
		t.Fatal("conflict fixture did not reuse the replay identity with a different receipt digest")
	}
	if _, err := lifecycle.ApplySealedPullRequestCheckReceipt(fingerprint, testSealedPullRequestCheckUpdate(t, conflict)); err == nil || !strings.Contains(err.Error(), "different sealed receipt") {
		t.Fatalf("replay conflict was not rejected before Ready status handling: %v", err)
	}
	after := lifecycle.Snapshot().Findings[fingerprint]
	if after.LastPullRequestCheckReceipt == nil || after.LastPullRequestCheckReceipt.ReceiptSHA256 != update.ReceiptSHA256 || after.LastCheckSummary != identity.Check.Summary {
		t.Fatalf("conflicting check update changed lifecycle evidence: %+v", after)
	}
}

func TestPullRequestCheckUpdateRequiresCheckOnlyExternalReceiptShape(t *testing.T) {
	update := testPullRequestCheckUpdate(t)
	authority := update.Identity.Authority
	if update.Identity.SchemaVersion != PullRequestCheckReceiptSchema || !authority.CheckEvidenceOnly ||
		authority.ResolveIssues || authority.WriteBaselines || authority.MergePullRequest || authority.WriteTargetRepository {
		t.Fatalf("receipt authority is not explicitly check-only: %+v", authority)
	}

	badDigest := update
	badDigest.ReceiptSHA256 = "not-a-digest"
	if _, err := testApplyPullRequestCheckUpdate(t, badDigest); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("malformed external receipt digest was accepted: %v", err)
	}
	escalated := update
	escalated.Identity.Authority.ResolveIssues = true
	escalated.ReceiptSHA256 = testExternalReceiptDigest(t, escalated.Identity)
	if _, err := testApplyPullRequestCheckUpdate(t, escalated); err == nil || !strings.Contains(err.Error(), "beyond check evidence") {
		t.Fatalf("authority escalation was accepted: %v", err)
	}
}

func TestPullRequestCheckReceiptSnapshotCannotSelfAssertLiveProvenance(t *testing.T) {
	update := testPullRequestCheckUpdate(t)
	lifecycle, fingerprint := testPullRequestCheckLifecycle(t, update.Identity)
	if _, exists := reflect.TypeOf(lifecycle).MethodByName("ApplyVerifiedPullRequestCheckReceipt"); exists {
		t.Fatal("lifecycle still exposes the snapshot/provider receipt seam")
	}
	method, exists := reflect.TypeOf(lifecycle).MethodByName("ApplySealedPullRequestCheckReceipt")
	if !exists || method.Type.NumIn() != 3 || method.Type.In(2).PkgPath() != "github.com/kubestellar/hive/pkg/internal/visualhivepr" {
		t.Fatalf("lifecycle does not require the internal opaque receipt type: %+v", method.Type)
	}
	if _, err := lifecycle.ApplySealedPullRequestCheckReceipt(fingerprint, visualhivepr.SealedReceipt{}); err == nil || !strings.Contains(err.Error(), "seal is missing") {
		t.Fatalf("zero/internal receipt manufactured live provenance: %v", err)
	}
	state := lifecycle.Snapshot()
	if state.Findings[fingerprint].Status != StatusPROpen || len(state.ReplayKeys) != 0 || state.Findings[fingerprint].LastPullRequestCheckReceipt != nil {
		t.Fatalf("forged receipt changed lifecycle state: %+v", state)
	}
}

func TestApplyPullRequestCheckUpdateRequiresExactFinding(t *testing.T) {
	update := testPullRequestCheckUpdate(t)
	identity := update.Identity
	tests := []struct {
		name   string
		mutate func(*FindingLifecycle)
		want   string
	}{
		{name: "wrong repository", mutate: func(f *FindingLifecycle) { f.Repository = "owner/other" }, want: "exact finding"},
		{name: "wrong repository id", mutate: func(f *FindingLifecycle) { f.RepositoryID = "999" }, want: "exact finding"},
		{name: "wrong pull request", mutate: func(f *FindingLifecycle) { f.PRNumber++ }, want: "exact finding"},
		{name: "wrong repair sha", mutate: func(f *FindingLifecycle) { f.RepairCommitSHA = strings.Repeat("f", 40) }, want: "exact finding"},
		{name: "wrong repair branch", mutate: func(f *FindingLifecycle) { f.Branch = "other" }, want: "exact finding"},
		{name: "wrong status", mutate: func(f *FindingLifecycle) { f.Status = StatusReady }, want: "cannot apply"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lifecycle, fingerprint := testPullRequestCheckLifecycle(t, identity)
			test.mutate(lifecycle.state.Findings[fingerprint])
			if _, err := lifecycle.ApplySealedPullRequestCheckReceipt(fingerprint, testSealedPullRequestCheckUpdate(t, update)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("mismatched finding was accepted: %v", err)
			}
			state := lifecycle.Snapshot()
			if len(state.ReplayKeys) != 0 || len(state.Outbox) != 0 || state.Findings[fingerprint].LastPullRequestCheckReceipt != nil {
				t.Fatalf("rejected check update mutated lifecycle state: %+v", state)
			}
		})
	}
}

func TestApplyPullRequestCheckUpdateHasNoIssueBaselineMergeOrWriteAuthority(t *testing.T) {
	update := testPullRequestCheckUpdate(t)
	identity := update.Identity
	lifecycle, fingerprint := testPullRequestCheckLifecycle(t, identity)
	finding := lifecycle.state.Findings[fingerprint]
	finding.IssueNumber = 41
	finding.IssueURL = "https://github.test/owner/repo/issues/41"
	finding.BeadID = "bead-41"
	finding.LastBundleID = "authoritative-default-branch-bundle"
	finding.LastBundleDigest = strings.Repeat("e", 64)

	if _, err := lifecycle.ApplySealedPullRequestCheckReceipt(fingerprint, testSealedPullRequestCheckUpdate(t, update)); err != nil {
		t.Fatal(err)
	}
	after := lifecycle.Snapshot().Findings[fingerprint]
	if after.IssueNumber != 41 || after.IssueURL != finding.IssueURL || after.BeadID != "bead-41" ||
		after.LastBundleID != "authoritative-default-branch-bundle" || after.LastBundleDigest != strings.Repeat("e", 64) ||
		after.MergeSHA != "" || after.ResolvedAt != nil || after.ClosedAt != nil || len(lifecycle.Snapshot().Outbox) != 0 {
		t.Fatalf("check-only update mutated issue/bundle/merge/write authority state: %+v", after)
	}
	if err := lifecycle.MarkPRHeadUpdated(fingerprint, strings.Repeat("c", 40)); err != nil {
		t.Fatal(err)
	}
	after = lifecycle.Snapshot().Findings[fingerprint]
	if after.Status != StatusPROpen || after.LastPullRequestCheckReceipt != nil || after.LastCheckSummary != "" || len(after.LastCheckRuns) != 0 {
		t.Fatalf("stale PR receipt survived an exact head update: %+v", after)
	}
}

func TestReadyRepairCanRetireAfterIndependentDefaultBranchResolution(t *testing.T) {
	update := testPullRequestCheckUpdate(t)
	update.Identity.Source.Head.Ref = "hive/repair-ready"
	update.Identity.Source.Workflow.RunID = "88"
	update.Identity.Workflow.RunID = "88"
	update.Identity.Source.Workflow.SourceName = "visual-hive-pr-source-88-2"
	update.Identity.Source.Workflow.BundleName = "visual-hive-pr-bundle-88-2"
	update.Identity.SourceArtifact.Name = "visual-hive-pr-source-88-2"
	update.Identity.BundleArtifact.Name = "visual-hive-pr-bundle-88-2"
	update.Identity.ReplayKey = PullRequestCheckReplayKey(update.Identity)
	update.ReceiptSHA256 = testExternalReceiptDigest(t, update.Identity)
	lifecycle, fingerprint := testPullRequestCheckLifecycle(t, update.Identity)
	finding := lifecycle.state.Findings[fingerprint]
	finding.IssueNumber = 41
	finding.IssueURL = "https://github.test/owner/repo/issues/41"
	finding.PRURL = "https://github.test/owner/repo/pull/7"
	if _, err := lifecycle.ApplySealedPullRequestCheckReceipt(fingerprint, testSealedPullRequestCheckUpdate(t, update)); err != nil {
		t.Fatal(err)
	}
	receipt, err := json.Marshal(update.Identity)
	if err != nil {
		t.Fatal(err)
	}
	retired := RetiredRepair{
		PullRequestNumber: 7, PullRequestURL: finding.PRURL, Branch: update.Identity.Source.Head.Ref,
		HeadSHA: update.Identity.Source.Head.SHA, BaseBranch: update.Identity.Source.Base.Ref,
		BaseSHA: update.Identity.Source.Base.SHA, CurrentDefaultHeadSHA: strings.Repeat("c", 40),
		VerdictReceipt: receipt, VerdictReceiptSHA256: update.ReceiptSHA256,
	}
	if err := lifecycle.ValidateRepairRetirement(fingerprint, retired); err != nil {
		t.Fatalf("ready superseded repair failed read-only retirement validation: %v", err)
	}
	if err := lifecycle.RetireRepairForVerification(fingerprint, retired); err != nil {
		t.Fatal(err)
	}
	after, _ := lifecycle.Finding(fingerprint)
	if after.Status != StatusIssueOpen || after.PRNumber != 0 || after.Branch != "" || after.RepairCommitSHA != "" ||
		after.LastRetiredRepair == nil || after.LastRetiredRepair.VerdictReceiptSHA256 != update.ReceiptSHA256 {
		t.Fatalf("ready repair retirement lost issue or exact receipt history: %+v", after)
	}
}

func testApplyPullRequestCheckUpdate(t *testing.T, update PullRequestCheckReceiptSnapshot) (ApplyPullRequestCheckUpdateResult, error) {
	t.Helper()
	lifecycle, fingerprint := testPullRequestCheckLifecycle(t, update.Identity)
	identity, err := json.Marshal(update.Identity)
	if err != nil {
		return ApplyPullRequestCheckUpdateResult{}, err
	}
	sealed, err := visualhivepr.SealVerifiedReceipt(identity, update.ReceiptSHA256, update.Identity.ReplayKey)
	if err != nil {
		return ApplyPullRequestCheckUpdateResult{}, err
	}
	return lifecycle.ApplySealedPullRequestCheckReceipt(fingerprint, sealed)
}

func testSealedPullRequestCheckUpdate(t *testing.T, update PullRequestCheckReceiptSnapshot) visualhivepr.SealedReceipt {
	t.Helper()
	identity, err := json.Marshal(update.Identity)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := visualhivepr.SealVerifiedReceipt(identity, update.ReceiptSHA256, update.Identity.ReplayKey)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func testPullRequestCheckUpdate(t *testing.T) PullRequestCheckReceiptSnapshot {
	t.Helper()
	digest := func(value string) string { return strings.Repeat(value, 64) }
	file := func(path, value string) PullRequestFileIdentity {
		return PullRequestFileIdentity{Path: path, SHA256: digest(value), Bytes: 100}
	}
	identity := PullRequestCheckReceiptIdentity{
		SchemaVersion: PullRequestCheckReceiptSchema,
		Source: PullRequestSourceBinding{
			SchemaVersion: PullRequestSourceBindingSchema,
			Repository:    "owner/repo", RepositoryID: "123", PullRequest: 7,
			Base: PullRequestRevisionIdentity{Repository: "owner/repo", RepositoryID: "123", Ref: "main", SHA: strings.Repeat("a", 40)},
			Head: PullRequestRevisionIdentity{Repository: "fork/repo", RepositoryID: "456", Ref: "repair", SHA: strings.Repeat("b", 40)},
			Workflow: PullRequestWorkflowIdentity{
				Name: "Visual Hive PR", Path: ".github/workflows/visual-hive-pr.yml", Event: "pull_request", RunID: "77", RunAttempt: "2",
				Definition: file(PullRequestWorkflowPath, "c"), ProducerCommit: strings.Repeat("d", 40),
				SourceName: "visual-hive-pr-source-77-2", BundleName: "visual-hive-pr-bundle-77-2",
			},
			Plan: file(PullRequestPlanPath, "1"), Report: file(PullRequestReportPath, "2"), Config: file(PullRequestConfigPath, "3"),
			ChangedFiles:  file(PullRequestChangedFilesPath, "4"),
			PlanContracts: file(PullRequestPlanContractsPath, "b"), EvaluationScopes: file(PullRequestEvaluationScopesPath, "c"),
			RunnerAttestation: file(PullRequestRunnerAttestationPath, "d"),
			Runtime: PullRequestRuntimeIdentity{
				Receipt:         file(PullRequestRuntimePath, "5"),
				GeneratedSpec:   file(".visual-hive/generated/visual-hive.generated.spec.cjs", "0"),
				GeneratedConfig: file(".visual-hive/generated/visual-hive.generated.config.cjs", "e"),
				StableSHA256:    digest("6"), ExecutionBindingSHA256: digest("7"),
			},
			Baselines: PullRequestBaselineIdentity{
				SHA256: digest("8"), Files: []PullRequestFileIdentity{
					file(".visual-hive/proof/pr/baselines/desktop.png", "9"),
					file(".visual-hive/proof/pr/baselines/mobile.png", "a"),
				},
			},
		},
		Workflow: PullRequestCheckWorkflowIdentity{
			ID: "12", Name: "Visual Hive PR", Path: ".github/workflows/visual-hive-pr.yml", Event: "pull_request", RunID: "77", RunAttempt: "2",
			CheckSuiteID: "501", JobID: "701", CheckRunID: "801", AppID: "15368",
			Definition: file(PullRequestWorkflowPath, "c"),
		},
		SourceArtifact: PullRequestCheckArtifactIdentity{ID: "98", Name: "visual-hive-pr-source-77-2"},
		BundleArtifact: PullRequestCheckArtifactIdentity{ID: "99", Name: "visual-hive-pr-bundle-77-2"},
		Producer:       Producer{Name: "visual-hive", Version: "0.4.0", GitCommit: strings.Repeat("d", 40)},
		Bundle: PullRequestCheckBundleIdentity{
			SchemaVersion: ManifestSchemaV3, BundleID: "pr-bundle-77", OverallSHA256: digest("b"), ManifestSHA256: digest("c"),
			ArtifactIndex: ArtifactIndexBinding{
				Path: "files/.visual-hive/artifacts-index.json", SourcePath: ".visual-hive/artifacts-index.json", SHA256: digest("d"),
				SchemaVersion: artifactIndexSchemaVersion, ContentAddressed: true, Complete: true, ArtifactCount: 11, TotalBytes: 1100,
			},
		},
		Check:     PullRequestCheckResultIdentity{State: "success", Conclusion: "success", Summary: "Visual Hive exact PR rerun passed", RunURL: "https://github.test/owner/repo/actions/runs/77"},
		Authority: PullRequestCheckAuthority{CheckEvidenceOnly: true},
	}
	identity.ReplayKey = PullRequestCheckReplayKey(identity)
	receipt := PullRequestCheckReceiptSnapshot{Identity: identity, ReceiptSHA256: testExternalReceiptDigest(t, identity)}
	return receipt
}

func testExternalReceiptDigest(t *testing.T, identity PullRequestCheckReceiptIdentity) string {
	t.Helper()
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func testPullRequestCheckLifecycle(t *testing.T, identity PullRequestCheckReceiptIdentity) (*LifecycleStore, string) {
	t.Helper()
	lifecycle, err := NewLifecycleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("f", 64)
	lifecycle.state.Findings[fingerprint] = &FindingLifecycle{
		Repository: identity.Source.Repository, RepositoryID: identity.Source.RepositoryID,
		Fingerprint: "finding", RepositoryFingerprint: fingerprint, Status: StatusPROpen,
		Branch: identity.Source.Head.Ref, RepairCommitSHA: identity.Source.Head.SHA,
		PRNumber: identity.Source.PullRequest,
	}
	return lifecycle, fingerprint
}
