package visualhive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

func TestRepositoryTestBundlePublishesFailureAndClosesOnlyFromCompletePostMergePlan(t *testing.T) {
	root := t.TempDir()
	writeRepositoryTestLedger(t, root, "1\tnpm --prefix dashboard run test:all\n0\tpython -m pytest -q\n", "1\n")
	failedParent := repositoryTestParent("run-1", "artifact-1", strings.Repeat("a", 40), time.Date(2026, 7, 12, 14, 0, 0, 0, time.UTC))
	failed, err := buildRepositoryTestBundle(t, failedParent, root)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Validation.Authoritative || len(failed.Manifest.Observations) != 1 || failed.Manifest.Observations[0].State != "present" || failed.Manifest.Observations[0].IssueKind != RepositoryTestFailureKind {
		t.Fatalf("failed repository plan was not a bounded present finding: %+v", failed)
	}
	fingerprint := failed.Manifest.Observations[0].RepositoryFingerprint
	contract := failed.Manifest.Observations[0].AffectedContracts[0]
	if failed.Manifest.Scan.AuthoritativeForResolution || len(failed.Manifest.Scan.EvaluatedContracts) != 1 || failed.Manifest.Scan.EvaluatedContracts[0] != contract {
		t.Fatalf("incomplete plan claimed resolution authority: %+v", failed.Manifest.Scan)
	}

	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore, err := beads.NewStore(filepath.Join(root, "beads"))
	if err != nil {
		t.Fatal(err)
	}
	apply, err := lifecycle.ApplyBundle(failed, beadStore, ApplyLifecycleOptions{TargetRef: "main", CurrentTargetCommitSHA: failedParent.Manifest.Source.CommitSHA, MaxActiveIssues: 5, PreferRepairable: true})
	if err != nil || apply.Created != 1 || apply.OutboxCreated != 1 {
		t.Fatalf("failed plan did not create one finding: result=%+v err=%v", apply, err)
	}
	if err := lifecycle.MarkIssueOpened(fingerprint, 31, "https://example.test/issues/31"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkRepairStarted(fingerprint, "hive/repair-repository-test"); err != nil {
		t.Fatal(err)
	}
	repairSHA := strings.Repeat("b", 40)
	if err := lifecycle.MarkPROpen(fingerprint, repairSHA, 32, "https://example.test/pull/32"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkChecks(fingerprint, repairSHA, true); err != nil {
		t.Fatal(err)
	}
	mergeSHA := strings.Repeat("c", 40)
	if err := lifecycle.MarkMerged(fingerprint, mergeSHA); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPostMergeVerifying(fingerprint, "run-2", "https://example.test/actions/run-2"); err != nil {
		t.Fatal(err)
	}

	writeRepositoryTestLedger(t, root, "0\tnpm --prefix dashboard run test:all\n0\tpython -m pytest -q\n", "0\n")
	successParent := repositoryTestParent("run-2", "artifact-2", mergeSHA, time.Date(2026, 7, 12, 14, 5, 0, 0, time.UTC))
	success, err := buildRepositoryTestBundle(t, successParent, root)
	if err != nil {
		t.Fatal(err)
	}
	if !success.Validation.Authoritative || len(success.Manifest.Observations) != 2 {
		t.Fatalf("green repository plan was not exhaustive: %+v", success)
	}
	var matching *Observation
	for index := range success.Manifest.Observations {
		if success.Manifest.Observations[index].RepositoryFingerprint == fingerprint {
			matching = &success.Manifest.Observations[index]
		}
	}
	if matching == nil || matching.State != "absent" || matching.AffectedContracts[0] != contract {
		t.Fatalf("green plan did not preserve exact command identity: %+v", matching)
	}
	resolved, err := lifecycle.ApplyBundle(success, beadStore, ApplyLifecycleOptions{
		TargetRef: "main", CurrentTargetCommitSHA: mergeSHA,
		VerificationRunID: "run-2", VerificationURL: "https://example.test/actions/run-2", VerificationCommitSHA: mergeSHA,
	})
	if err != nil || resolved.Resolved != 1 || resolved.OutboxCreated != 1 {
		t.Fatalf("complete post-merge repository plan did not resolve exact finding: result=%+v err=%v", resolved, err)
	}
	got, exists := lifecycle.Finding(fingerprint)
	if !exists || got.Status != StatusResolved || got.ValidationRunID != "run-2" {
		t.Fatalf("repository finding lacks post-merge resolution proof: %+v", got)
	}
}

func TestRepositoryTestBundleIdentityIsStableAcrossRuns(t *testing.T) {
	root := t.TempDir()
	writeRepositoryTestLedger(t, root, "1\tnpm test\n", "1\n")
	firstParent := repositoryTestParent("run-a", "artifact-a", strings.Repeat("a", 40), time.Now().UTC())
	firstParent.Manifest.Source.Trusted = false // producer trust is advisory; independent validation is authoritative
	first, err := buildRepositoryTestBundle(t, firstParent, root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildRepositoryTestBundle(t, repositoryTestParent("run-b", "artifact-b", strings.Repeat("b", 40), time.Now().UTC().Add(time.Minute)), root)
	if err != nil {
		t.Fatal(err)
	}
	if first.Manifest.Observations[0].RepositoryFingerprint != second.Manifest.Observations[0].RepositoryFingerprint || first.Manifest.Observations[0].RootCauseKey != second.Manifest.Observations[0].RootCauseKey {
		t.Fatal("same repository command changed lifecycle identity across workflow runs")
	}
	if first.Manifest.ReplayProtection.Key == second.Manifest.ReplayProtection.Key {
		t.Fatal("different source runs reused a derived replay key")
	}
}

func TestRepositoryTestBundleRequiresVerifiedCompleteSourceParent(t *testing.T) {
	result, err := NewRepositoryTestResult("npm test", 0)
	if err != nil {
		t.Fatal(err)
	}
	results := []RepositoryTestResult{result}
	_, _, evidenceDigest, err := EncodeRepositoryTestEvidence(results, 0)
	if err != nil {
		t.Fatal(err)
	}

	localManifest, localSource := writeTestV3Bundle(t, nil)
	local, err := ValidateBundle(localManifest, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.VerifySourceArtifact(localSource); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildRepositoryTestBundle(local, results, 0, evidenceDigest); err == nil || !strings.Contains(err.Error(), "independently verified complete-source") {
		t.Fatalf("local v3 parent conferred derived repository-test authority: %v", err)
	}

	verifiedManifest, verifiedSource := writeTestV3Bundle(t, nil)
	verified, err := ValidateBundle(verifiedManifest, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3,
		VerifiedProvenance: true, ExpectedProducerGitCommit: testV3ProducerCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildRepositoryTestBundle(verified, results, 0, evidenceDigest); err == nil || !strings.Contains(err.Error(), "independently verified complete-source") {
		t.Fatalf("pre-source v3 parent conferred derived repository-test authority: %v", err)
	}
	writeTestData(t, filepath.Join(verifiedSource, sourceArtifactExtractionMarker), []byte("99\n"))
	if err := verified.VerifySourceArtifact(verifiedSource); err != nil {
		t.Fatal(err)
	}
	derived, err := BuildRepositoryTestBundle(verified, results, 0, evidenceDigest)
	if err != nil {
		t.Fatalf("verified complete-source parent was rejected: %v", err)
	}
	if !derived.Validation.Trusted || !derived.Validation.Authoritative || len(derived.Manifest.Observations) != 1 || derived.Manifest.Observations[0].State != "absent" {
		t.Fatalf("verified complete-source parent did not yield the expected derived authority: %+v", derived)
	}
}

func TestRepositoryTestEvidenceRejectsMismatchDuplicatesAndUnsafeCommands(t *testing.T) {
	cases := []struct {
		name, ledger, summary, want string
	}{
		{name: "summary mismatch", ledger: "1\tnpm test\n", summary: "0\n", want: "does not match"},
		{name: "duplicate", ledger: "1\tnpm test\n0\tnpm test\n", summary: "1\n", want: "repeats command"},
		{name: "unsafe shell", ledger: "1\tnpm test && curl example.test\n", summary: "1\n", want: "unsafe command"},
		{name: "credential", ledger: "1\\tnpm test " + "github_" + "pat_" + strings.Repeat("a", 24) + "\\n", summary: "1\\n", want: "unsafe"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeRepositoryTestLedger(t, root, test.ledger, test.summary)
			_, _, err := LoadRepositoryTestResults(root)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid evidence was accepted: %v", err)
			}
		})
	}
}

func repositoryTestParent(runID, artifactID, commit string, generatedAt time.Time) *ValidatedBundle {
	replay := "visual-parent:" + runID + ":" + artifactID
	return &ValidatedBundle{
		Manifest: Manifest{
			SchemaVersion: ManifestSchemaV3, BundleID: "parent-" + runID, GeneratedAt: generatedAt, ExpiresAt: generatedAt.Add(24 * time.Hour),
			Producer: Producer{Name: "visual-hive", Version: "0.2.4", GitCommit: strings.Repeat("d", 40)},
			Source: Source{
				Repository: "owner/repo", RepositoryID: "123", Ref: "main", CommitSHA: commit, Event: "workflow_dispatch",
				WorkflowName: "Hive Visual Hive Production", WorkflowRunID: runID, WorkflowArtifactID: artifactID, Conclusion: "success", Trusted: true,
			},
			Project: "proof", Mode: "full", Verdict: "passed", ACMMRequest: 6,
			Scan:             Scan{Scope: "full", ToolRegistryVersion: "visual-hive.tools.v1"},
			ReplayProtection: ReplayProtection{Nonce: "parent", Key: replay},
		},
		Validation:         Validation{SchemaVersion: ManifestSchemaV3, Status: "valid", Trusted: true},
		provenanceVerified: true,
		sourceVerified:     true,
	}
}

func writeRepositoryTestLedger(t *testing.T, root, ledger, summary string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "repository-tests.tsv"), []byte(ledger), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "repository-test-exit-code.txt"), []byte(summary), 0o600); err != nil {
		t.Fatal(err)
	}
}

func buildRepositoryTestBundle(t *testing.T, parent *ValidatedBundle, root string) (*ValidatedBundle, error) {
	t.Helper()
	results, overall, err := LoadRepositoryTestResults(root)
	if err != nil {
		return nil, err
	}
	_, _, digest, err := EncodeRepositoryTestEvidence(results, overall)
	if err != nil {
		return nil, err
	}
	return BuildRepositoryTestBundle(parent, results, overall, digest)
}
