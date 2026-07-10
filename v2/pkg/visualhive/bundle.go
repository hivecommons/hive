// Package visualhive validates and imports deterministic Visual Hive evidence.
package visualhive

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

const (
	ManifestSchema  = "visual-hive.bundle.v2"
	maxManifestSize = 2 << 20
	maxFileSize     = 25 << 20
	maxBundleSize   = 100 << 20
	maxBundleFiles  = 512
)

type Manifest struct {
	SchemaVersion     string           `json:"schemaVersion"`
	BundleID          string           `json:"bundleId"`
	GeneratedAt       time.Time        `json:"generatedAt"`
	ExpiresAt         time.Time        `json:"expiresAt"`
	Producer          Producer         `json:"producer"`
	Source            Source           `json:"source"`
	Project           string           `json:"project"`
	Mode              string           `json:"mode"`
	Verdict           string           `json:"verdict"`
	ACMMRequest       int              `json:"acmmRequest"`
	ExternalCallsMade int              `json:"externalCallsMade"`
	Scan              Scan             `json:"scan"`
	Observations      []Observation    `json:"observations"`
	Files             []File           `json:"files"`
	OverallDigest     string           `json:"overallDigest"`
	ReplayProtection  ReplayProtection `json:"replayProtection"`
	Provenance        Provenance       `json:"provenance"`
	Safety            Safety           `json:"safety"`
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
	State                 string   `json:"state"`
	IssueKind             string   `json:"issueKind"`
	Severity              string   `json:"severity"`
	OwningAgentHint       string   `json:"owningAgentHint"`
	Title                 string   `json:"title"`
	Body                  string   `json:"body"`
	Labels                []string `json:"labels"`
	SourceArtifacts       []string `json:"sourceArtifacts"`
	AffectedContracts     []string `json:"affectedContracts"`
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

type ValidationOptions struct {
	Now                   time.Time
	MaxACMM               int
	AllowLocal            bool
	VerifiedProvenance    bool
	ExpectedRepository    string
	ExpectedRepositoryID  string
	ExpectedWorkflowRunID string
}

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
	Manifest   Manifest
	Validation Validation
	Beads      []Projection
}

var (
	hexDigest    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	safeID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	absolutePath = regexp.MustCompile(`(?i)(/home/|/Users/|[A-Z]:[/\\]+Users[/\\]+)`)
	secretValue  = regexp.MustCompile(`(?i)(github_pat_[A-Za-z0-9_]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9_-]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)
	windowsDrive = regexp.MustCompile(`^[A-Za-z]:/`)
)

func ValidateBundle(manifestPath string, options ValidationOptions) (*ValidatedBundle, error) {
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		return nil, err
	}
	defer manifestFile.Close()
	var manifest Manifest
	if err := decodeStrict(io.LimitReader(manifestFile, maxManifestSize+1), &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := validateManifest(manifest, options); err != nil {
		return nil, err
	}

	root := filepath.Dir(manifestPath)
	seenPaths := make(map[string]bool, len(manifest.Files))
	fileLines := make([]string, 0, len(manifest.Files))
	var total int64
	var projections []Projection
	for _, file := range manifest.Files {
		if err := validateFileRecord(file, seenPaths); err != nil {
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
	if projections == nil {
		return nil, fmt.Errorf("bundle does not contain .visual-hive/hive/beads.json")
	}
	if err := validateProjections(projections); err != nil {
		return nil, err
	}
	return &ValidatedBundle{Manifest: manifest, Beads: projections, Validation: Validation{
		SchemaVersion: "hive.visual-hive-validation.v1", Status: "passed", BundleID: manifest.BundleID,
		Project: manifest.Project, Digest: overall, Files: len(manifest.Files), Bytes: total,
		Beads: len(projections), Trusted: options.AllowLocal || options.VerifiedProvenance,
		Authoritative: manifest.Scan.AuthoritativeForResolution, Observations: len(manifest.Observations),
	}}, nil
}

func (bundle *ValidatedBundle) Import(store *beads.Store) (beads.BatchResult, error) {
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

func validateManifest(m Manifest, options ValidationOptions) error {
	now := options.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if options.MaxACMM == 0 {
		options.MaxACMM = 3
	}
	if m.SchemaVersion != ManifestSchema || !safeID.MatchString(m.BundleID) {
		return fmt.Errorf("unsupported or invalid bundle identity")
	}
	if m.Producer.Name != "visual-hive" || strings.TrimSpace(m.Producer.Version) == "" || strings.TrimSpace(m.Producer.GitCommit) == "" {
		return fmt.Errorf("invalid Visual Hive producer")
	}
	if strings.TrimSpace(m.Project) == "" || strings.TrimSpace(m.Source.Repository) == "" || strings.TrimSpace(m.Source.CommitSHA) == "" {
		return fmt.Errorf("bundle source identity is incomplete")
	}
	for _, value := range []string{m.Source.Repository, m.Source.RepositoryID, m.Source.Ref, m.Source.CommitSHA, m.Source.WorkflowRunID, m.Source.WorkflowArtifactID, m.Source.Conclusion} {
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
	expectedReplayKey := digest([]byte(strings.Join([]string{
		m.Source.Repository,
		m.Source.CommitSHA,
		valueOr(m.Source.WorkflowRunID, "local"),
		m.BundleID,
	}, "\x00")))
	if m.ReplayProtection.Nonce != m.BundleID || m.ReplayProtection.Key != expectedReplayKey {
		return fmt.Errorf("bundle replay protection mismatch")
	}
	if options.AllowLocal {
		if m.Provenance.Kind != "local" && m.Provenance.Kind != "github-actions" {
			return fmt.Errorf("unsupported provenance kind %q", m.Provenance.Kind)
		}
	} else {
		if !options.VerifiedProvenance {
			return fmt.Errorf("bundle provenance was not independently verified by Hive")
		}
		if m.Source.Event == "pull_request" || m.Source.Conclusion != "success" || m.Provenance.Kind != "github-actions" || !m.Provenance.AttestationRequired {
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
	}
	return nil
}

func validateScanAndObservations(m Manifest) error {
	validScope := m.Scan.Scope == "full" || m.Scan.Scope == "partial" || m.Scan.Scope == "changed-files" || m.Scan.Scope == "targeted"
	if !validScope || strings.TrimSpace(m.Scan.TestPlanVersion) == "" || strings.TrimSpace(m.Scan.ToolRegistryVersion) == "" {
		return fmt.Errorf("bundle scan contract is invalid")
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
	evaluatedContracts := make(map[string]bool, len(m.Scan.EvaluatedContracts))
	for _, contract := range m.Scan.EvaluatedContracts {
		evaluatedContracts[contract] = true
	}
	for _, observation := range m.Observations {
		digestFields := []string{observation.Fingerprint, observation.IssueKind, observation.OwningAgentHint, observation.Title, observation.Body, observation.ValidationCommand, observation.SourceArtifact}
		digestFields = append(digestFields, observation.Labels...)
		digestFields = append(digestFields, observation.SourceArtifacts...)
		digestFields = append(digestFields, observation.AffectedContracts...)
		for _, value := range digestFields {
			if strings.ContainsRune(value, '\x00') {
				return fmt.Errorf("bundle lifecycle observations cannot contain NUL delimiters")
			}
		}
		if strings.TrimSpace(observation.Fingerprint) == "" || !hexDigest.MatchString(observation.RepositoryFingerprint) || seen[observation.RepositoryFingerprint] {
			return fmt.Errorf("bundle lifecycle observation identity is invalid")
		}
		seen[observation.RepositoryFingerprint] = true
		expectedFingerprint := digest([]byte(strings.ToLower(strings.TrimSpace(m.Source.Repository)) + "\x00" + observation.Fingerprint))
		if observation.RepositoryFingerprint != expectedFingerprint {
			return fmt.Errorf("bundle lifecycle observation repository fingerprint mismatch")
		}
		if observation.State != "present" && observation.State != "absent" {
			return fmt.Errorf("bundle lifecycle observation state is invalid")
		}
		if observation.State == "absent" && !m.Scan.AuthoritativeForResolution {
			return fmt.Errorf("absent lifecycle observation requires an authoritative scan")
		}
		if strings.TrimSpace(observation.IssueKind) == "" || strings.TrimSpace(observation.OwningAgentHint) == "" || strings.TrimSpace(observation.Title) == "" || strings.TrimSpace(observation.Body) == "" || strings.TrimSpace(observation.ValidationCommand) == "" {
			return fmt.Errorf("bundle lifecycle observation is incomplete")
		}
		if len(observation.Title) > 512 || len(observation.Body) > 60000 || len(observation.Labels) > 50 || !sortedUnique(observation.Labels) || !sortedUnique(observation.SourceArtifacts) {
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
		if observation.State == "absent" {
			for _, contract := range observation.AffectedContracts {
				if !evaluatedContracts[contract] {
					return fmt.Errorf("absent lifecycle observation references an unevaluated contract %q", contract)
				}
			}
		}
	}
	return nil
}

func validateFileRecord(file File, seen map[string]bool) error {
	if file.Path == "" || file.SourcePath == "" || strings.Contains(file.Path, "\\") || strings.Contains(file.SourcePath, "\\") {
		return fmt.Errorf("unsafe bundle file path")
	}
	if !strings.HasPrefix(file.Path, "files/") || path.IsAbs(file.Path) || path.Clean(file.Path) != file.Path || strings.HasPrefix(file.Path, "../") {
		return fmt.Errorf("unsafe bundle file path %q", file.Path)
	}
	if err := validateSourcePath(file.SourcePath); err != nil {
		return err
	}
	if seen[file.Path] || !hexDigest.MatchString(file.SHA256) || file.Size < 0 {
		return fmt.Errorf("invalid or duplicate bundle file %q", file.Path)
	}
	seen[file.Path] = true
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
	lines := append([]string(nil), fileLines...)
	sort.Strings(lines)
	observationLines := make([]string, 0, len(manifest.Observations))
	for _, observation := range manifest.Observations {
		observationLines = append(observationLines, strings.Join([]string{
			"observation",
			observation.RepositoryFingerprint,
			observation.Fingerprint,
			observation.State,
			observation.IssueKind,
			observation.Severity,
			observation.OwningAgentHint,
			observation.Title,
			observation.Body,
			strings.Join(observation.Labels, ","),
			strings.Join(observation.SourceArtifacts, ","),
			strings.Join(observation.AffectedContracts, ","),
			observation.ValidationCommand,
			observation.ObservedAt,
			observation.FirstSeenAt,
			observation.SourceArtifact,
		}, "\x00"))
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
	lines = append(lines, "replay\x00"+manifest.ReplayProtection.Key)
	return digest([]byte(strings.Join(lines, "\n")))
}

func sortedUnique(values []string) bool {
	for i, value := range values {
		if strings.TrimSpace(value) == "" || (i > 0 && values[i-1] >= value) {
			return false
		}
	}
	return true
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
