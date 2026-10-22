package agent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Test-process leak teardown and guard.
//
// The suite launches real agents into a per-run tmux server (socket
// `ht<pid>`, bound under TestMain's private TMUX_TMPDIR). Most tests never
// kill the session they launched, which is fine while the server is shared —
// but nothing ever killed the SERVER either, and TestMain then removed the
// socket directory. That left an unreachable tmux server (plus its `-zsh`
// panes with deleted cwds and the `sh -c … pluk watch …` pipe-pane children)
// orphaned after every `go test ./pkg/agent/...`: hundreds of them, ~1 GB RSS
// and a zsh spinning at 98% CPU on a deleted cwd on one developer laptop.
//
// Three layers fix that:
//
//  1. killTestTmuxServers runs in TestMain after m.Run(): kill-server on every
//     socket under the run's TMUX_TMPDIR, then SIGKILL the server's process
//     group if it survives (pipe-pane children share the server's pgid).
//  2. startTmuxReaper spawns a detached shell that waits for the test binary
//     to exit and repeats (1). It covers the paths TestMain never reaches: a
//     test panic, `go test -timeout` SIGQUIT, or an os.Exit mid-run.
//  3. findLeakedTestProcesses scans `ps` after (1) and fails the run if any
//     server or pluk watcher from THIS run is still alive.

// tmuxServerPID asks the server behind socket path sock for its pid.
func tmuxServerPID(sock string) (int, bool) {
	out, err := exec.Command("tmux", "-S", sock, "display-message", "-p", "#{pid}").Output()
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// waitProcessGone polls processAlive (reaper_test.go) until pid is gone or
// the timeout elapses.
func waitProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for processAlive(pid) {
		select {
		case <-deadline.C:
			return !processAlive(pid)
		case <-tick.C:
		}
	}
	return true
}

// killTestTmuxServers tears down every tmux server whose socket lives under
// tmuxDir (the run's TMUX_TMPDIR). It returns a description of anything it
// could not take down.
func killTestTmuxServers(tmuxDir string) []string {
	socks, _ := filepath.Glob(filepath.Join(tmuxDir, "tmux-*", "*"))
	var problems []string
	for _, sock := range socks {
		if fi, err := os.Lstat(sock); err != nil || fi.Mode()&os.ModeSocket == 0 {
			continue
		}
		pid, ok := tmuxServerPID(sock)
		_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
		if ok {
			if !waitProcessGone(pid, 3*time.Second) {
				// kill-server did not finish the job: take out the server's
				// whole process group (pipe-pane `sh -c … pluk watch` children
				// live there) and then the server itself.
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				_ = syscall.Kill(pid, syscall.SIGKILL)
				if !waitProcessGone(pid, time.Second) {
					problems = append(problems, fmt.Sprintf("tmux server pid %d (socket %s) survived kill-server and SIGKILL", pid, sock))
				}
			}
		}
		_ = os.Remove(sock)
	}
	return problems
}

// startTmuxReaper spawns a detached `sh` that outlives the test binary, waits
// for it to exit, and then tears down every tmux server under tmuxDir the way
// killTestTmuxServers does. On a clean run TestMain has already done that and
// the reaper finds nothing. On a panic or a `go test -timeout` kill the reaper
// is the only teardown that still runs.
//
// stdio is detached (nil → /dev/null) so `go test` does not wait on the
// reaper's inherited stdout pipe after the test binary exits.
func startTmuxReaper(tmuxDir string) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return
	}
	script := `
pid="$1"; dir="$2"
while kill -0 "$pid" 2>/dev/null; do sleep 1; done
for s in "$dir"/tmux-*/*; do
  [ -S "$s" ] || continue
  spid=$(tmux -S "$s" display-message -p '#{pid}' 2>/dev/null)
  tmux -S "$s" kill-server 2>/dev/null
  if [ -n "$spid" ]; then
    sleep 1
    kill -0 "$spid" 2>/dev/null && kill -9 -- "-$spid" "$spid" 2>/dev/null
  fi
done
rm -rf "$dir"
`
	cmd := exec.Command("sh", "-c", script, "hive-agent-test-tmux-reaper", strconv.Itoa(os.Getpid()), tmuxDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: starting tmux reaper: %v\n", err)
		return
	}
	// Reap the reaper when it eventually exits, without waiting for it here.
	go func() { _ = cmd.Wait() }()
}

// findLeakedTestProcesses lists processes started by THIS test run that are
// still alive: tmux servers on the run's sockets (`ht<pid>`, `hleak<pid>`,
// …) and pluk pipe-pane watchers writing into the run's stub dir. Servers
// from other runs (other pids) are deliberately not matched. The scan is
// best-effort: if `ps` is unavailable it reports nothing.
func findLeakedTestProcesses(stubDir string) []string {
	out, err := exec.Command("ps", "-axo", "pid=,pgid=,command=").Output()
	if err != nil {
		return nil
	}
	pid := strconv.Itoa(os.Getpid())
	markers := []string{
		"-L ht" + pid + " ",
		"-L ht" + pid + "b ",
		"-L hleak" + pid + " ",
		stubDir,
	}
	var lines []string
	for _, line := range bytes.Split(out, []byte("\n")) {
		if s := strings.TrimSpace(string(line)); s != "" && !strings.Contains(s, "hive-agent-test-tmux-reaper") {
			lines = append(lines, s)
		}
	}
	var leaked []string
	pgids := map[string]bool{}
	for _, s := range lines {
		for _, mk := range markers {
			if strings.Contains(s, mk) {
				leaked = append(leaked, s)
				if f := strings.Fields(s); len(f) >= 2 {
					pgids[f[1]] = true
				}
				break
			}
		}
	}
	// The `node … pluk watch` grandchild does not carry the stub dir on its
	// own command line (only the `sh -c` wrapper does); it shares the tmux
	// server's process group, so pick it up by pgid.
	for _, s := range lines {
		f := strings.Fields(s)
		if len(f) >= 2 && pgids[f[1]] && strings.Contains(s, "pluk watch") && !strings.Contains(s, stubDir) {
			leaked = append(leaked, s)
		}
	}
	return leaked
}

// killLeakedTestProcesses SIGKILLs the pids in the `ps` lines returned by
// findLeakedTestProcesses so a failed guard never itself leaves orphans.
func killLeakedTestProcesses(lines []string) {
	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) < 2 {
			continue
		}
		if pgid, err := strconv.Atoi(fields[1]); err == nil && pgid > 1 && pgid != os.Getpid() {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil && pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// teardownTestProcesses is TestMain's post-run hook: kill this run's tmux
// servers, then verify nothing from the run survived. It returns a non-zero
// exit code when leaks remain so the leak is a CI failure, not a silent
// resource drain. Skip-safe when tmux is not installed (no server could have
// been started).
func teardownTestProcesses(tmuxDir, stubDir string) int {
	if _, err := exec.LookPath("tmux"); err != nil {
		return 0
	}
	problems := killTestTmuxServers(tmuxDir)
	leaked := findLeakedTestProcesses(stubDir)
	if len(problems) == 0 && len(leaked) == 0 {
		return 0
	}
	fmt.Fprintln(os.Stderr, "TestMain: process leak guard: test processes survived the run:")
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "  %s\n", p)
	}
	for _, l := range leaked {
		fmt.Fprintf(os.Stderr, "  %s\n", l)
	}
	killLeakedTestProcesses(leaked)
	return 1
}
