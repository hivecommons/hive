package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/cline"
)

func TestClineProviderArgsRejectsClosedCapability(t *testing.T) {
	spec, _ := clineLaunchFixture(t)
	if err := spec.ClineProvider.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := PodmanArgs(spec); err == nil || !strings.Contains(err.Error(), "closed Cline provider capability") {
		t.Fatalf("closed capability produced argv: %v", err)
	}
}

func TestClineProviderCommandThinkingAndRequiredFlags(t *testing.T) {
	spec, _ := clineLaunchFixture(t)
	canonical := append([]string(nil), spec.Command...)
	for _, level := range []string{"none", "low", "medium", "high", "xhigh"} {
		spec.Command = append(append([]string(nil), canonical...), "--thinking", level)
		if _, err := PodmanArgs(spec); err != nil {
			t.Fatalf("--thinking %s rejected: %v", level, err)
		}
	}
	// Every flag present is individually valid; only a required one is missing.
	spec.Command = []string{"crux", cline.Subcommand, "--provider-file", cline.ProviderMountPath,
		"--state-dir", cline.ProviderStatePath, "--model", "openrouter/test", "--thinking", "low"}
	if _, err := PodmanArgs(spec); err == nil {
		t.Fatal("command without --prompt-file accepted")
	}
}

func TestRunDefaultsToPodmanBinaryFromPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := PodmanLauncher{}.Run(context.Background(), LaunchSpec{Image: "fixture-image", Workspace: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "podman is required but was not found in PATH") {
		t.Fatalf("expected missing-podman error, got %v", err)
	}
}

func TestRunReportsClineProviderCloseFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permissions; stage removal cannot be made to fail")
	}
	spec, file := clineLaunchFixture(t)
	binary := filepath.Join(t.TempDir(), "podman-fixture")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' '"+fixtureCredential+"'\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Revoking write permission on the staging root makes Close()'s RemoveAll
	// fail after the container ran, so Run must surface it in its error.
	parent := filepath.Dir(filepath.Dir(file))
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	res, err := PodmanLauncher{Binary: binary}.Run(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "cannot remove private Cline provider stage") {
		t.Fatalf("stage removal failure not reported: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "[REDACTED]") || strings.Contains(res.Stdout, fixtureCredential) {
		t.Fatalf("container result lost or unredacted: exit=%d stdout=%q", res.ExitCode, res.Stdout)
	}
	if _, statErr := os.Stat(filepath.Dir(file)); statErr != nil {
		t.Fatalf("stage directory should survive the failed removal: %v", statErr)
	}
}
