package integrated

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
)

const (
	VisualHiveReleaseSchema = "visual-hive.release.v1"
	maxReleaseFiles         = 10000
	maxReleaseFileSize      = 128 << 20
	maxReleaseSize          = 512 << 20
)

type VisualHiveReleaseManifest struct {
	SchemaVersion     string                  `json:"schemaVersion"`
	Name              string                  `json:"name"`
	Version           string                  `json:"version"`
	GitCommit         string                  `json:"gitCommit"`
	Node              string                  `json:"node"`
	Entrypoint        string                  `json:"entrypoint"`
	PlaywrightVersion string                  `json:"playwrightVersion"`
	Files             []VisualHiveReleaseFile `json:"files"`
}

type VisualHiveReleaseFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ValidateVisualHiveRelease verifies the immutable producer identity and every
// inventoried byte before Hive executes a packaged Visual Hive CLI.
func ValidateVisualHiveRelease(entrypoint, expectedCommit string) (VisualHiveReleaseManifest, error) {
	root := filepath.Dir(entrypoint)
	manifestPath := filepath.Join(root, "release-manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return VisualHiveReleaseManifest{}, fmt.Errorf("Visual Hive release manifest is missing: %w", err)
	}
	var manifest VisualHiveReleaseManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return VisualHiveReleaseManifest{}, fmt.Errorf("Visual Hive release manifest is invalid: %w", err)
	}
	if manifest.SchemaVersion != VisualHiveReleaseSchema || manifest.Name != "visual-hive" || manifest.Node != ">=22" || manifest.Entrypoint != filepath.Base(entrypoint) || manifest.Version == "" || manifest.PlaywrightVersion == "" || len(manifest.Files) == 0 || len(manifest.Files) > maxReleaseFiles {
		return VisualHiveReleaseManifest{}, fmt.Errorf("Visual Hive release manifest is invalid")
	}
	if !immutableCommit.MatchString(strings.ToLower(manifest.GitCommit)) {
		return VisualHiveReleaseManifest{}, fmt.Errorf("Visual Hive release manifest is not bound to an immutable commit")
	}
	if expectedCommit != "" && (len(expectedCommit) != 40 || !strings.EqualFold(manifest.GitCommit, expectedCommit)) {
		return VisualHiveReleaseManifest{}, fmt.Errorf("Visual Hive release commit does not match the configured immutable pin")
	}
	seen := map[string]bool{}
	var total int64
	for _, file := range manifest.Files {
		clean := pathpkg.Clean(file.Path)
		if file.Path == "" || clean != file.Path || clean == "." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(file.Path, "\\") || seen[file.Path] || len(file.SHA256) != 64 || file.Size < 0 || file.Size > maxReleaseFileSize {
			return VisualHiveReleaseManifest{}, fmt.Errorf("Visual Hive release inventory is unsafe")
		}
		seen[file.Path] = true
		total += file.Size
		if total > maxReleaseSize {
			return VisualHiveReleaseManifest{}, fmt.Errorf("Visual Hive release inventory exceeds the size limit")
		}
		target := filepath.Join(root, filepath.FromSlash(file.Path))
		info, statErr := os.Lstat(target)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
			return VisualHiveReleaseManifest{}, fmt.Errorf("Visual Hive release inventory does not match installed files")
		}
		contents, readErr := os.ReadFile(target)
		if readErr != nil || fmt.Sprintf("%x", sha256.Sum256(contents)) != file.SHA256 {
			return VisualHiveReleaseManifest{}, fmt.Errorf("Visual Hive release digest verification failed")
		}
	}
	if !seen[manifest.Entrypoint] {
		return VisualHiveReleaseManifest{}, fmt.Errorf("Visual Hive release entrypoint is not inventoried")
	}
	return manifest, nil
}
