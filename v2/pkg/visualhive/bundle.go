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
	ManifestSchema  = "visual-hive.bundle.v1"
	maxManifestSize = 2 << 20
	maxFileSize     = 25 << 20
	maxBundleSize   = 100 << 20
	maxBundleFiles  = 512
)

type Manifest struct {
	SchemaVersion     string     `json:"schemaVersion"`
	BundleID          string     `json:"bundleId"`
	GeneratedAt       time.Time  `json:"generatedAt"`
	ExpiresAt         time.Time  `json:"expiresAt"`
	Producer          Producer   `json:"producer"`
	Source            Source     `json:"source"`
	Project           string     `json:"project"`
	Mode              string     `json:"mode"`
	Verdict           string     `json:"verdict"`
	ACMMRequest       int        `json:"acmmRequest"`
	ExternalCallsMade int        `json:"externalCallsMade"`
	Files             []File     `json:"files"`
	OverallDigest     string     `json:"overallDigest"`
	Provenance        Provenance `json:"provenance"`
	Safety            Safety     `json:"safety"`
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
	Conclusion         string `json:"conclusion"`
	Trusted            bool   `json:"trusted"`
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
type Safety struct {
	AtomicWrite                 bool `json:"atomicWrite"`
	PathsAreRelative            bool `json:"pathsAreRelative"`
	DigestsRequired             bool `json:"digestsRequired"`
	ProducerCountersAreAdvisory bool `json:"producerCountersAreAdvisory"`
}

type ValidationOptions struct {
	Now        time.Time
	MaxACMM    int
	AllowLocal bool
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
	entries := make([]string, 0, len(manifest.Files))
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
		entries = append(entries, fmt.Sprintf("%s\x00%s\x00%d", file.Path, file.SHA256, file.Size))
		if strings.HasSuffix(file.SourcePath, "/hive/beads.json") || strings.HasSuffix(file.SourcePath, "/hive/hive-beads.json") || file.SourcePath == ".visual-hive/hive/beads.json" {
			if err := decodeStrict(bytes.NewReader(data), &projections); err != nil {
				return nil, fmt.Errorf("decode beads: %w", err)
			}
		}
	}
	sort.Strings(entries)
	overall := digest([]byte(strings.Join(entries, "\n")))
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
		Beads: len(projections), Trusted: manifest.Source.Trusted,
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
	if m.GeneratedAt.IsZero() || m.ExpiresAt.IsZero() || !m.ExpiresAt.After(m.GeneratedAt) || !now.Before(m.ExpiresAt) {
		return fmt.Errorf("bundle is expired or has an invalid lifetime")
	}
	if m.ACMMRequest < 1 || m.ACMMRequest > 6 || m.ACMMRequest > options.MaxACMM {
		return fmt.Errorf("bundle ACMM request %d exceeds allowed level %d", m.ACMMRequest, options.MaxACMM)
	}
	if len(m.Files) == 0 || len(m.Files) > maxBundleFiles || !hexDigest.MatchString(m.OverallDigest) {
		return fmt.Errorf("bundle file inventory is invalid")
	}
	if !m.Safety.AtomicWrite || !m.Safety.PathsAreRelative || !m.Safety.DigestsRequired || !m.Safety.ProducerCountersAreAdvisory {
		return fmt.Errorf("bundle safety contract is incomplete")
	}
	if options.AllowLocal {
		if m.Provenance.Kind != "local" && m.Provenance.Kind != "github-actions" {
			return fmt.Errorf("unsupported provenance kind %q", m.Provenance.Kind)
		}
	} else if !m.Source.Trusted || m.Source.Event == "pull_request" || m.Source.Conclusion != "success" || m.Provenance.Kind != "github-actions" || !m.Provenance.AttestationRequired {
		return fmt.Errorf("bundle is not from a trusted successful non-PR workflow")
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
	if path.IsAbs(file.SourcePath) || windowsDrive.MatchString(file.SourcePath) || path.Clean(file.SourcePath) != file.SourcePath || strings.HasPrefix(file.SourcePath, "../") {
		return fmt.Errorf("unsafe source path %q", file.SourcePath)
	}
	if seen[file.Path] || !hexDigest.MatchString(file.SHA256) || file.Size < 0 {
		return fmt.Errorf("invalid or duplicate bundle file %q", file.Path)
	}
	seen[file.Path] = true
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
