package integrated

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const managedCheckoutOwnerFile = "hive-checkout-owner.json"

type managedCheckoutOwner struct {
	SchemaVersion string `json:"schema_version"`
	Repository    string `json:"repository"`
}

func validateManagedCheckoutBeforeGit(checkout, repository string) (bool, error) {
	return validateManagedCheckoutBeforeGitWithLegacy(checkout, repository, nil)
}

func validateManagedCheckoutBeforeGitWithLegacy(checkout, repository string, legacy *legacyManagedCheckoutProof) (bool, error) {
	absolute, err := filepath.Abs(checkout)
	if err != nil {
		return false, err
	}
	parent := filepath.Dir(absolute)
	boundedLayout := filepath.Base(absolute) == "checkout" && filepath.Base(parent) == "integrated"
	legacyLayout := filepath.Base(parent) == "checkouts" && filepath.Base(filepath.Dir(parent)) == "integrated"
	if !boundedLayout && !legacyLayout {
		return false, fmt.Errorf("managed checkout must be the exact Hive state integrated/checkout leaf or a verified legacy integrated/checkouts leaf")
	}
	if err := ensureOrdinaryDirectoryChain(parent, 0o700); err != nil {
		return false, fmt.Errorf("managed checkout ancestor is unsafe: %w", err)
	}
	info, err := os.Lstat(absolute)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("managed checkout is not an ordinary directory")
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil || filepath.Clean(real) != filepath.Clean(absolute) {
		return false, fmt.Errorf("managed checkout resolves through a link or reparse point")
	}
	gitDir := filepath.Join(absolute, ".git")
	gitInfo, err := os.Lstat(gitDir)
	if err != nil || !gitInfo.IsDir() || gitInfo.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("managed checkout .git is not an ordinary directory")
	}
	realGit, err := filepath.EvalSymlinks(gitDir)
	if err != nil || filepath.Clean(realGit) != filepath.Clean(gitDir) {
		return false, fmt.Errorf("managed checkout .git resolves through a link or reparse point")
	}
	markerPath := filepath.Join(gitDir, managedCheckoutOwnerFile)
	markerInfo, markerErr := os.Lstat(markerPath)
	if os.IsNotExist(markerErr) {
		if legacy == nil {
			// A markerless directory is not adopted from its path or a Git config
			// substring. Only RunSetup can supply the exact durable legacy config
			// plus the immutable identity it has just read from GitHub.
			return true, fmt.Errorf("existing managed checkout has no exact Hive ownership marker; rerun setup with its original --state-dir so Hive can verify and migrate it, or rerun the same setup command with a new dedicated --state-dir to create a fresh clone")
		}
		if err := migrateLegacyManagedCheckout(absolute, repository, *legacy); err != nil {
			return true, fmt.Errorf("legacy managed checkout migration refused: %w; supported recovery: rerun the same hive setup command with --state-dir %q to create a fresh clone (the original state is unchanged)", err, legacyRecoveryStateDir(legacy.Config.StateDir))
		}
		markerInfo, markerErr = os.Lstat(markerPath)
	}
	if markerErr != nil || !markerInfo.Mode().IsRegular() || markerInfo.Mode()&os.ModeSymlink != 0 {
		return true, fmt.Errorf("existing managed checkout ownership marker is unsafe")
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return true, err
	}
	var owner managedCheckoutOwner
	if json.Unmarshal(data, &owner) != nil || owner.SchemaVersion != "hive.checkout-owner.v1" || !strings.EqualFold(owner.Repository, strings.TrimSpace(repository)) {
		return true, fmt.Errorf("managed checkout ownership marker does not match %s", repository)
	}
	return true, nil
}

func writeManagedCheckoutOwner(checkout, repository string) error {
	marker := managedCheckoutOwner{SchemaVersion: "hive.checkout-owner.v1", Repository: strings.TrimSpace(repository)}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(filepath.Join(checkout, ".git", managedCheckoutOwnerFile), append(data, '\n'))
}

func validateSetupStateRoot(stateDir, repository string) error {
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	absolute = filepath.Clean(absolute)
	home, _ := os.UserHomeDir()
	volume := filepath.Clean(filepath.VolumeName(absolute) + string(os.PathSeparator))
	leaf := strings.ToLower(filepath.Base(absolute))
	unsafeLeaf := map[string]bool{"documents": true, "desktop": true, "downloads": true, "project": true, "projects": true, "workspace": true, "workspaces": true, "src": true, "source": true, "data": true, "repo": true, "repository": true}
	if absolute == volume || absolute == filepath.Clean(home) || unsafeLeaf[leaf] {
		return fmt.Errorf("state directory %s must be a dedicated Hive-owned leaf, not a root, home, common parent, or generic project/data directory", absolute)
	}
	info, statErr := os.Lstat(absolute)
	if os.IsNotExist(statErr) {
		if err := ensureOrdinaryDirectoryChain(absolute, 0o700); err != nil {
			return fmt.Errorf("create exact ordinary state directory: %w", err)
		}
		return ensureOrdinaryDirectoryChain(filepath.Join(absolute, "integrated"), 0o700)
	}
	if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state directory must be an ordinary directory")
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil || filepath.Clean(real) != absolute {
		return fmt.Errorf("state directory resolves through a link or reparse point")
	}
	ownerPath := filepath.Join(absolute, stateOwnershipMarkerFile)
	ownerInfo, ownerStatErr := os.Lstat(ownerPath)
	if ownerStatErr == nil {
		if !ownerInfo.Mode().IsRegular() || ownerInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("state directory ownership marker is not an ordinary file")
		}
		data, readErr := os.ReadFile(ownerPath)
		if readErr != nil {
			return readErr
		}
		var owner stateOwnershipMarker
		if json.Unmarshal(data, &owner) != nil || owner.SchemaVersion != "hive.state-owner.v1" || !strings.EqualFold(owner.Repository, strings.TrimSpace(repository)) || strings.TrimSpace(owner.RepositoryID) == "" {
			return fmt.Errorf("state directory ownership marker does not match %s", repository)
		}
		return ensureOrdinaryDirectoryChain(filepath.Join(absolute, "integrated"), 0o700)
	} else if !os.IsNotExist(ownerStatErr) {
		return ownerStatErr
	}
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return ensureOrdinaryDirectoryChain(filepath.Join(absolute, "integrated"), 0o700)
	}
	// Migrate a legacy Hive root only when its durable config positively binds
	// this exact repository. Arbitrary nonempty directories are never adopted.
	integratedDir := filepath.Join(absolute, "integrated")
	integratedInfo, integratedStatErr := os.Lstat(integratedDir)
	if integratedStatErr != nil || !integratedInfo.IsDir() || integratedInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("nonempty state directory has no exact Hive ownership marker or ordinary legacy integrated directory")
	}
	if realIntegrated, evalErr := filepath.EvalSymlinks(integratedDir); evalErr != nil || filepath.Clean(realIntegrated) != filepath.Clean(integratedDir) {
		return fmt.Errorf("legacy integrated state path resolves through a link or reparse point")
	}
	configPath := filepath.Join(integratedDir, "config.json")
	configInfo, configStatErr := os.Lstat(configPath)
	if configStatErr != nil || !configInfo.Mode().IsRegular() || configInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("nonempty state directory has no exact Hive ownership marker or ordinary legacy config")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("nonempty state directory has no exact Hive ownership marker or legacy config")
	}
	var config Config
	if json.Unmarshal(data, &config) != nil || !supportedDurableConfigSchema(config.SchemaVersion) || !strings.EqualFold(config.Repository, strings.TrimSpace(repository)) || strings.TrimSpace(config.RepositoryID) == "" {
		return fmt.Errorf("legacy state directory does not bind exact repository %s", repository)
	}
	return nil
}

// ensureOrdinaryDirectoryChain creates missing directories one component at a
// time and revalidates every component with Lstat and EvalSymlinks. It therefore
// rejects Unix symlinks and Windows junction/reparse indirection before a caller
// can use MkdirAll, Git, or a state write through that path.
func ensureOrdinaryDirectoryChain(target string, mode fs.FileMode) error {
	absolute, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	absolute = filepath.Clean(absolute)
	volume := filepath.VolumeName(absolute)
	root := volume + string(os.PathSeparator)
	relative := strings.TrimPrefix(absolute, root)
	cursor := root
	for _, component := range strings.Split(relative, string(os.PathSeparator)) {
		if component == "" || component == "." {
			continue
		}
		cursor = filepath.Join(cursor, component)
		info, statErr := os.Lstat(cursor)
		if os.IsNotExist(statErr) {
			if mkdirErr := os.Mkdir(cursor, mode); mkdirErr != nil {
				return mkdirErr
			}
			info, statErr = os.Lstat(cursor)
		}
		if statErr != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || deletionPathIsReparsePoint(cursor) {
			return fmt.Errorf("directory component %s is linked, reparsed, missing, or non-directory", cursor)
		}
		real, evalErr := filepath.EvalSymlinks(cursor)
		if evalErr != nil {
			return fmt.Errorf("directory component %s resolves through a link or reparse point", cursor)
		}
		realInfo, realStatErr := os.Stat(real)
		if realStatErr != nil || !realInfo.IsDir() || !os.SameFile(info, realInfo) {
			return fmt.Errorf("directory component %s resolves through a link or reparse point", cursor)
		}
	}
	return nil
}

// validateOrdinarySetupCheckout rejects repository-controlled filesystem
// indirection before setup inspects or writes the state-owned clone. Git
// symlinks, submodules, junctions/reparse links, sockets, and device-like
// entries are outside the static setup trust model. The hosted target runner
// remains the place where repository code and richer filesystem layouts run.
func validateOrdinarySetupCheckout(root string) error {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	rootInfo, err := os.Lstat(absolute)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("setup checkout must be an ordinary state-owned directory")
	}
	realRoot, err := filepath.EvalSymlinks(absolute)
	if err != nil || filepath.Clean(realRoot) != filepath.Clean(absolute) {
		return fmt.Errorf("setup checkout resolves through a link or reparse point")
	}
	err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == absolute {
			return nil
		}
		relative, relErr := filepath.Rel(absolute, path)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("setup checkout entry escapes its root")
		}
		if filepath.ToSlash(relative) == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		info, infoErr := os.Lstat(path)
		if infoErr != nil {
			return infoErr
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("setup checkout entry %s is a link, reparse point, or non-regular file", filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		return err
	}
	command := exec.Command("git", "-C", absolute, "ls-files", "-s", "-z")
	command.Env = safeEnvironment()
	output, err := command.Output()
	if err != nil {
		if _, statErr := os.Lstat(filepath.Join(absolute, ".git")); os.IsNotExist(statErr) {
			return nil
		}
		return fmt.Errorf("inspect tracked setup file modes: %w", err)
	}
	for _, record := range strings.Split(string(output), "\x00") {
		if strings.TrimSpace(record) == "" {
			continue
		}
		metadata, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) < 3 || (fields[0] != "100644" && fields[0] != "100755") {
			return fmt.Errorf("tracked setup entry %s is not a regular Git blob", filepath.ToSlash(path))
		}
	}
	return nil
}

func setupCheckoutPath(root, relative string, createParents bool) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	relative = filepath.Clean(filepath.FromSlash(relative))
	if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("setup path is not a normalized repository-relative path")
	}
	destination := filepath.Join(root, relative)
	if rel, err := filepath.Rel(root, destination); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("setup path escapes its checkout")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("setup checkout root is not an ordinary directory")
	}
	cursor := root
	parts := strings.Split(filepath.Dir(relative), string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		cursor = filepath.Join(cursor, part)
		info, statErr := os.Lstat(cursor)
		if os.IsNotExist(statErr) && createParents {
			if mkdirErr := os.Mkdir(cursor, 0o755); mkdirErr != nil {
				return "", mkdirErr
			}
			info, statErr = os.Lstat(cursor)
		}
		if statErr != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("setup path parent %s is not an ordinary directory", filepath.ToSlash(cursor))
		}
		real, evalErr := filepath.EvalSymlinks(cursor)
		if evalErr != nil || filepath.Clean(real) != filepath.Clean(cursor) {
			return "", fmt.Errorf("setup path parent resolves through a link or reparse point")
		}
	}
	if info, statErr := os.Lstat(destination); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("setup path leaf %s is not a regular file", filepath.ToSlash(relative))
		}
	} else if !os.IsNotExist(statErr) {
		return "", statErr
	}
	return destination, nil
}

func readSetupCheckoutFile(root, relative string) ([]byte, error) {
	path, err := setupCheckoutPath(root, relative, false)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func writeSetupCheckoutFile(root, relative string, data []byte) error {
	path, err := setupCheckoutPath(root, relative, true)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func removeSetupCheckoutFile(root, relative string) error {
	candidate := filepath.Join(root, filepath.FromSlash(relative))
	if _, err := os.Lstat(candidate); os.IsNotExist(err) {
		return nil
	}
	path, err := setupCheckoutPath(root, relative, false)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
