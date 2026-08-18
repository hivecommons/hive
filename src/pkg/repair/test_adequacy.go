package repair

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const maxAutonomousTestFileBytes = 256 << 10

var (
	javascriptTestDeclaration = regexp.MustCompile(`(?m)\b(?:test|it)\s*\(`)
	javascriptAssertion       = regexp.MustCompile(`(?m)\b(?:expect|assert(?:\.[A-Za-z_][A-Za-z0-9_]*)?)\s*\(`)
	goTestDeclaration         = regexp.MustCompile(`(?m)^func\s+Test[A-Za-z0-9_]+\s*\(\s*t\s+\*testing\.T\s*\)`)
	goTestAssertion           = regexp.MustCompile(`(?m)\bt\.(?:Error|Errorf|Fatal|Fatalf|Fail|FailNow)\s*\(`)
	pythonTestDeclaration     = regexp.MustCompile(`(?m)^def\s+test_[A-Za-z0-9_]+\s*\(`)
	pythonAssertion           = regexp.MustCompile(`(?m)^\s*assert\s+.+`)
	weakeningTestConstruct    = regexp.MustCompile(`(?i)(?:\b(?:describe|context|it|test|suite)\s*\.\s*(?:skip|only|todo)\b|\b(?:xdescribe|xit|xtest)\s*\(|\bt\s*\.\s*skip(?:f|now)?\s*\(|pytest\s*\.\s*mark\s*\.\s*(?:skip|skipif|xfail)\b|unittest\s*\.\s*(?:skip|skipif|skipexpectedfailure)\b|@\s*(?:skip|skipif)\b|process\s*\.\s*exit\s*\(|os\s*\.\s*exit\s*\(|pytest\s*\.\s*exit\s*\()`)
)

func validateAdditiveTestFilesInWorktree(ctx context.Context, worktree string, files []string) error {
	for _, name := range normalizedExactFiles(files) {
		if !supportedAutonomousTestPath(name) {
			return fmt.Errorf("test adequacy repair path %s is not a supported autonomous test-file addition", name)
		}
		tracked, err := runGit(ctx, worktree, "ls-tree", "-z", "HEAD", "--", name)
		if err != nil {
			return patchEngineInfrastructureFailure(fmt.Errorf("inspect test adequacy base path %s: %w", name, err))
		}
		if tracked != "" {
			return fmt.Errorf("test adequacy autonomous repair must be additive; existing test file %s was modified, renamed, or deleted", name)
		}
		absolute := filepath.Join(worktree, filepath.FromSlash(name))
		info, err := os.Lstat(absolute)
		if err != nil {
			return fmt.Errorf("test adequacy autonomous repair must add regular test file %s: %w", name, err)
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxAutonomousTestFileBytes {
			return fmt.Errorf("test adequacy autonomous repair file %s must be a non-empty regular file no larger than %d bytes", name, maxAutonomousTestFileBytes)
		}
		content, err := os.ReadFile(absolute)
		if err != nil {
			return patchEngineInfrastructureFailure(fmt.Errorf("read additive test file %s: %w", name, err))
		}
		if err := validateAutonomousTestContent(name, content); err != nil {
			return err
		}
	}
	return nil
}

// ValidateAutonomousTestAdequacyCommit revalidates an exact repair commit at
// the final merge boundary. Only new, regular, runnable, non-trivial test files
// may receive automatic merge authority; existing tests cannot be changed.
func ValidateAutonomousTestAdequacyCommit(ctx context.Context, repositoryDir, commitSHA string, expectedFiles []string) error {
	if !validGitCommitSHA(commitSHA) || len(expectedFiles) == 0 {
		return fmt.Errorf("exact test adequacy commit and changed files are required")
	}
	parents, err := runGit(ctx, repositoryDir, "rev-list", "--parents", "-n", "1", commitSHA)
	if err != nil {
		return fmt.Errorf("read test adequacy commit parent: %w", err)
	}
	parentFields := strings.Fields(parents)
	if len(parentFields) != 2 || !strings.EqualFold(parentFields[0], commitSHA) || !validGitCommitSHA(parentFields[1]) {
		return fmt.Errorf("test adequacy autonomous repair must be one exact non-merge commit")
	}
	parent := parentFields[1]
	diff, err := runGit(ctx, repositoryDir, "diff-tree", "--no-commit-id", "--name-status", "--no-renames", "-r", "-z", parent, commitSHA, "--")
	if err != nil {
		return fmt.Errorf("inspect exact test adequacy commit: %w", err)
	}
	entries, err := parseAdditiveTestDiff(diff)
	if err != nil {
		return err
	}
	expected := normalizedExactFiles(expectedFiles)
	if !equalStringSets(entries, expected) {
		return fmt.Errorf("exact test adequacy commit changed files do not match durable repair state")
	}
	for _, name := range entries {
		if !supportedAutonomousTestPath(name) {
			return fmt.Errorf("test adequacy repair path %s is not a supported autonomous test-file addition", name)
		}
		kind, err := runGit(ctx, repositoryDir, "cat-file", "-t", commitSHA+":"+name)
		if err != nil || strings.TrimSpace(kind) != "blob" {
			return fmt.Errorf("test adequacy commit path %s is not a regular Git blob", name)
		}
		content, err := runGitBytes(ctx, repositoryDir, maxAutonomousTestFileBytes+1, "show", commitSHA+":"+name)
		if err != nil {
			return fmt.Errorf("read exact additive test file %s: %w", name, err)
		}
		if len(content) == 0 || len(content) > maxAutonomousTestFileBytes {
			return fmt.Errorf("test adequacy autonomous repair file %s has invalid size", name)
		}
		if err := validateAutonomousTestContent(name, content); err != nil {
			return err
		}
	}
	return nil
}

func parseAdditiveTestDiff(diff string) ([]string, error) {
	values := strings.Split(diff, "\x00")
	files := make([]string, 0, len(values)/2)
	for index := 0; index < len(values); {
		if values[index] == "" {
			index++
			continue
		}
		if index+1 >= len(values) {
			return nil, fmt.Errorf("test adequacy commit returned an incomplete changed-file record")
		}
		status, name := values[index], normalizeTestPath(values[index+1])
		if status != "A" || name == "" {
			return nil, fmt.Errorf("test adequacy autonomous repair must only add new test files; observed status %q for %q", status, name)
		}
		files = append(files, name)
		index += 2
	}
	files = normalizedExactFiles(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("test adequacy autonomous repair added no test files")
	}
	return files, nil
}

func validateAutonomousTestContent(name string, content []byte) error {
	if bytes.IndexByte(content, 0) >= 0 {
		return fmt.Errorf("additive test file %s contains binary content", name)
	}
	text := string(content)
	if weakeningTestConstruct.MatchString(text) {
		return fmt.Errorf("additive test file %s contains a skip, focus, expected-failure, or early-exit construct", name)
	}
	compact := strings.ToLower(strings.Join(strings.Fields(text), ""))
	for _, trivial := range []string{
		"expect(true).tobe(true)", "expect(false).tobe(false)", "expect(1).tobe(1)", "expect(0).tobe(0)",
		"assert(true)", "asserttrue(true)", "assert.true(true)", "assertfalse(false)", "assert.false(false)", "asserttrue", "asserttrue,",
		"asserttrue\n", "asserttrue\r\n", "asserttrue#", "asserttrue;",
	} {
		if strings.Contains(compact, trivial) {
			return fmt.Errorf("additive test file %s contains a trivial self-fulfilling assertion", name)
		}
	}
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, "_test.go"):
		if !goTestDeclaration.MatchString(text) || !goTestAssertion.MatchString(text) {
			return fmt.Errorf("additive Go test file %s must contain a runnable Test function and failure assertion", name)
		}
	case strings.HasSuffix(lower, ".py"):
		if !pythonTestDeclaration.MatchString(text) || !pythonAssertion.MatchString(text) || strings.Contains(compact, "asserttrue") {
			return fmt.Errorf("additive Python test file %s must contain a runnable test function and non-trivial assertion", name)
		}
	default:
		if !javascriptTestDeclaration.MatchString(text) || !javascriptAssertion.MatchString(text) {
			return fmt.Errorf("additive JavaScript/TypeScript test file %s must contain a runnable test and assertion", name)
		}
	}
	return nil
}

func supportedAutonomousTestPath(name string) bool {
	lower := strings.ToLower(normalizeTestPath(name))
	if strings.HasSuffix(lower, "_test.go") || strings.HasSuffix(lower, "_test.py") || strings.HasPrefix(filepath.Base(lower), "test_") && strings.HasSuffix(lower, ".py") {
		return true
	}
	for _, extension := range []string{".js", ".jsx", ".cjs", ".mjs", ".ts", ".tsx"} {
		if strings.HasSuffix(lower, ".test"+extension) || strings.HasSuffix(lower, ".spec"+extension) {
			return true
		}
	}
	return false
}

func normalizedExactFiles(files []string) []string {
	result := make([]string, 0, len(files))
	seen := map[string]bool{}
	for _, file := range files {
		file = normalizeTestPath(file)
		if file != "" && !seen[file] {
			seen[file] = true
			result = append(result, file)
		}
	}
	sort.Strings(result)
	return result
}

func normalizeTestPath(name string) string {
	return strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(name)), "./")
}
