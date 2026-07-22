package integrated

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

const (
	legacyCheckoutMaxEntries = 300_000
	legacyCheckoutMaxBytes   = int64(10 << 30)
	legacyGitConfigMaxBytes  = int64(64 << 10)
)

var (
	legacyGitSectionPattern = regexp.MustCompile(`^\[([A-Za-z][A-Za-z0-9.-]*)(?: "([A-Za-z0-9][A-Za-z0-9._/-]{0,199})")?\]$`)
	legacyGitKeyPattern     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.-]*$`)
	legacyWorktreeIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)
)

// legacyManagedCheckoutProof is deliberately internal. It can only be built
// after RunSetup has loaded the durable Config from this exact state directory
// and verified its immutable repository ID against GitHub. Other callers still
// reject markerless directories before invoking Git.
type legacyManagedCheckoutProof struct {
	Config           Config
	LiveRepositoryID string
}

func migrateLegacyManagedCheckout(checkout, repository string, proof legacyManagedCheckoutProof) error {
	if err := validateLegacyManagedCheckoutProof(checkout, repository, proof); err != nil {
		return err
	}
	if err := auditLegacyCheckoutTree(checkout); err != nil {
		return err
	}
	gitDir := filepath.Join(checkout, ".git")
	if err := validateLegacyGitControlFiles(gitDir); err != nil {
		return err
	}
	if err := validateLegacyGitConfig(filepath.Join(gitDir, "config"), repository, proof.Config.DefaultBranch); err != nil {
		return err
	}
	if err := validateLegacyGitHooks(filepath.Join(gitDir, "hooks")); err != nil {
		return err
	}
	if err := validateLegacyGitWorktrees(gitDir, proof.Config.StateDir); err != nil {
		return err
	}
	if err := writeManagedCheckoutOwner(checkout, repository); err != nil {
		return fmt.Errorf("write exact checkout ownership marker: %w", err)
	}
	return nil
}

func validateLegacyManagedCheckoutProof(checkout, repository string, proof legacyManagedCheckoutProof) error {
	config := proof.Config
	if !supportedDurableConfigSchema(config.SchemaVersion) || !strings.EqualFold(strings.TrimSpace(config.Repository), strings.TrimSpace(repository)) {
		return fmt.Errorf("durable legacy config does not bind exact repository %s", repository)
	}
	repositoryID := strings.TrimSpace(config.RepositoryID)
	liveRepositoryID := strings.TrimSpace(proof.LiveRepositoryID)
	parsedID, err := strconv.ParseInt(repositoryID, 10, 64)
	if err != nil || parsedID <= 0 || strconv.FormatInt(parsedID, 10) != repositoryID || liveRepositoryID != repositoryID {
		return fmt.Errorf("durable and live immutable repository identities do not match")
	}
	if !validLegacyBranchName(config.DefaultBranch) {
		return fmt.Errorf("durable legacy config has an unsafe default branch")
	}
	if !filepath.IsAbs(config.StateDir) || !filepath.IsAbs(config.CheckoutDir) {
		return fmt.Errorf("durable legacy state and checkout paths must be absolute")
	}
	absoluteCheckout, err := filepath.Abs(checkout)
	if err != nil {
		return err
	}
	absoluteCheckout = filepath.Clean(absoluteCheckout)
	stateDir := filepath.Clean(config.StateDir)
	expectedCheckout := filepath.Join(stateDir, "integrated", "checkouts", safeRepoName(repository))
	if !sameFilesystemPath(config.CheckoutDir, absoluteCheckout) || !sameFilesystemPath(expectedCheckout, absoluteCheckout) ||
		!sameFilesystemPath(filepath.Dir(filepath.Dir(filepath.Dir(absoluteCheckout))), stateDir) {
		return fmt.Errorf("durable legacy config does not bind this exact dedicated state checkout path")
	}
	return nil
}

func auditLegacyCheckoutTree(checkout string) error {
	root := filepath.Clean(checkout)
	entries := 0
	var bytes int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > legacyCheckoutMaxEntries {
			return fmt.Errorf("legacy checkout exceeds the bounded %d-entry migration inventory", legacyCheckoutMaxEntries)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("legacy checkout inventory escapes its exact root")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("legacy checkout entry %s is linked, reparsed, or non-regular", filepath.ToSlash(relative))
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !sameFilesystemPath(resolved, path) {
			return fmt.Errorf("legacy checkout entry %s resolves through a link or reparse point", filepath.ToSlash(relative))
		}
		if info.Mode().IsRegular() {
			if info.Size() < 0 || bytes > legacyCheckoutMaxBytes-info.Size() {
				return fmt.Errorf("legacy checkout exceeds the bounded %d-byte migration inventory", legacyCheckoutMaxBytes)
			}
			bytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("audit ordinary bounded legacy checkout: %w", err)
	}
	return nil
}

func validateLegacyGitControlFiles(gitDir string) error {
	for _, relative := range []string{
		"commondir", "gitdir", "config.worktree", "modules", "shallow",
		"objects/info/alternates", "objects/info/http-alternates",
		"info/attributes", "info/sparse-checkout",
	} {
		if _, err := os.Lstat(filepath.Join(gitDir, filepath.FromSlash(relative))); err == nil {
			return fmt.Errorf("legacy Git control path %s is not permitted", relative)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect legacy Git control path %s: %w", relative, err)
		}
	}
	return nil
}

func validateLegacyGitConfig(path, repository, defaultBranch string) error {
	data, err := readBoundedOrdinaryFile(path, legacyGitConfigMaxBytes)
	if err != nil {
		return fmt.Errorf("read static legacy Git config: %w", err)
	}
	if strings.ContainsRune(string(data), '\x00') {
		return fmt.Errorf("legacy Git config contains a NUL byte")
	}

	sections := map[string]map[string]string{}
	current := ""
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimSuffix(scanner.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			return fmt.Errorf("legacy Git config line %d uses continuation", lineNumber)
		}
		if strings.HasPrefix(line, "[") {
			match := legacyGitSectionPattern.FindStringSubmatch(line)
			if match == nil {
				return fmt.Errorf("legacy Git config line %d has an unsupported section", lineNumber)
			}
			kind, name := strings.ToLower(match[1]), match[2]
			if (kind == "core" && name != "") || (kind != "core" && name == "") {
				return fmt.Errorf("legacy Git config line %d has an unsupported section shape", lineNumber)
			}
			if kind != "core" && kind != "remote" && kind != "branch" {
				return fmt.Errorf("legacy Git config section %s is not permitted", kind)
			}
			current = kind + "\x00" + name
			if _, duplicate := sections[current]; duplicate {
				return fmt.Errorf("legacy Git config repeats section %s", line)
			}
			sections[current] = map[string]string{}
			continue
		}
		if current == "" {
			return fmt.Errorf("legacy Git config line %d is outside a section", lineNumber)
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		if !ok || !legacyGitKeyPattern.MatchString(key) || value == "" || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("legacy Git config line %d is not a strict key/value", lineNumber)
		}
		if _, duplicate := sections[current][key]; duplicate {
			return fmt.Errorf("legacy Git config repeats key %s", key)
		}
		sections[current][key] = value
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	core := sections["core\x00"]
	if core == nil || core["repositoryformatversion"] != "0" || !strings.EqualFold(core["bare"], "false") || !strings.EqualFold(core["logallrefupdates"], "true") {
		return fmt.Errorf("legacy Git core config is incomplete or non-standard")
	}
	coreAllowed := map[string]func(string) bool{
		"repositoryformatversion": func(value string) bool { return value == "0" },
		"filemode":                legacyGitBoolean,
		"bare":                    func(value string) bool { return strings.EqualFold(value, "false") },
		"logallrefupdates":        func(value string) bool { return strings.EqualFold(value, "true") },
		"symlinks":                legacyGitBoolean,
		"ignorecase":              legacyGitBoolean,
		"autocrlf":                func(value string) bool { return strings.EqualFold(value, "false") },
		"precomposeunicode":       legacyGitBoolean,
		"longpaths":               legacyGitBoolean,
		"protectntfs":             legacyGitBoolean,
	}
	for key, value := range core {
		validator, ok := coreAllowed[key]
		if !ok || !validator(value) {
			return fmt.Errorf("legacy Git core.%s is not permitted", key)
		}
	}

	origin := sections["remote\x00origin"]
	expectedURL := "https://github.com/" + strings.TrimSpace(repository) + ".git"
	if len(origin) != 2 || !strings.EqualFold(origin["url"], expectedURL) || origin["fetch"] != "+refs/heads/*:refs/remotes/origin/*" {
		return fmt.Errorf("legacy Git origin is not the exact canonical HTTPS repository remote")
	}
	for section, values := range sections {
		kind, name, _ := strings.Cut(section, "\x00")
		switch kind {
		case "core":
			continue
		case "remote":
			if name != "origin" {
				return fmt.Errorf("legacy Git remote %s is not permitted", name)
			}
		case "branch":
			if !validLegacyBranchName(name) || len(values) != 2 || values["remote"] != "origin" ||
				(values["merge"] != "refs/heads/"+name && values["merge"] != "refs/heads/"+defaultBranch) {
				return fmt.Errorf("legacy Git branch %s has unsupported tracking config", name)
			}
		}
	}
	if sections["branch\x00"+defaultBranch] == nil {
		return fmt.Errorf("legacy Git config has no exact default-branch tracking section")
	}
	return nil
}

func validateLegacyGitHooks(hooksDir string) error {
	entries, err := os.ReadDir(hooksDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect legacy Git hooks: %w", err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".sample") {
			return fmt.Errorf("legacy Git hook %s is active, linked, or non-standard", entry.Name())
		}
	}
	return nil
}

func validateLegacyGitWorktrees(gitDir, stateDir string) error {
	worktreesDir := filepath.Join(gitDir, "worktrees")
	entries, err := os.ReadDir(worktreesDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect legacy linked worktrees: %w", err)
	}
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !legacyWorktreeIDPattern.MatchString(id) {
			return fmt.Errorf("legacy Git worktree metadata %s is not an exact Hive repair identity", id)
		}
		metadataDir := filepath.Join(worktreesDir, id)
		if err := validateLegacyGitWorktreeMetadata(metadataDir, stateDir, id); err != nil {
			return err
		}
	}
	return nil
}

func validateLegacyGitWorktreeMetadata(metadataDir, stateDir, id string) error {
	entries, err := os.ReadDir(metadataDir)
	if err != nil {
		return err
	}
	allowed := map[string]bool{
		"HEAD": true, "index": true, "ORIG_HEAD": true, "COMMIT_EDITMSG": true,
		"commondir": true, "gitdir": true, "logs": true, "refs": true, "locked": true,
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return fmt.Errorf("legacy Hive repair worktree metadata %s is not permitted", entry.Name())
		}
		seen[entry.Name()] = true
	}
	for _, required := range []string{"HEAD", "index", "commondir", "gitdir"} {
		if !seen[required] {
			return fmt.Errorf("legacy Hive repair worktree metadata is missing %s", required)
		}
	}
	commondir, err := readBoundedOrdinaryFile(filepath.Join(metadataDir, "commondir"), 128)
	if err != nil || strings.TrimSpace(string(commondir)) != "../.." {
		return fmt.Errorf("legacy Hive repair worktree has unsafe common-directory indirection")
	}
	head, err := readBoundedOrdinaryFile(filepath.Join(metadataDir, "HEAD"), 1024)
	branch := strings.TrimPrefix(strings.TrimSpace(string(head)), "ref: refs/heads/")
	if err != nil || !strings.HasPrefix(branch, "hive/repair-"+id+"-") || !validLegacyBranchName(branch) {
		return fmt.Errorf("legacy Hive repair worktree has an unsafe HEAD binding")
	}
	worktreeDir := filepath.Join(filepath.Clean(stateDir), "repair", "worktrees", id)
	if err := validateExistingOrdinaryDirectory(worktreeDir); err != nil {
		return fmt.Errorf("legacy Hive repair worktree path is unsafe: %w", err)
	}
	worktreeGit := filepath.Join(worktreeDir, ".git")
	gitdir, err := readBoundedOrdinaryFile(filepath.Join(metadataDir, "gitdir"), 32<<10)
	if err != nil || !sameFilesystemPath(strings.TrimSpace(string(gitdir)), worktreeGit) {
		return fmt.Errorf("legacy Hive repair worktree gitdir does not bind its exact state-owned path")
	}
	backlink, err := readBoundedOrdinaryFile(worktreeGit, 32<<10)
	if err != nil {
		return fmt.Errorf("read legacy Hive repair worktree backlink: %w", err)
	}
	backlinkPath := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(backlink)), "gitdir:"))
	if !strings.HasPrefix(strings.TrimSpace(string(backlink)), "gitdir:") || !sameFilesystemPath(backlinkPath, metadataDir) {
		return fmt.Errorf("legacy Hive repair worktree backlink does not bind exact main metadata")
	}
	return nil
}

func validateExistingOrdinaryDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not an ordinary existing directory", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !sameFilesystemPath(resolved, path) {
		return fmt.Errorf("%s resolves through a link or reparse point", path)
	}
	return nil
}

func readBoundedOrdinaryFile(path string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > maximum {
		return nil, fmt.Errorf("%s is missing, linked, non-regular, or exceeds %d bytes", path, maximum)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !sameFilesystemPath(resolved, path) {
		return nil, fmt.Errorf("%s resolves through a link or reparse point", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maximum)
	}
	return data, nil
}

func validLegacyBranchName(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 200 && !strings.HasPrefix(value, ".") && !strings.HasPrefix(value, "/") &&
		!strings.HasSuffix(value, ".") && !strings.HasSuffix(value, "/") && !strings.HasSuffix(value, ".lock") &&
		!strings.Contains(value, "..") && !strings.Contains(value, "@{") && !strings.ContainsAny(value, "\\ ~^:?*[\t\r\n")
}

func legacyGitBoolean(value string) bool {
	return strings.EqualFold(value, "true") || strings.EqualFold(value, "false")
}

func sameFilesystemPath(left, right string) bool {
	left = filepath.Clean(filepath.FromSlash(strings.TrimSpace(left)))
	right = filepath.Clean(filepath.FromSlash(strings.TrimSpace(right)))
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func legacyRecoveryStateDir(stateDir string) string {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return "hive-state-recovered"
	}
	return filepath.Clean(stateDir) + "-recovered"
}
