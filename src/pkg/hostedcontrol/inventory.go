package hostedcontrol

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var portableRepairBundlePattern = regexp.MustCompile(`^[a-f0-9]{24}-r[0-9]+-a[1-9][0-9]*-(?:validated|committed)-[a-f0-9]{24}\.bundle$`)

var excludedDirectoryNames = map[string]struct{}{
	".git": {}, "checkout": {}, "checkouts": {}, "worktree": {}, "worktrees": {},
	"baseline-worktrees": {}, "artifacts": {}, "baseline-artifacts": {}, "revision-artifacts": {},
	"setup-baseline": {}, "runtime": {}, ".runtime": {}, "node_modules": {},
}

// PortableInventory returns the complete fail-closed inventory of Hive-owned
// durable state. Excluded runtime trees are skipped, but an unknown file or an
// unsafe filesystem object is an error rather than being silently lost.
func PortableInventory(root string) ([]string, error) {
	absolute, rootDevice, err := validateInventoryRoot(root)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(absolute, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("hosted runtime state contains link or reparse point %q", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		device, hasDevice, links, hasLinks, err := sourceFileMetadata(path, info)
		if err != nil {
			return err
		}
		if hasDevice && device != rootDevice {
			return fmt.Errorf("hosted runtime state entry %q crosses a filesystem boundary", relative)
		}
		if !sourcePermissionsSafe(info) {
			return fmt.Errorf("hosted runtime state entry %q has unsafe permissions", relative)
		}
		name := strings.ToLower(entry.Name())
		if entry.IsDir() {
			if _, excluded := excludedDirectoryNames[name]; excluded {
				return filepath.SkipDir
			}
			if !portableDirectory(relative) {
				return fmt.Errorf("hosted runtime state contains unknown directory %q", relative)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("hosted runtime state entry %q is not a regular file", relative)
		}
		if hasLinks && links != 1 {
			return fmt.Errorf("hosted runtime state file %q has hard links", relative)
		}
		if excludedRuntimeFile(name) {
			return nil
		}
		if !portableFile(relative) {
			return fmt.Errorf("hosted runtime state contains unclassified file %q", relative)
		}
		paths = append(paths, relative)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func portableDirectory(relative string) bool {
	parts := strings.Split(relative, "/")
	switch parts[0] {
	case "integrated", "visual-hive":
		return len(parts) == 1
	case "repair":
		return len(parts) == 1 || len(parts) == 2 && parts[1] == "portable-git"
	case "beads":
		return len(parts) <= 2
	default:
		return false
	}
}

func portableFile(relative string) bool {
	parts := strings.Split(relative, "/")
	if len(parts) == 1 {
		return relative == ".hive-state-owner.json"
	}
	switch parts[0] {
	case "integrated":
		if len(parts) != 2 {
			return false
		}
	case "visual-hive":
		if len(parts) != 2 {
			return false
		}
	case "repair":
		if len(parts) == 3 && parts[1] == "portable-git" {
			return portableRepairBundlePattern.MatchString(parts[2])
		}
		if len(parts) != 2 {
			return false
		}
	case "beads":
		// Bead stores use one caller-selected namespace, for example
		// beads/quality/beads.json and beads/quality/beads.archive.jsonl.
		if len(parts) != 3 {
			return false
		}
	default:
		return false
	}
	name := strings.ToLower(parts[len(parts)-1])
	return strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".jsonl") || name == "pause.requested"
}

func excludedRuntimeFile(name string) bool {
	return strings.Contains(name, "daemon") || name == "scheduler-start-intent.json" ||
		strings.HasSuffix(name, ".pid") || strings.HasSuffix(name, ".sock") || strings.HasSuffix(name, ".log") ||
		strings.HasSuffix(name, ".lock") || strings.Contains(name, ".lock.") || strings.HasSuffix(name, ".tmp")
}

func stagePortableFiles(root string, paths []string) (string, error) {
	stage, err := os.MkdirTemp("", "hive-hosted-state-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(stage, 0o700); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	for _, relative := range paths {
		source := filepath.Join(root, filepath.FromSlash(relative))
		destination := filepath.Join(stage, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			_ = os.RemoveAll(stage)
			return "", err
		}
		if err := privateDirectoryChain(stage, filepath.Dir(destination)); err != nil {
			_ = os.RemoveAll(stage)
			return "", err
		}
		content, err := readStableRegularFile(source)
		if err != nil {
			_ = os.RemoveAll(stage)
			return "", err
		}
		if err := os.WriteFile(destination, content, 0o600); err != nil {
			_ = os.RemoveAll(stage)
			return "", err
		}
		if err := os.Chmod(destination, 0o600); err != nil {
			_ = os.RemoveAll(stage)
			return "", err
		}
	}
	return stage, nil
}

func verifyStagedFilesUnchanged(root, stage string, paths []string) error {
	current, err := PortableInventory(root)
	if err != nil {
		return err
	}
	if strings.Join(current, "\x00") != strings.Join(paths, "\x00") {
		return errors.New("hosted runtime durable inventory changed during checkpoint")
	}
	for _, relative := range paths {
		currentContent, err := readStableRegularFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return err
		}
		stagedContent, err := os.ReadFile(filepath.Join(stage, filepath.FromSlash(relative)))
		if err != nil {
			return err
		}
		if !bytes.Equal(currentContent, stagedContent) {
			return fmt.Errorf("hosted runtime state file %q changed during checkpoint", relative)
		}
	}
	return nil
}

func readStableRegularFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("hosted runtime state file %q is linked or non-regular", filepath.Base(path))
	}
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	opened, err := handle.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("hosted runtime state file %q changed before open", filepath.Base(path))
	}
	content, err := io.ReadAll(io.LimitReader(handle, int64(32<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(content) > 32<<20 {
		return nil, fmt.Errorf("hosted runtime state file %q exceeds the portable-state bound", filepath.Base(path))
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("hosted runtime state file %q changed while read", filepath.Base(path))
	}
	return content, nil
}

func validateInventoryRoot(root string) (string, uint64, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", 0, err
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", 0, errors.New("hosted runtime root must be an ordinary directory")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || !samePath(resolved, absolute) {
		return "", 0, errors.New("hosted runtime root resolves through a link or reparse point")
	}
	if !sourcePermissionsSafe(info) {
		return "", 0, errors.New("hosted runtime root has unsafe permissions")
	}
	device, hasDevice, links, hasLinks, err := sourceFileMetadata(absolute, info)
	if err != nil {
		return "", 0, err
	}
	if !hasDevice {
		return "", 0, errors.New("hosted runtime root filesystem identity is unavailable")
	}
	if hasLinks && links < 1 {
		return "", 0, errors.New("hosted runtime root has invalid link metadata")
	}
	return absolute, device, nil
}

func privateDirectoryChain(root, leaf string) error {
	for current := leaf; ; current = filepath.Dir(current) {
		if err := os.Chmod(current, 0o700); err != nil {
			return err
		}
		if samePath(current, root) {
			return nil
		}
		if current == filepath.Dir(current) {
			return errors.New("portable state staging path escaped its root")
		}
	}
}
