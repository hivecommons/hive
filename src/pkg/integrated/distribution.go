package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const DistributionSchema = "hive.integrated-distribution.v1"

const linuxVisualHiveLauncher = `#!/bin/sh
launcher="$0"
while [ -L "$launcher" ]; do
  target="$(readlink -- "$launcher")" || {
    echo "Hive Visual Hive launcher error: could not resolve the installed launcher symlink: $launcher. Reinstall the immutable integrated Hive release." >&2
    exit 126
  }
  case "$target" in
    /*) launcher="$target" ;;
    *) launcher="$(dirname -- "$launcher")/$target" ;;
  esac
done
root_dir="$(CDPATH= cd -- "$(dirname -- "$launcher")/.." && pwd -P)" || {
  echo "Hive Visual Hive launcher error: could not resolve the integrated installation root. Reinstall the immutable integrated Hive release." >&2
  exit 126
}
node="$root_dir/runtime/node"
entrypoint="$root_dir/visual-hive/visual-hive.mjs"
if [ ! -x "$node" ]; then
  echo "Hive Visual Hive launcher error: bundled Node runtime is missing or not executable: $node. Reinstall the immutable integrated Hive release." >&2
  exit 127
fi
if [ ! -f "$entrypoint" ]; then
  echo "Hive Visual Hive launcher error: Visual Hive entrypoint is missing: $entrypoint. Reinstall the immutable integrated Hive release." >&2
  exit 127
fi
exec "$node" "$entrypoint" "$@"
`

const windowsVisualHiveLauncher = `@echo off
setlocal
set "HIVE_NODE=%~dp0runtime\node.exe"
set "HIVE_VISUAL_ENTRYPOINT=%~dp0visual-hive\visual-hive.mjs"
if not exist "%HIVE_NODE%" (
  >&2 echo Hive Visual Hive launcher error: bundled Node runtime is missing: "%HIVE_NODE%". Reinstall the immutable integrated Hive release.
  exit /b 127
)
if not exist "%HIVE_VISUAL_ENTRYPOINT%" (
  >&2 echo Hive Visual Hive launcher error: Visual Hive entrypoint is missing: "%HIVE_VISUAL_ENTRYPOINT%". Reinstall the immutable integrated Hive release.
  exit /b 127
)
"%HIVE_NODE%" "%HIVE_VISUAL_ENTRYPOINT%" %*
set "HIVE_VISUAL_EXIT=%ERRORLEVEL%"
endlocal & exit /b %HIVE_VISUAL_EXIT%
`

var immutableCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

type DistributionOptions struct {
	HiveBinary     string
	HiveCommit     string
	HiveVersion    string
	VisualHiveDir  string
	VisualCommit   string
	NodeBinary     string
	NodeRuntimeDir string
	NodeLicense    string
	NodeVersion    string
	SkillDir       string
	TargetOS       string
	TargetArch     string
	OutputDir      string
}

type DistributionFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type DistributionManifest struct {
	SchemaVersion            string             `json:"schema_version"`
	HiveVersion              string             `json:"hive_version"`
	HiveCommit               string             `json:"hive_commit"`
	VisualHiveCommit         string             `json:"visual_hive_commit"`
	VisualHiveVersion        string             `json:"visual_hive_version"`
	HostedControllerProtocol int                `json:"hosted_controller_protocol"`
	NodeVersion              string             `json:"node_version"`
	OS                       string             `json:"os"`
	Architecture             string             `json:"architecture"`
	Files                    []DistributionFile `json:"files"`
}

// BuildDistribution creates an atomic, self-contained Hive + Visual Hive tree.
// A consumer needs neither Go nor Node and does not need a source checkout.
func BuildDistribution(ctx context.Context, options DistributionOptions) (DistributionManifest, error) {
	if !immutableCommit.MatchString(options.HiveCommit) || !immutableCommit.MatchString(options.VisualCommit) {
		return DistributionManifest{}, fmt.Errorf("Hive and Visual Hive commits must be immutable 40-character SHA-1 values")
	}
	hiveVersion := strings.TrimSpace(options.HiveVersion)
	if matched, _ := regexp.MatchString(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`, hiveVersion); !matched {
		return DistributionManifest{}, fmt.Errorf("safe Hive integrated release version is required")
	}
	for label, source := range map[string]string{"Hive binary": options.HiveBinary, "Visual Hive bundle": options.VisualHiveDir, "Node binary": options.NodeBinary, "complete Node runtime": options.NodeRuntimeDir, "Hive Codex skill": options.SkillDir} {
		if strings.TrimSpace(source) == "" {
			return DistributionManifest{}, fmt.Errorf("%s is required", label)
		}
	}
	output, err := filepath.Abs(options.OutputDir)
	if err != nil || output == filepath.VolumeName(output)+string(filepath.Separator) {
		return DistributionManifest{}, fmt.Errorf("safe distribution output directory is required")
	}
	if _, err := os.Stat(output); err == nil {
		return DistributionManifest{}, fmt.Errorf("distribution output already exists: %s", output)
	} else if !os.IsNotExist(err) {
		return DistributionManifest{}, err
	}
	visualEntrypoint := filepath.Join(options.VisualHiveDir, "visual-hive.mjs")
	visualManifest, err := ValidateVisualHiveRelease(visualEntrypoint, options.VisualCommit)
	if err != nil {
		return DistributionManifest{}, err
	}
	nodeVersion := strings.TrimSpace(options.NodeVersion)
	if nodeVersion == "" {
		nodeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		output, runErr := exec.CommandContext(nodeCtx, options.NodeBinary, "--version").CombinedOutput()
		if runErr != nil {
			return DistributionManifest{}, fmt.Errorf("verify bundled Node runtime: %w", runErr)
		}
		nodeVersion = strings.TrimSpace(string(output))
	}
	if !strings.HasPrefix(nodeVersion, "v22.") {
		return DistributionManifest{}, fmt.Errorf("bundled Node runtime must be Node 22, got %q", nodeVersion)
	}

	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return DistributionManifest{}, err
	}
	staging, err := os.MkdirTemp(parent, ".hive-distribution-")
	if err != nil {
		return DistributionManifest{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()
	targetOS, targetArch := options.TargetOS, options.TargetArch
	if targetOS == "" {
		targetOS = runtime.GOOS
	}
	if targetArch == "" {
		targetArch = runtime.GOARCH
	}
	if (targetOS != "linux" && targetOS != "windows") || targetArch != "amd64" {
		return DistributionManifest{}, fmt.Errorf("unsupported integrated distribution target %s/%s", targetOS, targetArch)
	}
	hiveName, nodeName := "hive", "node"
	if targetOS == "windows" {
		hiveName, nodeName = "hive.exe", "node.exe"
	}
	if err := copyRegularFile(options.HiveBinary, filepath.Join(staging, hiveName), 0o755); err != nil {
		return DistributionManifest{}, err
	}
	if err := copyRegularTree(options.NodeRuntimeDir, filepath.Join(staging, "runtime")); err != nil {
		return DistributionManifest{}, err
	}
	packagedNode := filepath.Join(staging, "runtime", nodeName)
	launchers := []string{nodeName}
	if targetOS == "linux" {
		launchers = append(launchers, "npm", "npx", "corepack", "pnpm", "pnpx", "yarn", "yarnpkg")
	}
	for _, launcher := range launchers {
		if err := os.Chmod(filepath.Join(staging, "runtime", launcher), 0o755); err != nil {
			return DistributionManifest{}, fmt.Errorf("mark bundled runtime launcher %s executable: %w", launcher, err)
		}
	}
	if same, compareErr := regularFilesEqual(options.NodeBinary, packagedNode); compareErr != nil || !same {
		if compareErr != nil {
			return DistributionManifest{}, compareErr
		}
		return DistributionManifest{}, fmt.Errorf("complete Node runtime does not contain the exact verified %s binary", nodeName)
	}
	if options.NodeLicense != "" && !exists(filepath.Join(staging, "runtime", "LICENSE.node.txt")) {
		if err := copyRegularFile(options.NodeLicense, filepath.Join(staging, "runtime", "LICENSE.node.txt"), 0o644); err != nil {
			return DistributionManifest{}, err
		}
	}
	for _, required := range requiredNodeRuntimeFiles(targetOS) {
		if info, err := os.Lstat(filepath.Join(staging, "runtime", filepath.FromSlash(required))); err != nil || !info.Mode().IsRegular() {
			return DistributionManifest{}, fmt.Errorf("complete Node runtime is missing regular file %s", required)
		}
	}
	if err := copyRegularTree(options.VisualHiveDir, filepath.Join(staging, "visual-hive")); err != nil {
		return DistributionManifest{}, err
	}
	if err := writeVisualHiveLauncher(staging, targetOS); err != nil {
		return DistributionManifest{}, err
	}
	if err := copyRegularTree(options.SkillDir, filepath.Join(staging, "skills", "hive")); err != nil {
		return DistributionManifest{}, err
	}
	for _, relative := range []string{"skills/hive/SKILL.md", "skills/hive/agents/openai.yaml"} {
		if info, err := os.Lstat(filepath.Join(staging, filepath.FromSlash(relative))); err != nil || !info.Mode().IsRegular() {
			return DistributionManifest{}, fmt.Errorf("Hive Codex skill is missing required regular file %s", relative)
		}
	}
	files, err := inventoryFiles(staging)
	if err != nil {
		return DistributionManifest{}, err
	}
	manifest := DistributionManifest{
		SchemaVersion: DistributionSchema, HiveVersion: hiveVersion, HiveCommit: options.HiveCommit, VisualHiveCommit: options.VisualCommit,
		VisualHiveVersion: visualManifest.Version, HostedControllerProtocol: HostedControllerProtocol, NodeVersion: nodeVersion, OS: targetOS, Architecture: targetArch, Files: files,
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return DistributionManifest{}, err
	}
	if err := os.WriteFile(filepath.Join(staging, "distribution-manifest.json"), append(data, '\n'), 0o644); err != nil {
		return DistributionManifest{}, err
	}
	if err := renameDistribution(staging, output); err != nil {
		return DistributionManifest{}, fmt.Errorf("publish distribution atomically: %w", err)
	}
	committed = true
	return manifest, nil
}

func visualHiveLauncherPath(targetOS string) string {
	if targetOS == "windows" {
		return "visual-hive.cmd"
	}
	return "bin/visual-hive"
}

func writeVisualHiveLauncher(root, targetOS string) error {
	relative := visualHiveLauncherPath(targetOS)
	contents := linuxVisualHiveLauncher
	mode := os.FileMode(0o755)
	if targetOS == "windows" {
		contents = strings.ReplaceAll(windowsVisualHiveLauncher, "\n", "\r\n")
		mode = 0o644
	}
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create Visual Hive launcher directory: %w", err)
	}
	if err := os.WriteFile(target, []byte(contents), mode); err != nil {
		return fmt.Errorf("write integrated Visual Hive launcher: %w", err)
	}
	return nil
}

func requiredNodeRuntimeFiles(targetOS string) []string {
	if targetOS == "windows" {
		return []string{"node.exe", "npm.cmd", "npx.cmd", "corepack.cmd", "pnpm.cmd", "pnpx.cmd", "yarn.cmd", "yarnpkg.cmd", "node_modules/npm/bin/npm-cli.js", "node_modules/corepack/dist/corepack.js", "node_modules/corepack/dist/pnpm.js", "node_modules/corepack/dist/yarn.js"}
	}
	return []string{"node", "npm", "npx", "corepack", "pnpm", "pnpx", "yarn", "yarnpkg", "node_modules/npm/bin/npm-cli.js", "node_modules/corepack/dist/corepack.js", "node_modules/corepack/dist/pnpm.js", "node_modules/corepack/dist/yarn.js"}
}

func regularFilesEqual(left, right string) (bool, error) {
	leftData, err := os.ReadFile(left)
	if err != nil {
		return false, err
	}
	rightData, err := os.ReadFile(right)
	if err != nil {
		return false, err
	}
	return sha256.Sum256(leftData) == sha256.Sum256(rightData), nil
}

func renameDistribution(staging, output string) error {
	var last error
	for attempt := 0; attempt < 20; attempt++ {
		if err := os.Rename(staging, output); err == nil {
			return nil
		} else {
			last = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return last
}

func copyRegularTree(source, destination string) error {
	return filepath.WalkDir(source, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("distribution inputs cannot contain symlinks: %s", relative)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("distribution inputs must be regular files: %s", relative)
		}
		return copyRegularFile(current, target, 0o644)
	})
}

func copyRegularFile(source, destination string, mode os.FileMode) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("distribution input is not a regular file: %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	reader, err := os.Open(source)
	if err != nil {
		return err
	}
	defer reader.Close()
	writer, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(writer, reader); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

func inventoryFiles(root string) ([]DistributionFile, error) {
	files := []DistributionFile{}
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("distribution output contains a non-regular file: %s", current)
		}
		data, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		files = append(files, DistributionFile{Path: filepath.ToSlash(relative), SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data))})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}
