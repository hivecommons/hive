package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// stubPodman writes an executable stand-in for the podman binary that records
// the argv and environment it was handed, then exits with the requested code.
// Pointing PodmanLauncher.Binary at it exercises the whole Run body — argv
// construction, env sanitization, stdout/stderr capture, exit-code reporting —
// without a container runtime, so the assertions hold on every runner rather
// than only on the ones that ship a working rootless podman.
func stubPodman(t *testing.T, body string) (bin, argvLog string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "stub-podman")
	argvLog = filepath.Join(dir, "argv")
	script := "#!/bin/sh\nfor a in \"$@\"; do echo \"$a\" >> " + argvLog + "; done\n" + body
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub podman: %v", err)
	}
	return bin, argvLog
}

func TestPodmanArgsRejectsIncompleteSpecs(t *testing.T) {
	cases := []struct {
		name    string
		spec    LaunchSpec
		wantErr string
	}{
		{"no image", LaunchSpec{Workspace: "/src"}, "image is required"},
		{"blank image", LaunchSpec{Image: "   ", Workspace: "/src"}, "image is required"},
		{"no workspace", LaunchSpec{Image: "agent:latest"}, "workspace is required"},
		{"blank workspace", LaunchSpec{Image: "agent:latest", Workspace: "  "}, "workspace is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := PodmanArgs(tc.spec)
			if err == nil {
				t.Fatalf("PodmanArgs(%+v) = %v, want error", tc.spec, args)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// The named/read-only/custom-mount options are what callers reach for when the
// sandbox has to be addressable or the workspace lives somewhere other than
// /workspace; each has to survive into argv or the container silently runs with
// the defaults instead.
func TestPodmanArgsCarriesNameReadOnlyAndCustomMount(t *testing.T) {
	args, err := PodmanArgs(LaunchSpec{
		Image:          "agent:latest",
		Name:           "hive-run-7",
		Workspace:      "/srv/work/./repo/",
		WorkspaceMount: "/mnt/repo",
		WorkDir:        "/mnt/repo/sub",
		ReadOnly:       true,
		Command:        []string{"make", "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--name hive-run-7",
		"--read-only",
		"-v /srv/work/repo:/mnt/repo:Z", // filepath.Clean normalizes the source
		"--workdir /mnt/repo/sub",
		"make test",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
	// No network flag at all when neither NetworkMode nor NetworkNone is set:
	// the caller gets podman's default rather than a silently-injected one.
	if strings.Contains(joined, "--network") {
		t.Fatalf("args %q should not force a network mode", joined)
	}
}

// WorkDir defaults to the mount point, so a spec that only overrides the mount
// still lands the process in the workspace instead of the image's WORKDIR.
func TestPodmanArgsWorkDirDefaultsToMount(t *testing.T) {
	args, err := PodmanArgs(LaunchSpec{Image: "agent:latest", Workspace: "/src", WorkspaceMount: "/mnt/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, "--workdir /mnt/repo") {
		t.Fatalf("args %q should default --workdir to the mount", joined)
	}
}

func TestRunCapturesStreamsAndZeroExit(t *testing.T) {
	bin, argvLog := stubPodman(t, "echo out\necho err 1>&2\nexit 0\n")
	res, err := PodmanLauncher{Binary: bin}.Run(context.Background(), LaunchSpec{
		Image:        "agent:latest",
		Workspace:    t.TempDir(),
		Command:      []string{"true"},
		NetworkNone:  true,
		EnvAllowlist: []string{"PATH"},
		Env:          map[string]string{"SAFE": "1", "GITHUB_TOKEN": "leak-me"},
	})
	if err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err" {
		t.Fatalf("Run streams = stdout %q stderr %q, want \"out\"/\"err\"", res.Stdout, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	if !strings.Contains(string(argv), "--network=none") {
		t.Fatalf("argv %q missing --network=none", argv)
	}
	if strings.Contains(string(argv), "leak-me") {
		t.Fatalf("credential env reached podman argv: %q", argv)
	}
}

// A container that exits non-zero is a normal outcome the caller has to be able
// to diagnose: Run must return the exit code and the captured stderr alongside
// the error, not swallow them.
func TestRunReportsNonZeroExitWithOutput(t *testing.T) {
	bin, _ := stubPodman(t, "echo boom 1>&2\nexit 7\n")
	res, err := PodmanLauncher{Binary: bin}.Run(context.Background(), LaunchSpec{
		Image:        "agent:latest",
		Workspace:    t.TempDir(),
		Command:      []string{"false"},
		EnvAllowlist: []string{"PATH"},
	})
	if err == nil {
		t.Fatal("Run: expected error for non-zero container exit")
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
	if strings.TrimSpace(res.Stderr) != "boom" {
		t.Fatalf("Stderr = %q, want \"boom\"", res.Stderr)
	}
}

// An invalid spec must be rejected before anything is executed — otherwise the
// error surfaces as an opaque podman usage failure.
func TestRunRejectsInvalidSpecBeforeExec(t *testing.T) {
	bin, argvLog := stubPodman(t, "exit 0\n")
	_, err := PodmanLauncher{Binary: bin}.Run(context.Background(), LaunchSpec{Workspace: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "image is required") {
		t.Fatalf("Run error = %v, want the image-required validation error", err)
	}
	if _, statErr := os.Stat(argvLog); statErr == nil {
		t.Fatal("Run executed podman despite an invalid spec")
	}
}

// Available() is the caller-facing form of the same PATH probe Run makes, so it
// must not disagree with it — a true from Available followed by a
// "not found in PATH" from Run is the contradiction worth pinning.
func TestAvailableMatchesPathLookup(t *testing.T) {
	_, lookErr := exec.LookPath(PodmanBinary)
	if got, want := Available(), lookErr == nil; got != want {
		t.Fatalf("Available() = %v, want %v (LookPath err %v)", got, want, lookErr)
	}
}

// An allowlist is not an escape hatch: naming a credential variable in it must
// not pass the value through, and malformed entries in the inherited
// environment must not be forwarded as-is.
func TestSanitizedEnvIgnoresAllowlistedCredentialsAndMalformedEntries(t *testing.T) {
	env := SanitizedEnv(
		[]string{"NO_EQUALS_SIGN", "PATH=/bin", "APP_SECRET=hunter2", "NOT_ALLOWED=x"},
		[]string{"PATH", "APP_SECRET", "NO_EQUALS_SIGN"},
		nil,
	)
	if !slices.Contains(env, "PATH=/bin") {
		t.Fatalf("allowed entry dropped: %v", env)
	}
	if len(env) != 1 {
		t.Fatalf("env = %v, want only PATH=/bin", env)
	}
}
