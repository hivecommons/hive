package github

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestFetchAndVerifyVisualHiveBundleBindsGitHubProvenance(t *testing.T) {
	zipData := buildVerifiedBundleZip(t)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case "/repos/owner/repo/actions/runs/42":
			_, _ = io.WriteString(writer, `{"id":42,"name":"Visual Hive Scheduled","head_branch":"main","head_sha":"abc123","event":"schedule","status":"completed","conclusion":"success","html_url":"https://github.test/owner/repo/actions/runs/42"}`)
		case "/repos/owner/repo/actions/runs/42/artifacts":
			_, _ = io.WriteString(writer, fmt.Sprintf(`{"total_count":2,"artifacts":[{"id":98,"name":"visual-hive-evidence","size_in_bytes":100,"expired":false,"workflow_run":{"id":42,"repository_id":123,"head_sha":"abc123"}},{"id":99,"name":"visual-hive-bundle","size_in_bytes":%d,"expired":false,"workflow_run":{"id":42,"repository_id":123,"head_sha":"abc123"}}]}`, len(zipData)))
		case "/repos/owner/repo/actions/artifacts/99/zip":
			writer.Header().Set("Location", server.URL+"/signed-artifact")
			writer.WriteHeader(http.StatusFound)
		case "/signed-artifact":
			if request.Header.Get("Authorization") != "" {
				t.Errorf("signed artifact request leaked GitHub authorization")
			}
			writer.Header().Set("Content-Type", "application/zip")
			_, _ = writer.Write(zipData)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	bundle, verified, err := client.FetchAndVerifyVisualHiveBundle(context.Background(), VisualHiveArtifactRequest{
		Repository: "owner/repo", WorkflowRunID: 42, ArtifactID: 99, SourceArtifactID: 98, DestinationDir: t.TempDir(), TargetRef: "main", MaxACMM: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Validation.Trusted || verified.RepositoryID != "123" || verified.ArtifactID != "99" || verified.SourceArtifactID != "98" || verified.CommitSHA != "abc123" {
		t.Fatalf("unexpected verified artifact: bundle=%+v verified=%+v", bundle.Validation, verified)
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
		Repository: "owner/repo", WorkflowRunID: 42, ArtifactID: 99, DestinationDir: t.TempDir(), TargetRef: "main", MaxACMM: 6,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match target branch") {
		t.Fatalf("expected target branch rejection, got %v", err)
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

func buildVerifiedBundleZip(t *testing.T) []byte {
	t.Helper()
	beadsData := []byte("[]")
	fileDigest := testDigest(beadsData)
	now := time.Now().UTC().Truncate(time.Second)
	manifest := visualhive.Manifest{
		SchemaVersion: visualhive.ManifestSchema, BundleID: "verified-bundle", GeneratedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		Producer: visualhive.Producer{Name: "visual-hive", Version: "0.2.0", GitCommit: "producer-sha"},
		Source:   visualhive.Source{Repository: "owner/repo", RepositoryID: "123", Ref: "refs/heads/main", CommitSHA: "abc123", Event: "schedule", WorkflowName: "Visual Hive Scheduled", WorkflowRunID: "42", WorkflowArtifactID: "98", Conclusion: "success", Trusted: false},
		Project:  "demo", Mode: "measured", Verdict: "ready", ACMMRequest: 4,
		Scan:             visualhive.Scan{Scope: "full", AuthoritativeForResolution: true, EvaluatedContracts: []string{}, EvaluatedFiles: []string{}, TestPlanVersion: "plan-1", ToolRegistryVersion: "tools-1"},
		Observations:     []visualhive.Observation{},
		Files:            []visualhive.File{{Path: "files/.visual-hive/hive/beads.json", SourcePath: ".visual-hive/hive/beads.json", SHA256: fileDigest, Size: int64(len(beadsData)), MediaType: "application/json"}},
		ReplayProtection: visualhive.ReplayProtection{Nonce: "verified-bundle"},
		Provenance:       visualhive.Provenance{Kind: "github-actions", AttestationRequired: true},
		Safety:           visualhive.Safety{AtomicWrite: true, PathsAreRelative: true, DigestsRequired: true, ProducerCountersAreAdvisory: true, ProducerTrustClaimIsAdvisory: true, AbsenceRequiresAuthoritativeScan: true},
	}
	manifest.ReplayProtection.Key = testDigest([]byte("owner/repo\x00abc123\x0042\x00verified-bundle"))
	manifest.OverallDigest = testDigest([]byte(strings.Join([]string{
		"file\x00files/.visual-hive/hive/beads.json\x00" + fileDigest + "\x002",
		"scan\x00full\x00true\x00\x00\x00plan-1\x00tools-1",
		"source\x00owner/repo\x00123\x00refs/heads/main\x00abc123\x0042\x0098\x00success",
		"replay\x00" + manifest.ReplayProtection.Key,
	}, "\n")))
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	buffer := new(bytes.Buffer)
	archive := zip.NewWriter(buffer)
	writeZipEntry(t, archive, ".visual-hive/bundles/verified-bundle/manifest.json", manifestData)
	writeZipEntry(t, archive, ".visual-hive/bundles/verified-bundle/files/.visual-hive/hive/beads.json", beadsData)
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

func testDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
