package integrated

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/repair"
	"github.com/kubestellar/hive/pkg/visualhive"
)

func TestSpecialistResolutionUsesOnlyVerifiedFindingFields(t *testing.T) {
	grounded := visualhive.FindingLifecycle{
		IssueKind: "selector_contract_failure", OwningAgentHint: "visual-hive/test-maintainer",
		AffectedContracts: []string{"checkout-error"}, ValidationCommand: "npm run vh:run",
	}
	if got := specialistResolutionForFinding(grounded); !got.DispatchAllowed || got.Role != visualhive.SpecialistQuality {
		t.Fatalf("grounded selector finding route = %+v", got)
	}
	grounded.ValidationCommand = ""
	grounded.Body = "security architecture quality ci-maintainer"
	if got := specialistResolutionForFinding(grounded); !got.DispatchAllowed || got.Role != visualhive.SpecialistScanner {
		t.Fatalf("ungrounded selector finding route = %+v", got)
	}
	grounded.IssueKind, grounded.OwningAgentHint = "stale_baseline", "hive/quality"
	if got := specialistResolutionForFinding(grounded); got.DispatchAllowed || got.Role != "" {
		t.Fatalf("baseline finding route = %+v, want human hold", got)
	}
}

func TestNextSpecialistAttemptBindingUsesDurableWorkerAnchor(t *testing.T) {
	state, err := repair.NewStore(filepath.Join(t.TempDir(), "repair"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 13, 0, 0, 0, time.UTC)
	finding := visualhive.FindingLifecycle{RepositoryFingerprint: strings.Repeat("f", 64), RepairAttempts: 0, Recurrences: 0}

	first, err := nextSpecialistAttemptBinding(state, finding, now)
	if err != nil || first.Attempt != 1 || !first.StartedAt.Equal(now) {
		t.Fatalf("new attempt binding = %+v err=%v", first, err)
	}
	if err := state.Put(repair.Attempt{
		Repository: "owner/repo", RepositoryFingerprint: finding.RepositoryFingerprint, Recurrence: 0, Attempt: 1,
		Branch: "hive/repair-one", Worktree: "worktree", Stage: repair.StagePrepared, Provider: "hive-specialist-quality", StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	resumed, err := nextSpecialistAttemptBinding(state, finding, now.Add(time.Hour))
	if err != nil || resumed != first {
		t.Fatalf("resumed attempt binding = %+v err=%v, want %+v", resumed, err, first)
	}
	existing, _ := state.Get(finding.RepositoryFingerprint)
	existing.Stage = repair.StageNoChange
	if err := state.Put(existing); err != nil {
		t.Fatal(err)
	}
	next, err := nextSpecialistAttemptBinding(state, finding, now.Add(2*time.Hour))
	if err != nil || next.Attempt != 2 || !next.StartedAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("next attempt binding = %+v err=%v", next, err)
	}
}

func TestNextSpecialistAttemptBindingUsesRetryAuthorizationForDeadlineOnly(t *testing.T) {
	state, err := repair.NewStore(filepath.Join(t.TempDir(), "repair"))
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC().Add(-time.Hour)
	fingerprint := strings.Repeat("e", 64)
	attempt := repair.Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Recurrence: 0, Attempt: 2,
		Branch: "hive/repair-two", Worktree: "worktree", Stage: repair.StageFailed, Provider: "hive-specialist-quality",
		LastFailureClass: repair.FailureInfrastructure, LastFailureID: "expired-specialist-window", LastFailure: "transport unavailable",
		ResumeStage: repair.StagePrepared, StartedAt: startedAt,
	}
	if err := state.Put(attempt); err != nil {
		t.Fatal(err)
	}
	resumed, err := state.ResumeRetry(repair.RetryRequest{
		RepositoryFingerprint: fingerprint, ExpectedRecurrence: 0, ExpectedAttempt: 2,
		ExpectedFailureClass: repair.FailureInfrastructure, ExpectedFailureID: attempt.LastFailureID,
		Actor: "operator", Reason: "specialist transport restored",
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := nextSpecialistAttemptBinding(state, visualhive.FindingLifecycle{RepositoryFingerprint: fingerprint, Recurrences: 0, RepairAttempts: 1}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if binding.Attempt != 2 || !binding.StartedAt.Equal(startedAt) || !binding.DeadlineAnchor.Equal(resumed.RetryAuthorizedAt) || !binding.DeadlineAnchor.After(binding.StartedAt) {
		t.Fatalf("retry binding = %+v, retry=%+v", binding, resumed)
	}
	replayed, err := nextSpecialistAttemptBinding(state, visualhive.FindingLifecycle{RepositoryFingerprint: fingerprint, Recurrences: 0, RepairAttempts: 1}, time.Now().UTC().Add(time.Hour))
	if err != nil || replayed != binding {
		t.Fatalf("retry binding was not durable: %+v err=%v, want %+v", replayed, err, binding)
	}
}

func TestNextSpecialistAttemptBindingReservesOneExactRevision(t *testing.T) {
	state, err := repair.NewStore(filepath.Join(t.TempDir(), "repair"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 14, 0, 0, 0, time.UTC)
	head := strings.Repeat("b", 40)
	fingerprint := strings.Repeat("f", 64)
	if err := state.Put(repair.Attempt{
		Repository: "owner/repo", RepositoryFingerprint: fingerprint, Recurrence: 0, Attempt: 1,
		Branch: "hive/repair-one", Worktree: "worktree", Stage: repair.StagePROpen, Provider: "hive-specialist-quality",
		AttemptCounted: true, LifecycleStarted: true, LifecyclePROpen: true, CommitSHA: head, PRNumber: 7, StartedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	finding := visualhive.FindingLifecycle{
		RepositoryFingerprint: fingerprint, Status: visualhive.StatusNeedsRevision, RepairAttempts: 1, Recurrences: 0,
		Branch: "hive/repair-one", RepairCommitSHA: head, PRNumber: 7,
	}

	first, err := nextSpecialistAttemptBinding(state, finding, now)
	if err != nil || first.Attempt != 2 || !first.StartedAt.Equal(now) {
		t.Fatalf("revision reservation = %+v err=%v", first, err)
	}
	replayed, err := nextSpecialistAttemptBinding(state, finding, now.Add(time.Hour))
	if err != nil || replayed != first {
		t.Fatalf("replayed revision reservation = %+v err=%v, want %+v", replayed, err, first)
	}
	persisted, _ := state.Get(fingerprint)
	if persisted.SpecialistRevisionAttempt != 2 || !persisted.SpecialistRevisionStartedAt.Equal(now) || persisted.SpecialistRevisionBaseSHA != head ||
		persisted.SpecialistRevisionBranch != finding.Branch || persisted.SpecialistRevisionPRNumber != finding.PRNumber {
		t.Fatalf("revision reservation was not durably exact: %+v", persisted)
	}
	finding.RepairCommitSHA = strings.Repeat("c", 40)
	if _, err := nextSpecialistAttemptBinding(state, finding, now.Add(2*time.Hour)); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("changed PR head reused the specialist revision reservation: %v", err)
	}
}

func TestSpecialistEvidenceIdentityRejectsUnsealedArtifact(t *testing.T) {
	bundleDigest := strings.Repeat("a", 64)
	verified := hivegithub.VerifiedVisualHiveArtifact{
		RepositoryID: "123", WorkflowRunID: "42", WorkflowRunAttempt: "2", ArtifactID: "99", SourceArtifactID: "98",
		ArtifactName: "visual-hive-bundle", CommitSHA: strings.Repeat("b", 40), WorkflowName: "Visual Hive Scheduled",
		WorkflowRunName: "Visual Hive Scheduled [correlation]", WorkflowPath: ".github/workflows/visual-hive.yml",
		BundleSchemaVersion: visualhive.ManifestSchemaV3, BundleSHA256: bundleDigest, ManifestSHA256: strings.Repeat("c", 64), ArtifactIndexSHA256: strings.Repeat("d", 64),
	}
	finding := visualhive.FindingLifecycle{LastBundleDigest: bundleDigest, LastWorkflowRunID: "42"}

	if _, err := specialistEvidenceIdentity(verified, finding); err == nil || !strings.Contains(err.Error(), "sealed FetchAndVerifyVisualHiveBundle") {
		t.Fatalf("unsealed verified-artifact fields were accepted: %v", err)
	}
}

func TestSpecialistRevisionEvidenceIdentityBindsExactPRHead(t *testing.T) {
	head := strings.Repeat("b", 40)
	verified := hivegithub.VerifiedPullRequestArtifact{
		RepositoryID: "123", PullRequestNumber: 7, WorkflowRunID: 77, WorkflowRunAttempt: 2,
		ArtifactID: 88, ArtifactName: "visual-hive-pr", CommitSHA: head, HeadBranch: "hive/repair-one",
		WorkflowName: "Visual Hive PR", WorkflowPath: ".github/workflows/visual-hive-pr.yml", Conclusion: "failure",
		ReviewSchemaVersion: visualhive.ReviewEvidenceSchemaVersion, ArtifactIndexSHA256: strings.Repeat("d", 64),
	}
	finding := visualhive.FindingLifecycle{
		Repository: "owner/repo", RepositoryID: "123", Status: visualhive.StatusNeedsRevision,
		PRNumber: 7, Branch: "hive/repair-one", RepairCommitSHA: head,
	}

	identity, err := specialistRevisionEvidenceIdentity(verified, finding)
	if err != nil {
		t.Fatal(err)
	}
	if identity.BundleSchemaVersion != visualhive.ReviewEvidenceSchemaVersion || identity.WorkflowRunID != 77 ||
		identity.WorkflowRunAttempt != 2 || identity.WorkflowRunHeadSHA != head || identity.ArtifactID != 88 ||
		identity.BundleSHA256 != verified.ArtifactIndexSHA256 || identity.ArtifactSHA256 != verified.ArtifactIndexSHA256 || len(identity.VerificationReceiptSHA256) != 64 {
		t.Fatalf("specialist revision evidence identity = %+v", identity)
	}

	for name, mutate := range map[string]func(*hivegithub.VerifiedPullRequestArtifact, *visualhive.FindingLifecycle){
		"repository": func(v *hivegithub.VerifiedPullRequestArtifact, _ *visualhive.FindingLifecycle) {
			v.RepositoryID = "456"
		},
		"pull request": func(v *hivegithub.VerifiedPullRequestArtifact, _ *visualhive.FindingLifecycle) {
			v.PullRequestNumber = 8
		},
		"head": func(v *hivegithub.VerifiedPullRequestArtifact, _ *visualhive.FindingLifecycle) {
			v.CommitSHA = strings.Repeat("c", 40)
		},
		"branch": func(v *hivegithub.VerifiedPullRequestArtifact, _ *visualhive.FindingLifecycle) {
			v.HeadBranch = "hive/other"
		},
		"workflow": func(v *hivegithub.VerifiedPullRequestArtifact, _ *visualhive.FindingLifecycle) {
			v.WorkflowName = "Other"
		},
		"conclusion": func(v *hivegithub.VerifiedPullRequestArtifact, _ *visualhive.FindingLifecycle) {
			v.Conclusion = "success"
		},
		"digest": func(v *hivegithub.VerifiedPullRequestArtifact, _ *visualhive.FindingLifecycle) {
			v.ArtifactIndexSHA256 = "bad"
		},
		"status": func(_ *hivegithub.VerifiedPullRequestArtifact, f *visualhive.FindingLifecycle) {
			f.Status = visualhive.StatusReady
		},
	} {
		t.Run(name, func(t *testing.T) {
			changedVerified, changedFinding := verified, finding
			mutate(&changedVerified, &changedFinding)
			if _, err := specialistRevisionEvidenceIdentity(changedVerified, changedFinding); err == nil {
				t.Fatalf("%s mismatch was accepted", name)
			}
		})
	}
}

func TestFailedVisualHiveWorkflowRunIDRequiresOneProvenanceVerifiedFailure(t *testing.T) {
	finding := visualhive.FindingLifecycle{LastCheckRuns: []visualhive.CheckEvidence{{
		Name: "visual-hive", State: "failure", ProvenanceVerified: true, WorkflowRunID: 77,
		WorkflowPath: ".github/workflows/visual-hive-pr.yml", WorkflowEvent: "pull_request",
	}}}
	if runID, err := failedVisualHiveWorkflowRunID(finding); err != nil || runID != 77 {
		t.Fatalf("failed Visual Hive run = %d err=%v", runID, err)
	}
	finding.LastCheckRuns[0].ProvenanceVerified = false
	if _, err := failedVisualHiveWorkflowRunID(finding); err == nil {
		t.Fatal("unverified Visual Hive check was accepted")
	}
	finding.LastCheckRuns[0].ProvenanceVerified = true
	finding.LastCheckRuns = append(finding.LastCheckRuns, visualhive.CheckEvidence{
		Name: "visual-hive", State: "failure", ProvenanceVerified: true, WorkflowRunID: 78,
		WorkflowPath: ".github/workflows/visual-hive-pr.yml", WorkflowEvent: "pull_request",
	})
	if _, err := failedVisualHiveWorkflowRunID(finding); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("ambiguous Visual Hive workflow runs were accepted: %v", err)
	}
}
