package sandbox

import (
	"context"
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

func TestPodmanRunSkippedWhenUnavailable(t *testing.T) {
	if !Available() {
		t.Skip("podman not available")
	}
	res, err := (PodmanLauncher{}).Run(context.Background(), LaunchSpec{Image: "docker.io/library/alpine:latest", Workspace: t.TempDir(), Command: []string{"sh", "-c", "test -d /workspace"}, NetworkNone: true})
	if err != nil {
		t.Fatalf("Run: %v stderr=%s", err, res.Stderr)
	}
}
