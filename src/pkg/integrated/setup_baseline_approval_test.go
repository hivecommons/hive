package integrated

import (
	"strings"
	"testing"

	hivegithub "github.com/kubestellar/hive/pkg/github"
)

func setupBaselineApprovalFixture() setupBaselineApprovalBinding {
	return setupBaselineApprovalBinding{
		SchemaVersion: setupBaselineApprovalResultSchema, Repository: "owner/repo", RepositoryID: "123", CaptureRunID: 77, ArtifactID: 88,
		PRNumber: 9, PRURL: "https://example.test/owner/repo/pull/9", Branch: "hive/setup-baseline-123", HeadSHA: strings.Repeat("a", 40),
		BaseBranch: "main", BaseSHA: strings.Repeat("b", 40), DiffDigest: strings.Repeat("c", 64), CandidateDigest: strings.Repeat("d", 64),
		CaptureDigest:  strings.Repeat("f", 64),
		CandidatePaths: []string{".visual-hive/snapshots/linux/home.png"}, ActorID: 42,
	}
}

func TestSetupBaselineApprovalPlanBindsEveryExactAuthorityField(t *testing.T) {
	binding := setupBaselineApprovalFixture()
	digest, err := setupBaselineApprovalDigest(binding)
	if err != nil {
		t.Fatal(err)
	}
	options := ApproveSetupBaselineOptions{
		ExpectedRepositoryID: binding.RepositoryID, ExpectedCaptureRunID: binding.CaptureRunID, ExpectedArtifactID: binding.ArtifactID,
		ExpectedPRNumber: binding.PRNumber, ExpectedHeadSHA: binding.HeadSHA, ExpectedBaseSHA: binding.BaseSHA,
		ExpectedDiffDigest: binding.DiffDigest, ExpectedCandidateDigest: binding.CandidateDigest, ExpectedActorID: binding.ActorID, ExpectedPlanDigest: digest,
	}
	if err := validateSetupBaselineApprovalApply(options, binding, digest); err != nil {
		t.Fatalf("exact apply binding rejected: %v", err)
	}
	mutations := []func(*ApproveSetupBaselineOptions){
		func(value *ApproveSetupBaselineOptions) { value.ExpectedRepositoryID = "999" },
		func(value *ApproveSetupBaselineOptions) { value.ExpectedCaptureRunID++ },
		func(value *ApproveSetupBaselineOptions) { value.ExpectedArtifactID++ },
		func(value *ApproveSetupBaselineOptions) { value.ExpectedPRNumber++ },
		func(value *ApproveSetupBaselineOptions) { value.ExpectedHeadSHA = strings.Repeat("e", 40) },
		func(value *ApproveSetupBaselineOptions) { value.ExpectedBaseSHA = strings.Repeat("e", 40) },
		func(value *ApproveSetupBaselineOptions) { value.ExpectedDiffDigest = strings.Repeat("e", 64) },
		func(value *ApproveSetupBaselineOptions) { value.ExpectedCandidateDigest = strings.Repeat("e", 64) },
		func(value *ApproveSetupBaselineOptions) { value.ExpectedActorID++ },
		func(value *ApproveSetupBaselineOptions) { value.ExpectedPlanDigest = strings.Repeat("e", 64) },
	}
	for index, mutate := range mutations {
		changed := options
		mutate(&changed)
		if err := validateSetupBaselineApprovalApply(changed, binding, digest); err == nil {
			t.Fatalf("mutation %d retained setup baseline authority", index)
		}
	}
	changedBinding := binding
	changedBinding.CandidatePaths = append(changedBinding.CandidatePaths, ".visual-hive/snapshots/linux/extra.png")
	changedDigest, err := setupBaselineApprovalDigest(changedBinding)
	if err != nil || changedDigest == digest {
		t.Fatalf("complete candidate path set did not affect plan digest: digest=%s err=%v", changedDigest, err)
	}
	changedBinding = binding
	changedBinding.CaptureDigest = strings.Repeat("9", 64)
	changedDigest, err = setupBaselineApprovalDigest(changedBinding)
	if err != nil || changedDigest == digest {
		t.Fatalf("full hosted capture digest did not affect plan authority: digest=%s err=%v", changedDigest, err)
	}
}

func TestSetupBaselineSpecializedMergeGateRejectsPendingAndBypassSignals(t *testing.T) {
	gate := hivegithub.PullRequestGate{
		Open: true, MergeableKnown: true, Mergeable: true, BaselineChanged: true,
		BranchProtectionConfigured: true, BranchProtectionStrict: true, BranchProtectionAdminEnforced: true, BranchProtectionEnabled: true,
		VisualHiveVerdictGreen: true, VisualHiveProvenanceVerified: true, VisualHiveCheckState: "success",
		VisualHiveCheckRunID: 91, VisualHiveWorkflowRunID: 92, VisualHiveWorkflowPath: setupBaselinePRWorkflowPath,
		VisualHiveWorkflowEvent: "pull_request", VisualHiveRequired: true,
		RequiredCheckNames: []string{"visual-hive"}, RequiredCheckStates: []string{"success"},
	}
	if err := setupBaselineMergeGateReady(gate); err != nil {
		t.Fatalf("exact ready gate rejected: %v", err)
	}
	checks := []func(*hivegithub.PullRequestGate){
		func(value *hivegithub.PullRequestGate) { value.Hold = true },
		func(value *hivegithub.PullRequestGate) { value.Draft = true },
		func(value *hivegithub.PullRequestGate) { value.HumanReviewRequired = true },
		func(value *hivegithub.PullRequestGate) { value.BranchProtectionStrict = false },
		func(value *hivegithub.PullRequestGate) { value.BranchProtectionAdminEnforced = false },
		func(value *hivegithub.PullRequestGate) { value.VisualHiveProvenanceVerified = false },
		func(value *hivegithub.PullRequestGate) { value.VisualHiveCheckRunID = 0 },
		func(value *hivegithub.PullRequestGate) { value.RequiredCheckStates = []string{"pending"} },
		func(value *hivegithub.PullRequestGate) { value.RequiredCheckStates = nil },
	}
	for index, mutate := range checks {
		unsafe := gate
		unsafe.RequiredCheckStates = append([]string(nil), gate.RequiredCheckStates...)
		mutate(&unsafe)
		if err := setupBaselineMergeGateReady(unsafe); err == nil {
			t.Fatalf("unsafe specialized merge gate %d was accepted", index)
		}
	}
}

func TestSetupBaselineSpecializedMergeGateAllowsOnlyTrulyUnconfiguredBootstrap(t *testing.T) {
	bootstrap := hivegithub.PullRequestGate{
		Open: true, MergeableKnown: true, Mergeable: true, BaselineChanged: true,
		VisualHiveVerdictGreen: true, VisualHiveProvenanceVerified: true, VisualHiveCheckState: "success",
		VisualHiveCheckRunID: 91, VisualHiveWorkflowRunID: 92, VisualHiveWorkflowPath: setupBaselinePRWorkflowPath,
		VisualHiveWorkflowEvent: "pull_request",
	}
	if err := setupBaselineMergeGateReady(bootstrap); err != nil {
		t.Fatalf("truly unconfigured explicitly approved bootstrap rejected: %v", err)
	}
	held := bootstrap
	held.Hold = true
	if err := setupBaselineMergeGateReady(held); err == nil {
		t.Fatal("unreleased Hive hold was accepted at the final merge boundary")
	}
	if err := setupBaselineMergeGateReadyWithHold(held, true); err != nil {
		t.Fatalf("safe pre-release gate rejected the exact Hive hold: %v", err)
	}

	inconsistent := []hivegithub.PullRequestGate{bootstrap, bootstrap, bootstrap, bootstrap, bootstrap}
	inconsistent[0].BranchProtectionStrict = true
	inconsistent[1].BranchProtectionAdminEnforced = true
	inconsistent[2].BranchProtectionEnabled = true
	inconsistent[3].VisualHiveRequired = true
	inconsistent[4].RequiredCheckNames, inconsistent[4].RequiredCheckStates = []string{"organization-ci"}, []string{"success"}
	for index, gate := range inconsistent {
		if err := setupBaselineMergeGateReady(gate); err == nil {
			t.Fatalf("inconsistent unconfigured protection snapshot %d was accepted", index)
		}
	}
}

func TestSetupBaselineConfiguredProtectionRequiresStrictAdminAndEveryCheck(t *testing.T) {
	protected := hivegithub.PullRequestGate{
		Open: true, MergeableKnown: true, Mergeable: true, BaselineChanged: true,
		BranchProtectionConfigured: true, BranchProtectionStrict: true, BranchProtectionAdminEnforced: true, BranchProtectionEnabled: true,
		VisualHiveVerdictGreen: true, VisualHiveProvenanceVerified: true, VisualHiveCheckState: "success",
		VisualHiveCheckRunID: 91, VisualHiveWorkflowRunID: 92, VisualHiveWorkflowPath: setupBaselinePRWorkflowPath,
		VisualHiveWorkflowEvent: "pull_request", RequiredCheckNames: []string{"organization-ci", "security"},
		RequiredCheckStates: []string{"success", "success"},
	}
	if err := setupBaselineMergeGateReady(protected); err != nil {
		t.Fatalf("strict protected candidate with all checks green rejected: %v", err)
	}
	weak := []hivegithub.PullRequestGate{protected, protected, protected, protected, protected}
	weak[0].BranchProtectionStrict = false
	weak[1].BranchProtectionAdminEnforced = false
	weak[2].BranchProtectionEnabled = false
	weak[3].RequiredCheckStates = []string{"success", "failure"}
	weak[4].RequiredCheckStates = []string{"success"}
	for index, gate := range weak {
		if err := setupBaselineMergeGateReady(gate); err == nil {
			t.Fatalf("configured weak protection case %d was accepted", index)
		}
	}
}

func TestSetupBaselineSnapshotClassificationReachesSpecializedGateOnly(t *testing.T) {
	// InspectPullRequestGate owns path classification. Reproduce its public
	// result here to prove the specialized gate accepts the exact hosted path;
	// generic merge policy rejects this same BaselineChanged signal elsewhere.
	gate := hivegithub.PullRequestGate{
		Open: true, MergeableKnown: true, Mergeable: true,
		ChangedFiles: []string{".visual-hive/snapshots/linux/home.png"}, BaselineChanged: true,
		VisualHiveVerdictGreen: true, VisualHiveProvenanceVerified: true, VisualHiveCheckState: "success",
		VisualHiveCheckRunID: 91, VisualHiveWorkflowRunID: 92, VisualHiveWorkflowPath: setupBaselinePRWorkflowPath,
		VisualHiveWorkflowEvent: "pull_request",
	}
	if err := setupBaselineMergeGateReady(gate); err != nil {
		t.Fatalf("exact candidate-only hosted snapshot could not use the specialized bootstrap gate: %v", err)
	}
}
