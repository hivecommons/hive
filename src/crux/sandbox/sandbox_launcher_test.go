package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakePodman writes an executable shell script to a temp dir and returns its
// path. Pointing PodmanLauncher.Binary at it makes Run's success, failure and
// output-capture paths deterministic on every host — no real podman, no
// container pulls, no rootless uidmap prerequisites.
func fakePodman(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-podman shell script fixture is POSIX-only")
	}
	path := filepath.Join(t.TempDir(), "fake-podman")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPodmanRunSuccessCapturesStdoutStderr(t *testing.T) {
	bin := fakePodman(t, `echo "hello-out"; echo "hello-err" >&2; exit 0`)
	res, err := (PodmanLauncher{Binary: bin}).Run(context.Background(), LaunchSpec{
		Image:     "agent:latest",
		Workspace: t.TempDir(),
		Command:   []string{"true"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Stdout, "hello-out") {
		t.Errorf("Stdout = %q, want it to contain hello-out", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "hello-err") {
		t.Errorf("Stderr = %q, want it to contain hello-err", res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestPodmanRunNonZeroExitReturnsErrorAndExitCode(t *testing.T) {
	bin := fakePodman(t, `echo "boom" >&2; exit 7`)
	res, err := (PodmanLauncher{Binary: bin}).Run(context.Background(), LaunchSpec{
		Image:     "agent:latest",
		Workspace: t.TempDir(),
		Command:   []string{"true"},
	})
	if err == nil {
		t.Fatal("Run: expected error on non-zero exit, got nil")
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "boom") {
		t.Errorf("Stderr = %q, want it to contain boom", res.Stderr)
	}
}

func TestPodmanRunRejectsInvalidSpecBeforeExec(t *testing.T) {
	// A resolvable binary but an invalid spec: PodmanArgs must reject it
	// before anything is executed.
	bin := fakePodman(t, `echo "should never run"; exit 0`)
	_, err := (PodmanLauncher{Binary: bin}).Run(context.Background(), LaunchSpec{
		Workspace: t.TempDir(), // Image missing
	})
	if err == nil || !strings.Contains(err.Error(), "image is required") {
		t.Fatalf("Run error = %v, want image-is-required spec error", err)
	}
}

func TestPodmanRunStripsCredentialEnvFromChild(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "leak-me-not")
	bin := fakePodman(t, `env; exit 0`)
	res, err := (PodmanLauncher{Binary: bin}).Run(context.Background(), LaunchSpec{
		Image:        "agent:latest",
		Workspace:    t.TempDir(),
		Command:      []string{"true"},
		EnvAllowlist: []string{"HOME"},
		Env:          map[string]string{"SAFE_FLAG": "1"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(res.Stdout, "leak-me-not") || strings.Contains(res.Stdout, "GITHUB_TOKEN=") {
		t.Fatalf("credential env leaked into sandbox process env:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "SAFE_FLAG=1") {
		t.Errorf("explicit safe env missing from child env:\n%s", res.Stdout)
	}
}

func TestAvailableMatchesLookPath(t *testing.T) {
	_, err := exec.LookPath(PodmanBinary)
	if got, want := Available(), err == nil; got != want {
		t.Errorf("Available() = %v, want %v (LookPath err = %v)", got, want, err)
	}
}

func TestPodmanArgsRequiresWorkspace(t *testing.T) {
	if _, err := PodmanArgs(LaunchSpec{Image: "agent:latest"}); err == nil || !strings.Contains(err.Error(), "workspace is required") {
		t.Fatalf("PodmanArgs error = %v, want workspace-is-required", err)
	}
}

func TestPodmanArgsNameReadOnlyAndCustomMount(t *testing.T) {
	args, err := PodmanArgs(LaunchSpec{
		Image:          "agent:latest",
		Name:           "sbx-1",
		Workspace:      "/src",
		WorkspaceMount: "/mnt/work",
		WorkDir:        "/mnt/work/sub",
		ReadOnly:       true,
		Command:        []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--name sbx-1", "--read-only", "-v /src:/mnt/work:Z", "--workdir /mnt/work/sub"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
}
