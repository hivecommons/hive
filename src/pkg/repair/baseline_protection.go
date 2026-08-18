package repair

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxVisualHiveConfigBytes       = 1 << 20
	maxVisualBaselinePathBytes     = 4096
	maxVisualBaselineListingBytes  = 4 << 20
	maxVisualBaselineProtectedPath = 65536
)

// BaselineProtection is the exact repository-relative visual baseline scope
// that the repair worker must preserve. Enforced is intentionally explicit so
// non-Visual-Hive repair callers keep their existing behavior until they bind
// protection derived from a trusted checkout.
type BaselineProtection struct {
	Enforced bool
	Roots    []string
	Files    []string
}

// InspectVisualBaselineProtection derives the configured Visual Hive snapshot
// root and exact repository baseline files from one checkout. Production
// callers retain this trusted view while candidate worktrees are inspected
// again, preventing a repair from escaping protection by changing snapshotDir.
func InspectVisualBaselineProtection(ctx context.Context, repositoryDir string) (BaselineProtection, error) {
	result := BaselineProtection{Enforced: true}
	if strings.TrimSpace(repositoryDir) == "" {
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: repository directory is required")
	}
	if _, err := captureRepositoryGitControl(repositoryDir, ""); err != nil {
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: unsafe repository Git control state: %w", err)
	}

	configPath := filepath.Join(repositoryDir, "visual-hive.config.yaml")
	info, err := os.Lstat(configPath)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: visual-hive.config.yaml must be a regular file")
		}
		if info.Size() < 0 || info.Size() > maxVisualHiveConfigBytes {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: visual-hive.config.yaml exceeds %d bytes", maxVisualHiveConfigBytes)
		}
		data, readErr := os.ReadFile(configPath)
		if readErr != nil {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: read config: %w", readErr)
		}
		if int64(len(data)) != info.Size() {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: visual-hive.config.yaml changed while it was read")
		}
		normalized, normalizeErr := visualBaselineRootFromConfig(data)
		if normalizeErr != nil {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: %w", normalizeErr)
		}
		result.Roots = append(result.Roots, normalized)
	case os.IsNotExist(err):
		// A repository without a Visual Hive config has no configured root, but
		// exact conventional baseline files are still protected below.
	default:
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: stat config: %w", err)
	}

	listing, err := runGitBytes(ctx, repositoryDir, maxVisualBaselineListingBytes,
		"ls-files", "-z", "--cached", "--others", "--exclude-standard", "--")
	if err != nil {
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: list repository files: %w", err)
	}
	if err := collectVisualBaselineFiles(&result, listing); err != nil {
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection: %w", err)
	}
	return normalizeBaselineProtection(result)
}

// InspectVisualBaselineProtectionAtCommit derives protection from immutable Git
// objects at one exact commit. It never consults the mutable worktree or index.
func InspectVisualBaselineProtectionAtCommit(ctx context.Context, repositoryDir, commitSHA string) (BaselineProtection, error) {
	result := BaselineProtection{Enforced: true}
	if strings.TrimSpace(repositoryDir) == "" || !validGitCommitSHA(commitSHA) {
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection at commit: repository directory and exact commit are required")
	}
	if _, err := captureRepositoryGitControl(repositoryDir, ""); err != nil {
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection at commit: unsafe repository Git control state: %w", err)
	}
	commitSHA = strings.ToLower(commitSHA)
	treeEntry, err := runGitBytes(ctx, repositoryDir, maxVisualBaselinePathBytes+256,
		"ls-tree", "-z", commitSHA, "--", "visual-hive.config.yaml")
	if err != nil {
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection at commit: read config identity: %w", err)
	}
	if len(treeEntry) > 0 {
		objectID, parseErr := exactVisualConfigObject(treeEntry)
		if parseErr != nil {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection at commit: %w", parseErr)
		}
		data, readErr := runGitBytes(ctx, repositoryDir, maxVisualHiveConfigBytes, "cat-file", "blob", objectID)
		if readErr != nil {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection at commit: read config: %w", readErr)
		}
		root, normalizeErr := visualBaselineRootFromConfig(data)
		if normalizeErr != nil {
			return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection at commit: %w", normalizeErr)
		}
		result.Roots = append(result.Roots, root)
	}

	listing, err := runGitBytes(ctx, repositoryDir, maxVisualBaselineListingBytes,
		"ls-tree", "-r", "-z", "--name-only", commitSHA, "--")
	if err != nil {
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection at commit: list repository files: %w", err)
	}
	if err := collectVisualBaselineFiles(&result, listing); err != nil {
		return BaselineProtection{}, fmt.Errorf("inspect visual baseline protection at commit: %w", err)
	}
	return normalizeBaselineProtection(result)
}

func exactVisualConfigObject(entry []byte) (string, error) {
	parts := bytes.Split(entry, []byte{0})
	if len(parts) != 2 || len(parts[1]) != 0 {
		return "", fmt.Errorf("visual-hive.config.yaml has an ambiguous tree identity")
	}
	metadata, name, found := bytes.Cut(parts[0], []byte{'\t'})
	fields := strings.Fields(string(metadata))
	if !found || string(name) != "visual-hive.config.yaml" || len(fields) != 3 ||
		(fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" || !validGitCommitSHA(fields[2]) {
		return "", fmt.Errorf("visual-hive.config.yaml must be one regular Git blob")
	}
	return strings.ToLower(fields[2]), nil
}

func visualBaselineRootFromConfig(data []byte) (string, error) {
	if len(data) > maxVisualHiveConfigBytes {
		return "", fmt.Errorf("visual-hive.config.yaml exceeds %d bytes", maxVisualHiveConfigBytes)
	}
	var config struct {
		Visual struct {
			SnapshotDir string `yaml:"snapshotDir"`
		} `yaml:"visual"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&config); err != nil {
		return "", fmt.Errorf("parse config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple YAML documents are not supported")
		}
		return "", fmt.Errorf("parse config: %w", err)
	}
	snapshotDir := config.Visual.SnapshotDir
	if snapshotDir == "" {
		snapshotDir = ".visual-hive/snapshots"
	} else if strings.TrimSpace(snapshotDir) != snapshotDir {
		return "", fmt.Errorf("visual.snapshotDir contains surrounding whitespace")
	}
	normalized, err := normalizeVisualBaselinePath(snapshotDir)
	if err != nil {
		return "", fmt.Errorf("invalid visual.snapshotDir: %w", err)
	}
	return normalized, nil
}

func collectVisualBaselineFiles(result *BaselineProtection, listing []byte) error {
	for _, raw := range bytes.Split(listing, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		file, err := normalizeVisualBaselinePath(string(raw))
		if err != nil {
			return fmt.Errorf("invalid repository path: %w", err)
		}
		if visualBaselineConventionFile(file) || visualBaselinePathRestricted(file, *result) {
			result.Files = append(result.Files, file)
		}
		if len(result.Files) > maxVisualBaselineProtectedPath {
			return fmt.Errorf("more than %d protected files", maxVisualBaselineProtectedPath)
		}
	}
	return nil
}

func normalizeBaselineProtection(protection BaselineProtection) (BaselineProtection, error) {
	if len(protection.Roots) > maxVisualBaselineProtectedPath || len(protection.Files) > maxVisualBaselineProtectedPath {
		return BaselineProtection{}, fmt.Errorf("visual baseline protection exceeds its path-count bound")
	}
	result := BaselineProtection{Enforced: protection.Enforced}
	for _, root := range protection.Roots {
		normalized, err := normalizeVisualBaselinePath(root)
		if err != nil {
			return BaselineProtection{}, fmt.Errorf("invalid protected visual baseline root: %w", err)
		}
		result.Roots = append(result.Roots, normalized)
	}
	for _, file := range protection.Files {
		normalized, err := normalizeVisualBaselinePath(file)
		if err != nil {
			return BaselineProtection{}, fmt.Errorf("invalid protected visual baseline file: %w", err)
		}
		result.Files = append(result.Files, normalized)
	}
	result.Roots = sortedFoldedUnique(result.Roots)
	result.Files = sortedFoldedUnique(result.Files)
	return result, nil
}

func mergeBaselineProtection(values ...BaselineProtection) (BaselineProtection, error) {
	result := BaselineProtection{}
	for _, value := range values {
		normalized, err := normalizeBaselineProtection(value)
		if err != nil {
			return BaselineProtection{}, err
		}
		result.Enforced = result.Enforced || normalized.Enforced
		result.Roots = append(result.Roots, normalized.Roots...)
		result.Files = append(result.Files, normalized.Files...)
	}
	return normalizeBaselineProtection(result)
}

func baselineProtectionForWorktree(ctx context.Context, worktree string, trusted BaselineProtection) (BaselineProtection, error) {
	normalized, err := normalizeBaselineProtection(trusted)
	if err != nil {
		return BaselineProtection{}, err
	}
	if !normalized.Enforced {
		return normalized, nil
	}
	candidate, err := InspectVisualBaselineProtection(ctx, worktree)
	if err != nil {
		return BaselineProtection{}, fmt.Errorf("inspect candidate visual baseline protection: %w", err)
	}
	return mergeBaselineProtection(normalized, candidate)
}

func baselineProtectionForCommit(ctx context.Context, repositoryDir, commitSHA string, trusted BaselineProtection) (BaselineProtection, error) {
	normalized, err := normalizeBaselineProtection(trusted)
	if err != nil {
		return BaselineProtection{}, err
	}
	if !normalized.Enforced {
		return normalized, nil
	}
	candidate, err := InspectVisualBaselineProtectionAtCommit(ctx, repositoryDir, commitSHA)
	if err != nil {
		return BaselineProtection{}, fmt.Errorf("inspect exact candidate visual baseline protection: %w", err)
	}
	return mergeBaselineProtection(normalized, candidate)
}

func visualBaselinePathRestricted(file string, protection BaselineProtection) bool {
	if !protection.Enforced {
		return false
	}
	normalized, err := normalizeVisualBaselinePath(file)
	if err != nil {
		return true
	}
	for _, protectedFile := range protection.Files {
		if strings.EqualFold(normalized, protectedFile) {
			return true
		}
	}
	for _, root := range protection.Roots {
		if strings.EqualFold(normalized, root) || len(normalized) > len(root) && strings.EqualFold(normalized[:len(root)], root) && normalized[len(root)] == '/' {
			return true
		}
	}
	return false
}

func normalizeVisualBaselinePath(value string) (string, error) {
	if value == "" || len(value) > maxVisualBaselinePathBytes || strings.ContainsRune(value, 0) || strings.Contains(value, "\\") {
		return "", fmt.Errorf("path must be a bounded non-empty POSIX-style repository path")
	}
	looksLikeDrivePath := len(value) >= 2 && value[1] == ':' && (value[0] >= 'A' && value[0] <= 'Z' || value[0] >= 'a' && value[0] <= 'z')
	if filepath.IsAbs(value) || filepath.VolumeName(value) != "" || looksLikeDrivePath || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("path %q must be repository-relative", value)
	}
	normalized := path.Clean(strings.TrimPrefix(value, "./"))
	if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") || strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("path %q escapes the repository", value)
	}
	return normalized, nil
}

func visualBaselineConventionFile(file string) bool {
	lower := strings.ToLower(file)
	if !strings.HasSuffix(lower, ".png") && !strings.HasSuffix(lower, ".json") {
		return false
	}
	for _, suffix := range []string{"-actual.png", "-diff.png", ".actual.png", ".diff.png", "-actual.json", "-diff.json", ".actual.json", ".diff.json"} {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	return strings.Contains(lower, "baseline") || strings.Contains("/"+lower, "/__screenshots__/") ||
		strings.Contains(lower, "-snapshots/") || strings.HasPrefix(lower, ".visual-hive/snapshots/")
}

func sortedFoldedUnique(values []string) []string {
	sort.Slice(values, func(i, j int) bool {
		return strings.ToLower(values[i]) < strings.ToLower(values[j])
	})
	result := make([]string, 0, len(values))
	for _, value := range values {
		if len(result) == 0 || !strings.EqualFold(result[len(result)-1], value) {
			result = append(result, value)
		}
	}
	return result
}
