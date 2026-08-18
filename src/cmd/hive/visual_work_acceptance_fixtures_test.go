package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/visualhive"
)

const (
	acceptanceSourceWorkflowName = "Hive Visual Hive Production"
	acceptanceSourceWorkflowPath = ".github/workflows/hive-visual-hive.yml"
	acceptancePRWorkflowName     = "Visual Hive PR"
	acceptancePRWorkflowPath     = ".github/workflows/visual-hive-pr.yml"
)

type acceptanceSourceFixtureOptions struct {
	BundleID         string
	RunID            int64
	RunAttempt       int
	BundleArtifactID int64
	SourceArtifactID int64
	BaseSHA          string
	ProducerCommit   string
	IncludeFinding   bool
	Authoritative    bool
}

type acceptanceSourceFixture struct {
	BundleID         string
	RunID            int64
	RunAttempt       int
	BundleArtifactID int64
	SourceArtifactID int64
	BaseSHA          string
	BundleZip        []byte
	SourceZip        []byte
}

func seedAcceptanceRepository(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	remote := filepath.Join(root, "remote.git")
	acceptanceGit(t, root, "init", "--bare", remote)
	acceptanceGit(t, root, "init", "-b", "main", repository)
	acceptanceGit(t, repository, "config", "user.name", "Hive Acceptance")
	acceptanceGit(t, repository, "config", "user.email", "hive-acceptance@example.test")
	if err := os.MkdirAll(filepath.Join(repository, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repository, "public", "reviewed-reference"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "src", "value.txt"), []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "visual-hive.config.yaml"), []byte("visual:\n  snapshotDir: public/reviewed-reference\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "public", "reviewed-reference", "home.png"), []byte("reviewed-baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	acceptanceGit(t, repository, "add", "src/value.txt", "visual-hive.config.yaml", "public/reviewed-reference/home.png")
	acceptanceGit(t, repository, "commit", "-m", "seed broken value")
	acceptanceGit(t, repository, "remote", "add", "origin", remote)
	acceptanceGit(t, repository, "push", "-u", "origin", "main")
	return repository, remote, strings.TrimSpace(acceptanceGit(t, repository, "rev-parse", "HEAD"))
}

func acceptanceGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	commandArgs := append([]string{"-C", directory}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(commandArgs, " "), err, output)
	}
	return string(output)
}

func buildAcceptanceSourceFixture(t *testing.T, options acceptanceSourceFixtureOptions) acceptanceSourceFixture {
	t.Helper()
	beadsData := []byte("[]")
	verdictData := []byte(`{"schemaVersion":"visual-hive.verdict.v1","allContributions":[]}`)
	if options.IncludeFinding {
		verdictData = []byte(`{"schemaVersion":"visual-hive.verdict.v1","allContributions":[{"source":"visual","kind":"visual_regression","status":"failed","gating":true,"mode":"measured","contractId":"contract/app-shell","targetId":"app","reason":"value remains broken","key":"visual.visual_regression.app-shell","authority":"deterministic"}]}`)
	}
	parityData := acceptanceParityData(t)
	files := map[string][]byte{
		".visual-hive/capability-parity.json": parityData,
		".visual-hive/hive/beads.json":        beadsData,
		".visual-hive/verdict.json":           verdictData,
	}
	indexData, indexSummary := acceptanceArtifactIndex(t, files)
	now := time.Now().UTC().Truncate(time.Second).Add(time.Millisecond)
	manifest := visualhive.Manifest{
		SchemaVersion: visualhive.ManifestSchemaV3, DigestAlgorithm: visualhive.ContentAddressedDigestAlgorithm,
		BundleID: options.BundleID, GeneratedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		Producer: visualhive.Producer{Name: "visual-hive", Version: "0.4.0", GitCommit: options.ProducerCommit},
		Source: visualhive.Source{Repository: "owner/repo", RepositoryID: "123", Ref: "refs/heads/main", CommitSHA: options.BaseSHA,
			Event: "workflow_dispatch", WorkflowName: acceptanceSourceWorkflowName, WorkflowRunID: strconv.FormatInt(options.RunID, 10),
			WorkflowRunAttempt: strconv.Itoa(options.RunAttempt), WorkflowArtifactID: strconv.FormatInt(options.SourceArtifactID, 10), Conclusion: "success", Trusted: false},
		Project: "acceptance", Mode: "measured", Verdict: "ready", ACMMRequest: 4,
		Scan: visualhive.Scan{Scope: "changed-files", AuthoritativeForResolution: options.Authoritative,
			EvaluatedContracts: []string{}, EvaluatedFiles: []string{}, TestPlanVersion: "acceptance-plan-v1", ToolRegistryVersion: "acceptance-tools-v1"},
		Observations: []visualhive.Observation{},
		Files: []visualhive.File{
			{Path: "files/.visual-hive/artifacts-index.json", SourcePath: ".visual-hive/artifacts-index.json", SHA256: acceptanceSHA256(indexData), Size: int64(len(indexData)), MediaType: "application/json"},
			{Path: "files/.visual-hive/capability-parity.json", SourcePath: ".visual-hive/capability-parity.json", SHA256: acceptanceSHA256(parityData), Size: int64(len(parityData)), MediaType: "application/json"},
			{Path: "files/.visual-hive/hive/beads.json", SourcePath: ".visual-hive/hive/beads.json", SHA256: acceptanceSHA256(beadsData), Size: int64(len(beadsData)), MediaType: "application/json"},
		},
		ArtifactIndex:    &visualhive.ArtifactIndexBinding{Path: "files/.visual-hive/artifacts-index.json", SourcePath: ".visual-hive/artifacts-index.json", SHA256: acceptanceSHA256(indexData), SchemaVersion: 1, ContentAddressed: true, Complete: true, ArtifactCount: indexSummary.ArtifactCount, TotalBytes: indexSummary.TotalBytes},
		CapabilityParity: &visualhive.CapabilityParityBinding{Path: "files/.visual-hive/capability-parity.json", SourcePath: ".visual-hive/capability-parity.json", SHA256: acceptanceSHA256(parityData), SchemaVersion: "visual-hive.capability-parity.v1", BaselineVersion: "visual-hive.capability-baseline.v1", Status: "passed", RuntimeStatus: "ready", Summary: visualhive.CapabilityParitySummary{Expected: 1, Actual: 1, Present: 1}},
		ReplayProtection: visualhive.ReplayProtection{Nonce: options.BundleID},
		Provenance:       visualhive.Provenance{Kind: "github-actions", AttestationRequired: true},
		Safety:           visualhive.Safety{AtomicWrite: true, PathsAreRelative: true, DigestsRequired: true, ProducerCountersAreAdvisory: true, ProducerTrustClaimIsAdvisory: true, AbsenceRequiresAuthoritativeScan: true},
	}
	if options.Authoritative {
		manifest.Scan.Scope = "full"
		manifest.Scan.EvaluatedContracts = []string{"contract/app-shell"}
		manifest.Scan.EvaluatedFiles = []string{"src/value.txt"}
	}
	if options.IncludeFinding {
		root := "visual-regression/source"
		fingerprint := acceptanceSHA256([]byte("owner/repo\x00" + root))
		manifest.Observations = []visualhive.Observation{{
			Fingerprint: root, RepositoryFingerprint: fingerprint, PublicationRole: "canonical", RootCauseKey: root, BlockedByRootKeys: []string{},
			State: "present", IssueKind: "visual_regression", Severity: "high", OwningAgentHint: "hive/quality",
			Title: "visual_regression: governed value is broken", Body: "Deterministic Visual Hive evidence shows the governed value is broken.",
			Labels: []string{"visual-hive"}, SourceArtifacts: []string{".visual-hive/verdict.json"}, AffectedContracts: []string{"contract/app-shell"},
			ValidationCommand: "git diff --check", ObservedAt: "2026-07-16T12:00:00.000Z", FirstSeenAt: "2026-07-16T12:00:00.000Z", SourceArtifact: ".visual-hive/verdict.json",
		}}
	}
	manifest.ReplayProtection.Key = acceptanceSHA256([]byte(strings.Join([]string{
		"visual-hive.bundle.replay.v3", "owner/repo", "123", options.BaseSHA, strconv.FormatInt(options.RunID, 10), strconv.Itoa(options.RunAttempt), strconv.FormatInt(options.SourceArtifactID, 10), options.BundleID,
	}, "\x00")))
	manifest.OverallDigest = acceptanceV3BundleDigest(manifest)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	bundleEntries := map[string][]byte{
		filepath.ToSlash(filepath.Join(".visual-hive", "bundles", options.BundleID, "manifest.json")):                                   manifestData,
		filepath.ToSlash(filepath.Join(".visual-hive", "bundles", options.BundleID, "files", ".visual-hive", "artifacts-index.json")):   indexData,
		filepath.ToSlash(filepath.Join(".visual-hive", "bundles", options.BundleID, "files", ".visual-hive", "capability-parity.json")): parityData,
		filepath.ToSlash(filepath.Join(".visual-hive", "bundles", options.BundleID, "files", ".visual-hive", "hive", "beads.json")):     beadsData,
	}
	sourceEntries := map[string][]byte{".visual-hive/artifacts-index.json": indexData}
	for path, data := range files {
		sourceEntries[path] = data
	}
	return acceptanceSourceFixture{BundleID: options.BundleID, RunID: options.RunID, RunAttempt: options.RunAttempt,
		BundleArtifactID: options.BundleArtifactID, SourceArtifactID: options.SourceArtifactID, BaseSHA: options.BaseSHA,
		BundleZip: acceptanceZip(t, bundleEntries), SourceZip: acceptanceZip(t, sourceEntries)}
}

func acceptanceParityData(t *testing.T) []byte {
	t.Helper()
	summary := map[string]int{"expected": 1, "actual": 1, "present": 1, "blocked": 0, "missing": 0, "unexpected": 0, "mismatched": 0}
	identity := map[string]any{"path": "doctor", "aliases": []string{}, "contractSha256": strings.Repeat("a", 64)}
	data, err := json.Marshal(map[string]any{
		"schemaVersion": "visual-hive.capability-parity.v1", "baselineVersion": "visual-hive.capability-baseline.v1", "generatedAt": "2026-07-16T12:00:00.000Z",
		"status": "passed", "runtimeStatus": "ready", "summary": summary, "domains": acceptanceCapabilityDomains("cli", summary),
		"checks": []any{map[string]any{"domain": "cli", "key": "doctor", "status": "present", "parity": true, "message": "present", "expected": identity, "actual": identity}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func acceptanceCapabilityDomains(active string, counts map[string]int) []map[string]any {
	names := []string{"cli", "schemas", "evidenceResources", "artifactSurfaces", "planModes", "workflowLanes", "mutationOperators", "deterministicPrimitives", "providers", "openSourceAdapters", "controlPlane"}
	domains := make([]map[string]any, 0, len(names))
	for _, name := range names {
		current := map[string]int{"expected": 0, "actual": 0, "present": 0, "blocked": 0, "missing": 0, "unexpected": 0, "mismatched": 0}
		if name == active {
			current = counts
		}
		domains = append(domains, map[string]any{"domain": name, "expected": current["expected"], "actual": current["actual"], "present": current["present"], "blocked": current["blocked"], "missing": current["missing"], "unexpected": current["unexpected"], "mismatched": current["mismatched"]})
	}
	return domains
}

func acceptanceArtifactIndex(t *testing.T, files map[string][]byte) ([]byte, visualhive.ArtifactIndexSummary) {
	t.Helper()
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	entries := make([]visualhive.ArtifactIndexEntry, 0, len(paths))
	summary := visualhive.ArtifactIndexSummary{}
	for _, path := range paths {
		data := files[path]
		kind, contentType := "text", "text/plain"
		switch {
		case strings.HasSuffix(path, ".json"):
			kind, contentType, summary.JSON = "json", "application/json", summary.JSON+1
		case strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml"):
			kind, contentType, summary.YAML = "yaml", "application/yaml", summary.YAML+1
		case strings.HasSuffix(path, ".png"):
			kind, contentType, summary.Image = "image", "image/png", summary.Image+1
		default:
			summary.Text++
		}
		entries = append(entries, visualhive.ArtifactIndexEntry{Path: path, Kind: kind, ContentType: contentType, Bytes: int64(len(data)), SHA256: acceptanceSHA256(data), SafeToRender: kind != "image", Labels: []string{}})
		summary.TotalBytes += int64(len(data))
	}
	summary.ArtifactCount = int64(len(entries))
	summary.DiscoveredArtifactCount = summary.ArtifactCount
	data, err := json.Marshal(visualhive.ArtifactIndexReport{SchemaVersion: 1, Project: "acceptance", GeneratedAt: "2026-07-16T12:00:00.000Z", Root: ".visual-hive", ContentAddressed: true, Complete: true, Summary: summary, Artifacts: entries, Warnings: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	return data, summary
}

func acceptanceZip(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, filepath.ToSlash(path))
	}
	sort.Strings(paths)
	buffer := new(bytes.Buffer)
	archive := zip.NewWriter(buffer)
	for _, path := range paths {
		entry, err := archive.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(entries[path]); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type acceptanceDigestField struct{ name, value string }

func acceptanceV3BundleDigest(manifest visualhive.Manifest) string {
	stream := &bytes.Buffer{}
	stream.WriteString(visualhive.ContentAddressedDigestAlgorithm)
	files := make([][]byte, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		files = append(files, acceptanceDigestRecord("file",
			acceptanceDigestField{"path", file.Path}, acceptanceDigestField{"sourcePath", file.SourcePath}, acceptanceDigestField{"sha256", file.SHA256},
			acceptanceDigestField{"size", strconv.FormatInt(file.Size, 10)}, acceptanceDigestField{"mediaType", file.MediaType}, acceptanceDigestField{"schemaVersion", file.SchemaVersion}))
	}
	acceptanceDigestCollection(stream, "files", files)
	observations := make([][]byte, 0, len(manifest.Observations))
	for _, observation := range manifest.Observations {
		fields := []acceptanceDigestField{
			acceptanceDigestField{"repositoryFingerprint", observation.RepositoryFingerprint}, acceptanceDigestField{"fingerprint", observation.Fingerprint},
			acceptanceDigestField{"publicationRole", observation.PublicationRole}, acceptanceDigestField{"rootCauseKey", observation.RootCauseKey}, acceptanceDigestArray("blockedByRootKeys", observation.BlockedByRootKeys),
			acceptanceDigestField{"state", observation.State}, acceptanceDigestField{"issueKind", observation.IssueKind}, acceptanceDigestField{"severity", observation.Severity}, acceptanceDigestField{"owningAgentHint", observation.OwningAgentHint},
			acceptanceDigestField{"title", observation.Title}, acceptanceDigestField{"body", observation.Body}, acceptanceDigestArray("labels", observation.Labels), acceptanceDigestArray("sourceArtifacts", observation.SourceArtifacts),
			acceptanceDigestArray("affectedContracts", observation.AffectedContracts),
		}
		if observation.AffectedFiles != nil {
			fields = append(fields, acceptanceDigestArray("affectedFiles", observation.AffectedFiles))
		}
		fields = append(fields,
			acceptanceDigestField{"validationCommand", observation.ValidationCommand}, acceptanceDigestField{"observedAt", observation.ObservedAt},
			acceptanceDigestField{"firstSeenAt", observation.FirstSeenAt}, acceptanceDigestField{"sourceArtifact", observation.SourceArtifact})
		observations = append(observations, acceptanceDigestRecord("observation", fields...))
	}
	acceptanceDigestCollection(stream, "observations", observations)
	stream.Write(acceptanceDigestRecord("scan", acceptanceDigestField{"scope", manifest.Scan.Scope}, acceptanceDigestField{"authoritativeForResolution", strconv.FormatBool(manifest.Scan.AuthoritativeForResolution)}, acceptanceDigestArray("evaluatedContracts", manifest.Scan.EvaluatedContracts), acceptanceDigestArray("evaluatedFiles", manifest.Scan.EvaluatedFiles), acceptanceDigestField{"testPlanVersion", manifest.Scan.TestPlanVersion}, acceptanceDigestField{"toolRegistryVersion", manifest.Scan.ToolRegistryVersion}))
	stream.Write(acceptanceDigestRecord("source", acceptanceDigestField{"repository", manifest.Source.Repository}, acceptanceDigestField{"repositoryId", manifest.Source.RepositoryID}, acceptanceDigestField{"ref", manifest.Source.Ref}, acceptanceDigestField{"commitSha", manifest.Source.CommitSHA}, acceptanceDigestField{"event", manifest.Source.Event}, acceptanceDigestField{"workflowName", manifest.Source.WorkflowName}, acceptanceDigestField{"workflowRunId", manifest.Source.WorkflowRunID}, acceptanceDigestField{"workflowRunAttempt", manifest.Source.WorkflowRunAttempt}, acceptanceDigestField{"workflowArtifactId", manifest.Source.WorkflowArtifactID}, acceptanceDigestField{"conclusion", manifest.Source.Conclusion}, acceptanceDigestField{"trusted", strconv.FormatBool(manifest.Source.Trusted)}))
	stream.Write(acceptanceDigestRecord("metadata", acceptanceDigestField{"generatedAt", manifest.GeneratedAt.UTC().Format("2006-01-02T15:04:05.000Z")}, acceptanceDigestField{"expiresAt", manifest.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")}, acceptanceDigestField{"project", manifest.Project}, acceptanceDigestField{"mode", manifest.Mode}, acceptanceDigestField{"verdict", manifest.Verdict}, acceptanceDigestField{"acmmRequest", strconv.Itoa(manifest.ACMMRequest)}, acceptanceDigestField{"externalCallsMade", strconv.Itoa(manifest.ExternalCallsMade)}, acceptanceDigestField{"producerName", manifest.Producer.Name}, acceptanceDigestField{"producerVersion", manifest.Producer.Version}, acceptanceDigestField{"producerGitCommit", manifest.Producer.GitCommit}))
	stream.Write(acceptanceDigestRecord("artifactIndex", acceptanceDigestField{"path", manifest.ArtifactIndex.Path}, acceptanceDigestField{"sourcePath", manifest.ArtifactIndex.SourcePath}, acceptanceDigestField{"sha256", manifest.ArtifactIndex.SHA256}, acceptanceDigestField{"schemaVersion", strconv.Itoa(manifest.ArtifactIndex.SchemaVersion)}, acceptanceDigestField{"contentAddressed", strconv.FormatBool(manifest.ArtifactIndex.ContentAddressed)}, acceptanceDigestField{"complete", strconv.FormatBool(manifest.ArtifactIndex.Complete)}, acceptanceDigestField{"artifactCount", strconv.FormatInt(manifest.ArtifactIndex.ArtifactCount, 10)}, acceptanceDigestField{"totalBytes", strconv.FormatInt(manifest.ArtifactIndex.TotalBytes, 10)}))
	summary := manifest.CapabilityParity.Summary
	stream.Write(acceptanceDigestRecord("capabilityParity", acceptanceDigestField{"path", manifest.CapabilityParity.Path}, acceptanceDigestField{"sourcePath", manifest.CapabilityParity.SourcePath}, acceptanceDigestField{"sha256", manifest.CapabilityParity.SHA256}, acceptanceDigestField{"schemaVersion", manifest.CapabilityParity.SchemaVersion}, acceptanceDigestField{"baselineVersion", manifest.CapabilityParity.BaselineVersion}, acceptanceDigestField{"status", manifest.CapabilityParity.Status}, acceptanceDigestField{"runtimeStatus", manifest.CapabilityParity.RuntimeStatus}, acceptanceDigestField{"expected", strconv.FormatInt(summary.Expected, 10)}, acceptanceDigestField{"actual", strconv.FormatInt(summary.Actual, 10)}, acceptanceDigestField{"present", strconv.FormatInt(summary.Present, 10)}, acceptanceDigestField{"blocked", strconv.FormatInt(summary.Blocked, 10)}, acceptanceDigestField{"missing", strconv.FormatInt(summary.Missing, 10)}, acceptanceDigestField{"unexpected", strconv.FormatInt(summary.Unexpected, 10)}, acceptanceDigestField{"mismatched", strconv.FormatInt(summary.Mismatched, 10)}))
	stream.Write(acceptanceDigestRecord("replay", acceptanceDigestField{"key", manifest.ReplayProtection.Key}))
	return acceptanceSHA256(stream.Bytes())
}

func acceptanceDigestArray(name string, values []string) acceptanceDigestField {
	return acceptanceDigestField{name: "\x00array\x00" + name, value: strings.Join(values, "\x00")}
}

func acceptanceDigestRecord(domain string, fields ...acceptanceDigestField) []byte {
	record := &bytes.Buffer{}
	record.WriteByte('R')
	acceptanceDigestLP(record, []byte(domain))
	for _, field := range fields {
		if strings.HasPrefix(field.name, "\x00array\x00") {
			record.WriteByte('A')
			acceptanceDigestLP(record, []byte(strings.TrimPrefix(field.name, "\x00array\x00")))
			values := []string{}
			if field.value != "" {
				values = strings.Split(field.value, "\x00")
			}
			acceptanceDigestLP(record, []byte(strconv.Itoa(len(values))))
			for _, value := range values {
				record.WriteByte('E')
				acceptanceDigestLP(record, []byte(value))
			}
			continue
		}
		record.WriteByte('S')
		acceptanceDigestLP(record, []byte(field.name))
		acceptanceDigestLP(record, []byte(field.value))
	}
	record.WriteByte('Z')
	return record.Bytes()
}

func acceptanceDigestCollection(stream *bytes.Buffer, domain string, records [][]byte) {
	sort.Slice(records, func(i, j int) bool { return bytes.Compare(records[i], records[j]) < 0 })
	stream.WriteByte('C')
	acceptanceDigestLP(stream, []byte(domain))
	acceptanceDigestLP(stream, []byte(strconv.Itoa(len(records))))
	for _, record := range records {
		stream.WriteByte('I')
		acceptanceDigestLP(stream, record)
	}
}

func acceptanceDigestLP(target *bytes.Buffer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	target.Write(length[:])
	target.Write(value)
}

type acceptancePullRequestFixtureOptions struct {
	BaseSHA    string
	HeadSHA    string
	HeadBranch string
}

type acceptancePullRequestFixture struct {
	BundleZip []byte
	SourceZip []byte
	Workflow  []byte
	Config    []byte
	Baseline  []byte
}

func buildAcceptancePullRequestFixture(t *testing.T, options acceptancePullRequestFixtureOptions) acceptancePullRequestFixture {
	t.Helper()
	workflow := []byte("name: Visual Hive PR\non:\n  pull_request:\n")
	configData := []byte("project: acceptance\ncontracts:\n  - contracts/app-shell.yaml\n")
	executionBinding := map[string]string{
		"nonceSha256": strings.Repeat("1", 64), "generatedSpecSha256": "", "generatedConfigSha256": "",
		"payloadSha256": strings.Repeat("4", 64), "bindingMacSha256": strings.Repeat("5", 64),
	}
	generatedSpec := []byte("export const visualHive = true;\n")
	generatedConfig := []byte("module.exports = { workers: 1 };\n")
	executionBinding["generatedSpecSha256"] = acceptanceSHA256(generatedSpec)
	executionBinding["generatedConfigSha256"] = acceptanceSHA256(generatedConfig)
	type runtimeBrowser struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	type runtimeFont struct {
		Name      string `json:"name"`
		Available bool   `json:"available"`
	}
	type runtimeEnvironment struct {
		OS                string        `json:"os"`
		Architecture      string        `json:"architecture"`
		NodeVersion       string        `json:"nodeVersion"`
		PlaywrightVersion string        `json:"playwrightVersion"`
		Locale            string        `json:"locale"`
		Timezone          string        `json:"timezone"`
		UserAgent         string        `json:"userAgent"`
		DeviceScaleFactor int           `json:"deviceScaleFactor"`
		Fonts             []runtimeFont `json:"fonts"`
	}
	browser := runtimeBrowser{Name: "chromium", Version: "130.0.1"}
	environment := runtimeEnvironment{OS: "linux", Architecture: "x64", NodeVersion: "22.14.0", PlaywrightVersion: "1.51.0", Locale: "en-US", Timezone: "UTC", UserAgent: "VisualHive/1", DeviceScaleFactor: 1, Fonts: []runtimeFont{{Name: "Arial", Available: true}}}
	stableData, err := json.Marshal(struct {
		Browser     runtimeBrowser     `json:"browser"`
		Environment runtimeEnvironment `json:"environment"`
	}{browser, environment})
	if err != nil {
		t.Fatal(err)
	}
	runtimeData, err := json.Marshal(struct {
		SchemaVersion    string             `json:"schemaVersion"`
		ExecutionBinding map[string]string  `json:"executionBinding"`
		CapturedAt       string             `json:"capturedAt"`
		Browser          runtimeBrowser     `json:"browser"`
		Environment      runtimeEnvironment `json:"environment"`
	}{visualhive.PullRequestRuntimeSchema, executionBinding, "2026-07-16T12:00:00.000Z", browser, environment})
	if err != nil {
		t.Fatal(err)
	}
	executionData, err := json.Marshal(executionBinding)
	if err != nil {
		t.Fatal(err)
	}
	planData := []byte(`{"schemaVersion":1,"project":"acceptance","mode":"pr","changedFiles":["src/value.txt"],"effectiveChangedFiles":["src/value.txt"],"ignoredChangedFiles":[],"targets":[{"id":"app","kind":"web","url":"http://127.0.0.1:3000","prSafe":true,"cost":1}],"items":[{"contractId":"contract/app-shell","targetId":"app","targetUrl":"http://127.0.0.1:3000","severity":"high","cost":1,"reasons":[],"screenshots":["button"]}],"excluded":[],"mutation":{"allowed":false},"providerPolicy":{}}`)
	reportData, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"repository":    map[string]any{"provider": "github-actions", "repository": "owner/repo", "branch": options.HeadBranch, "baseBranch": "main", "commitSha": options.HeadSHA, "pullRequestNumber": 7, "runId": "77", "runAttempt": "3", "workflow": acceptancePRWorkflowName},
		"mode":          "pr", "status": "passed", "changedFiles": []string{"src/value.txt"},
		"selectedTargets":   []any{map[string]any{"id": "app", "kind": "web", "url": "http://127.0.0.1:3000", "prSafe": true, "cost": "low"}},
		"selectedContracts": []string{"contract/app-shell"}, "excludedContracts": []any{},
		"generatedSpecPath": ".visual-hive/generated/visual-hive.generated.spec.cjs", "executionBinding": executionBinding,
		"results": []any{map[string]any{"contractId": "contract/app-shell", "targetId": "app", "status": "passed", "durationMs": 10, "errors": []string{}, "artifacts": []string{}, "screenshotAssertions": []any{map[string]any{"baselinePath": "button.png", "status": "passed"}}}},
		"summary": map[string]int{"passed": 1, "failed": 0}, "artifacts": []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	baselinePath := ".visual-hive/proof/pr/baselines/button.png"
	baselineData := []byte("\x89PNG\r\n\x1a\nvisual-hive-acceptance-baseline")
	baselineIdentity := visualhive.PullRequestFileIdentity{Path: baselinePath, SHA256: acceptanceSHA256(baselineData), Bytes: int64(len(baselineData))}
	baselineDigest := acceptanceSHA256([]byte(baselineIdentity.Path + "\x00" + baselineIdentity.SHA256 + "\x00" + strconv.FormatInt(baselineIdentity.Bytes, 10)))
	changedData := []byte("src/value.txt\n")
	planContractsData := []byte("contract/app-shell\n")
	evaluationScopesData := []byte("contract/app-shell\nprovider-governance\nworkflow-safety\n")
	pipelineExitData := []byte("0\n")
	runnerOutcomeData := []byte("{\"schemaVersion\":\"hive.visual-runner-outcome.v1\",\"outcome\":\"success\",\"conclusion\":\"success\"}\n")
	beadsData := []byte("[]")
	verdictData := []byte(`{"schemaVersion":"visual-hive.verdict.v1","summary":{"status":"passed"}}`)
	parityData := acceptanceParityData(t)
	files := map[string][]byte{
		".visual-hive/capability-parity.json":                     parityData,
		".visual-hive/hive/beads.json":                            beadsData,
		".visual-hive/verdict.json":                               verdictData,
		visualhive.PullRequestPlanPath:                            planData,
		visualhive.PullRequestReportPath:                          reportData,
		visualhive.PullRequestRuntimePath:                         runtimeData,
		visualhive.PullRequestConfigPath:                          configData,
		visualhive.PullRequestWorkflowPath:                        workflow,
		visualhive.PullRequestChangedFilesPath:                    changedData,
		visualhive.PullRequestPlanContractsPath:                   planContractsData,
		visualhive.PullRequestEvaluationScopesPath:                evaluationScopesData,
		visualhive.PullRequestPipelineExitPath:                    pipelineExitData,
		visualhive.PullRequestRunnerOutcomePath:                   runnerOutcomeData,
		".visual-hive/generated/visual-hive.generated.spec.cjs":   generatedSpec,
		".visual-hive/generated/visual-hive.generated.config.cjs": generatedConfig,
		baselinePath: baselineData,
	}
	identity := func(path string) visualhive.PullRequestFileIdentity {
		data := files[path]
		return visualhive.PullRequestFileIdentity{Path: path, SHA256: acceptanceSHA256(data), Bytes: int64(len(data))}
	}
	binding := visualhive.PullRequestSourceBinding{
		SchemaVersion: visualhive.PullRequestSourceBindingSchema, Repository: "owner/repo", RepositoryID: "123", PullRequest: 7,
		Base:     visualhive.PullRequestRevisionIdentity{Repository: "owner/repo", RepositoryID: "123", Ref: "main", SHA: options.BaseSHA},
		Head:     visualhive.PullRequestRevisionIdentity{Repository: "owner/repo", RepositoryID: "123", Ref: options.HeadBranch, SHA: options.HeadSHA},
		Workflow: visualhive.PullRequestWorkflowIdentity{Name: acceptancePRWorkflowName, Path: acceptancePRWorkflowPath, Event: "pull_request", RunID: "77", RunAttempt: "3", Definition: identity(visualhive.PullRequestWorkflowPath), ProducerCommit: hivegithub.VisualHivePullRequestProducerCommit, SourceName: "visual-hive-pr-source-77-3", BundleName: "visual-hive-pr-bundle-77-3"},
		Plan:     identity(visualhive.PullRequestPlanPath), Report: identity(visualhive.PullRequestReportPath), Config: identity(visualhive.PullRequestConfigPath), ChangedFiles: identity(visualhive.PullRequestChangedFilesPath),
		PlanContracts: identity(visualhive.PullRequestPlanContractsPath), EvaluationScopes: identity(visualhive.PullRequestEvaluationScopesPath),
		Runtime:   visualhive.PullRequestRuntimeIdentity{Receipt: identity(visualhive.PullRequestRuntimePath), GeneratedSpec: identity(".visual-hive/generated/visual-hive.generated.spec.cjs"), GeneratedConfig: identity(".visual-hive/generated/visual-hive.generated.config.cjs"), StableSHA256: acceptanceSHA256(stableData), ExecutionBindingSHA256: acceptanceSHA256(executionData)},
		Baselines: visualhive.PullRequestBaselineIdentity{SHA256: baselineDigest, Files: []visualhive.PullRequestFileIdentity{baselineIdentity}},
	}
	runnerPayloadData, err := json.Marshal(map[string]any{
		"schemaVersion": visualhive.PullRequestRunnerExecutionSchema, "repository": binding.Repository, "pullRequest": binding.PullRequest,
		"baseRef": binding.Base.Ref, "headRef": binding.Head.Ref, "headSha": binding.Head.SHA, "runId": binding.Workflow.RunID, "runAttempt": binding.Workflow.RunAttempt,
		"workflow": binding.Workflow.Name, "producerCommit": binding.Workflow.ProducerCommit, "executionBinding": executionBinding,
		"report": binding.Report, "runtime": binding.Runtime.Receipt, "generatedSpec": binding.Runtime.GeneratedSpec, "generatedConfig": binding.Runtime.GeneratedConfig,
		"pipelineExit": identity(visualhive.PullRequestPipelineExitPath), "runnerOutcome": identity(visualhive.PullRequestRunnerOutcomePath),
	})
	if err != nil {
		t.Fatal(err)
	}
	runnerKey := make([]byte, 32)
	for index := range runnerKey {
		runnerKey[index] = byte(index + 1)
	}
	runnerMAC := hmac.New(sha256.New, runnerKey)
	_, _ = runnerMAC.Write(runnerPayloadData)
	runnerAttestationData, err := json.Marshal(map[string]any{
		"schemaVersion": visualhive.PullRequestRunnerAttestationSchema, "algorithm": "HMAC-SHA256",
		"keyCommitmentSha256": acceptanceSHA256(runnerKey), "keyBase64": base64.StdEncoding.EncodeToString(runnerKey),
		"payloadSha256": acceptanceSHA256(runnerPayloadData), "payloadBase64": base64.StdEncoding.EncodeToString(runnerPayloadData), "macSha256": hex.EncodeToString(runnerMAC.Sum(nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	files[visualhive.PullRequestRunnerAttestationPath] = runnerAttestationData
	binding.RunnerAttestation = identity(visualhive.PullRequestRunnerAttestationPath)
	bindingData, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	files[visualhive.PullRequestSourceBindingPath] = bindingData
	indexData, indexSummary := acceptanceArtifactIndex(t, files)
	now := time.Now().UTC().Truncate(time.Second).Add(time.Millisecond)
	manifest := visualhive.Manifest{
		SchemaVersion: visualhive.ManifestSchemaV3, DigestAlgorithm: visualhive.ContentAddressedDigestAlgorithm, BundleID: "acceptance-pr-bundle",
		GeneratedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		Producer: visualhive.Producer{Name: "visual-hive", Version: "0.4.0", GitCommit: hivegithub.VisualHivePullRequestProducerCommit},
		Source:   visualhive.Source{Repository: "owner/repo", RepositoryID: "123", Ref: "refs/heads/" + options.HeadBranch, CommitSHA: options.HeadSHA, Event: "pull_request", WorkflowName: acceptancePRWorkflowName, WorkflowRunID: "77", WorkflowRunAttempt: "3", WorkflowArtifactID: "298", Conclusion: "success", Trusted: false},
		Project:  "acceptance", Mode: "measured", Verdict: "ready", ACMMRequest: 4,
		Scan:         visualhive.Scan{Scope: "changed-files", AuthoritativeForResolution: false, EvaluatedContracts: []string{"contract/app-shell", "provider-governance", "workflow-safety"}, EvaluatedFiles: []string{"src/value.txt"}, TestPlanVersion: "acceptance-plan-v1", ToolRegistryVersion: "acceptance-tools-v1"},
		Observations: []visualhive.Observation{},
		Files: []visualhive.File{
			{Path: "files/.visual-hive/artifacts-index.json", SourcePath: ".visual-hive/artifacts-index.json", SHA256: acceptanceSHA256(indexData), Size: int64(len(indexData)), MediaType: "application/json"},
			{Path: "files/.visual-hive/capability-parity.json", SourcePath: ".visual-hive/capability-parity.json", SHA256: acceptanceSHA256(parityData), Size: int64(len(parityData)), MediaType: "application/json"},
			{Path: "files/.visual-hive/hive/beads.json", SourcePath: ".visual-hive/hive/beads.json", SHA256: acceptanceSHA256(beadsData), Size: int64(len(beadsData)), MediaType: "application/json"},
		},
		ArtifactIndex:    &visualhive.ArtifactIndexBinding{Path: "files/.visual-hive/artifacts-index.json", SourcePath: ".visual-hive/artifacts-index.json", SHA256: acceptanceSHA256(indexData), SchemaVersion: 1, ContentAddressed: true, Complete: true, ArtifactCount: indexSummary.ArtifactCount, TotalBytes: indexSummary.TotalBytes},
		CapabilityParity: &visualhive.CapabilityParityBinding{Path: "files/.visual-hive/capability-parity.json", SourcePath: ".visual-hive/capability-parity.json", SHA256: acceptanceSHA256(parityData), SchemaVersion: "visual-hive.capability-parity.v1", BaselineVersion: "visual-hive.capability-baseline.v1", Status: "passed", RuntimeStatus: "ready", Summary: visualhive.CapabilityParitySummary{Expected: 1, Actual: 1, Present: 1}},
		ReplayProtection: visualhive.ReplayProtection{Nonce: "acceptance-pr-bundle"}, Provenance: visualhive.Provenance{Kind: "github-actions", AttestationRequired: true},
		Safety: visualhive.Safety{AtomicWrite: true, PathsAreRelative: true, DigestsRequired: true, ProducerCountersAreAdvisory: true, ProducerTrustClaimIsAdvisory: true, AbsenceRequiresAuthoritativeScan: true},
	}
	manifest.ReplayProtection.Key = acceptanceSHA256([]byte(strings.Join([]string{"visual-hive.bundle.replay.v3", "owner/repo", "123", options.HeadSHA, "77", "3", "298", "acceptance-pr-bundle"}, "\x00")))
	manifest.OverallDigest = acceptanceV3BundleDigest(manifest)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	bundlePrefix := ".visual-hive/bundles/acceptance-pr-bundle/"
	bundleEntries := map[string][]byte{
		bundlePrefix + "manifest.json":                             manifestData,
		bundlePrefix + "files/.visual-hive/artifacts-index.json":   indexData,
		bundlePrefix + "files/.visual-hive/capability-parity.json": parityData,
		bundlePrefix + "files/.visual-hive/hive/beads.json":        beadsData,
	}
	sourceEntries := map[string][]byte{".visual-hive/artifacts-index.json": indexData}
	for path, data := range files {
		sourceEntries[path] = data
	}
	return acceptancePullRequestFixture{BundleZip: acceptanceZip(t, bundleEntries), SourceZip: acceptanceZip(t, sourceEntries), Workflow: workflow, Config: configData, Baseline: baselineData}
}

func newAcceptanceKnowledgeServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/search" {
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"total": 1, "results": []any{map[string]any{
			"slug": "acceptance-primer", "title": "ACCEPTANCE_PRIMER_FACT", "score": 1.0, "type": "gotcha", "status": "verified", "confidence": 0.99,
			"tags": []string{"acceptance"}, "snippet": "ACCEPTANCE_PRIMER_FACT requires the smallest deterministic repair.",
		}}})
	}))
}

type acceptanceGitHubSnapshot struct {
	PRCreates             int
	PREdits               int
	PRHeadSHA             string
	PRBranch              string
	RepositoryFingerprint string
	PRV3BundleDownloads   int
	PRV3SourceDownloads   int
	Merges                int
}

type acceptanceGitHubAPI struct {
	t      *testing.T
	remote string
	base   string

	mu        sync.Mutex
	server    *httptest.Server
	sources   map[int64]acceptanceSourceFixture
	artifacts map[int64]acceptanceSourceFixture
	prFixture *acceptancePullRequestFixture
	prState   string
	prMerged  bool
	prTitle   string
	prBody    string
	prBranch  string
	prHead    string
	snapshot  acceptanceGitHubSnapshot
}

func newAcceptanceGitHubAPI(t *testing.T, remote, base string, fixtures ...acceptanceSourceFixture) *acceptanceGitHubAPI {
	t.Helper()
	api := &acceptanceGitHubAPI{t: t, remote: remote, base: base, sources: map[int64]acceptanceSourceFixture{}, artifacts: map[int64]acceptanceSourceFixture{}, prState: "open"}
	for _, fixture := range fixtures {
		api.sources[fixture.RunID] = fixture
		api.artifacts[fixture.BundleArtifactID] = fixture
		api.artifacts[fixture.SourceArtifactID] = fixture
	}
	api.server = httptest.NewServer(http.HandlerFunc(api.serveHTTP))
	return api
}

func (api *acceptanceGitHubAPI) URL() string { return api.server.URL }
func (api *acceptanceGitHubAPI) Close()      { api.server.Close() }

func (api *acceptanceGitHubAPI) Snapshot() acceptanceGitHubSnapshot {
	api.mu.Lock()
	defer api.mu.Unlock()
	return api.snapshot
}

func (api *acceptanceGitHubAPI) SetPullRequestFixture(fixture acceptancePullRequestFixture) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.prFixture = &fixture
}

func (api *acceptanceGitHubAPI) SetPullRequestState(state string, merged bool) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.prState = state
	api.prMerged = merged
}

func (api *acceptanceGitHubAPI) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	path := request.URL.Path
	if request.Method == http.MethodPut && strings.HasSuffix(path, "/merge") {
		api.mu.Lock()
		api.snapshot.Merges++
		api.mu.Unlock()
		http.Error(writer, "merge forbidden by acceptance proof", http.StatusForbidden)
		return
	}
	if api.serveSourceFixture(writer, request) {
		return
	}
	switch {
	case request.Method == http.MethodGet && path == "/repos/owner/repo":
		acceptanceWriteJSON(api.t, writer, map[string]any{"id": 123, "full_name": "owner/repo", "default_branch": "main"})
	case request.Method == http.MethodGet && path == "/repos/owner/repo/actions/workflows/11":
		acceptanceWriteJSON(api.t, writer, map[string]any{"id": 11, "name": acceptanceSourceWorkflowName, "path": acceptanceSourceWorkflowPath, "state": "active"})
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/repos/owner/repo/git/ref/heads/"):
		branch := strings.TrimPrefix(path, "/repos/owner/repo/git/ref/heads/")
		head, err := acceptanceGitOutput(api.remote, "rev-parse", "refs/heads/"+branch)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusNotFound)
			return
		}
		acceptanceWriteJSON(api.t, writer, map[string]any{"ref": "refs/heads/" + branch, "object": map[string]any{"type": "commit", "sha": strings.TrimSpace(head)}})
	case path == "/repos/owner/repo/pulls" && request.Method == http.MethodGet:
		api.servePullList(writer, request)
	case path == "/repos/owner/repo/pulls" && request.Method == http.MethodPost:
		api.createPull(writer, request)
	case path == "/repos/owner/repo/pulls/7" && request.Method == http.MethodPatch:
		api.mu.Lock()
		api.snapshot.PREdits++
		api.mu.Unlock()
		http.Error(writer, "acceptance proof forbids PR edits", http.StatusForbidden)
	case path == "/repos/owner/repo/pulls/7" && request.Method == http.MethodGet && strings.Contains(request.Header.Get("Accept"), "diff"):
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = writer.Write([]byte(acceptanceRepairDiff))
	case path == "/repos/owner/repo/pulls/7" && request.Method == http.MethodGet:
		api.writePull(writer)
	case path == "/repos/owner/repo/pulls/7/files" && request.Method == http.MethodGet:
		acceptanceWriteJSON(api.t, writer, []any{map[string]any{"filename": "src/value.txt", "status": "modified"}})
	case path == "/repos/owner/repo/branches/main" && request.Method == http.MethodGet:
		acceptanceWriteJSON(api.t, writer, map[string]any{"name": "main", "commit": map[string]any{"sha": api.base}})
	case path == "/apps/github-actions" && request.Method == http.MethodGet:
		acceptanceWriteJSON(api.t, writer, map[string]any{"id": 15368, "slug": "github-actions"})
	case path == "/repos/owner/repo/actions/workflows/visual-hive-pr.yml/runs" && request.Method == http.MethodGet:
		api.writePRRunList(writer)
	case path == "/repos/owner/repo/actions/runs/77" && request.Method == http.MethodGet:
		api.writePRRun(writer)
	case path == "/repos/owner/repo/actions/runs/77/attempts/3/jobs" && request.Method == http.MethodGet:
		api.mu.Lock()
		head, branch := api.prHead, api.prBranch
		api.mu.Unlock()
		acceptanceWriteJSON(api.t, writer, map[string]any{"total_count": 1, "jobs": []any{map[string]any{
			"id": 700, "run_id": 77, "run_attempt": 3, "name": "visual-hive", "workflow_name": acceptancePRWorkflowName,
			"head_branch": branch, "head_sha": head, "status": "completed", "conclusion": "success",
			"html_url": "https://github.test/owner/repo/actions/runs/77/job/700", "check_run_url": "https://api.github.test/repos/owner/repo/check-runs/900",
		}}})
	case path == "/repos/owner/repo/check-runs/900" && request.Method == http.MethodGet:
		api.mu.Lock()
		head := api.prHead
		api.mu.Unlock()
		acceptanceWriteJSON(api.t, writer, map[string]any{"id": 900, "name": "visual-hive", "head_sha": head, "status": "completed", "conclusion": "success", "app": map[string]any{"id": 15368}, "check_suite": map[string]any{"id": 501}})
	case path == "/repos/owner/repo/actions/workflows/12" && request.Method == http.MethodGet:
		acceptanceWriteJSON(api.t, writer, map[string]any{"id": 12, "name": acceptancePRWorkflowName, "path": acceptancePRWorkflowPath, "state": "active"})
	case path == "/repos/owner/repo/contents/.github/workflows/visual-hive-pr.yml" && request.Method == http.MethodGet:
		fixture, ok := api.currentPRFixture()
		if !ok {
			http.Error(writer, "PR fixture not installed", http.StatusNotFound)
			return
		}
		acceptanceWriteContent(api.t, writer, acceptancePRWorkflowPath, fixture.Workflow)
	case path == "/repos/owner/repo/contents/visual-hive.config.yaml" && request.Method == http.MethodGet:
		fixture, ok := api.currentPRFixture()
		if !ok {
			http.Error(writer, "PR fixture not installed", http.StatusNotFound)
			return
		}
		acceptanceWriteContent(api.t, writer, "visual-hive.config.yaml", fixture.Config)
	case path == "/repos/owner/repo/contents/button.png" && request.Method == http.MethodGet:
		fixture, ok := api.currentPRFixture()
		if !ok {
			http.Error(writer, "PR fixture not installed", http.StatusNotFound)
			return
		}
		acceptanceWriteContent(api.t, writer, "button.png", fixture.Baseline)
	case path == "/repos/owner/repo/actions/runs/77/artifacts" && request.Method == http.MethodGet:
		api.writePRArtifacts(writer)
	case path == "/repos/owner/repo/actions/artifacts/299/zip" && request.Method == http.MethodGet:
		writer.Header().Set("Location", api.server.URL+"/signed-pr-bundle")
		writer.WriteHeader(http.StatusFound)
	case path == "/repos/owner/repo/actions/artifacts/298/zip" && request.Method == http.MethodGet:
		writer.Header().Set("Location", api.server.URL+"/signed-pr-source")
		writer.WriteHeader(http.StatusFound)
	case path == "/signed-pr-bundle" && request.Method == http.MethodGet:
		api.writeSignedPRArtifact(writer, request, true)
	case path == "/signed-pr-source" && request.Method == http.MethodGet:
		api.writeSignedPRArtifact(writer, request, false)
	default:
		http.Error(writer, request.Method+" "+path, http.StatusNotFound)
	}
}

func (api *acceptanceGitHubAPI) serveSourceFixture(writer http.ResponseWriter, request *http.Request) bool {
	for _, fixture := range api.sources {
		runPath := fmt.Sprintf("/repos/owner/repo/actions/runs/%d", fixture.RunID)
		switch request.URL.Path {
		case runPath:
			acceptanceWriteJSON(api.t, writer, map[string]any{"id": fixture.RunID, "run_attempt": fixture.RunAttempt, "workflow_id": 11,
				"name": acceptanceSourceWorkflowName, "display_title": acceptanceSourceWorkflowName, "path": acceptanceSourceWorkflowPath,
				"head_branch": "main", "head_sha": fixture.BaseSHA, "event": "workflow_dispatch", "status": "completed", "conclusion": "success",
				"html_url": fmt.Sprintf("https://github.test/owner/repo/actions/runs/%d", fixture.RunID)})
			return true
		case runPath + "/artifacts":
			acceptanceWriteJSON(api.t, writer, map[string]any{"total_count": 2, "artifacts": []any{
				map[string]any{"id": fixture.SourceArtifactID, "name": "visual-hive-evidence", "size_in_bytes": len(fixture.SourceZip), "expired": false, "workflow_run": map[string]any{"id": fixture.RunID, "repository_id": 123, "head_sha": fixture.BaseSHA}},
				map[string]any{"id": fixture.BundleArtifactID, "name": "visual-hive-bundle", "size_in_bytes": len(fixture.BundleZip), "expired": false, "workflow_run": map[string]any{"id": fixture.RunID, "repository_id": 123, "head_sha": fixture.BaseSHA}},
			}})
			return true
		}
	}
	if !strings.HasPrefix(request.URL.Path, "/repos/owner/repo/actions/artifacts/") && !strings.HasPrefix(request.URL.Path, "/signed-source-") {
		return false
	}
	for id, fixture := range api.artifacts {
		if request.URL.Path == fmt.Sprintf("/repos/owner/repo/actions/artifacts/%d/zip", id) {
			writer.Header().Set("Location", fmt.Sprintf("%s/signed-source-%d", api.server.URL, id))
			writer.WriteHeader(http.StatusFound)
			return true
		}
		if request.URL.Path == fmt.Sprintf("/signed-source-%d", id) {
			if request.Header.Get("Authorization") != "" {
				api.t.Errorf("signed source artifact %d leaked GitHub authorization", id)
			}
			writer.Header().Set("Content-Type", "application/zip")
			if id == fixture.BundleArtifactID {
				_, _ = writer.Write(fixture.BundleZip)
			} else {
				_, _ = writer.Write(fixture.SourceZip)
			}
			return true
		}
	}
	return false
}

func (api *acceptanceGitHubAPI) createPull(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	branch := strings.TrimPrefix(input.Head, "owner:")
	head, err := acceptanceGitOutput(api.remote, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if input.Base != "main" || input.Title == "" || input.Body == "" {
		http.Error(writer, "incomplete PR request", http.StatusBadRequest)
		return
	}
	fingerprint := acceptanceMarkerFingerprint(input.Body)
	if fingerprint == "" {
		http.Error(writer, "missing exact Hive marker", http.StatusBadRequest)
		return
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.snapshot.PRCreates != 0 {
		http.Error(writer, "duplicate PR", http.StatusConflict)
		return
	}
	api.prTitle, api.prBody, api.prBranch, api.prHead = input.Title, input.Body, branch, strings.TrimSpace(head)
	api.snapshot.PRCreates = 1
	api.snapshot.PRHeadSHA = api.prHead
	api.snapshot.PRBranch = branch
	api.snapshot.RepositoryFingerprint = fingerprint
	acceptanceWriteJSON(api.t, writer, map[string]any{"number": 7})
}

func acceptanceMarkerFingerprint(body string) string {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "<!-- hive-repair: ") && strings.HasSuffix(line, " -->") {
			value := strings.TrimSuffix(strings.TrimPrefix(line, "<!-- hive-repair: "), " -->")
			if len(value) == 64 {
				if _, err := hex.DecodeString(value); err == nil {
					return strings.ToLower(value)
				}
			}
		}
	}
	return ""
}

func (api *acceptanceGitHubAPI) servePullList(writer http.ResponseWriter, request *http.Request) {
	api.mu.Lock()
	created, state := api.snapshot.PRCreates == 1, api.prState
	api.mu.Unlock()
	wanted := request.URL.Query().Get("state")
	if !created || (wanted != "" && wanted != "all" && wanted != state) {
		acceptanceWriteJSON(api.t, writer, []any{})
		return
	}
	api.mu.Lock()
	pull := api.pullObjectLocked()
	api.mu.Unlock()
	acceptanceWriteJSON(api.t, writer, []any{pull})
}

func (api *acceptanceGitHubAPI) writePull(writer http.ResponseWriter) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.snapshot.PRCreates == 0 {
		http.Error(writer, "PR not created", http.StatusNotFound)
		return
	}
	acceptanceWriteJSON(api.t, writer, api.pullObjectLocked())
}

func (api *acceptanceGitHubAPI) pullObjectLocked() map[string]any {
	repo := map[string]any{"id": 123, "full_name": "owner/repo", "name": "repo"}
	return map[string]any{
		"number": 7, "html_url": "https://github.test/owner/repo/pull/7", "state": api.prState, "merged": api.prMerged,
		"title": api.prTitle, "body": api.prBody, "changed_files": 1,
		"base": map[string]any{"ref": "main", "sha": api.base, "repo": repo},
		"head": map[string]any{"ref": api.prBranch, "sha": api.prHead, "repo": repo},
	}
}

func (api *acceptanceGitHubAPI) writePRRunList(writer http.ResponseWriter) {
	api.mu.Lock()
	run := api.prRunObjectLocked()
	api.mu.Unlock()
	acceptanceWriteJSON(api.t, writer, map[string]any{"total_count": 1, "workflow_runs": []any{run}})
}

func (api *acceptanceGitHubAPI) writePRRun(writer http.ResponseWriter) {
	api.mu.Lock()
	run := api.prRunObjectLocked()
	api.mu.Unlock()
	acceptanceWriteJSON(api.t, writer, run)
}

func (api *acceptanceGitHubAPI) prRunObjectLocked() map[string]any {
	repo := map[string]any{"id": 123, "full_name": "owner/repo"}
	association := map[string]any{"number": 7, "base": map[string]any{"ref": "main", "sha": api.base, "repo": repo}, "head": map[string]any{"ref": api.prBranch, "sha": api.prHead, "repo": repo}}
	return map[string]any{
		"id": 77, "run_attempt": 3, "workflow_id": 12, "check_suite_id": 501, "name": acceptancePRWorkflowName, "path": acceptancePRWorkflowPath,
		"head_branch": api.prBranch, "head_sha": api.prHead, "event": "pull_request", "status": "completed", "conclusion": "success",
		"html_url": "https://github.test/owner/repo/actions/runs/77", "repository": repo, "head_repository": repo, "pull_requests": []any{association},
	}
}

func (api *acceptanceGitHubAPI) currentPRFixture() (acceptancePullRequestFixture, bool) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.prFixture == nil {
		return acceptancePullRequestFixture{}, false
	}
	return *api.prFixture, true
}

func (api *acceptanceGitHubAPI) writePRArtifacts(writer http.ResponseWriter) {
	fixture, ok := api.currentPRFixture()
	if !ok {
		http.Error(writer, "PR fixture not installed", http.StatusNotFound)
		return
	}
	api.mu.Lock()
	head, branch := api.prHead, api.prBranch
	api.mu.Unlock()
	workflowRun := map[string]any{"id": 77, "repository_id": 123, "head_repository_id": 123, "head_branch": branch, "head_sha": head}
	acceptanceWriteJSON(api.t, writer, map[string]any{"total_count": 2, "artifacts": []any{
		map[string]any{"id": 298, "name": "visual-hive-pr-source-77-3", "size_in_bytes": len(fixture.SourceZip), "expired": false, "workflow_run": workflowRun},
		map[string]any{"id": 299, "name": "visual-hive-pr-bundle-77-3", "size_in_bytes": len(fixture.BundleZip), "expired": false, "workflow_run": workflowRun},
	}})
}

func (api *acceptanceGitHubAPI) writeSignedPRArtifact(writer http.ResponseWriter, request *http.Request, bundle bool) {
	fixture, ok := api.currentPRFixture()
	if !ok {
		http.Error(writer, "PR fixture not installed", http.StatusNotFound)
		return
	}
	if request.Header.Get("Authorization") != "" {
		api.t.Errorf("signed PR artifact leaked GitHub authorization")
	}
	api.mu.Lock()
	if bundle {
		api.snapshot.PRV3BundleDownloads++
	} else {
		api.snapshot.PRV3SourceDownloads++
	}
	api.mu.Unlock()
	writer.Header().Set("Content-Type", "application/zip")
	if bundle {
		_, _ = writer.Write(fixture.BundleZip)
	} else {
		_, _ = writer.Write(fixture.SourceZip)
	}
}

func acceptanceGitOutput(directory string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, output)
	}
	return string(output), nil
}

func acceptanceWriteJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("write acceptance API JSON: %v", err)
	}
}

func acceptanceWriteContent(t *testing.T, writer http.ResponseWriter, path string, content []byte) {
	t.Helper()
	acceptanceWriteJSON(t, writer, map[string]any{"type": "file", "path": path, "sha": acceptanceGitBlobSHA(content), "size": len(content), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(content)})
}

func acceptanceGitBlobSHA(content []byte) string {
	hash := sha1.New()
	_, _ = hash.Write([]byte("blob " + strconv.Itoa(len(content)) + "\x00"))
	_, _ = hash.Write(content)
	return hex.EncodeToString(hash.Sum(nil))
}
