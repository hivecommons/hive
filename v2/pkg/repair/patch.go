package repair

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kubestellar/hive/v2/internal/gittransport"
)

type patchEngineInfrastructureError struct{ cause error }

func (e *patchEngineInfrastructureError) Error() string { return e.cause.Error() }
func (e *patchEngineInfrastructureError) Unwrap() error { return e.cause }

func patchEngineInfrastructureFailure(err error) error {
	if err == nil {
		return nil
	}
	if isPatchEngineInfrastructureFailure(err) {
		return err
	}
	return &patchEngineInfrastructureError{cause: err}
}

func isPatchEngineInfrastructureFailure(err error) bool {
	var infrastructure *patchEngineInfrastructureError
	return errors.As(err, &infrastructure)
}

const (
	modelPatchBegin = "HIVE_PATCH_BEGIN"
	modelPatchEnd   = "HIVE_PATCH_END"
	maxModelPatch   = 128 << 10
)

var diffHeader = regexp.MustCompile(`(?m)^diff --git a/([^\r\n]+) b/([^\r\n]+)\r?$`)

func extractModelPatch(output string) (string, error) {
	begin := strings.Index(output, modelPatchBegin)
	end := strings.Index(output, modelPatchEnd)
	if begin < 0 && end < 0 {
		return "", nil
	}
	if begin < 0 || end < 0 || end <= begin || strings.Count(output, modelPatchBegin) != 1 || strings.Count(output, modelPatchEnd) != 1 {
		return "", fmt.Errorf("model response has invalid Hive patch markers")
	}
	patch := strings.TrimSpace(output[begin+len(modelPatchBegin) : end])
	patch = strings.ReplaceAll(patch, "\r\n", "\n")
	patch = strings.TrimPrefix(patch, "```diff\n")
	patch = strings.TrimPrefix(patch, "```patch\n")
	patch = strings.TrimSuffix(patch, "```")
	patch = strings.TrimSpace(patch)
	if patch == "" || len(patch) > maxModelPatch {
		return "", fmt.Errorf("model patch is empty or exceeds %d bytes", maxModelPatch)
	}
	if strings.ContainsRune(patch, '\x00') {
		return "", fmt.Errorf("model patch contains an unsafe value")
	}
	if err := validateRepairTextSecrets(patch, "model patch"); err != nil {
		return "", err
	}
	return patch + "\n", nil
}

func patchChangedFiles(patchText string) ([]string, error) {
	if len(patchText) == 0 || len(patchText) > maxModelPatch {
		return nil, fmt.Errorf("model patch is empty or exceeds %d bytes", maxModelPatch)
	}
	if strings.ContainsRune(patchText, '\x00') {
		return nil, fmt.Errorf("model patch contains an unsafe value")
	}
	if err := validateRepairTextSecrets(patchText, "model patch"); err != nil {
		return nil, err
	}
	lower := strings.ToLower(patchText)
	for _, forbidden := range []string{"git binary patch", "binary files ", "rename from ", "rename to ", "copy from ", "copy to ", "old mode ", "new mode ", "submodule "} {
		if strings.Contains(lower, forbidden) {
			return nil, fmt.Errorf("model patch contains forbidden %s metadata", strings.TrimSpace(forbidden))
		}
	}
	matches := diffHeader.FindAllStringSubmatch(patchText, -1)
	if len(matches) == 0 || len(matches) > 64 {
		return nil, fmt.Errorf("model patch must contain between 1 and 64 text file diffs")
	}
	files := make([]string, 0, len(matches))
	seen := map[string]bool{}
	for _, match := range matches {
		left, right := strings.TrimSpace(match[1]), strings.TrimSpace(match[2])
		if left != right || !safePatchPath(right) {
			return nil, fmt.Errorf("model patch has an unsafe or renamed path")
		}
		if seen[right] {
			return nil, fmt.Errorf("model patch repeats file %s", right)
		}
		seen[right] = true
		files = append(files, right)
	}
	sort.Strings(files)
	return files, nil
}

func safePatchPath(file string) bool {
	return file != "" && strings.TrimSpace(file) == file && !strings.Contains(file, "\"") && !strings.Contains(file, "\\") && !strings.Contains(file, ":") && !strings.HasPrefix(file, "/") &&
		!strings.HasPrefix(file, "../") && path.Clean(file) == file && !strings.HasPrefix(file, ".git/")
}

func applyModelPatch(ctx context.Context, worktree, patchText string) error {
	for _, args := range [][]string{
		{"apply", "--check", "--whitespace=error-all", "--recount", "-"},
		{"apply", "--whitespace=error-all", "--recount", "-"},
	} {
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = worktree
		command.Env = gittransport.LocalEnvironment(providerEnvironment())
		command.Stdin = strings.NewReader(patchText)
		var output limitedBuffer
		command.Stdout, command.Stderr = &output, &output
		if err := command.Run(); err != nil {
			failure := fmt.Errorf("git %s model patch failed: %w: %s", strings.Join(args[:len(args)-1], " "), err, safeExcerpt(output.String()))
			return classifyPatchCommandFailure(ctx, err, output.String(), failure)
		}
	}
	return nil
}

func modelPatchAlreadyApplied(ctx context.Context, worktree, patchText string) (bool, error) {
	ok, _, err := checkModelPatch(ctx, worktree, patchText, true, false)
	return ok, err
}

func applyIncrementalModelPatch(ctx context.Context, worktree, patchText string) error {
	files, err := patchChangedFiles(patchText)
	if err != nil {
		return err
	}
	indexDirectory, err := os.MkdirTemp("", "hive-repair-index-")
	if err != nil {
		return patchEngineInfrastructureFailure(fmt.Errorf("create isolated incremental patch index: %w", err))
	}
	defer func() { _ = os.RemoveAll(indexDirectory) }()
	indexFile := filepath.Join(indexDirectory, "index")
	if _, err := runGitWithIndex(ctx, worktree, indexFile, "read-tree", "HEAD"); err != nil {
		return patchEngineInfrastructureFailure(fmt.Errorf("initialize isolated incremental patch index: %w", err))
	}
	stageFiles, err := incrementalPatchBaseFiles(ctx, worktree, indexFile, files)
	if err != nil {
		return patchEngineInfrastructureFailure(fmt.Errorf("inspect cumulative repair state for incremental patch: %w", err))
	}
	if len(stageFiles) > 0 {
		addArgs := append([]string{"add", "--"}, stageFiles...)
		if _, err := runGitWithIndex(ctx, worktree, indexFile, addArgs...); err != nil {
			return patchEngineInfrastructureFailure(fmt.Errorf("stage cumulative repair state in isolated incremental patch index: %w", err))
		}
	}
	return func() error {
		canApply, _, err := checkModelPatchWithIndex(ctx, worktree, patchText, false, true, indexFile)
		if err != nil {
			return patchEngineInfrastructureFailure(err)
		}
		if canApply {
			return applyIndexedModelPatch(ctx, worktree, patchText, indexFile)
		}
		hunks, err := splitModelPatchHunks(patchText)
		if err != nil {
			return err
		}
		pending := make([]string, 0, len(hunks))
		for index, hunk := range hunks {
			alreadyApplied, _, checkErr := checkModelPatchWithIndex(ctx, worktree, hunk, true, true, indexFile)
			if checkErr != nil {
				return patchEngineInfrastructureFailure(checkErr)
			}
			if alreadyApplied {
				continue
			}
			applicable, detail, checkErr := checkModelPatchWithIndex(ctx, worktree, hunk, false, true, indexFile)
			if checkErr != nil {
				return patchEngineInfrastructureFailure(checkErr)
			}
			if !applicable {
				return fmt.Errorf("incremental model patch hunk %d is neither applicable nor already present: %s", index+1, safeExcerpt(detail))
			}
			pending = append(pending, hunk)
		}
		if len(pending) == 0 {
			return nil
		}
		return applyIndexedModelPatch(ctx, worktree, strings.Join(pending, ""), indexFile)
	}()
}

// incrementalPatchBaseFiles returns the patch targets whose current worktree
// state must be copied into the isolated index before git apply --index. An
// absent path that is also absent from HEAD is deliberately omitted: it is the
// valid preimage of a new-file patch, and git add rejects that pathspec.
func incrementalPatchBaseFiles(ctx context.Context, worktree, indexFile string, files []string) ([]string, error) {
	result := make([]string, 0, len(files))
	for _, file := range files {
		_, statErr := os.Lstat(filepath.Join(worktree, filepath.FromSlash(file)))
		if statErr == nil {
			result = append(result, file)
			continue
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect incremental patch path %s: %w", file, statErr)
		}
		tracked, err := runGitWithIndex(ctx, worktree, indexFile, "ls-files", "--cached", "-z", "--", file)
		if err != nil {
			return nil, err
		}
		if tracked != "" {
			// Stage a cumulative deletion so the temporary index continues to
			// describe the exact worktree presented to the model.
			result = append(result, file)
		}
	}
	return result, nil
}

func applyIndexedModelPatch(ctx context.Context, worktree, patchText, indexFile string) error {
	for _, check := range []bool{true, false} {
		args := []string{"apply", "--index"}
		if check {
			args = append(args, "--check")
		}
		args = append(args, "--whitespace=error-all", "--recount", "-")
		command := exec.CommandContext(ctx, "git", args...)
		command.Dir = worktree
		command.Env = patchGitEnvironment(indexFile)
		command.Stdin = strings.NewReader(patchText)
		var output limitedBuffer
		command.Stdout, command.Stderr = &output, &output
		if err := command.Run(); err != nil {
			failure := fmt.Errorf("git %s incremental model patch failed: %w: %s", strings.Join(args[:len(args)-1], " "), err, safeExcerpt(output.String()))
			return classifyPatchCommandFailure(ctx, err, output.String(), failure)
		}
	}
	return nil
}

func classifyPatchCommandFailure(ctx context.Context, commandErr error, output string, failure error) error {
	var launchError *exec.Error
	if ctx.Err() != nil || errors.As(commandErr, &launchError) {
		return patchEngineInfrastructureFailure(failure)
	}
	detail := strings.ToLower(output + "\n" + commandErr.Error())
	for _, marker := range []string{
		"permission denied", "access is denied", "read-only file system", "no space left on device", "input/output error",
		"index.lock", "index file corrupt", "unable to write", "could not write", "cannot create",
		"fatal: not a git repository", "fatal: detected dubious ownership", "fatal: unable to", "fatal: cannot", "fatal: could not",
	} {
		if strings.Contains(detail, marker) {
			return patchEngineInfrastructureFailure(failure)
		}
	}
	return failure
}

func checkModelPatch(ctx context.Context, worktree, patchText string, reverse, indexed bool) (bool, string, error) {
	return checkModelPatchWithIndex(ctx, worktree, patchText, reverse, indexed, "")
}

func checkModelPatchWithIndex(ctx context.Context, worktree, patchText string, reverse, indexed bool, indexFile string) (bool, string, error) {
	args := []string{"apply"}
	if indexed {
		args = append(args, "--index")
	}
	if reverse {
		args = append(args, "--reverse")
	}
	args = append(args, "--check", "--whitespace=error-all", "--recount", "-")
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = worktree
	command.Env = patchGitEnvironment(indexFile)
	command.Stdin = strings.NewReader(patchText)
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		failure := fmt.Errorf("git %s model patch check failed: %w: %s", strings.Join(args[:len(args)-1], " "), err, safeExcerpt(output.String()))
		classified := classifyPatchCommandFailure(ctx, err, output.String(), failure)
		if isPatchEngineInfrastructureFailure(classified) {
			return false, output.String(), classified
		}
		return false, output.String(), nil
	}
	return true, output.String(), nil
}

func runGitWithIndex(ctx context.Context, worktree, indexFile string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = worktree
	command.Env = patchGitEnvironment(indexFile)
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return output.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, safeExcerpt(output.String()))
	}
	return output.String(), nil
}

func patchGitEnvironment(indexFile string) []string {
	environment := providerEnvironment()
	filtered := environment[:0]
	for _, pair := range environment {
		name, _, _ := strings.Cut(pair, "=")
		if strings.EqualFold(name, "GIT_INDEX_FILE") || strings.EqualFold(name, "GIT_CONFIG_COUNT") ||
			strings.EqualFold(name, "GIT_CONFIG_KEY_0") || strings.EqualFold(name, "GIT_CONFIG_VALUE_0") {
			continue
		}
		filtered = append(filtered, pair)
	}
	return gittransport.LocalEnvironmentWithOverrides(filtered, map[string]string{"GIT_INDEX_FILE": indexFile})
}

func splitModelPatchHunks(patchText string) ([]string, error) {
	if strings.Contains(patchText, "--- /dev/null") || strings.Contains(patchText, "+++ /dev/null") {
		return nil, fmt.Errorf("incremental revision patches cannot partially apply file creation or deletion")
	}
	var header, current strings.Builder
	hunks := []string{}
	flush := func() {
		if current.Len() > 0 {
			hunks = append(hunks, current.String())
			current.Reset()
		}
	}
	for _, line := range strings.SplitAfter(strings.ReplaceAll(patchText, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			header.Reset()
			header.WriteString(line)
		case strings.HasPrefix(line, "@@ "):
			flush()
			if header.Len() == 0 {
				return nil, fmt.Errorf("incremental model patch hunk is missing a file header")
			}
			current.WriteString(header.String())
			current.WriteString(line)
		case current.Len() > 0:
			current.WriteString(line)
		default:
			header.WriteString(line)
		}
	}
	flush()
	if len(hunks) == 0 {
		return nil, fmt.Errorf("incremental model patch contains no unified diff hunks")
	}
	return hunks, nil
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy, rightCopy := append([]string(nil), left...), append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}
