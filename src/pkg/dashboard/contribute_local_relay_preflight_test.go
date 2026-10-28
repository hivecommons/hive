package dashboard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
)

// hivecommons/hive#14832 and #14833: local-mode contributor preflight and stop.
//
// #14832: on Ubuntu, Claude's native sandbox starts but every command it runs
// fails (`apply-seccomp: write /proc/self/setgroups ... Permission denied`),
// because /etc/apparmor.d/bwrap-userns-restrict denies the nested user
// namespace write — even with kernel.apparmor_restrict_unprivileged_userns=0.
// Local mode must run a sandboxed no-op before it advertises readiness.
//
// #14833: `just contribute-stop` only stopped containers, so a local relay kept
// running, and the next local start ran a second relay under the same
// contributor ID. contributor-relay.pid is the only record of a local relay.
//
// The logic lives in bin/contributor-local-relay.sh, sourced by both recipes;
// these tests drive it for real with fake bwrap/unshare/tmux on PATH.

const localRelayLib = "../../../bin/contributor-local-relay.sh"

// runLocalRelayLib sources the library and runs script with binDir prepended
// to PATH. It returns combined output and the exit code.
func runLocalRelayLib(t *testing.T, binDir, script string, extraEnv ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", "-c", "set -euo pipefail\nsource "+localRelayLib+"\n"+script)
	var env []string
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "PATH=") || strings.HasPrefix(entry, "HIVE_RELAY_PID_FILE=") || strings.HasPrefix(entry, "HIVE_CONTRIBUTOR_CONFIG_DIR=") {
			continue
		}
		env = append(env, entry)
	}
	path := os.Getenv("PATH")
	if binDir != "" {
		path = binDir + string(os.PathListSeparator) + path
	}
	cmd.Env = append(append(env, "PATH="+path), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return string(out), exitErr.ExitCode()
	}
	t.Fatalf("run library: %v\n%s", err, out)
	return "", -1
}

func writeFakeBin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/usr/bin/env bash\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLocalSandboxPreflightFailsOnAppArmorRestrictedHost(t *testing.T) {
	bin := t.TempDir()
	argsLog := filepath.Join(bin, "bwrap.args")
	writeFakeBin(t, bin, "unshare", "exit 0")
	writeFakeBin(t, bin, "bwrap", `printf '%s\n' "$@" > `+fmt.Sprintf("%q", argsLog)+`
echo "apply-seccomp: write /proc/self/setgroups (nested userns is capability-restricted; caller must provide CAP_SYS_ADMIN): Permission denied" >&2
exit 1`)

	out, code := runLocalRelayLib(t, bin, `hive_local_sandbox_preflight claude`)
	if code != 1 {
		t.Fatalf("preflight exit = %d, want 1; output:\n%s", code, out)
	}
	for _, want := range []string{
		"cannot run commands on this host",
		"apply-seccomp: write /proc/self/setgroups",
		"kernel.apparmor_restrict_unprivileged_userns",
		"bwrap-userns-restrict",
		"just contribute-hive claude",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preflight message does not mention %q:\n%s", want, out)
		}
	}

	raw, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("bwrap was never invoked: %v", err)
	}
	args := strings.Fields(string(raw))
	joined := strings.Join(args, " ")
	// A real sandboxed command, in a user namespace, with the nested user
	// namespace + setgroups write that apply-seccomp performs.
	for _, want := range []string{"--unshare-user", "-- unshare --user --map-root-user true"} {
		if !strings.Contains(joined, want) {
			t.Errorf("bwrap probe argv %q does not contain %q", joined, want)
		}
	}
}

func TestLocalSandboxPreflightPassesWhenSandboxedTrueRuns(t *testing.T) {
	bin := t.TempDir()
	writeFakeBin(t, bin, "unshare", "exit 0")
	writeFakeBin(t, bin, "bwrap", "exit 0")
	out, code := runLocalRelayLib(t, bin, `hive_local_sandbox_preflight claude`)
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("preflight exit = %d, output %q; want a silent pass", code, out)
	}
}

// startFakeRelay starts a process whose command line names contributor-relay,
// reaped in the background so a stopped one does not linger as a zombie.
func startFakeRelay(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("bash", "-c", "exec -a contributor-relay.js sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
	})
	// Wait for exec -a to take effect before anything inspects the argv.
	testutil.Eventually(t, 5*time.Second, func() bool {
		pid := strconv.Itoa(cmd.Process.Pid)
		out, err := os.ReadFile("/proc/" + pid + "/cmdline")
		if err != nil {
			out, _ = exec.Command("ps", "-o", "args=", "-p", pid).Output()
		}
		return strings.Contains(string(out), "contributor-relay")
	}, "fake relay never showed contributor-relay in its argv")
	return cmd, done
}

func writeRelayPidRecord(t *testing.T, pid int, extra string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "contributor-relay.pid")
	body := fmt.Sprintf("{\n  \"pid\": %d,\n  \"started_at\": \"2026-10-28T09:54:38.000Z\",\n  \"session\": \"0b7c\",\n%s  \"env_file\": \"/home/u/.config/hive/contributor.env\"\n}\n", pid, extra)
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestLocalStartRefusesWhileARecordedRelayIsAlive(t *testing.T) {
	relay, _ := startFakeRelay(t)
	pid := relay.Process.Pid
	file := writeRelayPidRecord(t, pid, "  \"agent_session\": \"hive-claude-9dc7\",\n")

	out, code := runLocalRelayLib(t, "", `hive_refuse_if_local_relay_running`, "HIVE_RELAY_PID_FILE="+file)
	if code != 1 {
		t.Fatalf("refusal exit = %d, want 1; output:\n%s", code, out)
	}
	want := fmt.Sprintf("A local relay is already running (PID %d, tmux session hive-claude-9dc7). Run `just contribute-stop` first.", pid)
	if strings.TrimSpace(out) != want {
		t.Fatalf("refusal message = %q, want %q", strings.TrimSpace(out), want)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("a refused start must leave the record alone: %v", err)
	}
}

func TestLocalStartProceedsWithoutALiveLocalRelay(t *testing.T) {
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	notRelay := exec.Command("sleep", "60")
	if err := notRelay.Start(); err != nil {
		t.Fatal(err)
	}
	notRelayDone := make(chan struct{})
	go func() { _ = notRelay.Wait(); close(notRelayDone) }()
	t.Cleanup(func() { _ = notRelay.Process.Kill(); <-notRelayDone })
	live, _ := startFakeRelay(t)

	cases := map[string]string{
		"missing record":  filepath.Join(t.TempDir(), "contributor-relay.pid"),
		"stale pid":       writeRelayPidRecord(t, dead.Process.Pid, ""),
		"recycled pid":    writeRelayPidRecord(t, notRelay.Process.Pid, ""),
		"container relay": writeRelayPidRecord(t, live.Process.Pid, "  \"container_runtime\": \"docker\",\n  \"container_name\": \"hive-contributor-ab12\",\n"),
	}
	for name, file := range cases {
		out, code := runLocalRelayLib(t, "", `hive_refuse_if_local_relay_running`, "HIVE_RELAY_PID_FILE="+file)
		if code != 0 || strings.TrimSpace(out) != "" {
			t.Errorf("%s: exit = %d, output %q; want a silent pass", name, code, out)
		}
	}
}

func TestContributeStopStopsTheRecordedLocalRelayAndItsTmuxSession(t *testing.T) {
	relay, done := startFakeRelay(t)
	pid := relay.Process.Pid
	file := writeRelayPidRecord(t, pid, "  \"agent_session\": \"hive-claude-9dc7\",\n")
	bin := t.TempDir()
	tmuxLog := filepath.Join(bin, "tmux.args")
	writeFakeBin(t, bin, "tmux", `printf '%s\n' "$*" >> `+fmt.Sprintf("%q", tmuxLog))

	out, code := runLocalRelayLib(t, bin, `hive_stop_local_relay`, "HIVE_RELAY_PID_FILE="+file)
	if code != 0 {
		t.Fatalf("stop exit = %d, want 0; output:\n%s", code, out)
	}
	want := fmt.Sprintf("Stopped local relay (PID %d, tmux session hive-claude-9dc7).", pid)
	if strings.TrimSpace(out) != want {
		t.Fatalf("stop message = %q, want %q", strings.TrimSpace(out), want)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the recorded relay is still running after contribute-stop")
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("pid record still present after stop: %v", err)
	}
	raw, _ := os.ReadFile(tmuxLog)
	if !strings.Contains(string(raw), "kill-session -t hive-claude-9dc7") {
		t.Fatalf("tmux session was not killed; tmux calls: %q", raw)
	}

	// Nothing recorded: reports not-running and touches nothing.
	out, code = runLocalRelayLib(t, bin, `hive_stop_local_relay`, "HIVE_RELAY_PID_FILE="+file)
	if code != 1 || strings.TrimSpace(out) != "" {
		t.Fatalf("stop with no relay: exit = %d, output %q; want 1 and silence", code, out)
	}
}

// Without procps, the procfs fallback must still identify a relay, refuse a
// duplicate start, and stop it, while leaving unrelated live processes alone.
func TestLocalRelayWithoutWorkingPS(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("requires Linux procfs")
	}
	for name, ps := range map[string]string{
		"missing": "ps() { return 127; }\n",
		"failed":  "ps() { echo truncated; return 1; }\n",
		"empty":   "ps() { return 0; }\n",
	} {
		t.Run(name, func(t *testing.T) {
			relay, done := startFakeRelay(t)
			pid := relay.Process.Pid
			file := writeRelayPidRecord(t, pid, "  \"agent_session\": \"hive-test\",\n")
			out, code := runLocalRelayLib(t, "", ps+`hive_live_local_relay`, "HIVE_RELAY_PID_FILE="+file)
			if want := fmt.Sprintf("%d hive-test", pid); code != 0 || strings.TrimSpace(out) != want {
				t.Fatalf("liveness exit = %d, output %q; want %q", code, out, want)
			}
			out, code = runLocalRelayLib(t, "", ps+`hive_refuse_if_local_relay_running`, "HIVE_RELAY_PID_FILE="+file)
			if code != 1 || !strings.Contains(out, "A local relay is already running") {
				t.Fatalf("refusal exit = %d, output %q", code, out)
			}

			otherProcess := exec.Command("sleep", "60")
			if err := otherProcess.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = otherProcess.Process.Kill(); _ = otherProcess.Wait() })
			other := writeRelayPidRecord(t, otherProcess.Process.Pid, "")
			out, code = runLocalRelayLib(t, "", ps+`hive_stop_local_relay`, "HIVE_RELAY_PID_FILE="+other)
			if code != 1 || strings.TrimSpace(out) != "" {
				t.Fatalf("unrelated process stop: exit = %d, output %q", code, out)
			}
			if _, err := os.Stat(other); err != nil {
				t.Fatalf("unrelated process record removed: %v", err)
			}

			bin := t.TempDir()
			writeFakeBin(t, bin, "tmux", "exit 0")
			out, code = runLocalRelayLib(t, bin, ps+`hive_stop_local_relay`, "HIVE_RELAY_PID_FILE="+file)
			if code != 0 || !strings.Contains(out, "Stopped local relay") {
				t.Fatalf("stop exit = %d, output %q", code, out)
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("relay still running after stop without working ps")
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatalf("pid record still present after stop: %v", err)
			}
		})
	}
}

// The recipes must actually call the library, in the right place.
func TestLocalRelayRecipesUseThePreflightAndStopHelpers(t *testing.T) {
	block := contributeHiveLocalBranch(t)
	if !strings.Contains(block, "bin/contributor-local-relay.sh") || !strings.Contains(block, "hive_refuse_if_local_relay_running") {
		t.Error("contribute-hive local mode does not refuse to start beside a live local relay")
	}

	src := justfileSource(t)
	preflight := strings.Index(src, `hive_local_sandbox_preflight "$BACKEND"`)
	relayStart := strings.Index(src, `node "$RELAY" &`)
	ready := strings.Index(src, "Contributor running in local mode")
	if preflight < 0 || relayStart < 0 || ready < 0 || preflight > relayStart || preflight > ready {
		t.Errorf("the sandbox preflight must run before the relay starts and readiness is printed (preflight=%d relay=%d ready=%d)", preflight, relayStart, ready)
	}

	start := strings.Index(src, "\ncontribute-stop:")
	if start < 0 {
		t.Fatal("contribute-stop recipe not found in the Justfile")
	}
	recipe := src[start+1:]
	if end := strings.Index(recipe, "\n\n"); end >= 0 {
		recipe = recipe[:end]
	}
	for _, want := range []string{"hive-contributor-", "bin/contributor-local-relay.sh", "hive_stop_local_relay"} {
		if !strings.Contains(recipe, want) {
			t.Errorf("contribute-stop does not contain %q", want)
		}
	}
}
