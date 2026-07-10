package github

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const (
	maxVisualHiveArtifactBytes = 110 << 20
	maxVisualHiveArchiveFiles  = 1024
)

type VisualHiveArtifactRequest struct {
	Repository       string
	WorkflowRunID    int64
	ArtifactID       int64
	SourceArtifactID int64
	DestinationDir   string
	TargetRef        string
	MaxACMM          int
}

type VerifiedVisualHiveArtifact struct {
	RepositoryID     string `json:"repository_id"`
	WorkflowRunID    string `json:"workflow_run_id"`
	ArtifactID       string `json:"artifact_id"`
	SourceArtifactID string `json:"source_artifact_id"`
	ArtifactName     string `json:"artifact_name"`
	CommitSHA        string `json:"commit_sha"`
	HeadBranch       string `json:"head_branch"`
	Event            string `json:"event"`
	WorkflowName     string `json:"workflow_name"`
	RunURL           string `json:"run_url"`
	ManifestPath     string `json:"manifest_path"`
}

// FetchAndVerifyVisualHiveBundle downloads the artifact through Hive's GitHub
// client and binds the extracted manifest to authoritative repository and run
// metadata before allowing the bundle validator to mark provenance verified.
func (c *Client) FetchAndVerifyVisualHiveBundle(ctx context.Context, request VisualHiveArtifactRequest) (*visualhive.ValidatedBundle, VerifiedVisualHiveArtifact, error) {
	owner, repo, err := splitFullRepository(request.Repository)
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, err
	}
	if request.WorkflowRunID <= 0 || request.ArtifactID <= 0 || strings.TrimSpace(request.DestinationDir) == "" {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("workflow run id, artifact id, and destination directory are required")
	}
	repository, _, err := c.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("verify Visual Hive repository: %w", err)
	}
	run, _, err := c.client.Actions.GetWorkflowRunByID(ctx, owner, repo, request.WorkflowRunID)
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("verify Visual Hive workflow run: %w", err)
	}
	if run.GetID() != request.WorkflowRunID || run.GetConclusion() != "success" || run.GetEvent() == "pull_request" || strings.TrimSpace(run.GetHeadSHA()) == "" {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive workflow run is not a successful non-PR run")
	}
	if targetRef := strings.TrimPrefix(strings.TrimSpace(request.TargetRef), "refs/heads/"); targetRef != "" && run.GetHeadBranch() != targetRef {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive workflow run branch %q does not match target branch %q", run.GetHeadBranch(), targetRef)
	}
	artifact, err := c.findRunArtifact(ctx, owner, repo, request.WorkflowRunID, request.ArtifactID)
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, err
	}
	if artifact.GetExpired() || artifact.GetSizeInBytes() <= 0 || artifact.GetSizeInBytes() > maxVisualHiveArtifactBytes {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive artifact is expired or has an invalid size")
	}
	sourceArtifactID := request.SourceArtifactID
	if sourceArtifactID <= 0 {
		sourceArtifactID = request.ArtifactID
	}
	sourceArtifact := artifact
	if sourceArtifactID != request.ArtifactID {
		sourceArtifact, err = c.findRunArtifact(ctx, owner, repo, request.WorkflowRunID, sourceArtifactID)
		if err != nil {
			return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("verify Visual Hive source artifact: %w", err)
		}
		if sourceArtifact.GetExpired() || sourceArtifact.GetSizeInBytes() <= 0 || sourceArtifact.GetSizeInBytes() > maxVisualHiveArtifactBytes {
			return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive source artifact is expired or has an invalid size")
		}
	}
	if artifact.WorkflowRun != nil {
		if artifact.WorkflowRun.GetID() != 0 && artifact.WorkflowRun.GetID() != request.WorkflowRunID {
			return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive artifact workflow run mismatch")
		}
		if artifact.WorkflowRun.GetRepositoryID() != 0 && artifact.WorkflowRun.GetRepositoryID() != repository.GetID() {
			return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive artifact repository mismatch")
		}
		if artifact.WorkflowRun.GetHeadSHA() != "" && artifact.WorkflowRun.GetHeadSHA() != run.GetHeadSHA() {
			return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive artifact commit mismatch")
		}
	}
	manifestPath, err := c.downloadAndExtractVisualHiveArtifact(ctx, owner, repo, request.ArtifactID, request.DestinationDir)
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, err
	}
	verified := VerifiedVisualHiveArtifact{
		RepositoryID: strconv.FormatInt(repository.GetID(), 10), WorkflowRunID: strconv.FormatInt(request.WorkflowRunID, 10),
		ArtifactID: strconv.FormatInt(request.ArtifactID, 10), ArtifactName: artifact.GetName(), CommitSHA: run.GetHeadSHA(),
		SourceArtifactID: strconv.FormatInt(sourceArtifactID, 10),
		HeadBranch:       run.GetHeadBranch(), Event: run.GetEvent(), WorkflowName: run.GetName(), RunURL: run.GetHTMLURL(), ManifestPath: manifestPath,
	}
	bundle, err := visualhive.ValidateBundle(manifestPath, visualhive.ValidationOptions{
		MaxACMM: request.MaxACMM, VerifiedProvenance: true, ExpectedRepository: request.Repository,
		ExpectedRepositoryID: verified.RepositoryID, ExpectedWorkflowRunID: verified.WorkflowRunID,
	})
	if err != nil {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("validate independently fetched Visual Hive bundle: %w", err)
	}
	manifest := bundle.Manifest
	if manifest.Source.CommitSHA != verified.CommitSHA || manifest.Source.Event != verified.Event || manifest.Source.WorkflowArtifactID != verified.SourceArtifactID {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive manifest does not match independently fetched workflow metadata")
	}
	if strings.TrimPrefix(manifest.Source.Ref, "refs/heads/") != verified.HeadBranch {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive manifest ref does not match independently fetched workflow branch")
	}
	if manifest.Source.WorkflowName != "" && verified.WorkflowName != "" && manifest.Source.WorkflowName != verified.WorkflowName {
		return nil, VerifiedVisualHiveArtifact{}, fmt.Errorf("Visual Hive manifest workflow name mismatch")
	}
	return bundle, verified, nil
}

func (c *Client) findRunArtifact(ctx context.Context, owner, repo string, runID, artifactID int64) (*gh.Artifact, error) {
	page := 1
	for page > 0 {
		artifacts, response, err := c.client.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, runID, &gh.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, fmt.Errorf("list Visual Hive workflow artifacts: %w", err)
		}
		for _, artifact := range artifacts.Artifacts {
			if artifact.GetID() == artifactID {
				return artifact, nil
			}
		}
		page = response.NextPage
	}
	return nil, fmt.Errorf("Visual Hive artifact %d does not belong to workflow run %d", artifactID, runID)
}

func (c *Client) downloadAndExtractVisualHiveArtifact(ctx context.Context, owner, repo string, artifactID int64, destinationDir string) (string, error) {
	if err := os.MkdirAll(destinationDir, 0o700); err != nil {
		return "", fmt.Errorf("create Visual Hive artifact directory: %w", err)
	}
	finalDir := filepath.Join(destinationDir, fmt.Sprintf("artifact-%d", artifactID))
	if manifestPath, err := findVisualHiveManifest(finalDir); err == nil {
		return manifestPath, nil
	}
	downloadURL, _, err := c.client.Actions.DownloadArtifact(ctx, owner, repo, artifactID, 3)
	if err != nil {
		return "", fmt.Errorf("request Visual Hive artifact download: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL.String(), nil)
	if err != nil {
		return "", err
	}
	response, err := c.client.Client().Do(httpRequest)
	if err != nil {
		return "", fmt.Errorf("download Visual Hive artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download Visual Hive artifact: HTTP %d", response.StatusCode)
	}
	temporaryFile, err := os.CreateTemp(destinationDir, ".visual-hive-artifact-*.zip")
	if err != nil {
		return "", err
	}
	temporaryZip := temporaryFile.Name()
	defer os.Remove(temporaryZip)
	written, copyErr := io.Copy(temporaryFile, io.LimitReader(response.Body, maxVisualHiveArtifactBytes+1))
	closeErr := temporaryFile.Close()
	if copyErr != nil || closeErr != nil || written > maxVisualHiveArtifactBytes {
		return "", fmt.Errorf("Visual Hive artifact download exceeded limits or failed")
	}
	temporaryDir, err := os.MkdirTemp(destinationDir, ".visual-hive-extract-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporaryDir)
	if err := extractVisualHiveZip(temporaryZip, temporaryDir); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryDir, finalDir); err != nil {
		if _, statErr := os.Stat(finalDir); statErr != nil {
			return "", fmt.Errorf("publish Visual Hive artifact: %w", err)
		}
	}
	return findVisualHiveManifest(finalDir)
}

func extractVisualHiveZip(zipPath, destination string) error {
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open Visual Hive artifact zip: %w", err)
	}
	defer archive.Close()
	if len(archive.File) == 0 || len(archive.File) > maxVisualHiveArchiveFiles {
		return fmt.Errorf("Visual Hive artifact zip has an invalid file count")
	}
	var total uint64
	for _, entry := range archive.File {
		clean := filepath.Clean(filepath.FromSlash(entry.Name))
		if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || entry.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe Visual Hive artifact zip entry %q", entry.Name)
		}
		total += entry.UncompressedSize64
		if total > maxVisualHiveArtifactBytes {
			return fmt.Errorf("Visual Hive artifact uncompressed size exceeds limit")
		}
		target := filepath.Join(destination, clean)
		relative, err := filepath.Rel(destination, target)
		if err != nil || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
			return fmt.Errorf("unsafe Visual Hive artifact extraction path")
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		reader, err := entry.Open()
		if err != nil {
			return err
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			reader.Close()
			return err
		}
		_, copyErr := io.Copy(file, io.LimitReader(reader, int64(entry.UncompressedSize64)+1))
		closeFileErr := file.Close()
		closeReaderErr := reader.Close()
		if copyErr != nil || closeFileErr != nil || closeReaderErr != nil {
			return fmt.Errorf("extract Visual Hive artifact entry %q", entry.Name)
		}
	}
	return nil
}

func findVisualHiveManifest(root string) (string, error) {
	var matches []string
	err := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "manifest.json" {
			return nil
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		defer file.Close()
		var identity struct {
			SchemaVersion string `json:"schemaVersion"`
		}
		if err := json.NewDecoder(io.LimitReader(file, 2<<20)).Decode(&identity); err == nil && identity.SchemaVersion == visualhive.ManifestSchema {
			matches = append(matches, filePath)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("expected exactly one Visual Hive bundle manifest, found %d", len(matches))
	}
	return matches[0], nil
}
