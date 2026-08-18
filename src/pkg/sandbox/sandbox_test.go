package sandbox

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestPodmanArgsUseCredentialFreeNetworklessDefaults(t *testing.T) {
	args, err := PodmanArgs(LaunchSpec{Image: "agent:latest", Workspace: "/workspace-src", Command: []string{"echo", "ok"}, NetworkNone: true, Env: map[string]string{"SAFE": "1", "GITHUB_TOKEN": "nope"}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--network=none", "--userns=keep-id", "-v /workspace-src:/workspace:Z", "--workdir /workspace", "agent:latest", "echo ok"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "GITHUB_TOKEN") || strings.Contains(joined, "nope") {
		t.Fatalf("credential env in args: %q", joined)
	}
}

func TestSanitizedEnvAllowlistDropsCredentials(t *testing.T) {
	env := SanitizedEnv([]string{"PATH=/bin", "HOME=/home/dev", "GITHUB_TOKEN=secret", "MY_TOKEN=secret"}, []string{"PATH", "HOME", "GITHUB_TOKEN", "MY_TOKEN"}, map[string]string{"SAFE": "1", "GH_TOKEN": "nope"})
	if !slices.Contains(env, "PATH=/bin") || !slices.Contains(env, "HOME=/home/dev") || !slices.Contains(env, "SAFE=1") {
		t.Fatalf("missing allowed env: %v", env)
	}
	for _, entry := range env {
		if strings.Contains(entry, "secret") || strings.HasPrefix(entry, "GITHUB_TOKEN=") || strings.HasPrefix(entry, "GH_TOKEN=") || strings.HasPrefix(entry, "MY_TOKEN=") {
			t.Fatalf("credential leaked: %v", env)
		}
	}
}

func TestPodmanArgsHonorsRestrictedNetworkMode(t *testing.T) {
	args, err := PodmanArgs(LaunchSpec{Image: "agent:latest", Workspace: "/workspace-src", Command: []string{"true"}, NetworkMode: NetworkModeRestricted, NetworkNone: true})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--network=restricted") {
		t.Fatalf("args %q missing restricted network", joined)
	}
	if strings.Contains(joined, "--network=none") {
		t.Fatalf("NetworkMode should override legacy NetworkNone: %q", joined)
	}
}

// TestPodmanRunSkippedWhenUnavailable asserts the behavior the name promises:
// Run must fail fast with a clear error (never hang or panic) when its podman
// binary can't be found, instead of depending on whichever binary happens to
// be on the host PATH. exec.LookPath("podman") is true on some CI runner
// images and false on others (and on developer machines), so gating this on
// the real Available()/PATH made the test's outcome depend on the
// environment rather than on the code under test — it silently switched from
// exercising the "unavailable" path (skip) to actually shelling out to a real
// podman daemon, which then failed for unrelated reasons (no rootless
// subuid/subgid mapping, network egress restrictions, etc.) on CI runners
// that happen to ship podman.
//
// Using PodmanLauncher.Binary to point at a name that cannot exist on PATH
// makes the "podman unavailable" branch deterministic on every OS/runner,
// while still exercising the exact code path (the exec.LookPath check inside
// Run) that Available() is a thin wrapper around.
func TestPodmanRunSkippedWhenUnavailable(t *testing.T) {
	launcher := PodmanLauncher{Binary: "hive-sandbox-test-nonexistent-podman-binary"}
	_, err := launcher.Run(context.Background(), LaunchSpec{Image: "docker.io/library/alpine:latest", Workspace: t.TempDir(), Command: []string{"sh", "-c", "test -d /workspace"}, NetworkNone: true})
	if err == nil {
		t.Fatal("Run: expected error when podman binary is unavailable, got nil")
	}
	if !strings.Contains(err.Error(), "podman is required but was not found in PATH") {
		t.Fatalf("Run error = %q, want it to report podman missing from PATH", err.Error())
	}
}

// TestPodmanRunIntegration actually shells out to a real podman binary and
// runs a container. It only makes sense — and only reliably succeeds — on a
// host with a working rootless podman installation (subuid/subgid mapping,
// network egress to pull the test image, etc.), which CI's -short unit-test
// run does not guarantee even when a podman binary happens to be on PATH.
// Skipped under -short; run explicitly with `go test -run Integration` (no
// -short) on a machine known to have podman configured.
func TestPodmanRunIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping podman integration test in -short mode")
	}
	if !Available() {
		t.Skip("podman not available")
	}
	// Available() only checks for the podman binary. Rootless podman with
	// --userns=keep-id (this launcher's default) additionally needs the uidmap
	// helpers (newuidmap/newgidmap) to set up the user namespace; without them
	// the run aborts with "command required for rootless mode with multiple IDs:
	// newuidmap ... not found". That is a runner-provisioning gap, not a defect
	// in the launcher, so skip rather than fail when the helpers are absent (the
	// CI runner has podman but not the uidmap package).
	if _, err := exec.LookPath("newuidmap"); err != nil {
		t.Skip("rootless podman prerequisites (newuidmap) not installed")
	}
	res, err := (PodmanLauncher{}).Run(context.Background(), LaunchSpec{Image: "docker.io/library/alpine:latest", Workspace: t.TempDir(), Command: []string{"sh", "-c", "test -d /workspace"}, NetworkNone: true})
	if err != nil {
		t.Fatalf("Run: %v stderr=%s", err, res.Stderr)
	}
}
