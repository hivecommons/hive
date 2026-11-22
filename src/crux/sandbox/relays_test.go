package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeRelaySource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range RelayNames {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/bash\necho relay\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRelayNamesAreSandboxSafeOnly(t *testing.T) {
	// Only relays that write the sandbox spool (#14986, #18572, #18573); a
	// relay that needs the pod-local drop boxes must not be added here.
	if strings.Join(RelayNames, ",") != "hive-review,hive-open-issue,hive-topic" {
		t.Errorf("RelayNames = %v, want hive-review, hive-open-issue and hive-topic", RelayNames)
	}
}

func TestRelayCommandWrapsWithPreflightAndPath(t *testing.T) {
	cmd := []string{"claude", "-p", "@.hive/kick-prompt.txt"}
	got := RelayCommand(cmd)
	if got[0] != "/bin/sh" || got[1] != "-c" || got[3] != relayPreflightName || got[4] != RelayMountPath {
		t.Fatalf("RelayCommand = %q", got)
	}
	if strings.Join(got[5:], " ") != strings.Join(cmd, " ") {
		t.Errorf("the kick command must follow the wrapper unchanged: %q", got[5:])
	}
	script := got[2]
	for _, want := range []string{"python3", "bash", `PATH="$relay_dir:$PATH"`, `exec "$@"`, "exit 78", "hive-review"} {
		if !strings.Contains(script, want) {
			t.Errorf("preflight script missing %q:\n%s", want, script)
		}
	}
	if RelayCommand(nil) != nil {
		t.Error("an empty command (image entrypoint) cannot be wrapped and must stay empty")
	}
}

// TestRelayPreflightRunsAndFails runs the real wrapper under /bin/sh.
func TestRelayPreflightRunsAndFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell only")
	}
	relayDir := writeRelaySource(t)
	bin := t.TempDir()
	// Only the tools the test needs: sh's builtins plus fake bash/python3.
	for _, tool := range RelayRequirements {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(path string, command ...string) (string, int) {
		args := RelayCommand(command)
		args[4] = relayDir
		c := exec.Command(args[0], args[1:]...)
		c.Env = []string{"PATH=" + path}
		var stdout, stderr strings.Builder
		c.Stdout, c.Stderr = &stdout, &stderr
		err := c.Run()
		out := stdout.String()
		if code := RelayPreflightExitCode; errors.As(err, new(*exec.ExitError)) && err.(*exec.ExitError).ExitCode() == code {
			out += stderr.String()
		}
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return out, code
	}
	out, code := run(bin, "/bin/sh", "-c", `printf '%s' "$PATH"`)
	if code != 0 || !strings.HasPrefix(out, relayDir+":") {
		t.Errorf("with bash and python3 present: exit %d, PATH %q, want the relay dir first", code, out)
	}
	if err := os.Remove(filepath.Join(bin, "python3")); err != nil {
		t.Fatal(err)
	}
	out, code = run(bin, "/bin/sh", "-c", "echo should-not-run")
	if code != RelayPreflightExitCode || !strings.Contains(out, "no python3 on PATH") || strings.Contains(out, "should-not-run") {
		t.Errorf("without python3: exit %d, output %q, want exit %d and a clear message", code, out, RelayPreflightExitCode)
	}
}

func TestPodmanArgsMountsRelaysReadOnly(t *testing.T) {
	args, err := PodmanArgs(LaunchSpec{Image: "img", Workspace: "/w", Command: []string{"agent"}, RelayHostDir: "/tmp/hive-relays-1/"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "-v\x00/tmp/hive-relays-1:"+RelayMountPath+":ro,Z") {
		t.Errorf("args %q missing the read-only relay mount", args)
	}
	tail := args[len(args)-7:]
	if tail[0] != "img" || tail[1] != "/bin/sh" || tail[len(tail)-1] != "agent" {
		t.Errorf("image and wrapped command = %q", tail)
	}
	plain, _ := PodmanArgs(LaunchSpec{Image: "img", Workspace: "/w", Command: []string{"agent"}})
	if strings.Contains(strings.Join(plain, " "), RelayMountPath) || plain[len(plain)-1] != "agent" || plain[len(plain)-2] != "img" {
		t.Errorf("without a relay dir nothing changes: %q", plain)
	}
}

func TestStageRelaysCopiesFromEnvDir(t *testing.T) {
	t.Setenv(RelaySourceDirEnv, writeRelaySource(t))
	dir, cleanup, err := StageRelays()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"hive-review", "hive-open-issue", "hive-topic"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != relayFileMode {
			t.Fatalf("staged relay %s = %v, %v, want mode %o", name, info, err, relayFileMode)
		}
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s behind: %v", dir, err)
	}
}

func TestStageRelaysUsesPathLookup(t *testing.T) {
	t.Setenv(RelaySourceDirEnv, "")
	src := writeRelaySource(t)
	orig := relayLookPath
	t.Cleanup(func() { relayLookPath = orig })
	relayLookPath = func(name string) (string, error) { return filepath.Join(src, name), nil }
	_, cleanup, err := StageRelays()
	if err != nil {
		t.Fatal(err)
	}
	cleanup()

	relayLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if _, _, err := StageRelays(); err == nil || !strings.Contains(err.Error(), "hive-review") || !strings.Contains(err.Error(), RelaySourceDirEnv) {
		t.Errorf("StageRelays = %v, want a clear missing-relay error", err)
	}
}

func TestStageRelaysFailsWithoutATempDir(t *testing.T) {
	t.Setenv(RelaySourceDirEnv, writeRelaySource(t))
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	if _, cleanup, err := StageRelays(); err == nil || !strings.Contains(err.Error(), "staging relays") {
		t.Errorf("StageRelays = %v, want a staging error", err)
	} else {
		cleanup()
	}
}

func TestStageRelaysRejectsNonRegularSource(t *testing.T) {
	src := t.TempDir()
	if err := os.Mkdir(filepath.Join(src, "hive-review"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(RelaySourceDirEnv, src)
	if _, _, err := StageRelays(); err == nil || !strings.Contains(err.Error(), "hive-review") {
		t.Errorf("StageRelays = %v, want an error naming the relay", err)
	}
	t.Setenv(RelaySourceDirEnv, filepath.Join(src, "missing"))
	if _, _, err := StageRelays(); err == nil {
		t.Error("a missing relay dir must fail staging")
	}
}

func TestPodmanRunStagesAndMountsRelays(t *testing.T) {
	t.Setenv(RelaySourceDirEnv, writeRelaySource(t))
	argsFile := filepath.Join(t.TempDir(), "args")
	bin := fakePodman(t, `for a in "$@"; do printf '%s\n' "$a"; done > `+argsFile)
	if _, err := (PodmanLauncher{Binary: bin}).Run(context.Background(), LaunchSpec{
		Image: "agent:latest", Workspace: t.TempDir(), Command: []string{"true"}, Relays: true,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	var mount string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasSuffix(line, ":"+RelayMountPath+":ro,Z") {
			mount = strings.TrimSuffix(line, ":"+RelayMountPath+":ro,Z")
		}
	}
	if mount == "" {
		t.Fatalf("podman args carry no relay mount:\n%s", raw)
	}
	// Every relay is on the sandbox PATH: the wrapper prepends the mount,
	// and the preflight names each one it supplies.
	for _, want := range []string{RelayMountPath, `PATH="$relay_dir:$PATH"`, "hive-review, hive-open-issue, hive-topic"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("podman args missing %q:\n%s", want, raw)
		}
	}
	if _, err := os.Stat(mount); !os.IsNotExist(err) {
		t.Errorf("the staged relay dir %s must be removed after the run: %v", mount, err)
	}
}

func TestPodmanRunFailsWhenRelaysCannotBeStaged(t *testing.T) {
	t.Setenv(RelaySourceDirEnv, filepath.Join(t.TempDir(), "missing"))
	bin := fakePodman(t, `exit 0`)
	_, err := (PodmanLauncher{Binary: bin}).Run(context.Background(), LaunchSpec{
		Image: "agent:latest", Workspace: t.TempDir(), Command: []string{"true"}, Relays: true,
	})
	if err == nil || !strings.Contains(err.Error(), "hive-review") {
		t.Fatalf("Run = %v, want the missing-relay error", err)
	}
}
