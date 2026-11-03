package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/cli"
	"github.com/hivecommons/hive/crux/defs"
)

// The module binary's loader must satisfy the CLI's seam without the CLI
// importing it.
var _ cli.DefinitionLoader = defs.Loader{}

func testCtx() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func TestRunPrintsUsageWithoutACommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--workdir", t.TempDir()}, strings.NewReader(""), &stdout, &stderr, testCtx)
	if code != cli.ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitUsage)
	}
	if !strings.Contains(stderr.String(), "usage: crux") {
		t.Fatalf("stderr = %q, want usage", stderr.String())
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"--workdir", t.TempDir(), "frobnicate"}, strings.NewReader(""), &bytes.Buffer{}, &stderr, testCtx)
	if code != cli.ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, cli.ExitUsage)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunReportsVersion(t *testing.T) {
	var stdout bytes.Buffer
	code := run([]string{"--version"}, strings.NewReader(""), &stdout, &bytes.Buffer{}, testCtx)
	if code != cli.ExitOK || stdout.String() != "crux "+cli.DefaultVersion+"\n" {
		t.Fatalf("code %d, stdout %q", code, stdout.String())
	}
}

func TestRunPSFromDefsDirectory(t *testing.T) {
	dir := t.TempDir()
	defsDir := filepath.Join(dir, "defs")
	writeFile(t, filepath.Join(defsDir, "fixer.yaml"), "backend: codex\nmodel: gpt-5\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--workdir", filepath.Join(dir, "work"), "--local", "--defs", defsDir, "ps"}, strings.NewReader(""), &stdout, &stderr, testCtx)
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "fixer") {
		t.Fatalf("stdout = %q, want the fixer row", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	t.Setenv(cli.DefsEnvVar, filepath.Join(defsDir, "fixer.yaml"))
	code = run([]string{"--workdir", filepath.Join(dir, "work"), "--local", "ps"}, strings.NewReader(""), &stdout, &stderr, testCtx)
	if code != cli.ExitOK || !strings.Contains(stdout.String(), "fixer") {
		t.Fatalf("%s: code %d, stdout %q, stderr %q", cli.DefsEnvVar, code, stdout.String(), stderr.String())
	}
}

func TestRunReportsLoaderErrors(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"--workdir", t.TempDir(), "--local", "--defs", filepath.Join(t.TempDir(), "missing"), "ps"}, strings.NewReader(""), &bytes.Buffer{}, &stderr, testCtx)
	if code != cli.ExitError || !strings.Contains(stderr.String(), "reading definitions") {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
