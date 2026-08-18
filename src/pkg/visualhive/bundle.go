// Package visualhive validates and imports deterministic Visual Hive evidence.
package visualhive

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/beads"
)

const (
	ManifestSchema                  = "visual-hive.bundle.v2"
	ManifestSchemaV3                = "visual-hive.bundle.v3"
	PublicationDigestAlgorithm      = "visual-hive.bundle.publication-digest.v1"
	ContentAddressedDigestAlgorithm = "visual-hive.bundle.content-addressed-digest.v1"
	maxManifestSize                 = 2 << 20
	maxFileSize                     = 25 << 20
	maxBundleSize                   = 100 << 20
	maxBundleFiles                  = 512
)

type Manifest struct {
	SchemaVersion     string                   `json:"schemaVersion"`
	DigestAlgorithm   string                   `json:"digestAlgorithm,omitempty"`
	BundleID          string                   `json:"bundleId"`
	GeneratedAt       time.Time                `json:"generatedAt"`
	ExpiresAt         time.Time                `json:"expiresAt"`
	Producer          Producer                 `json:"producer"`
	Source            Source                   `json:"source"`
	Project           string                   `json:"project"`
	Mode              string                   `json:"mode"`
	Verdict           string                   `json:"verdict"`
	ACMMRequest       int                      `json:"acmmRequest"`
	ExternalCallsMade int                      `json:"externalCallsMade"`
	Scan              Scan                     `json:"scan"`
	Observations      []Observation            `json:"observations"`
	Files             []File                   `json:"files"`
	ArtifactIndex     *ArtifactIndexBinding    `json:"artifactIndex,omitempty"`
	CapabilityParity  *CapabilityParityBinding `json:"capabilityParity,omitempty"`
	OverallDigest     string                   `json:"overallDigest"`
	ReplayProtection  ReplayProtection         `json:"replayProtection"`
	Provenance        Provenance               `json:"provenance"`
	Safety            Safety                   `json:"safety"`
}

type Producer struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	GitCommit string `json:"gitCommit"`
}
type Source struct {
	Repository         string `json:"repository"`
	RepositoryID       string `json:"repositoryId,omitempty"`
	Ref                string `json:"ref"`
	CommitSHA          string `json:"commitSha"`
	Event              string `json:"event"`
	WorkflowName       string `json:"workflowName,omitempty"`
	WorkflowRunID      string `json:"workflowRunId,omitempty"`
	WorkflowRunAttempt string `json:"workflowRunAttempt,omitempty"`
	WorkflowArtifactID string `json:"workflowArtifactId,omitempty"`
	Conclusion         string `json:"conclusion"`
	Trusted            bool   `json:"trusted"`
}
type Scan struct {
	Scope                      string   `json:"scope"`
	AuthoritativeForResolution bool     `json:"authoritativeForResolution"`
	EvaluatedContracts         []string `json:"evaluatedContracts"`
	EvaluatedFiles             []string `json:"evaluatedFiles"`
	TestPlanVersion            string   `json:"testPlanVersion"`
	ToolRegistryVersion        string   `json:"toolRegistryVersion"`
}
type Observation struct {
	Fingerprint           string   `json:"fingerprint"`
	RepositoryFingerprint string   `json:"repositoryFingerprint"`
	PublicationRole       string   `json:"publicationRole,omitempty"`
	RootCauseKey          string   `json:"rootCauseKey,omitempty"`
	BlockedByRootKeys     []string `json:"blockedByRootKeys"`
	State                 string   `json:"state"`
	IssueKind             string   `json:"issueKind"`
	Severity              string   `json:"severity"`
	OwningAgentHint       string   `json:"owningAgentHint"`
	Title                 string   `json:"title"`
	Body                  string   `json:"body"`
	Labels                []string `json:"labels"`
	SourceArtifacts       []string `json:"sourceArtifacts"`
	AffectedContracts     []string `json:"affectedContracts"`
	AffectedFiles         []string `json:"affectedFiles,omitempty"`
	ValidationCommand     string   `json:"validationCommand"`
	ObservedAt            string   `json:"observedAt"`
	FirstSeenAt           string   `json:"firstSeenAt"`
	SourceArtifact        string   `json:"sourceArtifact"`
}
type File struct {
	Path          string `json:"path"`
	SourcePath    string `json:"sourcePath"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
	MediaType     string `json:"mediaType"`
	SchemaVersion string `json:"schemaVersion,omitempty"`
}
type Provenance struct {
	Kind                string `json:"kind"`
	SubjectDigest       string `json:"subjectDigest"`
	AttestationRequired bool   `json:"attestationRequired"`
}
type ReplayProtection struct {
	Nonce string `json:"nonce"`
	Key   string `json:"key"`
}
type Safety struct {
	AtomicWrite                      bool `json:"atomicWrite"`
	PathsAreRelative                 bool `json:"pathsAreRelative"`
	DigestsRequired                  bool `json:"digestsRequired"`
	ProducerCountersAreAdvisory      bool `json:"producerCountersAreAdvisory"`
	ProducerTrustClaimIsAdvisory     bool `json:"producerTrustClaimIsAdvisory"`
	AbsenceRequiresAuthoritativeScan bool `json:"absenceRequiresAuthoritativeScan"`
}

type ArtifactIndexBinding struct {
	Path             string `json:"path"`
	SourcePath       string `json:"sourcePath"`
	SHA256           string `json:"sha256"`
	SchemaVersion    int    `json:"schemaVersion"`
	ContentAddressed bool   `json:"contentAddressed"`
	Complete         bool   `json:"complete"`
	ArtifactCount    int64  `json:"artifactCount"`
	TotalBytes       int64  `json:"totalBytes"`
}

type CapabilityParitySummary struct {
	Expected   int64 `json:"expected"`
	Actual     int64 `json:"actual"`
	Present    int64 `json:"present"`
	Blocked    int64 `json:"blocked"`
	Missing    int64 `json:"missing"`
	Unexpected int64 `json:"unexpected"`
	Mismatched int64 `json:"mismatched"`
}

type CapabilityParityBinding struct {
	Path            string                  `json:"path"`
	SourcePath      string                  `json:"sourcePath"`
	SHA256          string                  `json:"sha256"`
	SchemaVersion   string                  `json:"schemaVersion"`
	BaselineVersion string                  `json:"baselineVersion"`
	Status          string                  `json:"status"`
	RuntimeStatus   string                  `json:"runtimeStatus"`
	Summary         CapabilityParitySummary `json:"summary"`
}

type ValidationOptions struct {
	Now                        time.Time
	MaxACMM                    int
	AllowLocal                 bool
	VerifiedProvenance         bool
	ExpectedProducerGitCommit  string
	ExpectedRepository         string
	ExpectedRepositoryID       string
	ExpectedWorkflowRunID      string
	ExpectedWorkflowRunAttempt string
	profile                    bundleValidationProfile
}

type bundleValidationProfile uint8

const (
	bundleValidationDefault bundleValidationProfile = iota
	bundleValidationPullRequestLocal
)

type Validation struct {
	SchemaVersion string `json:"schemaVersion"`
	Status        string `json:"status"`
	BundleID      string `json:"bundleId"`
	Project       string `json:"project"`
	Digest        string `json:"digest"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	Beads         int    `json:"beads"`
	Trusted       bool   `json:"trusted"`
	Authoritative bool   `json:"authoritativeForResolution"`
	Observations  int    `json:"observations"`
}

type Projection struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Type        string            `json:"type"`
	Status      string            `json:"status"`
	Priority    int               `json:"priority"`
	Actor       string            `json:"actor"`
	ExternalRef string            `json:"external_ref"`
	Metadata    map[string]string `json:"metadata"`
	Notes       string            `json:"notes"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
	ClosedAt    string            `json:"closed_at,omitempty"`
	DependsOn   []string          `json:"depends_on"`
}

type ValidatedBundle struct {
	Manifest           Manifest
	Validation         Validation
	Beads              []Projection
	manifestPath       string
	manifestSHA256     string
	artifactIndex      *ArtifactIndexReport
	validatedManifest  *Manifest
	provenanceVerified bool
	sourceVerified     bool
	verifiedSourceRoot string
	validationProfile  bundleValidationProfile
}

var (
	hexDigest    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	exactCommit  = regexp.MustCompile(`^[a-f0-9]{40}$`)
	safeID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	absolutePath = regexp.MustCompile(`(?i)(/home/|/Users/|[A-Z]:[/\\]+Users[/\\]+)`)
	secretValue  = regexp.MustCompile(`(?i)(github_pat_[A-Za-z0-9_]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9_-]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)
	windowsDrive = regexp.MustCompile(`^[A-Za-z]:/`)
	rootCauseKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~:/,%+-]{0,511}$`)
)

func ValidateBundle(manifestPath string, options ValidationOptions) (*ValidatedBundle, error) {
	if options.AllowLocal && options.VerifiedProvenance {
		return nil, fmt.Errorf("local bundle validation and independently verified provenance are mutually exclusive")
	}
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		return nil, err
	}
	defer manifestFile.Close()
	var manifest Manifest
	if err := decodeStrict(io.LimitReader(manifestFile, maxManifestSize+1), &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	manifestSHA256 := ""
	if manifest.SchemaVersion == ManifestSchemaV3 {
		manifestData, err := os.ReadFile(manifestPath)
		if err != nil {
			return nil, err
		}
		if len(manifestData) > maxManifestSize {
			return nil, fmt.Errorf("bundle v3 manifest exceeds %d bytes", maxManifestSize)
		}
		if err := validateV3ManifestJSONPresence(manifestData); err != nil {
			return nil, err
		}
		manifestSHA256 = digest(manifestData)
	}
	if err := validateManifest(manifest, options); err != nil {
		return nil, err
	}

	root := filepath.Dir(manifestPath)
	seenPaths := make(map[string]bool, len(manifest.Files))
	seenSourcePaths := make(map[string]bool, len(manifest.Files))
	fileLines := make([]string, 0, len(manifest.Files))
	boundEvidence := make(map[string][]byte, 2)
	var total int64
	var projections []Projection
	for _, file := range manifest.Files {
		var sourcePaths map[string]bool
		if manifest.SchemaVersion == ManifestSchemaV3 {
			sourcePaths = seenSourcePaths
		}
		if err := validateFileRecord(file, seenPaths, sourcePaths); err != nil {
			return nil, err
		}
		target := filepath.Join(root, filepath.FromSlash(file.Path))
		info, err := os.Lstat(target)
		if err != nil {
			return nil, fmt.Errorf("bundle file %q: %w", file.Path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("bundle file %q is not a regular file", file.Path)
		}
		if info.Size() != file.Size || info.Size() > maxFileSize {
			return nil, fmt.Errorf("bundle file %q has invalid size", file.Path)
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return nil, fmt.Errorf("read bundle file %q: %w", file.Path, err)
		}
		if digest(data) != file.SHA256 {
			return nil, fmt.Errorf("bundle file %q digest mismatch", file.Path)
		}
		if absolutePath.Match(data) || secretValue.Match(data) {
			return nil, fmt.Errorf("bundle file %q contains a forbidden path or credential value", file.Path)
		}
		total += int64(len(data))
		if total > maxBundleSize {
			return nil, fmt.Errorf("bundle exceeds %d bytes", maxBundleSize)
		}
		fileLines = append(fileLines, fmt.Sprintf("file\x00%s\x00%s\x00%d", file.Path, file.SHA256, file.Size))
		if manifest.SchemaVersion == ManifestSchemaV3 && ((manifest.ArtifactIndex != nil && file.SourcePath == manifest.ArtifactIndex.SourcePath) || (manifest.CapabilityParity != nil && file.SourcePath == manifest.CapabilityParity.SourcePath)) {
			boundEvidence[file.SourcePath] = data
		}
		if strings.HasSuffix(file.SourcePath, "/hive/beads.json") || strings.HasSuffix(file.SourcePath, "/hive/hive-beads.json") || file.SourcePath == ".visual-hive/hive/beads.json" {
			if err := decodeStrict(bytes.NewReader(data), &projections); err != nil {
				return nil, fmt.Errorf("decode beads: %w", err)
			}
		}
	}
	overall := digestBundleContent(manifest, fileLines)
	if overall != manifest.OverallDigest || manifest.Provenance.SubjectDigest != overall {
		return nil, fmt.Errorf("bundle overall digest mismatch")
	}
	var artifactIndex *ArtifactIndexReport
	if manifest.SchemaVersion == ManifestSchemaV3 {
		artifactIndex, err = validateV3BoundEvidence(manifest, boundEvidence, options.VerifiedProvenance || options.profile == bundleValidationPullRequestLocal)
		if err != nil {
			return nil, err
		}
	}
	if projections == nil {
		return nil, fmt.Errorf("bundle does not contain .visual-hive/hive/beads.json")
	}
	if err := validateProjections(projections); err != nil {
		return nil, err
	}
	validatedManifest, err := cloneValidatedManifest(manifest)
	if err != nil {
		return nil, err
	}
	absoluteManifestPath, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("resolve validated manifest path: %w", err)
	}
	return &ValidatedBundle{Manifest: manifest, validatedManifest: &validatedManifest, manifestPath: absoluteManifestPath, manifestSHA256: manifestSHA256, Beads: projections, artifactIndex: artifactIndex, provenanceVerified: options.VerifiedProvenance, validationProfile: options.profile, Validation: Validation{
		SchemaVersion: "hive.visual-hive-validation.v1", Status: "passed", BundleID: manifest.BundleID,
		Project: manifest.Project, Digest: overall, Files: len(manifest.Files), Bytes: total,
		Beads: len(projections), Trusted: options.AllowLocal,
		Authoritative: false, Observations: len(manifest.Observations),
	}}, nil
}

func cloneValidatedManifest(manifest Manifest) (Manifest, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return Manifest{}, fmt.Errorf("clone validated bundle manifest: %w", err)
	}
	var cloned Manifest
	if err := json.Unmarshal(data, &cloned); err != nil {
		return Manifest{}, fmt.Errorf("clone validated bundle manifest: %w", err)
	}
	return cloned, nil
}

func (bundle *ValidatedBundle) Import(store *beads.Store) (beads.BatchResult, error) {
	if bundle == nil || bundle.validationProfile == bundleValidationPullRequestLocal {
		return beads.BatchResult{}, fmt.Errorf("pull_request bundle parsing never grants bead import authority")
	}
	if store == nil {
		return beads.BatchResult{}, fmt.Errorf("persistent Hive bead store is required")
	}
	items := make([]beads.BatchInput, 0, len(bundle.Beads))
	for _, projection := range bundle.Beads {
		metadata := make(map[string]interface{}, len(projection.Metadata)+4)
		for key, value := range projection.Metadata {
			metadata[key] = value
		}
		metadata["visual_hive_bundle_id"] = bundle.Manifest.BundleID
		metadata["visual_hive_digest"] = bundle.Manifest.OverallDigest
		metadata["visual_hive_repository"] = bundle.Manifest.Source.Repository
		metadata["visual_hive_commit"] = bundle.Manifest.Source.CommitSHA
		items = append(items, beads.BatchInput{
			SourceID: projection.ID, Title: projection.Title, Type: beadType(projection.Type),
			Status: beadStatus(projection.Status), Priority: beadPriority(projection.Priority), Actor: projection.Actor,
			ExternalRef: projection.ExternalRef, Metadata: metadata, Notes: projection.Notes, DependsOn: projection.DependsOn,
		})
	}
	return store.ImportBatch(items)
}

// ImportByActor routes each validated Visual Hive projection to the Hive bead
// store named by its actor. This preserves agent ownership so channel-based
// agents, such as ux-scout, receive only their intended work items.
func (bundle *ValidatedBundle) ImportByActor(beadsRoot string) (beads.BatchResult, error) {
	if strings.TrimSpace(beadsRoot) == "" {
		return beads.BatchResult{}, fmt.Errorf("beads root is required")
	}
	result := beads.BatchResult{}
	byActor := make(map[string][]Projection)
	for _, projection := range bundle.Beads {
		actor, err := actorStoreName(projection.Actor)
		if err != nil {
			return result, err
		}
		byActor[actor] = append(byActor[actor], projection)
	}
	for actor, projections := range byActor {
		store, err := beads.NewStore(filepath.Join(beadsRoot, actor))
		if err != nil {
			return result, fmt.Errorf("open beads store for %s: %w", actor, err)
		}
		imported, err := importProjections(bundle, projections, store)
		if err != nil {
			return result, fmt.Errorf("import beads for %s: %w", actor, err)
		}
		result.Created += imported.Created
		result.Skipped += imported.Skipped
	}
	return result, nil
}

func importProjections(bundle *ValidatedBundle, projections []Projection, store *beads.Store) (beads.BatchResult, error) {
	if bundle == nil || bundle.validationProfile == bundleValidationPullRequestLocal {
		return beads.BatchResult{}, fmt.Errorf("pull_request bundle parsing never grants bead import authority")
	}
	if store == nil {
		return beads.BatchResult{}, fmt.Errorf("persistent Hive bead store is required")
	}
	items := make([]beads.BatchInput, 0, len(projections))
	for _, projection := range projections {
		metadata := make(map[string]interface{}, len(projection.Metadata)+4)
		for key, value := range projection.Metadata {
			metadata[key] = value
		}
		metadata["visual_hive_bundle_id"] = bundle.Manifest.BundleID
		metadata["visual_hive_digest"] = bundle.Manifest.OverallDigest
		metadata["visual_hive_repository"] = bundle.Manifest.Source.Repository
		metadata["visual_hive_commit"] = bundle.Manifest.Source.CommitSHA
		items = append(items, beads.BatchInput{
			SourceID: projection.ID, Title: projection.Title, Type: beadType(projection.Type),
			Status: beadStatus(projection.Status), Priority: beadPriority(projection.Priority), Actor: projection.Actor,
			ExternalRef: projection.ExternalRef, Metadata: metadata, Notes: projection.Notes, DependsOn: projection.DependsOn,
		})
	}
	return store.ImportBatch(items)
}

func actorStoreName(actor string) (string, error) {
	name := strings.TrimSpace(actor)
	name = strings.TrimPrefix(name, "hive/")
	if name == "" || !safeID.MatchString(name) {
		return "", fmt.Errorf("Visual Hive bead actor %q is not a safe Hive agent name", actor)
	}
	return name, nil
}

func validateManifest(m Manifest, options ValidationOptions) error {
	now := options.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if options.MaxACMM == 0 {
		options.MaxACMM = 3
	}
	if (m.SchemaVersion != ManifestSchema && m.SchemaVersion != ManifestSchemaV3) || !safeID.MatchString(m.BundleID) {
		return fmt.Errorf("unsupported or invalid bundle identity")
	}
	if m.SchemaVersion == ManifestSchema && m.DigestAlgorithm != "" && m.DigestAlgorithm != PublicationDigestAlgorithm {
		return fmt.Errorf("unsupported bundle digest algorithm %q", m.DigestAlgorithm)
	}
	if m.SchemaVersion == ManifestSchemaV3 && m.DigestAlgorithm != ContentAddressedDigestAlgorithm {
		return fmt.Errorf("unsupported bundle digest algorithm %q", m.DigestAlgorithm)
	}
	if err := validateV3ManifestContract(m); err != nil {
		return err
	}
	if m.Producer.Name != "visual-hive" || strings.TrimSpace(m.Producer.Version) == "" || strings.TrimSpace(m.Producer.GitCommit) == "" {
		return fmt.Errorf("invalid Visual Hive producer")
	}
	if strings.TrimSpace(m.Project) == "" || strings.TrimSpace(m.Source.Repository) == "" || strings.TrimSpace(m.Source.CommitSHA) == "" {
		return fmt.Errorf("bundle source identity is incomplete")
	}
	sourceIdentity := []string{m.Source.Repository, m.Source.RepositoryID, m.Source.Ref, m.Source.CommitSHA, m.Source.WorkflowRunID, m.Source.WorkflowArtifactID, m.Source.Conclusion}
	if m.SchemaVersion == ManifestSchemaV3 {
		sourceIdentity = append(sourceIdentity, m.Source.Event, m.Source.WorkflowName, m.Source.WorkflowRunAttempt)
	}
	for _, value := range sourceIdentity {
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("bundle source identity cannot contain NUL delimiters")
		}
	}
	if m.GeneratedAt.IsZero() || m.ExpiresAt.IsZero() || !m.ExpiresAt.After(m.GeneratedAt) || !now.Before(m.ExpiresAt) {
		return fmt.Errorf("bundle is expired or has an invalid lifetime")
	}
	if m.ACMMRequest < 1 || m.ACMMRequest > 6 || m.ACMMRequest > options.MaxACMM {
		return fmt.Errorf("bundle ACMM request %d exceeds allowed level %d", m.ACMMRequest, options.MaxACMM)
	}
	if len(m.Files) == 0 || len(m.Files) > maxBundleFiles || !hexDigest.MatchString(m.OverallDigest) {
		return fmt.Errorf("bundle file inventory is invalid")
	}
	if !m.Safety.AtomicWrite || !m.Safety.PathsAreRelative || !m.Safety.DigestsRequired || !m.Safety.ProducerCountersAreAdvisory || !m.Safety.ProducerTrustClaimIsAdvisory || !m.Safety.AbsenceRequiresAuthoritativeScan {
		return fmt.Errorf("bundle safety contract is incomplete")
	}
	if err := validateScanAndObservations(m); err != nil {
		return err
	}
	if !safeID.MatchString(m.ReplayProtection.Nonce) || !hexDigest.MatchString(m.ReplayProtection.Key) {
		return fmt.Errorf("bundle replay protection is invalid")
	}
	expectedReplayKey := replayKeyForManifest(m)
	if m.ReplayProtection.Nonce != m.BundleID || m.ReplayProtection.Key != expectedReplayKey {
		return fmt.Errorf("bundle replay protection mismatch")
	}
	if options.AllowLocal {
		if m.Provenance.Kind != "local" && m.Provenance.Kind != "github-actions" {
			return fmt.Errorf("unsupported provenance kind %q", m.Provenance.Kind)
		}
	} else {
		if !options.VerifiedProvenance && options.profile != bundleValidationPullRequestLocal {
			return fmt.Errorf("bundle provenance was not independently verified by Hive")
		}
		if m.SchemaVersion != ManifestSchemaV3 {
			return fmt.Errorf("integrated trusted production authority requires a Visual Hive bundle v3")
		}
		expectedProducerCommit := strings.TrimSpace(options.ExpectedProducerGitCommit)
		if !exactCommit.MatchString(expectedProducerCommit) {
			return fmt.Errorf("independently verified provenance requires an exact 40-character Visual Hive producer commit")
		}
		if m.Producer.GitCommit != expectedProducerCommit {
			return fmt.Errorf("Visual Hive producer commit does not match the independently pinned release commit")
		}
		if options.profile == bundleValidationPullRequestLocal {
			if m.Source.Event != "pull_request" || m.Source.Conclusion != "success" || m.Provenance.Kind != "github-actions" || !m.Provenance.AttestationRequired || m.Source.Trusted {
				return fmt.Errorf("bundle is not from a successful attested untrusted pull_request workflow")
			}
			if m.Scan.Scope != "changed-files" || m.Scan.AuthoritativeForResolution || len(m.Scan.EvaluatedContracts) == 0 || len(m.Scan.EvaluatedFiles) == 0 {
				return fmt.Errorf("pull request bundle requires a non-authoritative changed-files scan with an actual evaluated contract and file")
			}
			if !exactCommit.MatchString(m.Source.CommitSHA) || !strings.HasPrefix(m.Source.Ref, "refs/heads/") || strings.HasPrefix(m.Source.Ref, "refs/pull/") {
				return fmt.Errorf("pull request bundle source is not an exact non-merge head")
			}
		} else if m.Source.Event == "pull_request" || m.Source.Event == "pull_request_target" || m.Source.Conclusion != "success" || m.Provenance.Kind != "github-actions" || !m.Provenance.AttestationRequired {
			return fmt.Errorf("bundle is not from a successful attested non-PR workflow")
		}
		if options.ExpectedRepository != "" && !strings.EqualFold(options.ExpectedRepository, m.Source.Repository) {
			return fmt.Errorf("bundle repository does not match independently verified source")
		}
		if options.ExpectedRepositoryID != "" && options.ExpectedRepositoryID != m.Source.RepositoryID {
			return fmt.Errorf("bundle repository id does not match independently verified source")
		}
		if options.ExpectedWorkflowRunID != "" && options.ExpectedWorkflowRunID != m.Source.WorkflowRunID {
			return fmt.Errorf("bundle workflow run does not match independently verified source")
		}
		if m.SchemaVersion == ManifestSchemaV3 && options.ExpectedWorkflowRunAttempt != "" && options.ExpectedWorkflowRunAttempt != m.Source.WorkflowRunAttempt {
			return fmt.Errorf("bundle workflow run attempt does not match independently verified source")
		}
	}
	return nil
}

func validateScanAndObservations(m Manifest) error {
	validScope := m.Scan.Scope == "full" || m.Scan.Scope == "partial" || m.Scan.Scope == "changed-files" || m.Scan.Scope == "targeted"
	if !validScope || strings.TrimSpace(m.Scan.TestPlanVersion) == "" || strings.TrimSpace(m.Scan.ToolRegistryVersion) == "" {
		return fmt.Errorf("bundle scan contract is invalid")
	}
	// V2 artifact evidence has no versioned, digest-bound broker/executor
	// receipt. It remains useful for advisory repair context, but its producer
	// fields cannot establish finding-resolution authority.
	if m.SchemaVersion == ManifestSchema && m.Scan.AuthoritativeForResolution {
		return fmt.Errorf("bundle v2 artifact evidence is advisory and cannot claim authoritative resolution")
	}
	if m.Scan.AuthoritativeForResolution && m.Scan.Scope != "full" {
		return fmt.Errorf("only a full scan can be authoritative for resolution")
	}
	if !sortedUnique(m.Scan.EvaluatedContracts) || !sortedUnique(m.Scan.EvaluatedFiles) {
		return fmt.Errorf("bundle evaluated contract and file lists must be sorted and unique")
	}
	for _, value := range append(append([]string{m.Scan.Scope, m.Scan.TestPlanVersion, m.Scan.ToolRegistryVersion}, m.Scan.EvaluatedContracts...), m.Scan.EvaluatedFiles...) {
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("bundle scan metadata cannot contain NUL delimiters")
		}
	}
	seen := make(map[string]bool, len(m.Observations))
	canonicalRoots := make(map[string]bool)
	legacyPublicationMetadata := false
	explicitPublicationMetadata := false
	evaluatedContracts := make(map[string]bool, len(m.Scan.EvaluatedContracts))
	for _, contract := range m.Scan.EvaluatedContracts {
		evaluatedContracts[contract] = true
	}
	for _, observation := range m.Observations {
		digestFields := []string{observation.Fingerprint, observation.PublicationRole, observation.RootCauseKey, observation.IssueKind, observation.OwningAgentHint, observation.Title, observation.Body, observation.ValidationCommand, observation.SourceArtifact}
		digestFields = append(digestFields, observation.BlockedByRootKeys...)
		digestFields = append(digestFields, observation.Labels...)
		digestFields = append(digestFields, observation.SourceArtifacts...)
		digestFields = append(digestFields, observation.AffectedContracts...)
		digestFields = append(digestFields, observation.AffectedFiles...)
		for _, value := range digestFields {
			if strings.ContainsRune(value, '\x00') {
				return fmt.Errorf("bundle lifecycle observations cannot contain NUL delimiters")
			}
		}
		if err := validatePublicationMetadata(observation, canonicalRoots); err != nil {
			return err
		}
		if observation.PublicationRole == "" && observation.RootCauseKey == "" && observation.BlockedByRootKeys == nil {
			legacyPublicationMetadata = true
		} else {
			explicitPublicationMetadata = true
		}
		if strings.TrimSpace(observation.Fingerprint) == "" || !hexDigest.MatchString(observation.RepositoryFingerprint) || seen[observation.RepositoryFingerprint] {
			return fmt.Errorf("bundle lifecycle observation identity is invalid")
		}
		seen[observation.RepositoryFingerprint] = true
		fingerprintSource := observation.Fingerprint
		if observation.PublicationRole == "canonical" {
			fingerprintSource = observation.RootCauseKey
		}
		expectedFingerprint := digest([]byte(strings.ToLower(strings.TrimSpace(m.Source.Repository)) + "\x00" + fingerprintSource))
		if observation.RepositoryFingerprint != expectedFingerprint {
			return fmt.Errorf("bundle lifecycle observation repository fingerprint mismatch")
		}
		if observation.State != "present" && observation.State != "absent" {
			return fmt.Errorf("bundle lifecycle observation state is invalid")
		}
		if m.SchemaVersion == ManifestSchema && observation.State == "absent" {
			return fmt.Errorf("bundle v2 artifact evidence is advisory and cannot claim finding absence or resolution")
		}
		if observation.State == "absent" && !m.Scan.AuthoritativeForResolution {
			return fmt.Errorf("absent lifecycle observation requires an authoritative scan")
		}
		if strings.TrimSpace(observation.IssueKind) == "" || strings.TrimSpace(observation.OwningAgentHint) == "" || strings.TrimSpace(observation.Title) == "" || strings.TrimSpace(observation.Body) == "" || strings.TrimSpace(observation.ValidationCommand) == "" {
			return fmt.Errorf("bundle lifecycle observation is incomplete")
		}
		if len(observation.Title) > 512 || len(observation.Body) > 60000 || len(observation.Labels) > 50 || !sortedUnique(observation.Labels) || !sortedUnique(observation.SourceArtifacts) || !sortedUniqueOrEmpty(observation.AffectedFiles) {
			return fmt.Errorf("bundle lifecycle observation issue content is invalid")
		}
		for _, sourceArtifact := range observation.SourceArtifacts {
			if err := validateSourcePath(sourceArtifact); err != nil {
				return fmt.Errorf("bundle lifecycle observation source artifacts: %w", err)
			}
		}
		if observation.Severity != "low" && observation.Severity != "medium" && observation.Severity != "high" && observation.Severity != "critical" {
			return fmt.Errorf("bundle lifecycle observation severity is invalid")
		}
		observedAt, observedErr := time.Parse(time.RFC3339Nano, observation.ObservedAt)
		firstSeenAt, firstSeenErr := time.Parse(time.RFC3339Nano, observation.FirstSeenAt)
		if observedErr != nil || firstSeenErr != nil || firstSeenAt.After(observedAt) {
			return fmt.Errorf("bundle lifecycle observation timestamps are invalid")
		}
		if err := validateSourcePath(observation.SourceArtifact); err != nil {
			return fmt.Errorf("bundle lifecycle observation source artifact: %w", err)
		}
		if !sortedUnique(observation.AffectedContracts) {
			return fmt.Errorf("bundle lifecycle affected contracts must be sorted and unique")
		}
		for _, affectedFile := range observation.AffectedFiles {
			if err := validateSourcePath(affectedFile); err != nil {
				return fmt.Errorf("bundle lifecycle affected files: %w", err)
			}
		}
		if observation.State == "absent" {
			for _, contract := range observation.AffectedContracts {
				if !evaluatedContracts[contract] {
					return fmt.Errorf("absent lifecycle observation references an unevaluated contract %q", contract)
				}
			}
		}
	}
	if legacyPublicationMetadata && explicitPublicationMetadata {
		return fmt.Errorf("bundle cannot mix legacy and explicit publication metadata")
	}
	if m.SchemaVersion == ManifestSchemaV3 && legacyPublicationMetadata {
		return fmt.Errorf("bundle v3 requires complete explicit publication metadata")
	}
	if m.SchemaVersion == ManifestSchema && m.DigestAlgorithm == "" && explicitPublicationMetadata {
		return fmt.Errorf("explicit publication metadata requires digest algorithm %q", PublicationDigestAlgorithm)
	}
	if m.DigestAlgorithm == PublicationDigestAlgorithm && legacyPublicationMetadata {
		return fmt.Errorf("publication digest algorithm requires complete explicit publication metadata")
	}
	return nil
}

func validatePublicationMetadata(observation Observation, canonicalRoots map[string]bool) error {
	hasRole := observation.PublicationRole != ""
	hasRoot := observation.RootCauseKey != ""
	hasBlockedField := observation.BlockedByRootKeys != nil
	hasBlockedRoots := len(observation.BlockedByRootKeys) > 0
	if !hasRole && !hasRoot && !hasBlockedField {
		return nil // Legacy bundle: publication remains one issue per observation.
	}
	if !hasRole || !hasRoot || !hasBlockedField || !validRootCauseKey(observation.RootCauseKey) {
		return fmt.Errorf("bundle lifecycle observation publication metadata is incomplete or invalid")
	}
	if !sortedUnique(observation.BlockedByRootKeys) {
		return fmt.Errorf("bundle lifecycle blocked root keys must be sorted and unique")
	}
	for _, root := range observation.BlockedByRootKeys {
		if !validRootCauseKey(root) {
			return fmt.Errorf("bundle lifecycle blocked root key %q is invalid", root)
		}
	}
	knownKind := map[string]bool{
		"setup_needed": true, "map_drift": true, "missing_visual_coverage": true,
		"test_adequacy_gap": true, "weak_visual_test": true, "stale_baseline": true,
		"baseline_churn": true, "visual_regression": true, "selector_contract_failure": true,
		"screenshot_diff": true, "mutation_survivor": true, "workflow_safety": true,
		"provider_governance": true, "protected_target_blocked": true, "external_repo_onboarding": true,
	}
	if !knownKind[observation.IssueKind] {
		return fmt.Errorf("bundle lifecycle observation publication kind %q is unsupported", observation.IssueKind)
	}
	switch observation.PublicationRole {
	case "canonical":
		if hasBlockedRoots {
			return fmt.Errorf("canonical publication observation cannot block on other roots")
		}
		if canonicalRoots[observation.RootCauseKey] {
			return fmt.Errorf("bundle contains more than one canonical observation for root %q", observation.RootCauseKey)
		}
		canonicalRoots[observation.RootCauseKey] = true
	case "derivative":
		if hasBlockedRoots {
			return fmt.Errorf("derivative publication observation cannot block on other roots")
		}
		if observation.IssueKind != "missing_visual_coverage" && observation.IssueKind != "weak_visual_test" && observation.IssueKind != "external_repo_onboarding" {
			return fmt.Errorf("issue kind %q cannot be a derivative publication", observation.IssueKind)
		}
	case "aggregate":
		if !hasBlockedRoots {
			return fmt.Errorf("aggregate publication observation requires blocked root keys")
		}
		if observation.IssueKind != "external_repo_onboarding" && observation.IssueKind != "provider_governance" {
			return fmt.Errorf("issue kind %q cannot be an aggregate publication", observation.IssueKind)
		}
	default:
		return fmt.Errorf("bundle lifecycle observation publication role %q is unsupported", observation.PublicationRole)
	}
	return nil
}

func validRootCauseKey(value string) bool {
	if !rootCauseKey.MatchString(value) {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] != '%' {
			continue
		}
		if index+2 >= len(value) || !isHexByte(value[index+1]) || !isHexByte(value[index+2]) {
			return false
		}
		index += 2
	}
	return true
}

func isHexByte(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}

func validateFileRecord(file File, seen, seenSources map[string]bool) error {
	if file.Path == "" || file.SourcePath == "" || strings.Contains(file.Path, "\\") || strings.Contains(file.SourcePath, "\\") {
		return fmt.Errorf("unsafe bundle file path")
	}
	if !strings.HasPrefix(file.Path, "files/") || path.IsAbs(file.Path) || path.Clean(file.Path) != file.Path || strings.HasPrefix(file.Path, "../") {
		return fmt.Errorf("unsafe bundle file path %q", file.Path)
	}
	if err := validateSourcePath(file.SourcePath); err != nil {
		return err
	}
	if seen[file.Path] || (seenSources != nil && seenSources[file.SourcePath]) || !hexDigest.MatchString(file.SHA256) || file.Size < 0 {
		return fmt.Errorf("invalid or duplicate bundle file %q", file.Path)
	}
	seen[file.Path] = true
	if seenSources != nil {
		seenSources[file.SourcePath] = true
	}
	return nil
}

func validateSourcePath(sourcePath string) error {
	if sourcePath == "" || strings.Contains(sourcePath, "\\") || path.IsAbs(sourcePath) || windowsDrive.MatchString(sourcePath) || path.Clean(sourcePath) != sourcePath || strings.HasPrefix(sourcePath, "../") {
		return fmt.Errorf("unsafe source path %q", sourcePath)
	}
	return nil
}

func validateProjections(items []Projection) error {
	seenID, seenRef := map[string]bool{}, map[string]bool{}
	for _, item := range items {
		if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.Title) == "" || strings.TrimSpace(item.ExternalRef) == "" {
			return fmt.Errorf("Visual Hive bead requires id, title, and external_ref")
		}
		if seenID[item.ID] || seenRef[item.ExternalRef] {
			return fmt.Errorf("duplicate Visual Hive bead id or external_ref")
		}
		seenID[item.ID], seenRef[item.ExternalRef] = true, true
		if item.Priority < 0 || item.Priority > 4 {
			return fmt.Errorf("invalid Visual Hive bead priority")
		}
		if beadType(item.Type) == "" || beadStatus(item.Status) == "" {
			return fmt.Errorf("invalid Visual Hive bead type or status")
		}
	}
	for _, item := range items {
		for _, dependency := range item.DependsOn {
			if !seenID[dependency] {
				return fmt.Errorf("Visual Hive bead %q references unknown dependency %q", item.ID, dependency)
			}
		}
	}
	return nil
}

func decodeStrict(reader io.Reader, target interface{}) error {
	decoder := json.NewDecoder(bufio.NewReader(reader))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

func digestBundleContent(manifest Manifest, fileLines []string) string {
	if manifest.DigestAlgorithm == ContentAddressedDigestAlgorithm {
		return digestV3BundleContent(manifest)
	}
	if manifest.DigestAlgorithm == PublicationDigestAlgorithm {
		return digestPublicationBundleContent(manifest)
	}
	return digestLegacyBundleContent(manifest, fileLines)
}

func digestLegacyBundleContent(manifest Manifest, fileLines []string) string {
	lines := append([]string(nil), fileLines...)
	sort.Strings(lines)
	observationLines := make([]string, 0, len(manifest.Observations))
	for _, observation := range manifest.Observations {
		fields := []string{
			"observation",
			observation.RepositoryFingerprint,
			observation.Fingerprint,
		}
		if observation.PublicationRole != "" || observation.RootCauseKey != "" || len(observation.BlockedByRootKeys) > 0 {
			fields = append(fields, observation.PublicationRole, observation.RootCauseKey, strings.Join(observation.BlockedByRootKeys, ","))
		}
		fields = append(fields,
			observation.State,
			observation.IssueKind,
			observation.Severity,
			observation.OwningAgentHint,
			observation.Title,
			observation.Body,
			strings.Join(observation.Labels, ","),
			strings.Join(observation.SourceArtifacts, ","),
			strings.Join(observation.AffectedContracts, ","),
		)
		if observation.AffectedFiles != nil {
			fields = append(fields, strings.Join(observation.AffectedFiles, ","))
		}
		fields = append(fields,
			observation.ValidationCommand,
			observation.ObservedAt,
			observation.FirstSeenAt,
			observation.SourceArtifact,
		)
		observationLines = append(observationLines, strings.Join(fields, "\x00"))
	}
	sort.Strings(observationLines)
	lines = append(lines, observationLines...)
	lines = append(lines, strings.Join([]string{
		"scan",
		manifest.Scan.Scope,
		fmt.Sprintf("%t", manifest.Scan.AuthoritativeForResolution),
		strings.Join(manifest.Scan.EvaluatedContracts, ","),
		strings.Join(manifest.Scan.EvaluatedFiles, ","),
		manifest.Scan.TestPlanVersion,
		manifest.Scan.ToolRegistryVersion,
	}, "\x00"))
	lines = append(lines, strings.Join([]string{
		"source",
		manifest.Source.Repository,
		manifest.Source.RepositoryID,
		manifest.Source.Ref,
		manifest.Source.CommitSHA,
		manifest.Source.WorkflowRunID,
		manifest.Source.WorkflowArtifactID,
		manifest.Source.Conclusion,
	}, "\x00"))
	lines = append(lines, strings.Join([]string{
		"metadata",
		manifest.Project,
		manifest.Mode,
		manifest.Verdict,
		fmt.Sprintf("%d", manifest.ACMMRequest),
		fmt.Sprintf("%d", manifest.ExternalCallsMade),
		manifest.Producer.Name,
		manifest.Producer.Version,
		manifest.Producer.GitCommit,
	}, "\x00"))
	lines = append(lines, "replay\x00"+manifest.ReplayProtection.Key)
	return digest([]byte(strings.Join(lines, "\n")))
}

type publicationDigestRecord struct {
	bytes.Buffer
}

func newPublicationDigestRecord(domain string) *publicationDigestRecord {
	record := &publicationDigestRecord{}
	record.WriteByte('R')
	writePublicationDigestLP(&record.Buffer, []byte(domain))
	return record
}

func (record *publicationDigestRecord) scalar(field, value string) {
	record.WriteByte('S')
	writePublicationDigestLP(&record.Buffer, []byte(field))
	writePublicationDigestLP(&record.Buffer, []byte(value))
}

func (record *publicationDigestRecord) array(field string, values []string) {
	record.WriteByte('A')
	writePublicationDigestLP(&record.Buffer, []byte(field))
	writePublicationDigestLP(&record.Buffer, []byte(strconv.Itoa(len(values))))
	for _, value := range values {
		record.WriteByte('E')
		writePublicationDigestLP(&record.Buffer, []byte(value))
	}
}

func (record *publicationDigestRecord) finish() []byte {
	record.WriteByte('Z')
	return append([]byte(nil), record.Bytes()...)
}

func writePublicationDigestLP(target *bytes.Buffer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	target.Write(length[:])
	target.Write(value)
}

func writePublicationDigestCollection(target *bytes.Buffer, domain string, records [][]byte) {
	sort.Slice(records, func(left, right int) bool { return bytes.Compare(records[left], records[right]) < 0 })
	target.WriteByte('C')
	writePublicationDigestLP(target, []byte(domain))
	writePublicationDigestLP(target, []byte(strconv.Itoa(len(records))))
	for _, record := range records {
		target.WriteByte('I')
		writePublicationDigestLP(target, record)
	}
}

func digestPublicationBundleContent(manifest Manifest) string {
	stream := &bytes.Buffer{}
	stream.WriteString(PublicationDigestAlgorithm)

	fileRecords := make([][]byte, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		record := newPublicationDigestRecord("file")
		record.scalar("path", file.Path)
		record.scalar("sha256", file.SHA256)
		record.scalar("size", strconv.FormatInt(file.Size, 10))
		fileRecords = append(fileRecords, record.finish())
	}
	writePublicationDigestCollection(stream, "files", fileRecords)

	observationRecords := make([][]byte, 0, len(manifest.Observations))
	for _, observation := range manifest.Observations {
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
		observationRecords = append(observationRecords, record.finish())
	}
	writePublicationDigestCollection(stream, "observations", observationRecords)

	scan := newPublicationDigestRecord("scan")
	scan.scalar("scope", manifest.Scan.Scope)
	scan.scalar("authoritativeForResolution", strconv.FormatBool(manifest.Scan.AuthoritativeForResolution))
	scan.array("evaluatedContracts", manifest.Scan.EvaluatedContracts)
	scan.array("evaluatedFiles", manifest.Scan.EvaluatedFiles)
	scan.scalar("testPlanVersion", manifest.Scan.TestPlanVersion)
	scan.scalar("toolRegistryVersion", manifest.Scan.ToolRegistryVersion)
	stream.Write(scan.finish())

	source := newPublicationDigestRecord("source")
	source.scalar("repository", manifest.Source.Repository)
	source.scalar("repositoryId", manifest.Source.RepositoryID)
	source.scalar("ref", manifest.Source.Ref)
	source.scalar("commitSha", manifest.Source.CommitSHA)
	source.scalar("workflowRunId", manifest.Source.WorkflowRunID)
	source.scalar("workflowArtifactId", manifest.Source.WorkflowArtifactID)
	source.scalar("conclusion", manifest.Source.Conclusion)
	stream.Write(source.finish())

	metadata := newPublicationDigestRecord("metadata")
	metadata.scalar("project", manifest.Project)
	metadata.scalar("mode", manifest.Mode)
	metadata.scalar("verdict", manifest.Verdict)
	metadata.scalar("acmmRequest", strconv.Itoa(manifest.ACMMRequest))
	metadata.scalar("externalCallsMade", strconv.Itoa(manifest.ExternalCallsMade))
	metadata.scalar("producerName", manifest.Producer.Name)
	metadata.scalar("producerVersion", manifest.Producer.Version)
	metadata.scalar("producerGitCommit", manifest.Producer.GitCommit)
	stream.Write(metadata.finish())

	replay := newPublicationDigestRecord("replay")
	replay.scalar("key", manifest.ReplayProtection.Key)
	stream.Write(replay.finish())

	return digest(stream.Bytes())
}

func sortedUnique(values []string) bool {
	for i, value := range values {
		if strings.TrimSpace(value) == "" || (i > 0 && values[i-1] >= value) {
			return false
		}
	}
	return true
}

func sortedUniqueOrEmpty(values []string) bool {
	return len(values) == 0 || sortedUnique(values)
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func beadType(value string) beads.BeadType {
	switch value {
	case "bug":
		return beads.TypeBug
	case "feature":
		return beads.TypeFeature
	case "task":
		return beads.TypeTask
	case "epic":
		return beads.TypeEpic
	case "chore":
		return beads.TypeChore
	case "decision":
		return beads.TypeDecision
	case "advisory":
		return beads.TypeAdvisory
	}
	return ""
}
func beadStatus(value string) beads.Status {
	switch value {
	case "open":
		return beads.StatusOpen
	case "in_progress":
		return beads.StatusInProgress
	case "blocked":
		return beads.StatusBlocked
	case "done":
		return beads.StatusDone
	case "closed":
		return beads.StatusClosed
	}
	return ""
}
func beadPriority(value int) beads.Priority { return beads.Priority(value) }
