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
	if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("expected tamper rejection, got %v", err)
	}
}

func TestValidateBundleRejectsExpiredEvidence(t *testing.T) {
	manifestPath := writeTestBundle(t, t.TempDir(), false)
	if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired evidence rejection, got %v", err)
	}
}

func TestValidateBundleRejectsUntrustedByDefault(t *testing.T) {
	manifestPath := writeTestBundle(t, t.TempDir(), false)
	if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3}); err == nil || !strings.Contains(err.Error(), "independently verified") {
		t.Fatalf("expected provenance rejection, got %v", err)
	}
}

func TestValidateBundleKeepsVerifiedV2ForLocalCompatibilityButRejectsProductionAuthority(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeTestBundle(t, root, false)
	bindTestBundleToIndependentProvenance(t, manifestPath)

	if _, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3,
		VerifiedProvenance: true, ExpectedRepository: "owner/repo", ExpectedWorkflowRunID: "42",
	}); err == nil || !strings.Contains(err.Error(), "requires a Visual Hive bundle v3") {
		t.Fatalf("expected v2 production authority rejection, got %v", err)
	}
	validated, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true})
	if err != nil || !validated.Validation.Trusted {
		t.Fatalf("v2 local/backward compatibility was lost: bundle=%+v err=%v", validated, err)
	}
}

func TestValidateBundleRejectsV2BeforeProductionIdentityAuthority(t *testing.T) {
	manifestPath := writeTestBundle(t, t.TempDir(), false)
	bindTestBundleToIndependentProvenance(t, manifestPath)

	base := ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3,
		VerifiedProvenance: true, ExpectedRepository: "owner/repo", ExpectedWorkflowRunID: "42",
	}
	crossRepository := base
	crossRepository.ExpectedRepository = "owner/other"
	if _, err := ValidateBundle(manifestPath, crossRepository); err == nil || !strings.Contains(err.Error(), "bundle v3") {
		t.Fatalf("expected v2 production authority rejection, got %v", err)
	}
	staleRun := base
	staleRun.ExpectedWorkflowRunID = "43"
	if _, err := ValidateBundle(manifestPath, staleRun); err == nil || !strings.Contains(err.Error(), "bundle v3") {
		t.Fatalf("expected v2 production authority rejection, got %v", err)
	}
}

func TestValidateBundleRejectsAbsentObservationFromPartialScan(t *testing.T) {
	manifestPath := writeTestBundle(t, t.TempDir(), false)
	manifest := readManifest(t, manifestPath)
	manifest.Scan.Scope = "partial"
	manifest.Scan.AuthoritativeForResolution = false
	manifest.Observations[0].State = "absent"
	writeManifest(t, manifestPath, manifest)
	if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "cannot claim finding absence") {
		t.Fatalf("expected unsafe resolution rejection, got %v", err)
	}
}

func TestValidateV2ArtifactBundleRejectsCraftedResolutionAuthority(t *testing.T) {
	manifestPath := writeTestBundle(t, t.TempDir(), false)
	manifest := readManifest(t, manifestPath)
	manifest.Scan.AuthoritativeForResolution = true
	sealTestManifest(t, manifestPath, &manifest)
	if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "cannot claim authoritative resolution") {
		t.Fatalf("crafted v2 resolution authority was accepted: %v", err)
	}
}

func TestValidateBundleAcceptsExplicitCanonicalRootIdentity(t *testing.T) {
	manifestPath := writeTestBundle(t, t.TempDir(), false)
	manifest := readManifest(t, manifestPath)
	root := "mutation/api-500/localPreview/dashboard-shell"
	manifest.Observations[0].PublicationRole = "canonical"
	manifest.Observations[0].RootCauseKey = root
	manifest.Observations[0].BlockedByRootKeys = []string{}
	manifest.Observations[0].IssueKind = "mutation_survivor"
	manifest.Observations[0].RepositoryFingerprint = digest([]byte("owner/repo\x00" + root))
	manifest.DigestAlgorithm = PublicationDigestAlgorithm
	sealTestManifest(t, manifestPath, &manifest)

	bundle, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := bundle.Manifest.Observations[0]; got.RootCauseKey != root || got.RepositoryFingerprint == digest([]byte("owner/repo\x00"+got.Fingerprint)) {
		t.Fatalf("canonical root identity was not preserved independently of source wording: %+v", got)
	}
}

func TestValidateBundleRejectsInvalidPublicationMetadata(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Manifest)
		message string
	}{
		{name: "partial", mutate: func(m *Manifest) { m.Observations[0].PublicationRole = "canonical" }, message: "incomplete"},
		{name: "unknown-role", mutate: func(m *Manifest) {
			m.Observations[0].PublicationRole, m.Observations[0].RootCauseKey = "future", "root/future"
		}, message: "role"},
		{name: "unsafe-root", mutate: func(m *Manifest) {
			m.Observations[0].PublicationRole, m.Observations[0].RootCauseKey = "canonical", "root with space"
		}, message: "invalid"},
		{name: "invalid-percent-escape", mutate: func(m *Manifest) {
			m.Observations[0].PublicationRole, m.Observations[0].RootCauseKey = "canonical", "root/%zz"
		}, message: "invalid"},
		{name: "derivative-kind", mutate: func(m *Manifest) {
			m.Observations[0].PublicationRole, m.Observations[0].RootCauseKey = "derivative", "root/visual"
		}, message: "cannot be a derivative"},
		{name: "aggregate-without-blockers", mutate: func(m *Manifest) {
			m.Observations[0].PublicationRole, m.Observations[0].RootCauseKey, m.Observations[0].IssueKind = "aggregate", "aggregate/readiness", "external_repo_onboarding"
		}, message: "requires blocked"},
		{name: "unsorted-blockers", mutate: func(m *Manifest) {
			m.Observations[0].PublicationRole, m.Observations[0].RootCauseKey, m.Observations[0].IssueKind, m.Observations[0].BlockedByRootKeys = "aggregate", "aggregate/readiness", "external_repo_onboarding", []string{"z/root", "a/root"}
		}, message: "sorted and unique"},
		{name: "duplicate-canonical", mutate: func(m *Manifest) {
			root := "root/duplicate"
			m.Observations[0].PublicationRole, m.Observations[0].RootCauseKey = "canonical", root
			m.Observations[0].RepositoryFingerprint = digest([]byte("owner/repo\x00" + root))
			duplicate := m.Observations[0]
			duplicate.Fingerprint = "different-source-wording"
			m.Observations = append(m.Observations, duplicate)
		}, message: "more than one canonical"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			manifestPath := writeTestBundle(t, t.TempDir(), false)
			manifest := readManifest(t, manifestPath)
			manifest.DigestAlgorithm = PublicationDigestAlgorithm
			manifest.Observations[0].BlockedByRootKeys = []string{}
			test.mutate(&manifest)
			writeManifest(t, manifestPath, manifest)
			_, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true})
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("expected %q rejection, got %v", test.message, err)
			}
		})
	}
}

func TestBundleDigestBindsPublicationMetadataButPreservesLegacyV2(t *testing.T) {
	legacyPath := writeTestBundle(t, t.TempDir(), false)
	legacyData, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	var legacyDocument map[string]interface{}
	if err := json.Unmarshal(legacyData, &legacyDocument); err != nil {
		t.Fatal(err)
	}
	legacyObservations := legacyDocument["observations"].([]interface{})
	legacyObservation := legacyObservations[0].(map[string]interface{})
	delete(legacyObservation, "publicationRole")
	delete(legacyObservation, "rootCauseKey")
	delete(legacyObservation, "blockedByRootKeys")
	legacyData, err = json.Marshal(legacyDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, legacyData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBundle(legacyPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true}); err != nil {
		t.Fatalf("legacy v2 digest compatibility was lost: %v", err)
	}

	manifestPath := writeTestBundle(t, t.TempDir(), false)
	manifest := readManifest(t, manifestPath)
	manifest.Observations[0].PublicationRole = "derivative"
	manifest.Observations[0].RootCauseKey = "root/one"
	manifest.Observations[0].BlockedByRootKeys = []string{}
	manifest.Observations[0].IssueKind = "missing_visual_coverage"
	manifest.DigestAlgorithm = PublicationDigestAlgorithm
	sealTestManifest(t, manifestPath, &manifest)
	manifest.Observations[0].RootCauseKey = "root/two"
	writeManifest(t, manifestPath, manifest)
	_, err = ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true})
	if err == nil || !strings.Contains(err.Error(), "overall digest") {
		t.Fatalf("publication metadata was not bound by the bundle digest: %v", err)
	}
}

func TestPublicationDigestRejectsAmbiguousArrayEncodings(t *testing.T) {
	basePath := writeTestBundle(t, t.TempDir(), false)
	base := readManifest(t, basePath)
	base.DigestAlgorithm = PublicationDigestAlgorithm
	base.Observations[0].PublicationRole = "aggregate"
	base.Observations[0].RootCauseKey = "aggregate/readiness"
	base.Observations[0].BlockedByRootKeys = []string{"a", "b,c"}
	base.Observations[0].IssueKind = "external_repo_onboarding"

	tests := []struct {
		name   string
		mutate func(*Manifest, []string)
	}{
		{name: "blocked roots", mutate: func(m *Manifest, values []string) { m.Observations[0].BlockedByRootKeys = values }},
		{name: "labels", mutate: func(m *Manifest, values []string) { m.Observations[0].Labels = values }},
		{name: "source artifacts", mutate: func(m *Manifest, values []string) { m.Observations[0].SourceArtifacts = values }},
		{name: "affected contracts", mutate: func(m *Manifest, values []string) { m.Observations[0].AffectedContracts = values }},
		{name: "scan contracts", mutate: func(m *Manifest, values []string) { m.Scan.EvaluatedContracts = values }},
		{name: "scan files", mutate: func(m *Manifest, values []string) { m.Scan.EvaluatedFiles = values }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			left, right := cloneManifest(t, base), cloneManifest(t, base)
			test.mutate(&left, []string{"a", "b,c"})
			test.mutate(&right, []string{"a,b", "c"})
			if leftDigest, rightDigest := digestBundleContent(left, nil), digestBundleContent(right, nil); leftDigest == rightDigest {
				t.Fatalf("length-prefixed digest collided for %s: %s", test.name, leftDigest)
			}
		})
	}
}

func TestPublicationDigestEmptyAuthoritativeCrossLanguageVector(t *testing.T) {
	manifest := Manifest{
		SchemaVersion: ManifestSchema, DigestAlgorithm: PublicationDigestAlgorithm, BundleID: "vector-1",
		Producer: Producer{Name: "visual-hive", Version: "0.3.0", GitCommit: "abc123"},
		Source:   Source{Repository: "owner/repo", Ref: "refs/heads/main", CommitSHA: "abc123", Conclusion: "success"},
		Project:  "demo", Mode: "measured", Verdict: "ready", ACMMRequest: 3,
		Scan:             Scan{Scope: "full", AuthoritativeForResolution: true, EvaluatedContracts: []string{"a", "b,c"}, EvaluatedFiles: []string{"src/a.ts", "src/b,c.ts"}, TestPlanVersion: "plan-1", ToolRegistryVersion: "tools-1"},
		Observations:     []Observation{},
		Files:            []File{{Path: "files/.visual-hive/hive/beads.json", SHA256: "efd15aa78678e52c4c172f146620a25b5f111644e8417b5330ea2021a6bc6138", Size: 57}},
		ReplayProtection: ReplayProtection{Key: "89386a711a2fbeb6f0097713e415b27072b2805311a18cbd2fc93c2122e9b435"},
	}
	const expected = "8831ed6c7209ebe1b6e7dfb845b3ed9a1df1cbc8d72fd839339fb3140ee1bf32"
	if got := digestBundleContent(manifest, nil); got != expected {
		t.Fatalf("publication digest cross-language vector = %s, want %s", got, expected)
	}
}

func TestPublicationDigestUnicodeCrossLanguageVector(t *testing.T) {
	const bmp = "\uE000"
	const supplementary = "\U00010000"
	utf8Order := []string{bmp, supplementary}
	manifest := Manifest{
		SchemaVersion: ManifestSchema, DigestAlgorithm: PublicationDigestAlgorithm, BundleID: "unicode-vector",
		Producer: Producer{Name: "visual-hive", Version: "0.3.0", GitCommit: "unicode"},
		Source:   Source{Repository: "owner/repo", Ref: "refs/heads/main", CommitSHA: "abc123", Conclusion: "success"},
		Project:  "unicode", Mode: "full", Verdict: "ready", ACMMRequest: 4,
		Scan: Scan{
			Scope: "full", AuthoritativeForResolution: true,
			EvaluatedContracts: append([]string(nil), utf8Order...),
			EvaluatedFiles:     []string{"src/" + bmp + ".ts", "src/" + supplementary + ".ts"},
			TestPlanVersion:    "unicode-plan", ToolRegistryVersion: "unicode-tools",
		},
		Observations: []Observation{{
			Fingerprint: "unicode-observation", RepositoryFingerprint: "58541d3572f87f73e2e9d7ced3f1cdd6ee3db034da1229772acdaeb665dc04e6",
			PublicationRole: "canonical", RootCauseKey: "finding/visual_regression/unicode", BlockedByRootKeys: []string{},
			State: "present", IssueKind: "visual_regression", Severity: "high", OwningAgentHint: "hive/quality",
			Title: "Evidence observation", Body: "Evidence-backed observation", Labels: append([]string(nil), utf8Order...),
			SourceArtifacts:   []string{"evidence/" + bmp + ".json", "evidence/" + supplementary + ".json"},
			AffectedContracts: append([]string(nil), utf8Order...), ValidationCommand: "npm test",
			ObservedAt: "2026-07-09T12:00:00.000Z", FirstSeenAt: "2026-07-09T12:00:00.000Z", SourceArtifact: ".visual-hive/issues.json",
		}},
		Files:            []File{{Path: "files/.visual-hive/hive/beads.json", SHA256: "efd15aa78678e52c4c172f146620a25b5f111644e8417b5330ea2021a6bc6138", Size: 57}},
		ReplayProtection: ReplayProtection{Key: "38a796cc809ff6e85a7e1e97e085809678ce2b3935c196e4a6efb2354a438d26"},
	}
	const expected = "289d4b4a672d4b6b32c6f7fc6c970070f40f7afff3151411a47e982e447950d7"
	if got := digestBundleContent(manifest, nil); got != expected {
		t.Fatalf("Unicode publication digest cross-language vector = %s, want %s", got, expected)
	}
}

func TestSortedUniqueUsesUTF8ByteOrder(t *testing.T) {
	// UTF-16 code-unit order puts U+10000's high surrogate before U+E000.
	// The cross-runtime contract instead sorts their UTF-8 bytes, where U+E000
	// (0xEE...) precedes U+10000 (0xF0...).
	utf8Order := []string{"\uE000", "\U00010000"}
	if !sortedUnique(utf8Order) {
		t.Fatalf("valid UTF-8 byte order was rejected: %q", utf8Order)
	}
	if sortedUnique([]string{utf8Order[1], utf8Order[0]}) {
		t.Fatal("UTF-16-only ordering was accepted by the Hive validator")
	}
}

func TestValidateBundleRejectsDigestModeDowngradeAndMixedMetadata(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	t.Run("unknown algorithm", func(t *testing.T) {
		manifestPath := writeTestBundle(t, t.TempDir(), false)
		manifest := readManifest(t, manifestPath)
		manifest.DigestAlgorithm = "visual-hive.bundle.future-digest.v9"
		writeManifest(t, manifestPath, manifest)
		if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: now, MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "unsupported bundle digest algorithm") {
			t.Fatalf("unknown digest algorithm was accepted: %v", err)
		}
	})
	t.Run("marked legacy observations", func(t *testing.T) {
		manifestPath := writeTestBundle(t, t.TempDir(), false)
		manifest := readManifest(t, manifestPath)
		manifest.DigestAlgorithm = PublicationDigestAlgorithm
		sealTestManifest(t, manifestPath, &manifest)
		if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: now, MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "complete explicit publication metadata") {
			t.Fatalf("marked legacy observation was accepted: %v", err)
		}
	})
	t.Run("unmarked explicit observations", func(t *testing.T) {
		manifestPath := writeTestBundle(t, t.TempDir(), false)
		manifest := readManifest(t, manifestPath)
		manifest.Observations[0].PublicationRole = "canonical"
		manifest.Observations[0].RootCauseKey = "root/one"
		manifest.Observations[0].BlockedByRootKeys = []string{}
		manifest.Observations[0].RepositoryFingerprint = digest([]byte("owner/repo\x00root/one"))
		sealTestManifest(t, manifestPath, &manifest)
		if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: now, MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "requires digest algorithm") {
			t.Fatalf("unmarked explicit observation was accepted: %v", err)
		}
	})
	t.Run("mixed observations", func(t *testing.T) {
		manifestPath := writeTestBundle(t, t.TempDir(), false)
		manifest := readManifest(t, manifestPath)
		explicit := manifest.Observations[0]
		explicit.PublicationRole = "canonical"
		explicit.RootCauseKey = "root/one"
		explicit.BlockedByRootKeys = []string{}
		explicit.RepositoryFingerprint = digest([]byte("owner/repo\x00root/one"))
		manifest.DigestAlgorithm = PublicationDigestAlgorithm
		manifest.Observations = append(manifest.Observations, explicit)
		sealTestManifest(t, manifestPath, &manifest)
		if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: now, MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "cannot mix") {
			t.Fatalf("mixed publication metadata was accepted: %v", err)
		}
	})
}

func TestValidateBundleProducedByVisualHive(t *testing.T) {
	manifestPath := os.Getenv("VISUAL_HIVE_TEST_BUNDLE")
	if manifestPath == "" {
		t.Skip("VISUAL_HIVE_TEST_BUNDLE is not set")
	}
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{MaxACMM: 6, AllowLocal: true})
	if err != nil {
		t.Fatalf("Visual Hive producer and Hive consumer contract diverged: %v", err)
	}
	if sourceRoot := os.Getenv("VISUAL_HIVE_TEST_SOURCE"); sourceRoot != "" {
		if err := bundle.VerifySourceArtifact(sourceRoot); err != nil {
			t.Fatalf("Visual Hive producer source index and Hive consumer diverged: %v", err)
		}
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
		Scan:             Scan{Scope: "full", AuthoritativeForResolution: false, EvaluatedContracts: []string{"app-shell"}, EvaluatedFiles: []string{"src/App.tsx"}, TestPlanVersion: "plan-1", ToolRegistryVersion: "tools-1"},
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

func bindTestBundleToIndependentProvenance(t *testing.T, manifestPath string) {
	t.Helper()
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

func cloneManifest(t *testing.T, manifest Manifest) Manifest {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var cloned Manifest
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
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

func sealTestManifest(t *testing.T, path string, manifest *Manifest) {
	t.Helper()
	fileLines := make([]string, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		fileLines = append(fileLines, fmt.Sprintf("file\x00%s\x00%s\x00%d", file.Path, file.SHA256, file.Size))
	}
	manifest.OverallDigest = digestBundleContent(*manifest, fileLines)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	writeManifest(t, path, *manifest)
}
