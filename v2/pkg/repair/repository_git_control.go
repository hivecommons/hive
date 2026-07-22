package repair

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxRepositoryGitControlFile = 1 << 20

type unsafeRepositoryGitControlError struct{ cause error }

func (e *unsafeRepositoryGitControlError) Error() string { return e.cause.Error() }
func (e *unsafeRepositoryGitControlError) Unwrap() error { return e.cause }

func unsafeRepositoryGitControlFailure(err error) error {
	if err == nil {
		return nil
	}
	var existing *unsafeRepositoryGitControlError
	if errors.As(err, &existing) {
		return err
	}
	return &unsafeRepositoryGitControlError{cause: err}
}

func isUnsafeRepositoryGitControlFailure(err error) bool {
	var unsafe *unsafeRepositoryGitControlError
	return errors.As(err, &unsafe)
}

type repositoryGitControlPath struct {
	Path      string
	Exists    bool
	Directory bool
	Mode      os.FileMode
	Size      int64
	SHA256    string
	Data      []byte
	info      os.FileInfo
}

type repositoryGitControlSnapshot struct {
	Worktree       string
	Locator        repositoryGitControlPath
	GitDir         repositoryGitControlPath
	GitDirBacklink repositoryGitControlPath
	CommonDirFile  repositoryGitControlPath
	CommonDir      repositoryGitControlPath
	CommonConfig   repositoryGitControlPath
	WorktreeConfig repositoryGitControlPath
}

func captureRepositoryGitControl(worktree, expectedCommonDir string) (repositoryGitControlSnapshot, error) {
	root, err := filepath.Abs(filepath.Clean(worktree))
	if err != nil {
		return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(err)
	}
	rootInfo, err := snapshotRepositoryGitControlPath(root, false, true)
	if err != nil || !rootInfo.Directory {
		return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(fmt.Errorf("repair worktree is not an ordinary directory: %w", err))
	}
	locator, err := snapshotRepositoryGitControlPath(filepath.Join(root, ".git"), false, false)
	if err != nil {
		return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(fmt.Errorf("inspect repair Git metadata locator: %w", err))
	}
	gitDir := locator.Path
	if !locator.Directory {
		gitDir, err = parseGitDirectoryPointer(root, locator.Data)
		if err != nil {
			return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(err)
		}
	}
	gitDirInfo, err := snapshotRepositoryGitControlPath(gitDir, false, true)
	if err != nil || !gitDirInfo.Directory {
		return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(fmt.Errorf("repair Git directory is not ordinary: %w", err))
	}
	commonFile, err := snapshotRepositoryGitControlPath(filepath.Join(gitDir, "commondir"), true, false)
	if err != nil {
		return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(fmt.Errorf("inspect repair Git common-directory locator: %w", err))
	}
	commonDir := gitDir
	if commonFile.Exists {
		commonDir, err = parseCommonDirectoryPointer(gitDir, commonFile.Data)
		if err != nil {
			return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(err)
		}
	}
	commonInfo, err := snapshotRepositoryGitControlPath(commonDir, false, true)
	if err != nil || !commonInfo.Directory {
		return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(fmt.Errorf("repair Git common directory is not ordinary: %w", err))
	}
	if expectedCommonDir != "" && !sameRepairCommonDir(commonInfo.Path, expectedCommonDir) {
		return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(fmt.Errorf("repair worktree Git common directory differs from the controller repository"))
	}
	gitDirBacklink := repositoryGitControlPath{Path: filepath.Join(gitDirInfo.Path, "gitdir")}
	if !sameRepairCommonDir(gitDirInfo.Path, commonInfo.Path) {
		relative, relErr := filepath.Rel(commonInfo.Path, gitDirInfo.Path)
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if relErr != nil || len(parts) != 2 || parts[0] != "worktrees" || parts[1] == "" || parts[1] == "." || parts[1] == ".." {
			return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(fmt.Errorf("linked repair worktree Git directory is outside the exact common worktrees namespace"))
		}
		gitDirBacklink, err = snapshotRepositoryGitControlPath(filepath.Join(gitDirInfo.Path, "gitdir"), false, false)
		if err != nil {
			return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(fmt.Errorf("inspect linked-worktree Git directory backlink: %w", err))
		}
		backlinkTarget, backlinkErr := parseCommonDirectoryPointer(gitDirInfo.Path, gitDirBacklink.Data)
		if backlinkErr != nil || !sameRepairCommonDir(backlinkTarget, locator.Path) {
			return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(fmt.Errorf("linked-worktree Git directory backlink does not bind the exact .git locator"))
		}
	}
	commonConfig, err := snapshotRepositoryGitControlPath(filepath.Join(commonInfo.Path, "config"), true, false)
	if err != nil {
		return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(fmt.Errorf("inspect repository common Git config: %w", err))
	}
	worktreeConfig, err := snapshotRepositoryGitControlPath(filepath.Join(gitDirInfo.Path, "config.worktree"), true, false)
	if err != nil {
		return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(fmt.Errorf("inspect repository worktree Git config: %w", err))
	}
	for _, config := range []repositoryGitControlPath{commonConfig, worktreeConfig} {
		if config.Exists {
			if err := rejectUnsafeGitConfigBytes(config.Path, config.Data); err != nil {
				return repositoryGitControlSnapshot{}, unsafeRepositoryGitControlFailure(err)
			}
		}
	}
	return repositoryGitControlSnapshot{
		Worktree: root, Locator: locator, GitDir: gitDirInfo, GitDirBacklink: gitDirBacklink, CommonDirFile: commonFile,
		CommonDir: commonInfo, CommonConfig: commonConfig, WorktreeConfig: worktreeConfig,
	}, nil
}

func snapshotRepositoryGitControlPath(path string, allowMissing, requireDirectory bool) (repositoryGitControlPath, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return repositoryGitControlPath{}, err
	}
	info, err := os.Lstat(absolute)
	if errors.Is(err, os.ErrNotExist) && allowMissing {
		if parentErr := rejectCodexProviderLinkedPath(filepath.Dir(absolute)); parentErr != nil {
			return repositoryGitControlPath{}, parentErr
		}
		return repositoryGitControlPath{Path: absolute}, nil
	}
	if err != nil {
		return repositoryGitControlPath{}, err
	}
	if err := rejectCodexProviderLinkedPath(absolute); err != nil {
		return repositoryGitControlPath{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return repositoryGitControlPath{}, fmt.Errorf("Git control path is linked")
	}
	if requireDirectory || info.IsDir() {
		if !info.IsDir() {
			return repositoryGitControlPath{}, fmt.Errorf("Git control path is not a directory")
		}
		return repositoryGitControlPath{Path: absolute, Exists: true, Directory: true, Mode: info.Mode(), info: info}, nil
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxRepositoryGitControlFile {
		return repositoryGitControlPath{}, fmt.Errorf("Git control file is not a bounded ordinary file")
	}
	file, err := os.Open(absolute)
	if err != nil {
		return repositoryGitControlPath{}, err
	}
	data := make([]byte, info.Size())
	read, readErr := io.ReadFull(file, data)
	if errors.Is(readErr, io.EOF) && info.Size() == 0 {
		readErr = nil
	}
	if readErr == nil && int64(read) != info.Size() {
		readErr = fmt.Errorf("short Git control file read")
	}
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, afterErr := os.Lstat(absolute)
	if readErr != nil || statErr != nil || closeErr != nil || afterErr != nil || !os.SameFile(info, opened) || !os.SameFile(info, after) || after.Size() != info.Size() || after.Mode() != info.Mode() {
		return repositoryGitControlPath{}, fmt.Errorf("Git control file changed while it was inspected")
	}
	digest := sha256.Sum256(data)
	return repositoryGitControlPath{
		Path: absolute, Exists: true, Mode: info.Mode(), Size: info.Size(), SHA256: hex.EncodeToString(digest[:]), Data: data, info: info,
	}, nil
}

func parseGitDirectoryPointer(worktree string, data []byte) (string, error) {
	line := strings.TrimSpace(string(data))
	if strings.ContainsAny(line, "\r\n\x00") || !strings.HasPrefix(strings.ToLower(line), "gitdir:") {
		return "", fmt.Errorf("repair worktree .git locator is malformed")
	}
	value := strings.TrimSpace(line[len("gitdir:"):])
	return resolveGitControlPointer(worktree, value)
}

func parseCommonDirectoryPointer(gitDir string, data []byte) (string, error) {
	line := strings.TrimSpace(string(data))
	if line == "" || strings.ContainsAny(line, "\r\n\x00") {
		return "", fmt.Errorf("repair Git common-directory locator is malformed")
	}
	return resolveGitControlPointer(gitDir, line)
}

func resolveGitControlPointer(base, value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("repair Git metadata pointer is empty or malformed")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	value, err := filepath.Abs(filepath.Clean(value))
	if err != nil {
		return "", err
	}
	return value, nil
}

func sameRepositoryGitControl(left, right repositoryGitControlSnapshot) bool {
	return sameRepairCommonDir(left.Worktree, right.Worktree) &&
		sameRepositoryGitControlPath(left.Locator, right.Locator) &&
		sameRepositoryGitControlPath(left.GitDir, right.GitDir) &&
		sameRepositoryGitControlPath(left.GitDirBacklink, right.GitDirBacklink) &&
		sameRepositoryGitControlPath(left.CommonDirFile, right.CommonDirFile) &&
		sameRepositoryGitControlPath(left.CommonDir, right.CommonDir) &&
		sameRepositoryGitControlPath(left.CommonConfig, right.CommonConfig) &&
		sameRepositoryGitControlPath(left.WorktreeConfig, right.WorktreeConfig)
}

func sameRepositoryGitControlPath(left, right repositoryGitControlPath) bool {
	if !sameRepairCommonDir(left.Path, right.Path) || left.Exists != right.Exists || left.Directory != right.Directory || left.Mode != right.Mode || left.Size != right.Size || left.SHA256 != right.SHA256 {
		return false
	}
	if !left.Exists {
		return true
	}
	return left.info != nil && right.info != nil && os.SameFile(left.info, right.info)
}

var gitConfigSectionPattern = regexp.MustCompile(`^\[([A-Za-z0-9.-]+)(?:[ \t]+"((?:[^"\\]|\\.)*)")?\][ \t]*(?:[#;].*)?$`)
var gitConfigKeyPattern = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*)(?:[ \t]*=|[ \t]+|$)`)

func rejectUnsafeGitConfigBytes(path string, data []byte) error {
	if bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("repository Git config %s contains NUL", path)
	}
	section := ""
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxRepositoryGitControlFile)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			match := gitConfigSectionPattern.FindStringSubmatch(line)
			if match == nil {
				return fmt.Errorf("repository Git config %s has an unsupported section syntax", path)
			}
			section = strings.ToLower(match[1])
			if match[2] != "" {
				section += "." + strings.ToLower(match[2])
			}
			continue
		}
		match := gitConfigKeyPattern.FindStringSubmatch(line)
		if section == "" || match == nil {
			return fmt.Errorf("repository Git config %s has unsupported variable syntax", path)
		}
		key := section + "." + strings.ToLower(match[1])
		if key == "core.bare" && gitConfigBooleanIsNotFalse(line[len(match[0]):]) {
			return fmt.Errorf("repository Git config %s contains unsafe value for core.bare", path)
		}
		if repositoryGitConfigCanExecuteOrRedirect(key) {
			return fmt.Errorf("repository Git config %s contains unsafe directive %s", path, key)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read repository Git config %s: %w", path, err)
	}
	return nil
}

func gitConfigBooleanIsNotFalse(raw string) bool {
	value := strings.TrimSpace(raw)
	value = strings.TrimSpace(strings.TrimPrefix(value, "="))
	if comment := strings.IndexAny(value, "#;"); comment >= 0 {
		value = strings.TrimSpace(value[:comment])
	}
	value = strings.Trim(strings.ToLower(value), `"`)
	switch value {
	case "false", "no", "off", "0":
		return false
	default:
		return true
	}
}
