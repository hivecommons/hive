package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// automaxprocsBannerPattern matches the line go.uber.org/automaxprocs's
// default logger writes to stderr at init when imported blank, e.g.
// "2026/08/14 08:03:25 maxprocs: Leaving GOMAXPROCS=12: CPU quota undefined".
var automaxprocsBannerPattern = regexp.MustCompile(`maxprocs:`)

// TestNoBlankAutomaxprocsImport is the source-asserting half of the
// regression: it fails if anyone reinstates
// `_ "go.uber.org/automaxprocs"` in this file. That import's init writes an
// unconditional banner to the default logger (stderr) with no way to
// silence it, which is exactly the defect TestChildProcessOutputHasNoAutomaxprocsBanner
// below exercises at runtime. Asserting the source shape catches the
// regression even before a test binary is built, and pins down *why* the
// runtime test would start failing again.
func TestNoBlankAutomaxprocsImport(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	if strings.Contains(string(src), `_ "go.uber.org/automaxprocs"`) {
		t.Fatal(`main.go must not blank-import "go.uber.org/automaxprocs": its init ` +
			`writes an unconditional banner to stderr that corrupts any caller ` +
			`merging this process's stdout and stderr (e.g. the pushbroker git ` +
			`shim's CombinedOutput()). Use "go.uber.org/automaxprocs/maxprocs" ` +
			`with an explicit maxprocs.Set(maxprocs.Logger(...noop...)) call instead.`)
	}
}

// TestChildProcessOutputHasNoAutomaxprocsBanner is the runtime half of the
// regression. `hive` re-execs itself as a Git transport shim, and
// pkg/pushbroker's ExecRunner captures a child's stdout AND stderr into one
// buffer via cmd.CombinedOutput() (see pkg/pushbroker/pushbroker.go). This
// test builds the real hive binary and inspects exactly that combined
// stream from a real child process spawn — the same code path a blank
// automaxprocs import would corrupt with a stray
// "maxprocs: Leaving GOMAXPROCS=... " line ahead of (or interleaved with)
// the caller's expected output, which is what broke branch parsing (see
// PR #3820 on branch dd: a `git symbolic-ref` answer got the banner
// prepended and the resulting "branch name" made `git reset --hard` fail
// with "fatal: invalid object name").
//
// `hive --version` is used as the child invocation because it returns
// before any config/flag parsing, but init() — where automaxprocs runs —
// always executes first regardless of arguments, so it faithfully exercises
// the banner-at-init behaviour without needing a hive.yaml or network access.
func TestChildProcessOutputHasNoAutomaxprocsBanner(t *testing.T) {
	bin := buildHiveBinaryForTest(t)

	cmd := exec.Command(bin, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hive --version failed: %v\noutput: %s", err, out)
	}

	if automaxprocsBannerPattern.Match(out) {
		t.Fatalf("child hive process's combined stdout+stderr contains an "+
			"automaxprocs banner line, which corrupts any caller parsing this "+
			"output as if it were the expected answer (e.g. a git shim reading "+
			"a ref name):\n%s", out)
	}

	if !strings.Contains(string(out), "hive 3.0.0") {
		t.Fatalf("expected combined output to contain the --version banner, got: %s", out)
	}
}

// buildHiveBinaryForTest compiles the cmd/hive package under test into a
// temp binary and returns its path. Building (rather than `go run`) ensures
// the test observes the real init() ordering and stderr behaviour of the
// actual shipped binary, matching how the pushbroker git shim invokes a
// real child hive process.
func buildHiveBinaryForTest(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "hive-under-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building hive test binary: %v\n%s", err, out)
	}
	return bin
}
