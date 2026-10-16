package complexity

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// analyzerTimeout caps one tool's run over one checkout (all modules).
const analyzerTimeout = 15 * time.Minute

// maxModulesPerRepo bounds how many go.mod directories a tool is run in.
const maxModulesPerRepo = 8

// goModDirs returns the directories under root containing a go.mod, skipping
// vendor/testdata/node_modules/.git, at most maxModulesPerRepo, sorted.
func goModDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", "testdata", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" {
			dirs = append(dirs, filepath.Dir(path))
			if len(dirs) >= maxModulesPerRepo {
				return fs.SkipAll
			}
		}
		return nil
	})
	return dirs, err
}

// toolEnv is the environment static-analysis tools run with: no toolchain
// downloads, no prompts, module mode.
func toolEnv() []string {
	return append(os.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod", "GIT_TERMINAL_PROMPT=0", "CGO_ENABLED=0")
}

// runTool runs name args... in every module dir of root and returns the
// concatenated combined output. A non-zero exit with diagnostics on output
// is the tool reporting findings, not a failure; a non-zero exit with no
// diagnostics is returned as an error.
func runTool(ctx context.Context, root, name string, args ...string) (string, error) {
	bin, err := exec.LookPath(name)
	if err != nil {
		return "", ErrAnalyzerUnavailable
	}
	dirs, err := goModDirs(root)
	if err != nil {
		return "", err
	}
	if len(dirs) == 0 {
		return "", errors.New("complexity: no go.mod under checkout")
	}
	ctx, cancel := context.WithTimeout(ctx, analyzerTimeout)
	defer cancel()
	var out bytes.Buffer
	for _, dir := range dirs {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = dir
		cmd.Env = toolEnv()
		cmd.Stdin = nil
		b, rerr := cmd.CombinedOutput()
		out.Write(b)
		if rerr != nil && CountFindings(string(b)) == 0 {
			msg := strings.TrimSpace(string(b))
			if len(msg) > 200 {
				msg = msg[:200]
			}
			return out.String(), errors.New(name + " in " + filepath.Base(dir) + ": " + rerr.Error() + ": " + msg)
		}
	}
	return out.String(), nil
}

// GoVet runs `go vet ./...` in each module of the checkout.
func GoVet() Analyzer {
	return Analyzer{Tool: "go vet", Run: func(ctx context.Context, dir string) (string, error) {
		return runTool(ctx, dir, "go", "vet", "./...")
	}}
}

// Staticcheck runs `staticcheck ./...` in each module when the binary is on
// PATH; otherwise the snapshot omits it.
func Staticcheck() Analyzer {
	return Analyzer{Tool: "staticcheck", Run: func(ctx context.Context, dir string) (string, error) {
		return runTool(ctx, dir, "staticcheck", "./...")
	}}
}

// DefaultAnalyzers is the static-analysis set the spoke runner uses when
// static analysis is enabled.
func DefaultAnalyzers() []Analyzer { return []Analyzer{GoVet(), Staticcheck()} }
