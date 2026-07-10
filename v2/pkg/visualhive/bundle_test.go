package visualhive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

func TestValidateAndImportBundle(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeTestBundle(t, root, false)
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true})
	if err != nil {
		t.Fatal(err)
	}
	store, err := beads.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := bundle.Import(store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := bundle.Import(store)
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 1 || second.Skipped != 1 || store.Count() != 1 {
		t.Fatalf("import was not idempotent: first=%+v second=%+v count=%d", first, second, store.Count())
	}
}

func TestValidateBundleRejectsTampering(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeTestBundle(t, root, false)
	beadsPath := filepath.Join(root, "files", ".visual-hive", "hive", "beads.json")
	if err := os.WriteFile(beadsPath, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBundle(manifestPath, ValidationOptions{MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("expected tamper rejection, got %v", err)
	}
}

func TestValidateBundleRejectsUntrustedByDefault(t *testing.T) {
	manifestPath := writeTestBundle(t, t.TempDir(), false)
	if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3}); err == nil || !strings.Contains(err.Error(), "independently verified") {
		t.Fatalf("expected provenance rejection, got %v", err)
	}
}

func TestValidateBundleAcceptsIndependentProvenanceWithoutProducerTrustClaim(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeTestBundle(t, root, false)
	manifest := readManifest(t, manifestPath)
	manifest.Source.Event = "workflow_dispatch"
	manifest.Source.Conclusion = "success"
	manifest.Source.WorkflowRunID = "42"
	manifest.Source.WorkflowArtifactID = "99"
	manifest.Provenance.Kind = "github-actions"
	manifest.Provenance.AttestationRequired = true
	manifest.ReplayProtection.Key = replayKey(manifest)
	fileLines := []string{fmt.Sprintf("file\x00%s\x00%s\x00%d", manifest.Files[0].Path, manifest.Files[0].SHA256, manifest.Files[0].Size)}
	manifest.OverallDigest = digestBundleContent(manifest, fileLines)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	writeManifest(t, manifestPath, manifest)

	validated, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3,
		VerifiedProvenance: true, ExpectedRepository: "owner/repo", ExpectedWorkflowRunID: "42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !validated.Validation.Trusted {
		t.Fatal("independently verified bundle must be trusted")
	}
}

func TestValidateBundleRejectsAbsentObservationFromPartialScan(t *testing.T) {
	manifestPath := writeTestBundle(t, t.TempDir(), false)
	manifest := readManifest(t, manifestPath)
	manifest.Scan.Scope = "partial"
	manifest.Scan.AuthoritativeForResolution = false
	manifest.Observations[0].State = "absent"
	writeManifest(t, manifestPath, manifest)
	if _, err := ValidateBundle(manifestPath, ValidationOptions{MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "authoritative") {
		t.Fatalf("expected unsafe resolution rejection, got %v", err)
	}
}

func TestValidateBundleProducedByVisualHive(t *testing.T) {
	manifestPath := os.Getenv("VISUAL_HIVE_TEST_BUNDLE")
	if manifestPath == "" {
		t.Skip("VISUAL_HIVE_TEST_BUNDLE is not set")
	}
	if _, err := ValidateBundle(manifestPath, ValidationOptions{MaxACMM: 6, AllowLocal: true}); err != nil {
		t.Fatalf("Visual Hive producer and Hive consumer contract diverged: %v", err)
	}
}

func writeTestBundle(t *testing.T, root string, trusted bool) string {
	t.Helper()
	relative := "files/.visual-hive/hive/beads.json"
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	projection := []Projection{{ID: "vh-1", Title: "Fix contrast", Type: "bug", Status: "open", Priority: 1, Actor: "quality", ExternalRef: "visual-hive:demo:1", Metadata: map[string]string{}, Notes: "deterministic failure", CreatedAt: "2026-07-09T11:00:00Z", UpdatedAt: "2026-07-09T11:00:00Z", DependsOn: []string{}}}
	data, _ := json.Marshal(projection)
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fileDigest := digest(data)
	fileLines := []string{fmt.Sprintf("file\x00%s\x00%s\x00%d", relative, fileDigest, len(data))}
	manifest := Manifest{
		SchemaVersion: ManifestSchema, BundleID: "test-bundle", GeneratedAt: time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC),
		Producer: Producer{Name: "visual-hive", Version: "0.2.0", GitCommit: "abc123"}, Source: Source{Repository: "owner/repo", Ref: "refs/heads/main", CommitSHA: "abc123", Event: "local", Conclusion: "local", Trusted: trusted},
		Project: "demo", Mode: "measured", Verdict: "ready", ACMMRequest: 3,
		Scan:             Scan{Scope: "full", AuthoritativeForResolution: true, EvaluatedContracts: []string{"app-shell"}, EvaluatedFiles: []string{"src/App.tsx"}, TestPlanVersion: "plan-1", ToolRegistryVersion: "tools-1"},
		Observations:     []Observation{{Fingerprint: "visual-hive:test:app-shell", RepositoryFingerprint: digest([]byte("owner/repo\x00visual-hive:test:app-shell")), State: "present", IssueKind: "visual_regression", Severity: "high", OwningAgentHint: "hive/quality", Title: "App shell regression", Body: "Evidence-backed regression", Labels: []string{"visual-hive"}, SourceArtifacts: []string{".visual-hive/report.json"}, AffectedContracts: []string{"app-shell"}, ValidationCommand: "npm run vh:run:ci", ObservedAt: "2026-07-09T11:00:00.000Z", FirstSeenAt: "2026-07-09T11:00:00.000Z", SourceArtifact: ".visual-hive/issues.json"}},
		Files:            []File{{Path: relative, SourcePath: ".visual-hive/hive/beads.json", SHA256: fileDigest, Size: int64(len(data)), MediaType: "application/json"}},
		ReplayProtection: ReplayProtection{Nonce: "test-bundle"},
		Provenance:       Provenance{Kind: "local"}, Safety: Safety{AtomicWrite: true, PathsAreRelative: true, DigestsRequired: true, ProducerCountersAreAdvisory: true, ProducerTrustClaimIsAdvisory: true, AbsenceRequiresAuthoritativeScan: true},
	}
	manifest.ReplayProtection.Key = replayKey(manifest)
	manifest.OverallDigest = digestBundleContent(manifest, fileLines)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	manifestData, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	return manifestPath
}

func replayKey(manifest Manifest) string {
	return digest([]byte(strings.Join([]string{manifest.Source.Repository, manifest.Source.CommitSHA, valueOr(manifest.Source.WorkflowRunID, "local"), manifest.BundleID}, "\x00")))
}

func readManifest(t *testing.T, path string) Manifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeManifest(t *testing.T, path string, manifest Manifest) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
