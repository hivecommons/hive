package visualhive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	artifactIndexSchemaVersion            = 1
	capabilityParitySchemaVersion         = "visual-hive.capability-parity.v1"
	capabilityBaselineSchemaVersion       = "visual-hive.capability-baseline.v1"
	maxJSONSafeInteger              int64 = 1<<53 - 1
	sourceArtifactExtractionMarker        = ".hive-extraction-complete"
	v3TimestampLayout                     = "2006-01-02T15:04:05.000Z"
	ReviewEvidenceSchemaVersion           = "visual-hive.pr-review-evidence.v1"
)

type ArtifactIndexReport struct {
	SchemaVersion    int                  `json:"schemaVersion"`
	Project          string               `json:"project"`
	GeneratedAt      string               `json:"generatedAt"`
	Root             string               `json:"root"`
	ContentAddressed bool                 `json:"contentAddressed"`
	Complete         bool                 `json:"complete"`
	Summary          ArtifactIndexSummary `json:"summary"`
	Artifacts        []ArtifactIndexEntry `json:"artifacts"`
	Warnings         []string             `json:"warnings"`
}

type ArtifactIndexSummary struct {
	DiscoveredArtifactCount int64 `json:"discoveredArtifactCount"`
	ArtifactCount           int64 `json:"artifactCount"`
	OmittedArtifactCount    int64 `json:"omittedArtifactCount"`
	TotalBytes              int64 `json:"totalBytes"`
	JSON                    int64 `json:"json"`
	Markdown                int64 `json:"markdown"`
	Image                   int64 `json:"image"`
	Text                    int64 `json:"text"`
	TypeScript              int64 `json:"typescript"`
	YAML                    int64 `json:"yaml"`
	Log                     int64 `json:"log"`
	Other                   int64 `json:"other"`
	Previewed               int64 `json:"previewed"`
	RedactedPreviews        int64 `json:"redactedPreviews"`
	TruncatedPreviews       int64 `json:"truncatedPreviews"`
}

type ArtifactIndexEntry struct {
	Path             string   `json:"path"`
	Kind             string   `json:"kind"`
	ContentType      string   `json:"contentType"`
	Bytes            int64    `json:"bytes"`
	SHA256           string   `json:"sha256"`
	SafeToRender     bool     `json:"safeToRender"`
	PreviewTruncated bool     `json:"previewTruncated"`
	PreviewRedacted  bool     `json:"previewRedacted"`
	Preview          *string  `json:"preview,omitempty"`
	Labels           []string `json:"labels"`
}

type capabilityParityReport struct {
	SchemaVersion   string                    `json:"schemaVersion"`
	BaselineVersion string                    `json:"baselineVersion"`
	GeneratedAt     time.Time                 `json:"generatedAt"`
	Status          string                    `json:"status"`
	RuntimeStatus   string                    `json:"runtimeStatus"`
	Summary         CapabilityParitySummary   `json:"summary"`
	Domains         []capabilityDomainSummary `json:"domains"`
	Checks          []capabilityParityCheck   `json:"checks"`
}

type capabilityDomainSummary struct {
	Domain string `json:"domain"`
	CapabilityParitySummary
}

type capabilityParityCheck struct {
	Domain   string          `json:"domain"`
	Key      string          `json:"key"`
	Status   string          `json:"status"`
	Parity   bool            `json:"parity"`
	Message  string          `json:"message"`
	Expected json.RawMessage `json:"expected,omitempty"`
	Actual   json.RawMessage `json:"actual,omitempty"`
}

func validateV3ManifestJSONPresence(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("decode bundle v3 manifest presence: %w", err)
	}
	if err := requireJSONFields(root, "bundle v3 manifest", "schemaVersion", "digestAlgorithm", "bundleId", "generatedAt", "expiresAt", "producer", "source", "project", "mode", "verdict", "acmmRequest", "externalCallsMade", "scan", "observations", "files", "artifactIndex", "capabilityParity", "overallDigest", "replayProtection", "provenance", "safety"); err != nil {
		return err
	}
	for _, field := range []string{"generatedAt", "expiresAt"} {
		if err := requireCanonicalV3Timestamp(root[field], field); err != nil {
			return err
		}
	}
	objects := []struct {
		field    string
		required []string
	}{
		{"producer", []string{"name", "version", "gitCommit"}},
		{"source", []string{"repository", "ref", "commitSha", "event", "workflowRunId", "workflowRunAttempt", "workflowArtifactId", "conclusion", "trusted"}},
		{"scan", []string{"scope", "authoritativeForResolution", "evaluatedContracts", "evaluatedFiles", "testPlanVersion", "toolRegistryVersion"}},
		{"artifactIndex", []string{"path", "sourcePath", "sha256", "schemaVersion", "contentAddressed", "complete", "artifactCount", "totalBytes"}},
		{"capabilityParity", []string{"path", "sourcePath", "sha256", "schemaVersion", "baselineVersion", "status", "runtimeStatus", "summary"}},
		{"replayProtection", []string{"nonce", "key"}},
		{"provenance", []string{"kind", "subjectDigest", "attestationRequired"}},
		{"safety", []string{"atomicWrite", "pathsAreRelative", "digestsRequired", "producerCountersAreAdvisory", "producerTrustClaimIsAdvisory", "absenceRequiresAuthoritativeScan"}},
	}
	for _, object := range objects {
		value, err := rawJSONObject(root[object.field], "bundle v3 "+object.field)
		if err != nil {
			return err
		}
		if err := requireJSONFields(value, "bundle v3 "+object.field, object.required...); err != nil {
			return err
		}
	}
	parityBinding, _ := rawJSONObject(root["capabilityParity"], "bundle v3 capabilityParity")
	paritySummary, err := rawJSONObject(parityBinding["summary"], "bundle v3 capabilityParity.summary")
	if err != nil {
		return err
	}
	if err := requireJSONFields(paritySummary, "bundle v3 capabilityParity.summary", parityCounterNames()...); err != nil {
		return err
	}
	var files []map[string]json.RawMessage
	if err := json.Unmarshal(root["files"], &files); err != nil {
		return fmt.Errorf("bundle v3 files must be an array of objects")
	}
	for index, file := range files {
		if err := requireJSONFields(file, fmt.Sprintf("bundle v3 files[%d]", index), "path", "sourcePath", "sha256", "size", "mediaType"); err != nil {
			return err
		}
	}
	var observations []map[string]json.RawMessage
	if err := json.Unmarshal(root["observations"], &observations); err != nil {
		return fmt.Errorf("bundle v3 observations must be an array of objects")
	}
	for index, observation := range observations {
		observationFields := []string{"fingerprint", "repositoryFingerprint", "publicationRole", "rootCauseKey", "blockedByRootKeys", "state", "issueKind", "severity", "owningAgentHint", "title", "body", "labels", "sourceArtifacts", "affectedContracts", "validationCommand", "observedAt", "firstSeenAt", "sourceArtifact"}
		if _, present := observation["affectedFiles"]; present {
			observationFields = append(observationFields, "affectedFiles")
		}
		if err := requireJSONFields(observation, fmt.Sprintf("bundle v3 observations[%d]", index), observationFields...); err != nil {
			return err
		}
	}
	return nil
}

func validateCapabilityParityJSONPresence(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("decode capability parity receipt: %w", err)
	}
	if err := requireJSONFields(root, "capability parity receipt", "schemaVersion", "baselineVersion", "generatedAt", "status", "runtimeStatus", "summary", "domains", "checks"); err != nil {
		return err
	}
	summary, err := rawJSONObject(root["summary"], "capability parity summary")
	if err != nil {
		return err
	}
	if err := requireJSONFields(summary, "capability parity summary", parityCounterNames()...); err != nil {
		return err
	}
	var domains []map[string]json.RawMessage
	if err := json.Unmarshal(root["domains"], &domains); err != nil {
		return fmt.Errorf("capability parity domains must be an array of objects")
	}
	for index, domain := range domains {
		if err := requireJSONFields(domain, fmt.Sprintf("capability parity domains[%d]", index), append([]string{"domain"}, parityCounterNames()...)...); err != nil {
			return err
		}
	}
	var checks []map[string]json.RawMessage
	if err := json.Unmarshal(root["checks"], &checks); err != nil {
		return fmt.Errorf("capability parity checks must be an array of objects")
	}
	for index, check := range checks {
		if err := requireJSONFields(check, fmt.Sprintf("capability parity checks[%d]", index), "domain", "key", "status", "parity", "message"); err != nil {
			return err
		}
	}
	return nil
}

func parityCounterNames() []string {
	return []string{"expected", "actual", "present", "blocked", "missing", "unexpected", "mismatched"}
}

func capabilityParityDomainNames() []string {
	return []string{"cli", "schemas", "evidenceResources", "artifactSurfaces", "planModes", "workflowLanes", "mutationOperators", "deterministicPrimitives", "providers", "openSourceAdapters", "controlPlane"}
}

func requireJSONFields(object map[string]json.RawMessage, label string, fields ...string) error {
	if object == nil {
		return fmt.Errorf("%s must be an object", label)
	}
	for _, field := range fields {
		value, ok := object[field]
		if !ok {
			return fmt.Errorf("%s is incomplete: missing %s", label, field)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("%s is invalid: %s cannot be null", label, field)
		}
	}
	return nil
}

func rawJSONObject(data json.RawMessage, label string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s must be an object", label)
	}
	return object, nil
}

func requireCanonicalV3Timestamp(data json.RawMessage, field string) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("bundle v3 %s must be a timestamp string", field)
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || value != parsed.UTC().Format(v3TimestampLayout) {
		return fmt.Errorf("bundle v3 %s must use canonical UTC millisecond form", field)
	}
	return nil
}

func validateV3ManifestContract(m Manifest) error {
	if m.SchemaVersion == ManifestSchema {
		if m.ArtifactIndex != nil || m.CapabilityParity != nil {
			return fmt.Errorf("bundle v2 cannot contain v3 evidence bindings")
		}
		return nil
	}
	if m.SchemaVersion != ManifestSchemaV3 {
		return nil
	}
	required := []string{m.Source.Repository, m.Source.Ref, m.Source.CommitSHA, m.Source.Event, m.Source.WorkflowRunID, m.Source.WorkflowRunAttempt, m.Source.WorkflowArtifactID, m.Source.Conclusion}
	for _, value := range required {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || trimmed == "unknown" || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("bundle v3 source identity is incomplete")
		}
	}
	if strings.TrimSpace(m.Mode) == "" || strings.TrimSpace(m.Verdict) == "" || m.ExternalCallsMade < 0 {
		return fmt.Errorf("bundle v3 metadata is incomplete")
	}
	for _, file := range m.Files {
		if file.MediaType != "application/json" && file.MediaType != "text/markdown" && file.MediaType != "text/plain" && file.MediaType != "application/octet-stream" {
			return fmt.Errorf("bundle v3 file %q has an invalid media type", file.Path)
		}
	}
	if m.ArtifactIndex == nil || m.CapabilityParity == nil {
		return fmt.Errorf("bundle v3 requires artifact index and capability parity bindings")
	}
	if err := validateArtifactIndexBinding(*m.ArtifactIndex, m.Files); err != nil {
		return err
	}
	if err := validateCapabilityParityBinding(*m.CapabilityParity, m.Files); err != nil {
		return err
	}
	return nil
}

func validateArtifactIndexBinding(binding ArtifactIndexBinding, files []File) error {
	if binding.SchemaVersion != artifactIndexSchemaVersion || !binding.ContentAddressed || !binding.Complete || !safeNonnegativeInteger(binding.ArtifactCount) || !safeNonnegativeInteger(binding.TotalBytes) {
		return fmt.Errorf("bundle v3 artifact index binding is invalid")
	}
	return validateBoundFile(binding.Path, binding.SourcePath, binding.SHA256, files, "artifact index")
}

func validateCapabilityParityBinding(binding CapabilityParityBinding, files []File) error {
	if binding.SchemaVersion != capabilityParitySchemaVersion || binding.BaselineVersion != capabilityBaselineSchemaVersion || binding.Status != "passed" || !nonnegativeParity(binding.Summary) {
		return fmt.Errorf("bundle v3 capability parity binding is invalid")
	}
	if binding.Summary.Missing != 0 || binding.Summary.Unexpected != 0 || binding.Summary.Mismatched != 0 {
		return fmt.Errorf("bundle v3 capability parity failed")
	}
	expectedRuntime := "ready"
	if binding.Summary.Blocked > 0 {
		expectedRuntime = "blocked"
	}
	if binding.RuntimeStatus != expectedRuntime {
		return fmt.Errorf("bundle v3 capability parity runtime status is inconsistent")
	}
	return validateBoundFile(binding.Path, binding.SourcePath, binding.SHA256, files, "capability parity")
}

func validateBoundFile(bindingPath, sourcePath, sha string, files []File, label string) error {
	if err := validateSourcePath(sourcePath); err != nil {
		return fmt.Errorf("%s binding: %w", label, err)
	}
	if bindingPath != "files/"+sourcePath || !hexDigest.MatchString(sha) {
		return fmt.Errorf("bundle v3 %s binding path or digest is invalid", label)
	}
	for _, file := range files {
		if file.Path == bindingPath && file.SourcePath == sourcePath && file.SHA256 == sha {
			return nil
		}
	}
	return fmt.Errorf("bundle v3 %s binding does not match a compact bundle file", label)
}

func validateV3BoundEvidence(m Manifest, evidence map[string][]byte, requireCanonicalIdentities bool) (*ArtifactIndexReport, error) {
	indexData := evidence[m.ArtifactIndex.SourcePath]
	parityData := evidence[m.CapabilityParity.SourcePath]
	if indexData == nil || parityData == nil {
		return nil, fmt.Errorf("bundle v3 compact evidence is incomplete")
	}
	index, err := decodeArtifactIndexReport(indexData)
	if err != nil {
		return nil, err
	}
	if index.Project != m.Project || index.Summary.ArtifactCount != m.ArtifactIndex.ArtifactCount || index.Summary.TotalBytes != m.ArtifactIndex.TotalBytes {
		return nil, fmt.Errorf("bundle v3 artifact index identity does not match its manifest binding")
	}
	entries, err := validateArtifactIndexReport(index, m.ArtifactIndex.SourcePath)
	if err != nil {
		return nil, err
	}
	var parity capabilityParityReport
	if err := validateCapabilityParityJSONPresence(parityData); err != nil {
		return nil, err
	}
	if err := decodeStrict(bytes.NewReader(parityData), &parity); err != nil {
		return nil, fmt.Errorf("decode capability parity receipt: %w", err)
	}
	if err := validateCapabilityParityReport(parity, requireCanonicalIdentities); err != nil {
		return nil, err
	}
	if parity.SchemaVersion != m.CapabilityParity.SchemaVersion || parity.BaselineVersion != m.CapabilityParity.BaselineVersion || parity.Status != m.CapabilityParity.Status || parity.RuntimeStatus != m.CapabilityParity.RuntimeStatus || parity.Summary != m.CapabilityParity.Summary {
		return nil, fmt.Errorf("bundle v3 capability parity receipt does not match its manifest binding")
	}
	parityEntry, ok := entries[m.CapabilityParity.SourcePath]
	if !ok || parityEntry.SHA256 != m.CapabilityParity.SHA256 || parityEntry.Bytes != int64(len(parityData)) {
		return nil, fmt.Errorf("bundle v3 capability parity receipt is missing or mismatched in the artifact index")
	}
	for _, file := range m.Files {
		if file.SourcePath == m.ArtifactIndex.SourcePath {
			continue
		}
		entry, ok := entries[file.SourcePath]
		if !ok || entry.SHA256 != file.SHA256 || entry.Bytes != file.Size {
			return nil, fmt.Errorf("bundle v3 compact file %q is missing or mismatched in the artifact index", file.SourcePath)
		}
	}
	return &index, nil
}

func decodeArtifactIndexReport(data []byte) (ArtifactIndexReport, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return ArtifactIndexReport{}, fmt.Errorf("decode artifact index: %w", err)
	}
	if err := requireJSONFields(raw, "artifact index", "schemaVersion", "project", "generatedAt", "root", "contentAddressed", "complete", "summary", "artifacts", "warnings"); err != nil {
		return ArtifactIndexReport{}, err
	}
	var summary map[string]json.RawMessage
	if err := json.Unmarshal(raw["summary"], &summary); err != nil {
		return ArtifactIndexReport{}, fmt.Errorf("artifact index summary is invalid")
	}
	if err := requireJSONFields(summary, "artifact index summary", "discoveredArtifactCount", "artifactCount", "omittedArtifactCount", "totalBytes", "json", "markdown", "image", "text", "typescript", "yaml", "log", "other", "previewed", "redactedPreviews", "truncatedPreviews"); err != nil {
		return ArtifactIndexReport{}, err
	}
	var report ArtifactIndexReport
	if err := json.Unmarshal(data, &report); err != nil {
		return ArtifactIndexReport{}, fmt.Errorf("decode artifact index: %w", err)
	}
	var artifacts []map[string]json.RawMessage
	if err := json.Unmarshal(raw["artifacts"], &artifacts); err != nil {
		return ArtifactIndexReport{}, fmt.Errorf("artifact index entries are invalid")
	}
	for position, artifact := range artifacts {
		if err := requireJSONFields(artifact, fmt.Sprintf("artifact index entry %d", position), "path", "kind", "contentType", "bytes", "sha256", "safeToRender", "previewTruncated", "previewRedacted", "labels"); err != nil {
			return ArtifactIndexReport{}, err
		}
	}
	return report, nil
}

func validateArtifactIndexReport(index ArtifactIndexReport, indexPath string) (map[string]ArtifactIndexEntry, error) {
	if index.SchemaVersion != artifactIndexSchemaVersion || !index.ContentAddressed || !index.Complete || strings.TrimSpace(index.Project) == "" || index.Warnings == nil || index.Artifacts == nil {
		return nil, fmt.Errorf("bundle v3 requires a complete content-addressed artifact index")
	}
	if _, err := time.Parse(time.RFC3339Nano, index.GeneratedAt); err != nil {
		return nil, fmt.Errorf("artifact index generatedAt is invalid")
	}
	if err := validateSourcePath(index.Root); err != nil {
		return nil, fmt.Errorf("artifact index root: %w", err)
	}
	counts := []int64{index.Summary.DiscoveredArtifactCount, index.Summary.ArtifactCount, index.Summary.OmittedArtifactCount, index.Summary.TotalBytes, index.Summary.JSON, index.Summary.Markdown, index.Summary.Image, index.Summary.Text, index.Summary.TypeScript, index.Summary.YAML, index.Summary.Log, index.Summary.Other, index.Summary.Previewed, index.Summary.RedactedPreviews, index.Summary.TruncatedPreviews}
	for _, count := range counts {
		if !safeNonnegativeInteger(count) {
			return nil, fmt.Errorf("artifact index counts are not non-negative safe integers")
		}
	}
	kindTotal := index.Summary.JSON + index.Summary.Markdown + index.Summary.Image + index.Summary.Text + index.Summary.TypeScript + index.Summary.YAML + index.Summary.Log + index.Summary.Other
	if index.Summary.OmittedArtifactCount != 0 || index.Summary.ArtifactCount != int64(len(index.Artifacts)) || index.Summary.DiscoveredArtifactCount != index.Summary.ArtifactCount || kindTotal != index.Summary.ArtifactCount || index.Summary.Previewed > index.Summary.ArtifactCount || index.Summary.RedactedPreviews > index.Summary.Previewed || index.Summary.TruncatedPreviews > index.Summary.Previewed {
		return nil, fmt.Errorf("artifact index counts are incomplete or inconsistent")
	}
	entries := make(map[string]ArtifactIndexEntry, len(index.Artifacts))
	var total int64
	kinds := map[string]int64{}
	var previewed, redacted, truncated int64
	for _, entry := range index.Artifacts {
		if err := validateSourcePath(entry.Path); err != nil {
			return nil, fmt.Errorf("artifact index entry: %w", err)
		}
		validKind := entry.Kind == "json" || entry.Kind == "markdown" || entry.Kind == "image" || entry.Kind == "text" || entry.Kind == "typescript" || entry.Kind == "yaml" || entry.Kind == "log" || entry.Kind == "other"
		if !pathWithin(index.Root, entry.Path) || entry.Path == indexPath || !safeNonnegativeInteger(entry.Bytes) || !hexDigest.MatchString(entry.SHA256) || !validKind || strings.TrimSpace(entry.ContentType) == "" || entry.Labels == nil {
			return nil, fmt.Errorf("artifact index entry %q is invalid", entry.Path)
		}
		if _, exists := entries[entry.Path]; exists {
			return nil, fmt.Errorf("artifact index contains duplicate path %q", entry.Path)
		}
		if entry.Bytes > int64(^uint64(0)>>1)-total {
			return nil, fmt.Errorf("artifact index total size overflows")
		}
		total += entry.Bytes
		entries[entry.Path] = entry
		kinds[entry.Kind]++
		if entry.PreviewRedacted {
			redacted++
		}
		if entry.PreviewTruncated {
			truncated++
		}
		if entry.Preview != nil {
			previewed++
		}
	}
	if total != index.Summary.TotalBytes {
		return nil, fmt.Errorf("artifact index totalBytes does not match its entries")
	}
	if kinds["json"] != index.Summary.JSON || kinds["markdown"] != index.Summary.Markdown || kinds["image"] != index.Summary.Image || kinds["text"] != index.Summary.Text || kinds["typescript"] != index.Summary.TypeScript || kinds["yaml"] != index.Summary.YAML || kinds["log"] != index.Summary.Log || kinds["other"] != index.Summary.Other || previewed != index.Summary.Previewed || redacted != index.Summary.RedactedPreviews || truncated != index.Summary.TruncatedPreviews {
		return nil, fmt.Errorf("artifact index summary does not match its entries")
	}
	return entries, nil
}

func validateCapabilityParityReport(report capabilityParityReport, requireCanonicalIdentities bool) error {
	if report.SchemaVersion != capabilityParitySchemaVersion || report.BaselineVersion != capabilityBaselineSchemaVersion || report.GeneratedAt.IsZero() || !nonnegativeParity(report.Summary) {
		return fmt.Errorf("capability parity receipt identity or counters are invalid")
	}
	knownDomains := make(map[string]bool, len(capabilityParityDomainNames()))
	for _, domain := range capabilityParityDomainNames() {
		knownDomains[domain] = true
	}
	domains := make(map[string]bool, len(report.Domains))
	domainSummaries := make(map[string]CapabilityParitySummary, len(report.Domains))
	var totals CapabilityParitySummary
	for _, domain := range report.Domains {
		if !knownDomains[domain.Domain] || domains[domain.Domain] || !nonnegativeParity(domain.CapabilityParitySummary) {
			return fmt.Errorf("capability parity receipt contains an invalid or duplicate domain")
		}
		domains[domain.Domain] = true
		domainSummaries[domain.Domain] = domain.CapabilityParitySummary
		totals = addParity(totals, domain.CapabilityParitySummary)
	}
	if len(domains) != len(knownDomains) {
		return fmt.Errorf("capability parity receipt must contain every required domain exactly once")
	}
	if totals != report.Summary {
		return fmt.Errorf("capability parity summary does not match domain totals")
	}
	var checks CapabilityParitySummary
	domainChecks := make(map[string]CapabilityParitySummary, len(domains))
	seenChecks := make(map[string]bool, len(report.Checks))
	for _, check := range report.Checks {
		key := strings.TrimSpace(check.Key)
		checkIdentity := check.Domain + "\x00" + key
		if !domains[check.Domain] || key == "" || key != check.Key || strings.ContainsRune(key, '\x00') || strings.TrimSpace(check.Message) == "" || strings.ContainsRune(check.Message, '\x00') || seenChecks[checkIdentity] {
			return fmt.Errorf("capability parity receipt contains a malformed check")
		}
		seenChecks[checkIdentity] = true
		parity := check.Status == "present" || check.Status == "blocked"
		if check.Parity != parity {
			return fmt.Errorf("capability parity boolean disagrees with check status")
		}
		expectedKey, expectedCanonical, err := capabilityDetailIdentity(check.Domain, check.Expected)
		if err != nil {
			return err
		}
		actualKey, actualCanonical, err := capabilityDetailIdentity(check.Domain, check.Actual)
		if err != nil {
			return err
		}
		if (len(expectedCanonical) == 0) != (len(actualCanonical) == 0) {
			return fmt.Errorf("capability parity expected and actual identities must be paired")
		}
		if requireCanonicalIdentities && parity && len(expectedCanonical) == 0 {
			return fmt.Errorf("trusted capability parity check %s:%s lacks canonical baseline and inventory identities", check.Domain, check.Key)
		}
		if len(expectedCanonical) != 0 {
			if expectedKey != check.Key || actualKey != check.Key || !bytes.Equal(expectedCanonical, actualCanonical) {
				return fmt.Errorf("capability parity check identity does not match its canonical baseline and inventory details")
			}
			runtimeStatus := capabilityDetailRuntimeStatus(check.Actual)
			if (check.Status == "blocked" && runtimeStatus != "blocked") || (check.Status == "present" && runtimeStatus == "blocked") {
				return fmt.Errorf("capability parity check status does not match its canonical inventory runtime status")
			}
		}
		domainCount := domainChecks[check.Domain]
		switch check.Status {
		case "present":
			checks.Present++
			domainCount.Present++
		case "blocked":
			checks.Blocked++
			domainCount.Blocked++
		case "missing":
			checks.Missing++
			domainCount.Missing++
		case "unexpected":
			checks.Unexpected++
			domainCount.Unexpected++
		case "mismatched":
			checks.Mismatched++
			domainCount.Mismatched++
		default:
			return fmt.Errorf("capability parity receipt contains an invalid check status")
		}
		domainChecks[check.Domain] = domainCount
	}
	for domain, summary := range domainSummaries {
		counted := domainChecks[domain]
		counted.Expected = counted.Present + counted.Blocked + counted.Missing + counted.Mismatched
		counted.Actual = counted.Present + counted.Blocked + counted.Unexpected + counted.Mismatched
		if counted != summary {
			return fmt.Errorf("capability parity domain %s summary does not match its unique checks", domain)
		}
	}
	checks.Expected = checks.Present + checks.Blocked + checks.Missing + checks.Mismatched
	checks.Actual = checks.Present + checks.Blocked + checks.Unexpected + checks.Mismatched
	if checks != report.Summary {
		return fmt.Errorf("capability parity summary does not match checks")
	}
	if report.Summary.Expected == 0 || report.Summary.Actual == 0 {
		return fmt.Errorf("capability parity receipt cannot bind an empty baseline or inventory")
	}
	failed := report.Summary.Missing + report.Summary.Unexpected + report.Summary.Mismatched
	expectedStatus := "passed"
	if failed != 0 {
		expectedStatus = "failed"
	}
	expectedRuntime := "ready"
	if report.Summary.Blocked != 0 {
		expectedRuntime = "blocked"
	}
	if report.Status != expectedStatus || report.RuntimeStatus != expectedRuntime || report.Status != "passed" {
		return fmt.Errorf("capability parity receipt failed or is internally inconsistent")
	}
	return nil
}

func capabilityDetailRuntimeStatus(data json.RawMessage) string {
	var object map[string]interface{}
	if err := json.Unmarshal(data, &object); err != nil {
		return ""
	}
	value, _ := object["runtimeStatus"].(string)
	return value
}

func capabilityDetailIdentity(domain string, data json.RawMessage) (string, []byte, error) {
	if len(data) == 0 {
		return "", nil, nil
	}
	var object map[string]interface{}
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return "", nil, fmt.Errorf("capability parity expected and actual details must be objects")
	}
	field := "id"
	switch domain {
	case "cli":
		field = "path"
	case "schemas":
		field = "filename"
	case "evidenceResources":
		field = "id"
	case "artifactSurfaces":
		field = "path"
	case "planModes":
		field = "mode"
	case "workflowLanes", "mutationOperators", "deterministicPrimitives", "providers", "openSourceAdapters":
		field = "id"
	case "controlPlane":
		method, methodOK := object["method"].(string)
		pathValue, pathOK := object["path"].(string)
		if !methodOK || !pathOK || strings.TrimSpace(method) == "" || strings.TrimSpace(pathValue) == "" {
			return "", nil, fmt.Errorf("capability parity control-plane identity is incomplete")
		}
		if err := validateCapabilityDetailContract(domain, object); err != nil {
			return "", nil, err
		}
		canonical, err := json.Marshal(object)
		return method + " " + pathValue, canonical, err
	default:
		return "", nil, fmt.Errorf("capability parity detail has an unknown domain %q", domain)
	}
	value, ok := object[field].(string)
	if !ok || strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || strings.ContainsRune(value, '\x00') {
		return "", nil, fmt.Errorf("capability parity %s identity is incomplete", domain)
	}
	if err := validateCapabilityDetailContract(domain, object); err != nil {
		return "", nil, err
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return "", nil, err
	}
	return value, canonical, nil
}

func validateCapabilityDetailContract(domain string, object map[string]interface{}) error {
	requireString := func(field string) error {
		value, ok := object[field].(string)
		if !ok || strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("capability parity %s detail is missing required string %s", domain, field)
		}
		return nil
	}
	requireDigest := func(field string) error {
		if err := requireString(field); err != nil {
			return err
		}
		if !hexDigest.MatchString(object[field].(string)) {
			return fmt.Errorf("capability parity %s detail has invalid digest %s", domain, field)
		}
		return nil
	}
	requireStrings := func(field string) error {
		values, ok := object[field].([]interface{})
		if !ok {
			return fmt.Errorf("capability parity %s detail is missing required array %s", domain, field)
		}
		seen := make(map[string]bool, len(values))
		for _, raw := range values {
			value, ok := raw.(string)
			if !ok || strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || strings.ContainsRune(value, '\x00') || seen[value] {
				return fmt.Errorf("capability parity %s detail has invalid array %s", domain, field)
			}
			seen[value] = true
		}
		return nil
	}
	requireRuntime := func() error {
		if err := requireString("runtimeStatus"); err != nil {
			return err
		}
		status := object["runtimeStatus"].(string)
		if status != "supported" && status != "blocked" {
			return fmt.Errorf("capability parity %s detail has invalid runtime status", domain)
		}
		if status == "blocked" {
			if err := requireString("blockedReason"); err != nil {
				return err
			}
		}
		return nil
	}

	switch domain {
	case "cli":
		if err := requireStrings("aliases"); err != nil {
			return err
		}
		return requireDigest("contractSha256")
	case "schemas":
		if err := requireString("id"); err != nil {
			return err
		}
		return requireDigest("sha256")
	case "evidenceResources":
		if err := requireString("uri"); err != nil {
			return err
		}
		if err := requireString("relativePath"); err != nil {
			return err
		}
		if _, exists := object["readTool"]; exists {
			return requireString("readTool")
		}
		return nil
	case "artifactSurfaces":
		if err := requireDigest("contractSha256"); err != nil {
			return err
		}
		return requireRuntime()
	case "planModes":
		return requireRuntime()
	case "workflowLanes":
		if err := requireString("kind"); err != nil {
			return err
		}
		kind := object["kind"].(string)
		if kind != "execution" && kind != "template" {
			return fmt.Errorf("capability parity workflow lane has invalid kind")
		}
		if err := requireString("laneId"); err != nil {
			return err
		}
		if kind == "template" {
			if err := requireString("path"); err != nil {
				return err
			}
			if err := requireDigest("sha256"); err != nil {
				return err
			}
		}
		return requireRuntime()
	case "mutationOperators":
		for _, field := range []string{"description", "defaultHeuristic"} {
			if err := requireString(field); err != nil {
				return err
			}
		}
		for _, field := range []string{"relevantSelectors", "recommendedContracts", "expectedFailureKinds"} {
			if err := requireStrings(field); err != nil {
				return err
			}
		}
		return requireRuntime()
	case "deterministicPrimitives":
		if err := requireString("kind"); err != nil {
			return err
		}
		kind := object["kind"].(string)
		if kind != "selector_assertion" && kind != "flow_action" {
			return fmt.Errorf("capability parity deterministic primitive has invalid kind")
		}
		return requireRuntime()
	case "providers":
		for _, field := range []string{"category", "deterministicRole"} {
			if err := requireString(field); err != nil {
				return err
			}
		}
		if err := requireStrings("supportedOperations"); err != nil {
			return err
		}
		operations, ok := object["operations"].([]interface{})
		if !ok {
			return fmt.Errorf("capability parity provider detail is missing operations")
		}
		seenOperations := make(map[string]bool, len(operations))
		for _, raw := range operations {
			operation, ok := raw.(map[string]interface{})
			if !ok {
				return fmt.Errorf("capability parity provider operation is invalid")
			}
			name, nameOK := operation["operation"].(string)
			status, statusOK := operation["runtimeStatus"].(string)
			if !nameOK || strings.TrimSpace(name) == "" || seenOperations[name] || !statusOK || (status != "supported" && status != "blocked") {
				return fmt.Errorf("capability parity provider operation identity is invalid")
			}
			seenOperations[name] = true
			if status == "blocked" {
				reason, ok := operation["blockedReason"].(string)
				if !ok || strings.TrimSpace(reason) == "" {
					return fmt.Errorf("capability parity blocked provider operation lacks a reason")
				}
			}
		}
		return requireRuntime()
	case "openSourceAdapters":
		for _, field := range []string{"version", "license", "command"} {
			if err := requireString(field); err != nil {
				return err
			}
		}
		return requireRuntime()
	case "controlPlane":
		method := object["method"].(string)
		if method != "GET" && method != "POST" {
			return fmt.Errorf("capability parity control-plane method is invalid")
		}
		return requireRuntime()
	default:
		return fmt.Errorf("capability parity detail has an unknown domain %q", domain)
	}
}

func nonnegativeParity(summary CapabilityParitySummary) bool {
	return safeNonnegativeInteger(summary.Expected) && safeNonnegativeInteger(summary.Actual) && safeNonnegativeInteger(summary.Present) && safeNonnegativeInteger(summary.Blocked) && safeNonnegativeInteger(summary.Missing) && safeNonnegativeInteger(summary.Unexpected) && safeNonnegativeInteger(summary.Mismatched)
}

func safeNonnegativeInteger(value int64) bool { return value >= 0 && value <= maxJSONSafeInteger }

func addParity(left, right CapabilityParitySummary) CapabilityParitySummary {
	return CapabilityParitySummary{Expected: left.Expected + right.Expected, Actual: left.Actual + right.Actual, Present: left.Present + right.Present, Blocked: left.Blocked + right.Blocked, Missing: left.Missing + right.Missing, Unexpected: left.Unexpected + right.Unexpected, Mismatched: left.Mismatched + right.Mismatched}
}

func replayKeyForManifest(m Manifest) string {
	if m.SchemaVersion == ManifestSchemaV3 {
		return digest([]byte(strings.Join([]string{"visual-hive.bundle.replay.v3", m.Source.Repository, m.Source.RepositoryID, m.Source.CommitSHA, m.Source.WorkflowRunID, m.Source.WorkflowRunAttempt, m.Source.WorkflowArtifactID, m.BundleID}, "\x00")))
	}
	return digest([]byte(strings.Join([]string{m.Source.Repository, m.Source.CommitSHA, valueOr(m.Source.WorkflowRunID, "local"), m.BundleID}, "\x00")))
}

func digestV3BundleContent(m Manifest) string {
	stream := &bytes.Buffer{}
	stream.WriteString(ContentAddressedDigestAlgorithm)
	files := make([][]byte, 0, len(m.Files))
	for _, file := range m.Files {
		record := newPublicationDigestRecord("file")
		record.scalar("path", file.Path)
		record.scalar("sourcePath", file.SourcePath)
		record.scalar("sha256", file.SHA256)
		record.scalar("size", strconv.FormatInt(file.Size, 10))
		record.scalar("mediaType", file.MediaType)
		record.scalar("schemaVersion", file.SchemaVersion)
		files = append(files, record.finish())
	}
	writePublicationDigestCollection(stream, "files", files)
	observations := make([][]byte, 0, len(m.Observations))
	for _, observation := range m.Observations {
		record := newPublicationDigestRecord("observation")
		record.scalar("repositoryFingerprint", observation.RepositoryFingerprint)
		record.scalar("fingerprint", observation.Fingerprint)
		record.scalar("publicationRole", observation.PublicationRole)
		record.scalar("rootCauseKey", observation.RootCauseKey)
		record.array("blockedByRootKeys", observation.BlockedByRootKeys)
		record.scalar("state", observation.State)
		record.scalar("issueKind", observation.IssueKind)
		record.scalar("severity", observation.Severity)
		record.scalar("owningAgentHint", observation.OwningAgentHint)
		record.scalar("title", observation.Title)
		record.scalar("body", observation.Body)
		record.array("labels", observation.Labels)
		record.array("sourceArtifacts", observation.SourceArtifacts)
		record.array("affectedContracts", observation.AffectedContracts)
		if observation.AffectedFiles != nil {
			record.array("affectedFiles", observation.AffectedFiles)
		}
		record.scalar("validationCommand", observation.ValidationCommand)
		record.scalar("observedAt", observation.ObservedAt)
		record.scalar("firstSeenAt", observation.FirstSeenAt)
		record.scalar("sourceArtifact", observation.SourceArtifact)
		observations = append(observations, record.finish())
	}
	writePublicationDigestCollection(stream, "observations", observations)
	scan := newPublicationDigestRecord("scan")
	scan.scalar("scope", m.Scan.Scope)
	scan.scalar("authoritativeForResolution", strconv.FormatBool(m.Scan.AuthoritativeForResolution))
	scan.array("evaluatedContracts", m.Scan.EvaluatedContracts)
	scan.array("evaluatedFiles", m.Scan.EvaluatedFiles)
	scan.scalar("testPlanVersion", m.Scan.TestPlanVersion)
	scan.scalar("toolRegistryVersion", m.Scan.ToolRegistryVersion)
	stream.Write(scan.finish())
	source := newPublicationDigestRecord("source")
	source.scalar("repository", m.Source.Repository)
	source.scalar("repositoryId", m.Source.RepositoryID)
	source.scalar("ref", m.Source.Ref)
	source.scalar("commitSha", m.Source.CommitSHA)
	source.scalar("event", m.Source.Event)
	source.scalar("workflowName", m.Source.WorkflowName)
	source.scalar("workflowRunId", m.Source.WorkflowRunID)
	source.scalar("workflowRunAttempt", m.Source.WorkflowRunAttempt)
	source.scalar("workflowArtifactId", m.Source.WorkflowArtifactID)
	source.scalar("conclusion", m.Source.Conclusion)
	source.scalar("trusted", strconv.FormatBool(m.Source.Trusted))
	stream.Write(source.finish())
	metadata := newPublicationDigestRecord("metadata")
	metadata.scalar("generatedAt", m.GeneratedAt.UTC().Format(v3TimestampLayout))
	metadata.scalar("expiresAt", m.ExpiresAt.UTC().Format(v3TimestampLayout))
	metadata.scalar("project", m.Project)
	metadata.scalar("mode", m.Mode)
	metadata.scalar("verdict", m.Verdict)
	metadata.scalar("acmmRequest", strconv.Itoa(m.ACMMRequest))
	metadata.scalar("externalCallsMade", strconv.Itoa(m.ExternalCallsMade))
	metadata.scalar("producerName", m.Producer.Name)
	metadata.scalar("producerVersion", m.Producer.Version)
	metadata.scalar("producerGitCommit", m.Producer.GitCommit)
	stream.Write(metadata.finish())
	index := newPublicationDigestRecord("artifactIndex")
	index.scalar("path", m.ArtifactIndex.Path)
	index.scalar("sourcePath", m.ArtifactIndex.SourcePath)
	index.scalar("sha256", m.ArtifactIndex.SHA256)
	index.scalar("schemaVersion", strconv.Itoa(m.ArtifactIndex.SchemaVersion))
	index.scalar("contentAddressed", strconv.FormatBool(m.ArtifactIndex.ContentAddressed))
	index.scalar("complete", strconv.FormatBool(m.ArtifactIndex.Complete))
	index.scalar("artifactCount", strconv.FormatInt(m.ArtifactIndex.ArtifactCount, 10))
	index.scalar("totalBytes", strconv.FormatInt(m.ArtifactIndex.TotalBytes, 10))
	stream.Write(index.finish())
	parity := newPublicationDigestRecord("capabilityParity")
	parity.scalar("path", m.CapabilityParity.Path)
	parity.scalar("sourcePath", m.CapabilityParity.SourcePath)
	parity.scalar("sha256", m.CapabilityParity.SHA256)
	parity.scalar("schemaVersion", m.CapabilityParity.SchemaVersion)
	parity.scalar("baselineVersion", m.CapabilityParity.BaselineVersion)
	parity.scalar("status", m.CapabilityParity.Status)
	parity.scalar("runtimeStatus", m.CapabilityParity.RuntimeStatus)
	parity.scalar("expected", strconv.FormatInt(m.CapabilityParity.Summary.Expected, 10))
	parity.scalar("actual", strconv.FormatInt(m.CapabilityParity.Summary.Actual, 10))
	parity.scalar("present", strconv.FormatInt(m.CapabilityParity.Summary.Present, 10))
	parity.scalar("blocked", strconv.FormatInt(m.CapabilityParity.Summary.Blocked, 10))
	parity.scalar("missing", strconv.FormatInt(m.CapabilityParity.Summary.Missing, 10))
	parity.scalar("unexpected", strconv.FormatInt(m.CapabilityParity.Summary.Unexpected, 10))
	parity.scalar("mismatched", strconv.FormatInt(m.CapabilityParity.Summary.Mismatched, 10))
	stream.Write(parity.finish())
	replay := newPublicationDigestRecord("replay")
	replay.scalar("key", m.ReplayProtection.Key)
	stream.Write(replay.finish())
	return digest(stream.Bytes())
}

// VerifiedReviewEvidenceArtifact is complete content-addressed PR evidence,
// but deliberately carries no trusted, authoritative, absence, baseline, or
// lifecycle authority. Hive may use it only to ground a bounded revision.
type VerifiedReviewEvidenceArtifact struct {
	SchemaVersion       string
	ArtifactIndexSHA256 string
	EvidenceRoot        string
	ArtifactCount       int64
	TotalBytes          int64
}

// VerifyReviewEvidenceArtifact verifies the complete artifacts-index.json
// emitted by Visual Hive's PR workflow. It never constructs a ValidatedBundle
// and therefore cannot confer deterministic resolution authority.
func VerifyReviewEvidenceArtifact(root string, artifactID int64) (VerifiedReviewEvidenceArtifact, error) {
	if artifactID <= 0 {
		return VerifiedReviewEvidenceArtifact{}, fmt.Errorf("positive PR evidence artifact ID is required")
	}
	const indexPath = ".visual-hive/artifacts-index.json"
	indexFile, err := safeSourceTarget(root, indexPath)
	if err != nil {
		return VerifiedReviewEvidenceArtifact{}, err
	}
	info, err := os.Lstat(indexFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxFileSize {
		return VerifiedReviewEvidenceArtifact{}, fmt.Errorf("PR evidence artifact index is not a bounded ordinary file")
	}
	data, err := os.ReadFile(indexFile)
	if err != nil {
		return VerifiedReviewEvidenceArtifact{}, fmt.Errorf("read stable PR evidence artifact index: %w", err)
	}
	if int64(len(data)) != info.Size() {
		return VerifiedReviewEvidenceArtifact{}, fmt.Errorf("PR evidence artifact index changed while being read")
	}
	index, err := decodeArtifactIndexReport(data)
	if err != nil {
		return VerifiedReviewEvidenceArtifact{}, err
	}
	if index.Root != ".visual-hive" {
		return VerifiedReviewEvidenceArtifact{}, fmt.Errorf("PR evidence artifact index root must be .visual-hive")
	}
	indexDigest := digest(data)
	_, evidenceRoot, err := verifyCompleteArtifactIndex(root, index, indexPath, indexDigest, strconv.FormatInt(artifactID, 10))
	if err != nil {
		return VerifiedReviewEvidenceArtifact{}, err
	}
	return VerifiedReviewEvidenceArtifact{
		SchemaVersion: ReviewEvidenceSchemaVersion, ArtifactIndexSHA256: indexDigest, EvidenceRoot: evidenceRoot,
		ArtifactCount: index.Summary.ArtifactCount, TotalBytes: index.Summary.TotalBytes,
	}, nil
}

// VerifySourceArtifact verifies the full source artifact against the v3
// content-addressed index. V2 bundles remain source-compatible and are a no-op.
func (bundle *ValidatedBundle) VerifySourceArtifact(root string) error {
	if bundle == nil || bundle.validatedManifest == nil {
		return fmt.Errorf("validated Visual Hive manifest snapshot is required")
	}
	currentManifest, err := json.Marshal(bundle.Manifest)
	if err != nil {
		return fmt.Errorf("encode current Visual Hive manifest: %w", err)
	}
	validatedManifest, err := json.Marshal(bundle.validatedManifest)
	if err != nil {
		return fmt.Errorf("encode validated Visual Hive manifest: %w", err)
	}
	if !bytes.Equal(currentManifest, validatedManifest) {
		return fmt.Errorf("Visual Hive manifest changed after validation")
	}
	manifest := *bundle.validatedManifest
	if manifest.SchemaVersion != ManifestSchemaV3 {
		return nil
	}
	bundle.sourceVerified = false
	bundle.verifiedSourceRoot = ""
	if bundle.provenanceVerified {
		// Re-verification must fail closed. A failed second verification cannot
		// retain authority established by an earlier source tree.
		bundle.Validation.Trusted = false
		bundle.Validation.Authoritative = false
	}
	if bundle.artifactIndex == nil || manifest.ArtifactIndex == nil {
		return fmt.Errorf("bundle v3 has no validated artifact index")
	}
	index := *bundle.artifactIndex
	absoluteRoot, _, err := verifyCompleteArtifactIndex(root, index, manifest.ArtifactIndex.SourcePath, manifest.ArtifactIndex.SHA256, manifest.Source.WorkflowArtifactID)
	if err != nil {
		return err
	}
	if bundle.provenanceVerified && bundle.validationProfile != bundleValidationPullRequestLocal {
		bundle.Validation.Trusted = true
		bundle.Validation.Authoritative = manifest.Scan.AuthoritativeForResolution
	}
	bundle.sourceVerified = true
	bundle.verifiedSourceRoot = absoluteRoot
	return nil
}

func verifyCompleteArtifactIndex(root string, index ArtifactIndexReport, indexPath, indexDigest, artifactID string) (string, string, error) {
	entries, err := validateArtifactIndexReport(index, indexPath)
	if err != nil {
		return "", "", err
	}
	indexFile, err := safeSourceTarget(root, indexPath)
	if err != nil {
		return "", "", err
	}
	if err := verifyIndexedFile(indexFile, int64(-1), indexDigest); err != nil {
		return "", "", fmt.Errorf("source artifact index identity mismatch: %w", err)
	}
	for relative, entry := range entries {
		target, err := safeSourceTarget(root, relative)
		if err != nil {
			return "", "", err
		}
		if err := verifyIndexedFile(target, entry.Bytes, entry.SHA256); err != nil {
			return "", "", fmt.Errorf("source artifact entry %q: %w", relative, err)
		}
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	indexedRoot, err := safeSourceTarget(absoluteRoot, index.Root)
	if err != nil {
		return "", "", err
	}
	if info, err := os.Lstat(indexedRoot); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("source artifact indexed root is not an ordinary directory")
	}
	err = filepath.WalkDir(absoluteRoot, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source artifact contains symbolic link %q", filePath)
		}
		relative, err := filepath.Rel(absoluteRoot, filePath)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "." {
			return nil
		}
		if isSourceBundlePath(index.Root, relative) {
			return fmt.Errorf("source artifact contains excluded bundle path %q", relative)
		}
		if entry.IsDir() {
			if pathWithin(index.Root, relative) || pathWithin(relative, index.Root) {
				return nil
			}
			return fmt.Errorf("source artifact contains unindexed path %q", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("source artifact contains non-regular file %q", relative)
		}
		if relative == sourceArtifactExtractionMarker {
			expected := []byte(artifactID + "\n")
			if info.Size() != int64(len(expected)) {
				return fmt.Errorf("source artifact extraction marker is invalid")
			}
			actual, err := os.ReadFile(filePath)
			if err != nil {
				return err
			}
			if !bytes.Equal(actual, expected) {
				return fmt.Errorf("source artifact extraction marker does not match the bound artifact")
			}
			return nil
		}
		if !pathWithin(index.Root, relative) {
			return fmt.Errorf("source artifact contains unindexed file %q", relative)
		}
		if relative == indexPath {
			return nil
		}
		if _, ok := entries[relative]; !ok {
			return fmt.Errorf("source artifact contains unindexed file %q", relative)
		}
		return nil
	})
	if err != nil {
		return "", "", fmt.Errorf("verify complete source artifact index: %w", err)
	}
	return absoluteRoot, indexedRoot, nil
}

// VerifiedEvidenceRoot returns the content-addressed evidence directory only
// after VerifySourceArtifact has accepted the complete source artifact. The
// artifact index, rather than a caller guess, selects the relative root.
func (bundle *ValidatedBundle) VerifiedEvidenceRoot() (string, error) {
	if bundle == nil || bundle.Manifest.SchemaVersion != ManifestSchemaV3 || !bundle.sourceVerified || bundle.verifiedSourceRoot == "" || bundle.artifactIndex == nil {
		return "", fmt.Errorf("Visual Hive source artifact has not been completely verified")
	}
	evidenceRoot, err := safeSourceTarget(bundle.verifiedSourceRoot, bundle.artifactIndex.Root)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(evidenceRoot)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("verified Visual Hive evidence root is not an ordinary directory")
	}
	return evidenceRoot, nil
}

// VerifiedSourceFileIdentity re-hashes one fixed file from a completely
// verified source artifact. It never grants lifecycle authority; callers use
// it to cross-bind source-only receipts that are intentionally absent from the
// compact bundle.
func (bundle *ValidatedBundle) VerifiedSourceFileIdentity(relative string) (string, int64, string, error) {
	if bundle == nil || !bundle.sourceVerified || bundle.verifiedSourceRoot == "" || bundle.artifactIndex == nil {
		return "", 0, "", fmt.Errorf("Visual Hive source artifact has not been completely verified")
	}
	var indexed *ArtifactIndexEntry
	for index := range bundle.artifactIndex.Artifacts {
		entry := &bundle.artifactIndex.Artifacts[index]
		if entry.Path == relative {
			indexed = entry
			break
		}
	}
	if indexed == nil {
		return "", 0, "", fmt.Errorf("source artifact file %q is not present in the immutable index", relative)
	}
	target, err := safeSourceTarget(bundle.verifiedSourceRoot, relative)
	if err != nil {
		return "", 0, "", err
	}
	if err := verifyIndexedFile(target, indexed.Bytes, indexed.SHA256); err != nil {
		return "", 0, "", fmt.Errorf("source artifact file %q changed after verification: %w", relative, err)
	}
	return target, indexed.Bytes, indexed.SHA256, nil
}

func (bundle *ValidatedBundle) readVerifiedSourceFile(relative string) ([]byte, int64, string, error) {
	target, size, sha, err := bundle.VerifiedSourceFileIdentity(relative)
	if err != nil {
		return nil, 0, "", err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, 0, "", err
	}
	if int64(len(data)) != size || digest(data) != sha {
		return nil, 0, "", fmt.Errorf("source artifact file %q changed while being read", relative)
	}
	return data, size, sha, nil
}

// VerifiedManifestSHA256 rechecks the exact manifest bytes that produced this
// validation result. A caller cannot substitute a same-sized manifest between
// parsing and receipt sealing.
func (bundle *ValidatedBundle) VerifiedManifestSHA256() (string, error) {
	if bundle == nil || bundle.validationProfile != bundleValidationPullRequestLocal || bundle.manifestPath == "" || !hexDigest.MatchString(bundle.manifestSHA256) {
		return "", fmt.Errorf("validated PR bundle manifest identity is unavailable")
	}
	data, err := os.ReadFile(bundle.manifestPath)
	if err != nil {
		return "", fmt.Errorf("read validated PR bundle manifest: %w", err)
	}
	if len(data) == 0 || len(data) > maxManifestSize || digest(data) != bundle.manifestSHA256 {
		return "", fmt.Errorf("validated PR bundle manifest changed before receipt sealing")
	}
	return bundle.manifestSHA256, nil
}

func safeSourceTarget(root, relative string) (string, error) {
	if err := validateSourcePath(relative); err != nil {
		return "", err
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(absoluteRoot, filepath.FromSlash(relative))
	back, err := filepath.Rel(absoluteRoot, target)
	if err != nil || filepath.IsAbs(back) || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe source artifact path %q", relative)
	}
	return target, nil
}

func verifyIndexedFile(filePath string, expectedSize int64, expectedDigest string) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || (expectedSize >= 0 && info.Size() != expectedSize) {
		return fmt.Errorf("file is missing, non-regular, or has the wrong size")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("hash file: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close hashed file: %w", closeErr)
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return fmt.Errorf("SHA-256 mismatch")
	}
	return nil
}

func pathWithin(root, candidate string) bool {
	return candidate == root || strings.HasPrefix(candidate, root+"/")
}

func isSourceBundlePath(root, candidate string) bool {
	if !pathWithin(root, candidate) {
		return false
	}
	relative := strings.TrimPrefix(strings.TrimPrefix(candidate, root), "/")
	parts := strings.Split(relative, "/")
	return len(parts) >= 1 && parts[0] == "bundles"
}
