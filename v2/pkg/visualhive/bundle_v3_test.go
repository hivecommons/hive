package visualhive

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testV3ProducerCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestValidateFinalVisualHiveV3UnicodeFixture(t *testing.T) {
	fixtureRoot := filepath.Join("testdata", "v3-unicode")
	bundle, err := ValidateBundle(filepath.Join(fixtureRoot, "manifest.json"), ValidationOptions{
		Now: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC), MaxACMM: 6, AllowLocal: true,
	})
	if err != nil {
		t.Fatalf("final Visual Hive v3 producer fixture was rejected: %v", err)
	}
	const expectedDigest = "78434b490e7c50f03550c1d9a5bf2eb1a5042a4bbd56b73d070d2b97b4943f38"
	if bundle.Manifest.OverallDigest != expectedDigest || bundle.Validation.Digest != expectedDigest {
		t.Fatalf("final Visual Hive v3 digest = %s, want %s", bundle.Manifest.OverallDigest, expectedDigest)
	}
	if err := bundle.VerifySourceArtifact(filepath.Join(fixtureRoot, "files")); err != nil {
		t.Fatalf("final Visual Hive v3 source fixture was rejected: %v", err)
	}
	beadsData, err := os.ReadFile(filepath.Join(fixtureRoot, "files", ".visual-hive", "hive", "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beadsData, []byte("[]\n")) || len(bundle.Beads) != 0 {
		t.Fatalf("final Visual Hive fixture did not preserve a schema-valid top-level beads array: %q", beadsData)
	}
}

func TestValidateV3BundleAndCompleteSourceArtifact(t *testing.T) {
	manifestPath, sourceRoot := writeTestV3Bundle(t, nil)
	now := time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC)
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: now, MaxACMM: 3, VerifiedProvenance: true,
		ExpectedProducerGitCommit: testV3ProducerCommit,
		ExpectedRepository:        "owner/repo", ExpectedRepositoryID: "123",
		ExpectedWorkflowRunID: "42", ExpectedWorkflowRunAttempt: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Validation.Trusted || bundle.Validation.Authoritative || bundle.Manifest.SchemaVersion != ManifestSchemaV3 {
		t.Fatalf("v3 production bundle became trusted before full source verification: %+v", bundle.Validation)
	}
	if _, err := bundle.VerifiedEvidenceRoot(); err == nil {
		t.Fatal("unverified source artifact exposed an evidence root")
	}
	writeTestData(t, filepath.Join(sourceRoot, sourceArtifactExtractionMarker), []byte("99\n"))
	if err := bundle.VerifySourceArtifact(sourceRoot); err != nil {
		t.Fatalf("valid complete v3 source artifact was rejected: %v", err)
	}
	if !bundle.Validation.Trusted || !bundle.Validation.Authoritative {
		t.Fatal("full source verification did not establish trusted authoritative v3 ingestion")
	}
	evidenceRoot, err := bundle.VerifiedEvidenceRoot()
	if err != nil {
		t.Fatalf("verified source artifact did not expose its indexed evidence root: %v", err)
	}
	wantEvidenceRoot, err := filepath.Abs(filepath.Join(sourceRoot, ".visual-hive"))
	if err != nil {
		t.Fatal(err)
	}
	if evidenceRoot != wantEvidenceRoot {
		t.Fatalf("verified evidence root = %q, want %q", evidenceRoot, wantEvidenceRoot)
	}
	writeTestData(t, filepath.Join(sourceRoot, ".visual-hive", "hive", "beads.json"), []byte("[]"))
	if err := bundle.VerifySourceArtifact(sourceRoot); err == nil {
		t.Fatal("changed source unexpectedly passed re-verification")
	}
	if bundle.Validation.Trusted || bundle.Validation.Authoritative {
		t.Fatalf("failed re-verification retained trusted authority: %+v", bundle.Validation)
	}
	if _, err := bundle.VerifiedEvidenceRoot(); err == nil {
		t.Fatal("failed source re-verification retained an evidence root")
	}

	wrongAttempt := ValidationOptions{Now: now, MaxACMM: 3, VerifiedProvenance: true, ExpectedProducerGitCommit: testV3ProducerCommit, ExpectedWorkflowRunAttempt: "3"}
	if _, err := ValidateBundle(manifestPath, wrongAttempt); err == nil || !strings.Contains(err.Error(), "run attempt") {
		t.Fatalf("expected run-attempt identity rejection, got %v", err)
	}
}

func TestVerifyReviewEvidenceArtifactRequiresCompleteContentAddressedBytes(t *testing.T) {
	root := t.TempDir()
	report := []byte(`{"status":"failed","kind":"visual_regression"}`)
	reportPath := ".visual-hive/report.json"
	writeTestData(t, filepath.Join(root, filepath.FromSlash(reportPath)), report)
	index := ArtifactIndexReport{
		SchemaVersion: 1, Project: "demo", GeneratedAt: "2026-07-15T12:00:00.000Z", Root: ".visual-hive", ContentAddressed: true, Complete: true,
		Summary:   ArtifactIndexSummary{DiscoveredArtifactCount: 1, ArtifactCount: 1, TotalBytes: int64(len(report)), JSON: 1},
		Artifacts: []ArtifactIndexEntry{{Path: reportPath, Kind: "json", ContentType: "application/json", Bytes: int64(len(report)), SHA256: digest(report), SafeToRender: true, Labels: []string{}}},
		Warnings:  []string{},
	}
	indexData, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	writeTestData(t, filepath.Join(root, ".visual-hive", "artifacts-index.json"), indexData)
	writeTestData(t, filepath.Join(root, sourceArtifactExtractionMarker), []byte("88\n"))

	verified, err := VerifyReviewEvidenceArtifact(root, 88)
	if err != nil {
		t.Fatal(err)
	}
	if verified.SchemaVersion != ReviewEvidenceSchemaVersion || verified.ArtifactIndexSHA256 != digest(indexData) ||
		verified.EvidenceRoot != filepath.Join(root, ".visual-hive") || verified.ArtifactCount != 1 || verified.TotalBytes != int64(len(report)) {
		t.Fatalf("review evidence identity = %+v", verified)
	}

	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(reportPath)), []byte(`{"tampered":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyReviewEvidenceArtifact(root, 88); err == nil || !strings.Contains(err.Error(), "source artifact entry") {
		t.Fatalf("tampered review evidence was accepted: %v", err)
	}
	writeTestData(t, filepath.Join(root, filepath.FromSlash(reportPath)), report)
	writeTestData(t, filepath.Join(root, ".visual-hive", "extra.json"), []byte("{}"))
	if _, err := VerifyReviewEvidenceArtifact(root, 88); err == nil || !strings.Contains(err.Error(), "unindexed") {
		t.Fatalf("extra review evidence was accepted: %v", err)
	}
}

func TestValidateV3RejectsLocalAndVerifiedProvenanceCombination(t *testing.T) {
	manifestPath, _ := writeTestV3Bundle(t, nil)
	_, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3,
		AllowLocal: true, VerifiedProvenance: true,
		ExpectedProducerGitCommit: testV3ProducerCommit,
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("local validation combined with verified provenance was accepted: %v", err)
	}
}

func TestLocalV3CannotConferResolutionAuthority(t *testing.T) {
	manifestPath, sourceRoot := writeTestV3Bundle(t, nil)
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Validation.Trusted || bundle.Validation.Authoritative || !bundle.Manifest.Scan.AuthoritativeForResolution {
		t.Fatalf("local v3 authority was not kept advisory: %+v", bundle.Validation)
	}
	if err := bundle.VerifySourceArtifact(sourceRoot); err != nil {
		t.Fatal(err)
	}
	if bundle.Validation.Authoritative {
		t.Fatal("local source verification conferred resolution authority")
	}
}

func TestValidateTrustedV3RequiresPinnedProducerCommit(t *testing.T) {
	manifestPath, _ := writeTestV3Bundle(t, nil)
	base := ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3, VerifiedProvenance: true,
		ExpectedProducerGitCommit: testV3ProducerCommit,
	}
	if _, err := ValidateBundle(manifestPath, base); err != nil {
		t.Fatalf("exact producer pin was rejected: %v", err)
	}
	for _, test := range []struct {
		name     string
		expected string
		want     string
	}{
		{name: "stripped expected commit", want: "exact 40-character"},
		{name: "symbolic expected ref", expected: "v0.4.0", want: "exact 40-character"},
		{name: "different expected commit", expected: strings.Repeat("f", 40), want: "does not match"},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := base
			options.ExpectedProducerGitCommit = test.expected
			if _, err := ValidateBundle(manifestPath, options); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unanchored producer identity was accepted: %v", err)
			}
		})
	}

	manifest := readManifest(t, manifestPath)
	manifest.Producer.GitCommit = ""
	manifest.ReplayProtection.Key = replayKeyForManifest(manifest)
	manifest.OverallDigest = digestV3BundleContent(manifest)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	writeManifest(t, manifestPath, manifest)
	if _, err := ValidateBundle(manifestPath, base); err == nil || !strings.Contains(err.Error(), "invalid Visual Hive producer") {
		t.Fatalf("stripped manifest producer commit was accepted: %v", err)
	}
}

func TestVerifyV3SourceArtifactRejectsMissingChangedAndExtraFiles(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string)
		want   string
	}{
		{name: "missing", mutate: func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, ".visual-hive", "capability-parity.json")); err != nil {
				t.Fatal(err)
			}
		}, want: "source artifact entry"},
		{name: "size or hash", mutate: func(t *testing.T, root string) {
			writeTestData(t, filepath.Join(root, ".visual-hive", "hive", "beads.json"), []byte("[]"))
		}, want: "wrong size"},
		{name: "hash", mutate: func(t *testing.T, root string) {
			target := filepath.Join(root, ".visual-hive", "hive", "beads.json")
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			data[0] ^= 1
			writeTestData(t, target, data)
		}, want: "sha-256 mismatch"},
		{name: "extra", mutate: func(t *testing.T, root string) {
			writeTestData(t, filepath.Join(root, ".visual-hive", "unindexed.json"), []byte("{}"))
		}, want: "unindexed file"},
		{name: "outside root extra", mutate: func(t *testing.T, root string) {
			writeTestData(t, filepath.Join(root, "outside.json"), []byte("{}"))
		}, want: "unindexed file"},
		{name: "bundle payload extra", mutate: func(t *testing.T, root string) {
			writeTestData(t, filepath.Join(root, ".visual-hive", "bundles", "transport", "files", "payload.json"), []byte(`{"transport":true}`))
		}, want: "excluded bundle path"},
		{name: "wrong extraction marker", mutate: func(t *testing.T, root string) {
			writeTestData(t, filepath.Join(root, sourceArtifactExtractionMarker), []byte("98\n"))
		}, want: "extraction marker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifestPath, sourceRoot := writeTestV3Bundle(t, nil)
			bundle, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true})
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, sourceRoot)
			if err := bundle.VerifySourceArtifact(sourceRoot); err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("expected %q source rejection, got %v", test.want, err)
			}
		})
	}
}

func TestValidateV3BundleRejectsIncompleteIndexTraversalAndFailedParity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ArtifactIndexReport, *capabilityParityReport)
		want   string
	}{
		{name: "incomplete index", mutate: func(index *ArtifactIndexReport, _ *capabilityParityReport) { index.Complete = false }, want: "artifact index binding"},
		{name: "traversal", mutate: func(index *ArtifactIndexReport, _ *capabilityParityReport) {
			index.Artifacts[0].Path = "../escape.json"
		}, want: "unsafe source path"},
		{name: "project identity mismatch", mutate: func(index *ArtifactIndexReport, _ *capabilityParityReport) {
			index.Project = "other"
		}, want: "artifact index identity"},
		{name: "compact hash mismatch", mutate: func(index *ArtifactIndexReport, _ *capabilityParityReport) {
			index.Artifacts[1].SHA256 = strings.Repeat("0", 64)
		}, want: "compact file"},
		{name: "failed parity", mutate: func(_ *ArtifactIndexReport, parity *capabilityParityReport) {
			parity.Status = "failed"
			parity.Summary = CapabilityParitySummary{Expected: 1, Missing: 1}
			parity.Domains = testCapabilityDomainSummaries("cli", parity.Summary)
			parity.Checks = []capabilityParityCheck{{Domain: "cli", Key: "doctor", Status: "missing", Message: "missing", Parity: false}}
		}, want: "capability parity"},
		{name: "missing required parity domain", mutate: func(_ *ArtifactIndexReport, parity *capabilityParityReport) {
			parity.Domains = parity.Domains[:len(parity.Domains)-1]
		}, want: "every required domain"},
		{name: "empty baseline and inventory", mutate: func(_ *ArtifactIndexReport, parity *capabilityParityReport) {
			parity.Summary = CapabilityParitySummary{}
			parity.Domains = testCapabilityDomainSummaries("", CapabilityParitySummary{})
			parity.Checks = []capabilityParityCheck{}
		}, want: "empty baseline or inventory"},
		{name: "duplicate capability key", mutate: func(_ *ArtifactIndexReport, parity *capabilityParityReport) {
			parity.Summary = CapabilityParitySummary{Expected: 2, Actual: 2, Present: 2}
			parity.Domains = testCapabilityDomainSummaries("cli", parity.Summary)
			parity.Checks = append(parity.Checks, parity.Checks[0])
		}, want: "malformed check"},
		{name: "domain counters do not match checks", mutate: func(_ *ArtifactIndexReport, parity *capabilityParityReport) {
			parity.Domains = testCapabilityDomainSummaries("schemas", parity.Summary)
		}, want: "summary does not match its unique checks"},
		{name: "expected inventory counts are fabricated", mutate: func(_ *ArtifactIndexReport, parity *capabilityParityReport) {
			parity.Summary.Expected = 2
			parity.Domains = testCapabilityDomainSummaries("cli", parity.Summary)
		}, want: "unique checks"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifestPath, _ := writeTestV3Bundle(t, test.mutate)
			if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("expected %q rejection, got %v", test.want, err)
			}
		})
	}
}

func TestValidateTrustedV3ParityRequiresCanonicalBaselineAndInventoryIdentities(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*capabilityParityReport)
		want   string
	}{
		{name: "missing identities", mutate: func(parity *capabilityParityReport) {
			parity.Checks[0].Expected = nil
			parity.Checks[0].Actual = nil
		}, want: "identit"},
		{name: "mismatched actual identity", mutate: func(parity *capabilityParityReport) {
			parity.Checks[0].Actual = json.RawMessage(`{"path":"status","aliases":[],"contractSha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
		}, want: "identit"},
		{name: "check key does not match details", mutate: func(parity *capabilityParityReport) {
			parity.Checks[0].Key = "status"
		}, want: "identit"},
		{name: "blocked status with supported runtime", mutate: func(parity *capabilityParityReport) {
			detail := json.RawMessage(`{"id":"external","category":"external","deterministicRole":"advisory","supportedOperations":["availability"],"operations":[{"operation":"availability","runtimeStatus":"supported"}],"runtimeStatus":"supported"}`)
			parity.RuntimeStatus = "blocked"
			parity.Summary = CapabilityParitySummary{Expected: 1, Actual: 1, Blocked: 1}
			parity.Domains = testCapabilityDomainSummaries("providers", parity.Summary)
			parity.Checks = []capabilityParityCheck{{Domain: "providers", Key: "external", Status: "blocked", Parity: true, Message: "blocked", Expected: detail, Actual: detail}}
		}, want: "runtime status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifestPath, _ := writeTestV3Bundle(t, func(_ *ArtifactIndexReport, parity *capabilityParityReport) { test.mutate(parity) })
			_, err := ValidateBundle(manifestPath, ValidationOptions{
				Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3, VerifiedProvenance: true,
				ExpectedProducerGitCommit: testV3ProducerCommit,
			})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("trusted parity without canonical identities was accepted: %v", err)
			}
		})
	}
}

func TestValidateTrustedV3RejectsPullRequestWorkflowEvents(t *testing.T) {
	for _, event := range []string{"pull_request", "pull_request_target"} {
		t.Run(event, func(t *testing.T) {
			manifestPath, _ := writeTestV3Bundle(t, nil)
			manifest := readManifest(t, manifestPath)
			manifest.Source.Event = event
			manifest.ReplayProtection.Key = replayKeyForManifest(manifest)
			manifest.OverallDigest = digestV3BundleContent(manifest)
			manifest.Provenance.SubjectDigest = manifest.OverallDigest
			writeManifest(t, manifestPath, manifest)
			_, err := ValidateBundle(manifestPath, ValidationOptions{
				Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3, VerifiedProvenance: true,
				ExpectedProducerGitCommit: testV3ProducerCommit,
			})
			if err == nil || !strings.Contains(err.Error(), "non-PR workflow") {
				t.Fatalf("trusted %s bundle was accepted: %v", event, err)
			}
		})
	}
}

func TestValidateV3BundleAcceptsConsistentBlockedCapability(t *testing.T) {
	manifestPath, _ := writeTestV3Bundle(t, func(_ *ArtifactIndexReport, parity *capabilityParityReport) {
		parity.RuntimeStatus = "blocked"
		parity.Summary = CapabilityParitySummary{Expected: 1, Actual: 1, Blocked: 1}
		parity.Domains = testCapabilityDomainSummaries("providers", parity.Summary)
		detail := json.RawMessage(`{"id":"external","category":"external","deterministicRole":"advisory","supportedOperations":[],"operations":[{"operation":"availability","runtimeStatus":"blocked","blockedReason":"explicitly blocked"}],"runtimeStatus":"blocked","blockedReason":"explicitly blocked"}`)
		parity.Checks = []capabilityParityCheck{{Domain: "providers", Key: "external", Status: "blocked", Parity: true, Message: "explicitly blocked", Expected: detail, Actual: detail}}
	})
	if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true}); err != nil {
		t.Fatalf("consistent blocked runtime parity was rejected: %v", err)
	}
}

func TestV3DigestAndReplayBindWorkflowAttemptAndEvidenceBindings(t *testing.T) {
	manifestPath, _ := writeTestV3Bundle(t, nil)
	manifest := readManifest(t, manifestPath)
	baseDigest := digestV3BundleContent(manifest)
	for _, mutate := range []func(*Manifest){
		func(m *Manifest) { m.GeneratedAt = m.GeneratedAt.Add(time.Millisecond) },
		func(m *Manifest) { m.ExpiresAt = m.ExpiresAt.Add(time.Millisecond) },
		func(m *Manifest) { m.Source.WorkflowRunAttempt = "3" },
		func(m *Manifest) { m.ArtifactIndex.ArtifactCount++ },
		func(m *Manifest) { m.CapabilityParity.Summary.Present++ },
	} {
		changed := cloneManifest(t, manifest)
		mutate(&changed)
		if digestV3BundleContent(changed) == baseDigest {
			t.Fatal("v3 digest did not bind a required identity field")
		}
	}
	changed := cloneManifest(t, manifest)
	changed.Source.WorkflowRunAttempt = "3"
	if replayKeyForManifest(changed) == manifest.ReplayProtection.Key {
		t.Fatal("v3 replay key did not bind workflow run attempt")
	}
}

func TestV3SchemaPresenceFailsClosed(t *testing.T) {
	manifestPath, _ := writeTestV3Bundle(t, nil)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]interface{}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	delete(document["source"].(map[string]interface{}), "trusted")
	data, _ = json.Marshal(document)
	writeTestData(t, manifestPath, data)
	if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "missing trusted") {
		t.Fatalf("v3 manifest with a missing required boolean was accepted: %v", err)
	}
	if err := validateCapabilityParityJSONPresence([]byte(`{"schemaVersion":"visual-hive.capability-parity.v1"}`)); err == nil || !strings.Contains(err.Error(), "missing baselineVersion") {
		t.Fatalf("incomplete capability receipt was accepted: %v", err)
	}

	manifestPath, _ = writeTestV3Bundle(t, nil)
	data, err = os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	document["generatedAt"] = "2026-07-09T12:00:00Z"
	data, _ = json.Marshal(document)
	writeTestData(t, manifestPath, data)
	if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "canonical UTC millisecond") {
		t.Fatalf("noncanonical v3 timestamp spelling was accepted: %v", err)
	}
}

func TestValidateV3BundleRejectsNonIncreasingExpiry(t *testing.T) {
	for _, test := range []struct {
		name  string
		delta time.Duration
	}{
		{name: "equal", delta: 0},
		{name: "before", delta: -2 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifestPath, _ := writeTestV3Bundle(t, nil)
			manifest := readManifest(t, manifestPath)
			manifest.ExpiresAt = manifest.GeneratedAt.Add(test.delta)
			manifest.OverallDigest = digestV3BundleContent(manifest)
			manifest.Provenance.SubjectDigest = manifest.OverallDigest
			writeManifest(t, manifestPath, manifest)
			_, err := ValidateBundle(manifestPath, ValidationOptions{
				Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true,
			})
			if err == nil || !strings.Contains(err.Error(), "invalid lifetime") {
				t.Fatalf("v3 expiry %s generatedAt was accepted: %v", test.name, err)
			}
		})
	}
}

func TestV3SchemaPresenceRejectsNullRequiredValues(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]interface{})
		field  string
	}{
		{name: "root scalar", field: "externalCallsMade", mutate: func(document map[string]interface{}) {
			document["externalCallsMade"] = nil
		}},
		{name: "nested boolean", field: "trusted", mutate: func(document map[string]interface{}) {
			document["source"].(map[string]interface{})["trusted"] = nil
		}},
		{name: "required array", field: "evaluatedContracts", mutate: func(document map[string]interface{}) {
			document["scan"].(map[string]interface{})["evaluatedContracts"] = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifestPath, _ := writeTestV3Bundle(t, nil)
			data, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]interface{}
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			test.mutate(document)
			data, _ = json.Marshal(document)
			writeTestData(t, manifestPath, data)
			_, err = ValidateBundle(manifestPath, ValidationOptions{
				Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true,
			})
			if err == nil || !strings.Contains(err.Error(), test.field+" cannot be null") {
				t.Fatalf("null required field %s was accepted: %v", test.field, err)
			}
		})
	}
}

func writeTestV3Bundle(t *testing.T, mutate func(*ArtifactIndexReport, *capabilityParityReport)) (string, string) {
	return writeTestV3BundleFixture(t, mutate, false)
}

func writeTestV3ObservationBundle(t *testing.T) (string, string) {
	return writeTestV3BundleFixture(t, nil, true)
}

func writeTestV3BundleFixture(t *testing.T, mutate func(*ArtifactIndexReport, *capabilityParityReport), includeObservation bool) (string, string) {
	t.Helper()
	root, sourceRoot := t.TempDir(), t.TempDir()
	beads := []Projection{{ID: "vh-v3", Title: "Fix visual regression", Type: "bug", Status: "open", Priority: 1, Actor: "quality", ExternalRef: "visual-hive:demo:v3", Metadata: map[string]string{}, Notes: "deterministic", CreatedAt: "2026-07-09T12:00:00Z", UpdatedAt: "2026-07-09T12:00:00Z", DependsOn: []string{}}}
	beadsData, _ := json.Marshal(beads)
	beadsPath := ".visual-hive/hive/beads.json"
	writeTestData(t, filepath.Join(sourceRoot, filepath.FromSlash(beadsPath)), beadsData)

	paritySummary := CapabilityParitySummary{Expected: 1, Actual: 1, Present: 1}
	parity := capabilityParityReport{
		SchemaVersion: capabilityParitySchemaVersion, BaselineVersion: capabilityBaselineSchemaVersion,
		GeneratedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), Status: "passed", RuntimeStatus: "ready", Summary: paritySummary,
		Domains: testCapabilityDomainSummaries("cli", paritySummary),
		Checks: []capabilityParityCheck{{
			Domain: "cli", Key: "doctor", Status: "present", Parity: true, Message: "present",
			Expected: json.RawMessage(`{"path":"doctor","aliases":[],"contractSha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
			Actual:   json.RawMessage(`{"path":"doctor","aliases":[],"contractSha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
		}},
	}
	parityPath := ".visual-hive/capability-parity.json"
	parityData, _ := json.Marshal(parity)
	writeTestData(t, filepath.Join(sourceRoot, filepath.FromSlash(parityPath)), parityData)

	index := ArtifactIndexReport{
		SchemaVersion: 1, Project: "demo", GeneratedAt: "2026-07-09T12:00:00.000Z", Root: ".visual-hive", ContentAddressed: true, Complete: true,
		Summary: ArtifactIndexSummary{DiscoveredArtifactCount: 2, ArtifactCount: 2, TotalBytes: int64(len(beadsData) + len(parityData)), JSON: 2},
		Artifacts: []ArtifactIndexEntry{
			{Path: parityPath, Kind: "json", ContentType: "application/json", Bytes: int64(len(parityData)), SHA256: digest(parityData), SafeToRender: true, Labels: []string{}},
			{Path: beadsPath, Kind: "json", ContentType: "application/json", Bytes: int64(len(beadsData)), SHA256: digest(beadsData), SafeToRender: true, Labels: []string{}},
		}, Warnings: []string{},
	}
	evidencePath := ".visual-hive/evidence/visual-regression.json"
	if includeObservation {
		evidenceData := []byte(`{"regression":true}`)
		writeTestData(t, filepath.Join(sourceRoot, filepath.FromSlash(evidencePath)), evidenceData)
		index.Artifacts = append(index.Artifacts, ArtifactIndexEntry{
			Path: evidencePath, Kind: "json", ContentType: "application/json", Bytes: int64(len(evidenceData)),
			SHA256: digest(evidenceData), SafeToRender: true, Labels: []string{},
		})
		index.Summary.DiscoveredArtifactCount++
		index.Summary.ArtifactCount++
		index.Summary.TotalBytes += int64(len(evidenceData))
		index.Summary.JSON++
	}
	if mutate != nil {
		mutate(&index, &parity)
		parityData, _ = json.Marshal(parity)
		writeTestData(t, filepath.Join(sourceRoot, filepath.FromSlash(parityPath)), parityData)
		index.Artifacts[0].Bytes, index.Artifacts[0].SHA256 = int64(len(parityData)), digest(parityData)
		index.Summary.TotalBytes = index.Artifacts[0].Bytes + index.Artifacts[1].Bytes
	}
	indexData, _ := json.Marshal(index)
	indexPath := ".visual-hive/artifacts-index.json"
	writeTestData(t, filepath.Join(sourceRoot, filepath.FromSlash(indexPath)), indexData)

	files := []File{}
	for _, source := range []struct {
		path string
		data []byte
	}{{indexPath, indexData}, {parityPath, parityData}, {beadsPath, beadsData}} {
		target := filepath.Join(root, "files", filepath.FromSlash(source.path))
		writeTestData(t, target, source.data)
		files = append(files, File{Path: "files/" + source.path, SourcePath: source.path, SHA256: digest(source.data), Size: int64(len(source.data)), MediaType: "application/json"})
	}
	observations := []Observation{}
	if includeObservation {
		observations = []Observation{{
			Fingerprint: "visual-regression/source", RepositoryFingerprint: digest([]byte("owner/repo\x00visual-regression/source")),
			PublicationRole: "canonical", RootCauseKey: "visual-regression/source", BlockedByRootKeys: []string{},
			State: "present", IssueKind: "visual_regression", Severity: "high", OwningAgentHint: "hive/quality",
			Title: "Manifest-owned visual regression", Body: "Verified observation body", Labels: []string{"visual-hive"},
			SourceArtifacts: []string{evidencePath}, AffectedContracts: []string{"contract/app-shell"},
			ValidationCommand: "go test ./...", ObservedAt: "2026-07-09T12:00:00.000Z", FirstSeenAt: "2026-07-09T12:00:00.000Z",
			SourceArtifact: ".visual-hive/issues.json",
		}}
	}
	manifest := Manifest{
		SchemaVersion: ManifestSchemaV3, DigestAlgorithm: ContentAddressedDigestAlgorithm, BundleID: "test-v3",
		GeneratedAt: time.Date(2026, 7, 9, 12, 0, 0, int(time.Millisecond), time.UTC), ExpiresAt: time.Date(2026, 7, 10, 12, 0, 0, int(time.Millisecond), time.UTC),
		Producer: Producer{Name: "visual-hive", Version: "0.4.0", GitCommit: testV3ProducerCommit},
		Source:   Source{Repository: "owner/repo", RepositoryID: "123", Ref: "refs/heads/main", CommitSHA: "abc123", Event: "workflow_dispatch", WorkflowName: "Visual Hive", WorkflowRunID: "42", WorkflowRunAttempt: "2", WorkflowArtifactID: "99", Conclusion: "success"},
		Project:  "demo", Mode: "measured", Verdict: "ready", ACMMRequest: 3,
		Scan:         Scan{Scope: "full", AuthoritativeForResolution: true, EvaluatedContracts: []string{}, EvaluatedFiles: []string{}, TestPlanVersion: "plan-v3", ToolRegistryVersion: "tools-v3"},
		Observations: observations, Files: files,
		ArtifactIndex:    &ArtifactIndexBinding{Path: "files/" + indexPath, SourcePath: indexPath, SHA256: digest(indexData), SchemaVersion: 1, ContentAddressed: true, Complete: index.Complete, ArtifactCount: index.Summary.ArtifactCount, TotalBytes: index.Summary.TotalBytes},
		CapabilityParity: &CapabilityParityBinding{Path: "files/" + parityPath, SourcePath: parityPath, SHA256: digest(parityData), SchemaVersion: parity.SchemaVersion, BaselineVersion: parity.BaselineVersion, Status: parity.Status, RuntimeStatus: parity.RuntimeStatus, Summary: parity.Summary},
		ReplayProtection: ReplayProtection{Nonce: "test-v3"}, Provenance: Provenance{Kind: "github-actions", AttestationRequired: true},
		Safety: Safety{AtomicWrite: true, PathsAreRelative: true, DigestsRequired: true, ProducerCountersAreAdvisory: true, ProducerTrustClaimIsAdvisory: true, AbsenceRequiresAuthoritativeScan: true},
	}
	manifest.ReplayProtection.Key = replayKeyForManifest(manifest)
	manifest.OverallDigest = digestV3BundleContent(manifest)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	manifestData, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(root, "manifest.json")
	writeTestData(t, manifestPath, manifestData)
	return manifestPath, sourceRoot
}

func testCapabilityDomainSummaries(populated string, summary CapabilityParitySummary) []capabilityDomainSummary {
	domains := make([]capabilityDomainSummary, 0, len(capabilityParityDomainNames()))
	for _, domain := range capabilityParityDomainNames() {
		value := CapabilityParitySummary{}
		if domain == populated {
			value = summary
		}
		domains = append(domains, capabilityDomainSummary{Domain: domain, CapabilityParitySummary: value})
	}
	return domains
}

func writeTestData(t *testing.T, target string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
