package github

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const testVisualHiveProducerCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testVisualHiveHeadSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestFetchAndVerifyVisualHiveBundleBindsGitHubProvenance(t *testing.T) {
	zipData, evidenceZip := buildVerifiedV3Artifacts(t)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case "/repos/owner/repo/actions/runs/42":
			_, _ = io.WriteString(writer, `{"id":42,"run_attempt":2,"workflow_id":12,"name":"Visual Hive Scheduled [correlation]","display_title":"Visual Hive Scheduled [correlation]","path":".github/workflows/visual-hive.yml","head_branch":"main","head_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","event":"schedule","status":"completed","conclusion":"success","html_url":"https://github.test/owner/repo/actions/runs/42"}`)
		case "/repos/owner/repo/actions/workflows/12":
			_, _ = io.WriteString(writer, `{"id":12,"name":"Visual Hive Scheduled","path":".github/workflows/visual-hive.yml","state":"active"}`)
		case "/repos/owner/repo/actions/runs/42/artifacts":
			_, _ = io.WriteString(writer, fmt.Sprintf(`{"total_count":2,"artifacts":[{"id":98,"name":"visual-hive-evidence","size_in_bytes":%d,"expired":false,"workflow_run":{"id":42,"repository_id":123,"head_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},{"id":99,"name":"visual-hive-bundle","size_in_bytes":%d,"expired":false,"workflow_run":{"id":42,"repository_id":123,"head_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}]}`, len(evidenceZip), len(zipData)))
		case "/repos/owner/repo/actions/artifacts/99/zip":
			writer.Header().Set("Location", server.URL+"/signed-artifact")
			writer.WriteHeader(http.StatusFound)
		case "/repos/owner/repo/actions/artifacts/98/zip":
			writer.Header().Set("Location", server.URL+"/signed-evidence")
			writer.WriteHeader(http.StatusFound)
		case "/signed-artifact":
			if request.Header.Get("Authorization") != "" {
				t.Errorf("signed artifact request leaked GitHub authorization")
			}
			writer.Header().Set("Content-Type", "application/zip")
			_, _ = writer.Write(zipData)
		case "/signed-evidence":
			if request.Header.Get("Authorization") != "" {
				t.Errorf("signed evidence request leaked GitHub authorization")
			}
			writer.Header().Set("Content-Type", "application/zip")
			_, _ = writer.Write(evidenceZip)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	request := VisualHiveArtifactRequest{
		Repository: "owner/repo", WorkflowRunID: 42, ArtifactID: 99, SourceArtifactID: 98, FetchSourceArtifact: true, DestinationDir: t.TempDir(), TargetRef: "main", MaxACMM: 6,
		ExpectedProducerGitCommit: testVisualHiveProducerCommit,
		ExpectedWorkflowName:      "Visual Hive Scheduled", ExpectedWorkflowPath: ".github/workflows/visual-hive.yml", ExpectedEvent: "schedule", ExpectedRunName: "Visual Hive Scheduled [correlation]",
	}
	bundle, verified, err := client.FetchAndVerifyVisualHiveBundle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	_, manifestDigestErr := hex.DecodeString(verified.ManifestSHA256)
	if !bundle.Validation.Trusted || !bundle.Validation.Authoritative || verified.RepositoryID != "123" || verified.WorkflowRunAttempt != "2" || verified.ArtifactID != "99" || verified.SourceArtifactID != "98" || verified.CommitSHA != testVisualHiveHeadSHA || verified.SourceArtifactPath == "" || verified.EvidenceRootPath == "" ||
		verified.WorkflowName != "Visual Hive Scheduled" || verified.WorkflowRunName != "Visual Hive Scheduled [correlation]" || verified.WorkflowPath != ".github/workflows/visual-hive.yml" ||
		verified.BundleSchemaVersion != bundle.Manifest.SchemaVersion || verified.BundleSHA256 != bundle.Manifest.OverallDigest || len(verified.ManifestSHA256) != 64 || manifestDigestErr != nil ||
		bundle.Manifest.ArtifactIndex == nil || verified.ArtifactIndexSHA256 != bundle.Manifest.ArtifactIndex.SHA256 {
		t.Fatalf("unexpected verified artifact: bundle=%+v verified=%+v", bundle.Validation, verified)
	}
	if want := filepath.Join(verified.SourceArtifactPath, ".visual-hive"); verified.EvidenceRootPath != want {
		t.Fatalf("verified evidence root = %q, want %q", verified.EvidenceRootPath, want)
	}
	if data, err := os.ReadFile(filepath.Join(verified.EvidenceRootPath, "verdict.json")); err != nil || !strings.Contains(string(data), "allContributions") {
		t.Fatalf("verified source evidence was not extracted: %q err=%v", data, err)
	}
	plan, err := verified.ImportPlan()
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := verified.SpecialistEvidenceIdentity(visualhive.FindingLifecycle{LastBundleDigest: verified.BundleSHA256, LastWorkflowRunID: verified.WorkflowRunID})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Packet().WorkflowArtifactID != "98" || evidence.ArtifactID != 99 || evidence.SourceArtifactID != 98 || evidence.VerificationReceipt == nil || evidence.VerificationReceipt.ArtifactID != "99" || evidence.VerificationReceipt.SourceArtifactID != "98" {
		t.Fatalf("fetch did not preserve distinct bundle/source artifact identities: packet=%+v evidence=%+v", plan.Packet(), evidence)
	}
	request.ExpectedWorkflowPath = ".github/workflows/hive-visual-hive.yml"
	request.DestinationDir = t.TempDir()
	if _, _, err := client.FetchAndVerifyVisualHiveBundle(context.Background(), request); err == nil || !strings.Contains(err.Error(), "definition path mismatch") {
		t.Fatalf("bundle from an unapproved workflow definition was accepted: %v", err)
	}
	request.ExpectedWorkflowPath = ".github/workflows/visual-hive.yml"
	request.ExpectedProducerGitCommit = strings.Repeat("f", 40)
	request.DestinationDir = t.TempDir()
	if _, _, err := client.FetchAndVerifyVisualHiveBundle(context.Background(), request); err == nil || !strings.Contains(err.Error(), "producer commit does not match") {
		t.Fatalf("bundle from a different Visual Hive producer commit was accepted: %v", err)
	}
}

func TestFetchAndVerifyVisualHiveBundleRequiresFullSourceArtifact(t *testing.T) {
	client := &Client{}
	bundle, verified, err := client.FetchAndVerifyVisualHiveBundle(context.Background(), VisualHiveArtifactRequest{
		Repository: "owner/repo", WorkflowRunID: 42, ArtifactID: 99, DestinationDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "full source artifact") || bundle != nil || verified != (VerifiedVisualHiveArtifact{}) {
		t.Fatalf("trusted ingestion without full source verification was accepted: bundle=%+v verified=%+v err=%v", bundle, verified, err)
	}
}

func TestFetchAndVerifyVisualHiveBundleRequiresPinnedProducerCommit(t *testing.T) {
	client := &Client{}
	_, _, err := client.FetchAndVerifyVisualHiveBundle(context.Background(), VisualHiveArtifactRequest{
		Repository: "owner/repo", WorkflowRunID: 42, ArtifactID: 99, SourceArtifactID: 98,
		FetchSourceArtifact: true, DestinationDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "expected producer commit") {
		t.Fatalf("trusted fetch without a producer pin did not fail before network access: %v", err)
	}
}

func TestFetchAndVerifyVisualHiveBundleRequiresApprovedWorkflowIdentity(t *testing.T) {
	client := &Client{}
	_, _, err := client.FetchAndVerifyVisualHiveBundle(context.Background(), VisualHiveArtifactRequest{
		Repository: "owner/repo", WorkflowRunID: 42, ArtifactID: 99, SourceArtifactID: 98,
		FetchSourceArtifact: true, DestinationDir: t.TempDir(), ExpectedProducerGitCommit: testVisualHiveProducerCommit,
	})
	if err == nil || !strings.Contains(err.Error(), "approved workflow name and path") {
		t.Fatalf("trusted fetch without an approved workflow identity did not fail before network access: %v", err)
	}
}

func TestFetchAndVerifyVisualHiveBundleRejectsPullRequestTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case "/repos/owner/repo/actions/runs/42":
			_, _ = io.WriteString(writer, `{"id":42,"name":"Visual Hive","head_branch":"main","head_sha":"abc123","event":"pull_request_target","status":"completed","conclusion":"success"}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, _, err := client.FetchAndVerifyVisualHiveBundle(context.Background(), VisualHiveArtifactRequest{
		Repository: "owner/repo", WorkflowRunID: 42, ArtifactID: 99, SourceArtifactID: 98, FetchSourceArtifact: true, DestinationDir: t.TempDir(),
		ExpectedProducerGitCommit: testVisualHiveProducerCommit,
		ExpectedWorkflowName:      "Visual Hive", ExpectedWorkflowPath: ".github/workflows/visual-hive.yml", ExpectedEvent: "pull_request_target",
	})
	if err == nil || !strings.Contains(err.Error(), "non-PR run") {
		t.Fatalf("pull_request_target evidence was accepted as trusted: %v", err)
	}
}

func TestFetchAndVerifyVisualHiveBundleRejectsWrongTargetBranch(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case "/repos/owner/repo/actions/runs/42":
			_, _ = io.WriteString(writer, `{"id":42,"name":"Visual Hive Scheduled","head_branch":"feature","head_sha":"abc123","event":"schedule","status":"completed","conclusion":"success"}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, _, err := client.FetchAndVerifyVisualHiveBundle(context.Background(), VisualHiveArtifactRequest{
		Repository: "owner/repo", WorkflowRunID: 42, ArtifactID: 99, FetchSourceArtifact: true, DestinationDir: t.TempDir(), TargetRef: "main", MaxACMM: 6,
		ExpectedProducerGitCommit: testVisualHiveProducerCommit,
		ExpectedWorkflowName:      "Visual Hive Scheduled", ExpectedWorkflowPath: ".github/workflows/visual-hive.yml", ExpectedEvent: "schedule",
	})
	if err == nil || !strings.Contains(err.Error(), "does not match target branch") {
		t.Fatalf("expected target branch rejection, got %v", err)
	}
}

func TestFetchAndVerifyVisualHiveBundleSeparatesStaticNameFromCorrelatedTitle(t *testing.T) {
	for _, test := range []struct {
		name         string
		runName      string
		displayTitle string
		want         string
	}{
		{name: "spoofed runtime name", runName: "spoof", displayTitle: "Visual Hive Scheduled [correlation]", want: "runtime name mismatch"},
		{name: "spoofed correlated title", runName: "Visual Hive Scheduled", displayTitle: "Visual Hive Scheduled", want: "correlated workflow run name mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch request.URL.Path {
				case "/repos/owner/repo":
					_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
				case "/repos/owner/repo/actions/runs/42":
					_, _ = fmt.Fprintf(writer, `{"id":42,"workflow_id":12,"name":%q,"display_title":%q,"path":".github/workflows/visual-hive.yml","head_branch":"main","head_sha":"abc123","event":"schedule","status":"completed","conclusion":"success"}`, test.runName, test.displayTitle)
				case "/repos/owner/repo/actions/workflows/12":
					_, _ = io.WriteString(writer, `{"id":12,"name":"Visual Hive Scheduled","path":".github/workflows/visual-hive.yml","state":"active"}`)
				default:
					http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
				}
			}))
			defer server.Close()

			client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			_, _, err := client.FetchAndVerifyVisualHiveBundle(context.Background(), VisualHiveArtifactRequest{
				Repository: "owner/repo", WorkflowRunID: 42, ArtifactID: 99, FetchSourceArtifact: true, DestinationDir: t.TempDir(), TargetRef: "main", MaxACMM: 6,
				ExpectedProducerGitCommit: testVisualHiveProducerCommit,
				ExpectedWorkflowName:      "Visual Hive Scheduled", ExpectedWorkflowPath: ".github/workflows/visual-hive.yml", ExpectedEvent: "schedule", ExpectedRunName: "Visual Hive Scheduled [correlation]",
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("spoofed workflow identity was accepted: %v", err)
			}
		})
	}
}

func TestFetchAndVerifyPullRequestArtifactBindsFailedExactHead(t *testing.T) {
	artifactZip := buildPRReviewZip(t)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case "/repos/owner/repo/pulls/7":
			_, _ = io.WriteString(writer, `{"number":7,"state":"open","merged":false,"head":{"sha":"pr-head","ref":"hive/repair-one"}}`)
		case "/repos/owner/repo/actions/runs":
			if request.URL.Query().Get("event") != "pull_request" || request.URL.Query().Get("head_sha") != "pr-head" {
				t.Errorf("missing exact-head run filters: %s", request.URL.RawQuery)
			}
			_, _ = io.WriteString(writer, `{"total_count":1,"workflow_runs":[{"id":77,"run_attempt":2,"name":"Visual Hive PR","path":".github/workflows/visual-hive-pr.yml","head_branch":"hive/repair-one","head_sha":"pr-head","event":"pull_request","status":"completed","conclusion":"failure","html_url":"https://github.test/owner/repo/actions/runs/77","pull_requests":[{"number":7}]}]}`)
		case "/repos/owner/repo/actions/runs/77/artifacts":
			_, _ = io.WriteString(writer, fmt.Sprintf(`{"total_count":1,"artifacts":[{"id":88,"name":"visual-hive-pr","size_in_bytes":%d,"expired":false,"workflow_run":{"id":77,"repository_id":123,"head_sha":"pr-head"}}]}`, len(artifactZip)))
		case "/repos/owner/repo/actions/artifacts/88/zip":
			writer.Header().Set("Location", server.URL+"/signed-pr-artifact")
			writer.WriteHeader(http.StatusFound)
		case "/signed-pr-artifact":
			if request.Header.Get("Authorization") != "" {
				t.Errorf("signed PR artifact request leaked GitHub authorization")
			}
			writer.Header().Set("Content-Type", "application/zip")
			_, _ = writer.Write(artifactZip)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	verified, err := client.FetchAndVerifyPullRequestArtifact(context.Background(), PullRequestArtifactRequest{
		Repository: "owner/repo", PullRequestNumber: 7, ExpectedWorkflowRunID: 77, ExpectedHeadSHA: "pr-head", ExpectedHeadBranch: "hive/repair-one",
		ExpectedWorkflowName: "Visual Hive PR", ExpectedWorkflowPath: ".github/workflows/visual-hive-pr.yml", ArtifactName: "visual-hive-pr", DestinationDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if verified.RepositoryID != "123" || verified.PullRequestNumber != 7 || verified.WorkflowRunID != 77 || verified.WorkflowRunAttempt != 2 || verified.ArtifactID != 88 ||
		verified.Conclusion != "failure" || verified.CommitSHA != "pr-head" || verified.WorkflowName != "Visual Hive PR" ||
		verified.ReviewSchemaVersion != visualhive.ReviewEvidenceSchemaVersion || len(verified.ArtifactIndexSHA256) != 64 || verified.EvidenceRootPath == "" {
		t.Fatalf("unexpected verified PR artifact: %+v", verified)
	}
	if data, err := os.ReadFile(filepath.Join(verified.ArtifactRoot, ".visual-hive", "report.json")); err != nil || !strings.Contains(string(data), "missing_baseline") {
		t.Fatalf("PR review evidence was not extracted: %q err=%v", data, err)
	}
	if _, err := client.FetchAndVerifyPullRequestArtifact(context.Background(), PullRequestArtifactRequest{
		Repository: "owner/repo", PullRequestNumber: 7, ExpectedWorkflowRunID: 76, ExpectedHeadSHA: "pr-head", ExpectedHeadBranch: "hive/repair-one",
		ExpectedWorkflowName: "Visual Hive PR", ExpectedWorkflowPath: ".github/workflows/visual-hive-pr.yml", ArtifactName: "visual-hive-pr", DestinationDir: t.TempDir(),
	}); err == nil || !strings.Contains(err.Error(), "no completed failed PR run") {
		t.Fatalf("wrong workflow run identity was accepted: %v", err)
	}
}

func TestExtractVisualHiveZipRejectsTraversal(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "unsafe.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	entry, err := archive.Create("../escape.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("no"))
	_ = archive.Close()
	_ = file.Close()
	if err := extractVisualHiveZip(zipPath, t.TempDir()); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("expected traversal rejection, got %v", err)
	}
}

func TestSourceArtifactExtractionUsesDistinctWorkflowLimits(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "source.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for index := 0; index < maxVisualHiveArchiveFiles+1; index++ {
		writeZipEntry(t, archive, fmt.Sprintf(".visual-hive/evidence/%04d.json", index), []byte("{}"))
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractVisualHiveZip(zipPath, t.TempDir()); err == nil || !strings.Contains(err.Error(), "file count") {
		t.Fatalf("compact limit unexpectedly accepted a source-sized archive: %v", err)
	}
	if err := extractVisualHiveZipWithLimits(zipPath, t.TempDir(), maxVisualHiveSourceArtifactBytes, maxVisualHiveSourceArchiveFiles); err != nil {
		t.Fatalf("source workflow limits rejected %d files: %v", maxVisualHiveArchiveFiles+1, err)
	}
}

func TestSourceArtifactIdentityMismatchIsRejected(t *testing.T) {
	artifact := &gh.Artifact{WorkflowRun: &gh.ArtifactWorkflowRun{ID: gh.Ptr(int64(43)), RepositoryID: gh.Ptr(int64(123)), HeadSHA: gh.Ptr("abc123")}}
	if err := validateVisualHiveArtifactIdentity(artifact, 42, 123, "abc123", "source"); err == nil || !strings.Contains(err.Error(), "source artifact workflow run mismatch") {
		t.Fatalf("source artifact identity mismatch was accepted: %v", err)
	}
}

func buildVerifiedV3Artifacts(t *testing.T) ([]byte, []byte) {
	t.Helper()
	beadsData := []byte("[]")
	verdictData := []byte(`{"schemaVersion":"visual-hive.verdict.v1","allContributions":[]}`)
	paritySummary := map[string]int{"expected": 1, "actual": 1, "present": 1, "blocked": 0, "missing": 0, "unexpected": 0, "mismatched": 0}
	cliIdentity := map[string]interface{}{"path": "doctor", "aliases": []string{}, "contractSha256": strings.Repeat("a", 64)}
	parityData, err := json.Marshal(map[string]interface{}{
		"schemaVersion": "visual-hive.capability-parity.v1", "baselineVersion": "visual-hive.capability-baseline.v1",
		"generatedAt": "2026-07-09T12:00:00.000Z", "status": "passed", "runtimeStatus": "ready",
		"summary": paritySummary,
		"domains": testV3CapabilityDomains("cli", paritySummary),
		"checks":  []interface{}{map[string]interface{}{"domain": "cli", "key": "doctor", "status": "present", "parity": true, "message": "present", "expected": cliIdentity, "actual": cliIdentity}},
	})
	if err != nil {
		t.Fatal(err)
	}
	type indexEntry struct {
		Path             string   `json:"path"`
		Kind             string   `json:"kind"`
		ContentType      string   `json:"contentType"`
		Bytes            int      `json:"bytes"`
		SHA256           string   `json:"sha256"`
		SafeToRender     bool     `json:"safeToRender"`
		PreviewTruncated bool     `json:"previewTruncated"`
		PreviewRedacted  bool     `json:"previewRedacted"`
		Labels           []string `json:"labels"`
	}
	entries := []indexEntry{
		{Path: ".visual-hive/capability-parity.json", Kind: "json", ContentType: "application/json", Bytes: len(parityData), SHA256: testDigest(parityData), SafeToRender: true, Labels: []string{}},
		{Path: ".visual-hive/hive/beads.json", Kind: "json", ContentType: "application/json", Bytes: len(beadsData), SHA256: testDigest(beadsData), SafeToRender: true, Labels: []string{}},
		{Path: ".visual-hive/verdict.json", Kind: "json", ContentType: "application/json", Bytes: len(verdictData), SHA256: testDigest(verdictData), SafeToRender: true, Labels: []string{}},
	}
	totalBytes := len(parityData) + len(beadsData) + len(verdictData)
	indexData, err := json.Marshal(map[string]interface{}{
		"schemaVersion": 1, "project": "demo", "generatedAt": "2026-07-09T12:00:00.000Z", "root": ".visual-hive", "contentAddressed": true, "complete": true,
		"summary":   map[string]int{"discoveredArtifactCount": 3, "artifactCount": 3, "omittedArtifactCount": 0, "totalBytes": totalBytes, "json": 3, "markdown": 0, "image": 0, "text": 0, "typescript": 0, "yaml": 0, "log": 0, "other": 0, "previewed": 0, "redactedPreviews": 0, "truncatedPreviews": 0},
		"artifacts": entries, "warnings": []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second).Add(time.Millisecond)
	manifest := visualhive.Manifest{
		SchemaVersion: visualhive.ManifestSchemaV3, DigestAlgorithm: visualhive.ContentAddressedDigestAlgorithm, BundleID: "verified-bundle", GeneratedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		Producer: visualhive.Producer{Name: "visual-hive", Version: "0.4.0", GitCommit: testVisualHiveProducerCommit},
		Source:   visualhive.Source{Repository: "owner/repo", RepositoryID: "123", Ref: "refs/heads/main", CommitSHA: testVisualHiveHeadSHA, Event: "schedule", WorkflowName: "Visual Hive Scheduled", WorkflowRunID: "42", WorkflowRunAttempt: "2", WorkflowArtifactID: "98", Conclusion: "success", Trusted: false},
		Project:  "demo", Mode: "measured", Verdict: "ready", ACMMRequest: 4,
		Scan:         visualhive.Scan{Scope: "full", AuthoritativeForResolution: true, EvaluatedContracts: []string{}, EvaluatedFiles: []string{}, TestPlanVersion: "plan-1", ToolRegistryVersion: "tools-1"},
		Observations: []visualhive.Observation{},
		Files: []visualhive.File{
			{Path: "files/.visual-hive/artifacts-index.json", SourcePath: ".visual-hive/artifacts-index.json", SHA256: testDigest(indexData), Size: int64(len(indexData)), MediaType: "application/json"},
			{Path: "files/.visual-hive/capability-parity.json", SourcePath: ".visual-hive/capability-parity.json", SHA256: testDigest(parityData), Size: int64(len(parityData)), MediaType: "application/json"},
			{Path: "files/.visual-hive/hive/beads.json", SourcePath: ".visual-hive/hive/beads.json", SHA256: testDigest(beadsData), Size: int64(len(beadsData)), MediaType: "application/json"},
		},
		ArtifactIndex:    &visualhive.ArtifactIndexBinding{Path: "files/.visual-hive/artifacts-index.json", SourcePath: ".visual-hive/artifacts-index.json", SHA256: testDigest(indexData), SchemaVersion: 1, ContentAddressed: true, Complete: true, ArtifactCount: 3, TotalBytes: int64(totalBytes)},
		CapabilityParity: &visualhive.CapabilityParityBinding{Path: "files/.visual-hive/capability-parity.json", SourcePath: ".visual-hive/capability-parity.json", SHA256: testDigest(parityData), SchemaVersion: "visual-hive.capability-parity.v1", BaselineVersion: "visual-hive.capability-baseline.v1", Status: "passed", RuntimeStatus: "ready", Summary: visualhive.CapabilityParitySummary{Expected: 1, Actual: 1, Present: 1}},
		ReplayProtection: visualhive.ReplayProtection{Nonce: "verified-bundle"},
		Provenance:       visualhive.Provenance{Kind: "github-actions", AttestationRequired: true},
		Safety:           visualhive.Safety{AtomicWrite: true, PathsAreRelative: true, DigestsRequired: true, ProducerCountersAreAdvisory: true, ProducerTrustClaimIsAdvisory: true, AbsenceRequiresAuthoritativeScan: true},
	}
	manifest.ReplayProtection.Key = testDigest([]byte("visual-hive.bundle.replay.v3\x00owner/repo\x00123\x00" + testVisualHiveHeadSHA + "\x0042\x002\x0098\x00verified-bundle"))
	manifest.OverallDigest = testV3BundleDigest(manifest)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	buffer := new(bytes.Buffer)
	archive := zip.NewWriter(buffer)
	writeZipEntry(t, archive, ".visual-hive/bundles/verified-bundle/manifest.json", manifestData)
	writeZipEntry(t, archive, ".visual-hive/bundles/verified-bundle/files/.visual-hive/artifacts-index.json", indexData)
	writeZipEntry(t, archive, ".visual-hive/bundles/verified-bundle/files/.visual-hive/capability-parity.json", parityData)
	writeZipEntry(t, archive, ".visual-hive/bundles/verified-bundle/files/.visual-hive/hive/beads.json", beadsData)
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	evidence := new(bytes.Buffer)
	evidenceArchive := zip.NewWriter(evidence)
	writeZipEntry(t, evidenceArchive, ".visual-hive/artifacts-index.json", indexData)
	writeZipEntry(t, evidenceArchive, ".visual-hive/capability-parity.json", parityData)
	writeZipEntry(t, evidenceArchive, ".visual-hive/hive/beads.json", beadsData)
	writeZipEntry(t, evidenceArchive, ".visual-hive/verdict.json", verdictData)
	if err := evidenceArchive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes(), evidence.Bytes()
}

func testV3CapabilityDomains(active string, counts map[string]int) []map[string]interface{} {
	names := []string{"cli", "schemas", "evidenceResources", "artifactSurfaces", "planModes", "workflowLanes", "mutationOperators", "deterministicPrimitives", "providers", "openSourceAdapters", "controlPlane"}
	domains := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		summary := map[string]int{"expected": 0, "actual": 0, "present": 0, "blocked": 0, "missing": 0, "unexpected": 0, "mismatched": 0}
		if name == active {
			summary = counts
		}
		domains = append(domains, map[string]interface{}{
			"domain": name, "expected": summary["expected"], "actual": summary["actual"], "present": summary["present"], "blocked": summary["blocked"], "missing": summary["missing"], "unexpected": summary["unexpected"], "mismatched": summary["mismatched"],
		})
	}
	return domains
}

func buildPRReviewZip(t *testing.T) []byte {
	t.Helper()
	report := []byte(`{"status":"failed","kind":"missing_baseline"}`)
	index, err := json.Marshal(map[string]interface{}{
		"schemaVersion": 1, "project": "demo", "generatedAt": "2026-07-15T12:00:00.000Z", "root": ".visual-hive", "contentAddressed": true, "complete": true,
		"summary": map[string]int{
			"discoveredArtifactCount": 1, "artifactCount": 1, "omittedArtifactCount": 0, "totalBytes": len(report),
			"json": 1, "markdown": 0, "image": 0, "text": 0, "typescript": 0, "yaml": 0, "log": 0, "other": 0,
			"previewed": 0, "redactedPreviews": 0, "truncatedPreviews": 0,
		},
		"artifacts": []map[string]interface{}{{
			"path": ".visual-hive/report.json", "kind": "json", "contentType": "application/json", "bytes": len(report), "sha256": testDigest(report),
			"safeToRender": true, "previewTruncated": false, "previewRedacted": false, "labels": []string{},
		}},
		"warnings": []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	buffer := new(bytes.Buffer)
	archive := zip.NewWriter(buffer)
	writeZipEntry(t, archive, ".visual-hive/report.json", report)
	writeZipEntry(t, archive, ".visual-hive/artifacts-index.json", index)
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func writeZipEntry(t *testing.T, archive *zip.Writer, name string, data []byte) {
	t.Helper()
	entry, err := archive.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(data); err != nil {
		t.Fatal(err)
	}
}

type testDigestField struct {
	name, value string
}

func testV3BundleDigest(manifest visualhive.Manifest) string {
	stream := &bytes.Buffer{}
	stream.WriteString(visualhive.ContentAddressedDigestAlgorithm)
	files := make([][]byte, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		files = append(files, testDigestRecord("file",
			testDigestField{"path", file.Path}, testDigestField{"sourcePath", file.SourcePath}, testDigestField{"sha256", file.SHA256},
			testDigestField{"size", strconv.FormatInt(file.Size, 10)}, testDigestField{"mediaType", file.MediaType}, testDigestField{"schemaVersion", file.SchemaVersion}))
	}
	testDigestCollection(stream, "files", files)
	testDigestCollection(stream, "observations", nil)
	stream.Write(testDigestRecord("scan",
		testDigestField{"scope", manifest.Scan.Scope}, testDigestField{"authoritativeForResolution", strconv.FormatBool(manifest.Scan.AuthoritativeForResolution)},
		testDigestArray("evaluatedContracts", manifest.Scan.EvaluatedContracts), testDigestArray("evaluatedFiles", manifest.Scan.EvaluatedFiles),
		testDigestField{"testPlanVersion", manifest.Scan.TestPlanVersion}, testDigestField{"toolRegistryVersion", manifest.Scan.ToolRegistryVersion}))
	stream.Write(testDigestRecord("source",
		testDigestField{"repository", manifest.Source.Repository}, testDigestField{"repositoryId", manifest.Source.RepositoryID}, testDigestField{"ref", manifest.Source.Ref},
		testDigestField{"commitSha", manifest.Source.CommitSHA}, testDigestField{"event", manifest.Source.Event}, testDigestField{"workflowName", manifest.Source.WorkflowName},
		testDigestField{"workflowRunId", manifest.Source.WorkflowRunID}, testDigestField{"workflowRunAttempt", manifest.Source.WorkflowRunAttempt},
		testDigestField{"workflowArtifactId", manifest.Source.WorkflowArtifactID}, testDigestField{"conclusion", manifest.Source.Conclusion}, testDigestField{"trusted", strconv.FormatBool(manifest.Source.Trusted)}))
	stream.Write(testDigestRecord("metadata",
		testDigestField{"generatedAt", manifest.GeneratedAt.UTC().Format("2006-01-02T15:04:05.000Z")}, testDigestField{"expiresAt", manifest.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")},
		testDigestField{"project", manifest.Project}, testDigestField{"mode", manifest.Mode}, testDigestField{"verdict", manifest.Verdict},
		testDigestField{"acmmRequest", strconv.Itoa(manifest.ACMMRequest)}, testDigestField{"externalCallsMade", strconv.Itoa(manifest.ExternalCallsMade)},
		testDigestField{"producerName", manifest.Producer.Name}, testDigestField{"producerVersion", manifest.Producer.Version}, testDigestField{"producerGitCommit", manifest.Producer.GitCommit}))
	stream.Write(testDigestRecord("artifactIndex",
		testDigestField{"path", manifest.ArtifactIndex.Path}, testDigestField{"sourcePath", manifest.ArtifactIndex.SourcePath}, testDigestField{"sha256", manifest.ArtifactIndex.SHA256},
		testDigestField{"schemaVersion", strconv.Itoa(manifest.ArtifactIndex.SchemaVersion)}, testDigestField{"contentAddressed", strconv.FormatBool(manifest.ArtifactIndex.ContentAddressed)},
		testDigestField{"complete", strconv.FormatBool(manifest.ArtifactIndex.Complete)}, testDigestField{"artifactCount", strconv.FormatInt(manifest.ArtifactIndex.ArtifactCount, 10)},
		testDigestField{"totalBytes", strconv.FormatInt(manifest.ArtifactIndex.TotalBytes, 10)}))
	summary := manifest.CapabilityParity.Summary
	stream.Write(testDigestRecord("capabilityParity",
		testDigestField{"path", manifest.CapabilityParity.Path}, testDigestField{"sourcePath", manifest.CapabilityParity.SourcePath}, testDigestField{"sha256", manifest.CapabilityParity.SHA256},
		testDigestField{"schemaVersion", manifest.CapabilityParity.SchemaVersion}, testDigestField{"baselineVersion", manifest.CapabilityParity.BaselineVersion},
		testDigestField{"status", manifest.CapabilityParity.Status}, testDigestField{"runtimeStatus", manifest.CapabilityParity.RuntimeStatus},
		testDigestField{"expected", strconv.FormatInt(summary.Expected, 10)}, testDigestField{"actual", strconv.FormatInt(summary.Actual, 10)},
		testDigestField{"present", strconv.FormatInt(summary.Present, 10)}, testDigestField{"blocked", strconv.FormatInt(summary.Blocked, 10)},
		testDigestField{"missing", strconv.FormatInt(summary.Missing, 10)}, testDigestField{"unexpected", strconv.FormatInt(summary.Unexpected, 10)},
		testDigestField{"mismatched", strconv.FormatInt(summary.Mismatched, 10)}))
	stream.Write(testDigestRecord("replay", testDigestField{"key", manifest.ReplayProtection.Key}))
	return testDigest(stream.Bytes())
}

func testDigestArray(name string, values []string) testDigestField {
	// Arrays use a sentinel prefix only inside this test helper; testDigestRecord
	// decodes it into the producer's A/E field representation.
	return testDigestField{name: "\x00array\x00" + name, value: strings.Join(values, "\x00")}
}

func testDigestRecord(domain string, fields ...testDigestField) []byte {
	record := &bytes.Buffer{}
	record.WriteByte('R')
	testDigestLP(record, []byte(domain))
	for _, field := range fields {
		if strings.HasPrefix(field.name, "\x00array\x00") {
			record.WriteByte('A')
			testDigestLP(record, []byte(strings.TrimPrefix(field.name, "\x00array\x00")))
			values := []string{}
			if field.value != "" {
				values = strings.Split(field.value, "\x00")
			}
			testDigestLP(record, []byte(strconv.Itoa(len(values))))
			for _, value := range values {
				record.WriteByte('E')
				testDigestLP(record, []byte(value))
			}
			continue
		}
		record.WriteByte('S')
		testDigestLP(record, []byte(field.name))
		testDigestLP(record, []byte(field.value))
	}
	record.WriteByte('Z')
	return record.Bytes()
}

func testDigestCollection(stream *bytes.Buffer, domain string, records [][]byte) {
	sort.Slice(records, func(i, j int) bool { return bytes.Compare(records[i], records[j]) < 0 })
	stream.WriteByte('C')
	testDigestLP(stream, []byte(domain))
	testDigestLP(stream, []byte(strconv.Itoa(len(records))))
	for _, record := range records {
		stream.WriteByte('I')
		testDigestLP(stream, record)
	}
}

func testDigestLP(target *bytes.Buffer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	target.Write(length[:])
	target.Write(value)
}

func testDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
