package agent

import (
	"bufio"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func (m *Manager) reapAgentCLI(agent *AgentProcess) int {
	procPath := procRoot
	marker := "HIVE_AGENT=" + agent.Name

	entries, err := os.ReadDir(procPath)
	if err != nil {
		m.logger.Warn("reapAgentCLI: failed to read /proc", "agent", agent.Name, "error", err)
		return 0
	}

	killed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if pid == os.Getpid() {
			continue
		}

		// cmdline is NUL-separated; read it raw and check for a CLI binary.
		cmdlineRaw, err := os.ReadFile(filepath.Join(procPath, entry.Name(), "cmdline"))
		if err != nil || len(cmdlineRaw) == 0 {
			continue
		}
		cmdline := strings.ReplaceAll(string(cmdlineRaw), "\x00", " ")
		if !containsCLIMarker(cmdline) {
			continue
		}

		// environ is NUL-separated KEY=VALUE pairs. Match the exact agent so we
		// never kill another agent's CLI when UIDs are shared.
		environRaw, err := os.ReadFile(filepath.Join(procPath, entry.Name(), "environ"))
		if err != nil {
			continue
		}
		if !environHasMarker(string(environRaw), marker) {
			continue
		}

		if err := syscall.Kill(pid, syscall.SIGKILL); err == nil {
			killed++
			m.logger.Info("reaped agent CLI process",
				"agent", agent.Name, "pid", pid, "cmdline", truncateStr(cmdline, 120))
		}
	}
	if killed > 0 {
		m.logger.Info("reapAgentCLI complete", "agent", agent.Name, "killed", killed)
	}
	return killed
}

// containsCLIMarker reports whether a /proc cmdline names a known CLI binary.
func containsCLIMarker(cmdline string) bool {
	for _, marker := range cliProcessMarkers {
		if strings.Contains(cmdline, marker) {
			return true
		}
	}
	return false
}

// environHasMarker reports whether a raw NUL-separated /proc environ blob
// contains the exact HIVE_AGENT=<name> pair. Splitting on NUL and comparing
// whole entries avoids a prefix collision between "scanner" and "scanner-2".
func environHasMarker(environ, marker string) bool {
	for _, pair := range strings.Split(environ, "\x00") {
		if pair == marker {
			return true
		}
	}
	return false
}

// minAgentUID is the lowest UID killAgentProcesses will ever target. Per-agent
// UIDs are allocated from baseAgentUID (2001) upward, so any uid below this
// belongs to the system (root=0, the proxy user, etc.). Refusing to match on a
// sub-range UID guarantees that a stray killAgentProcesses(0) — which happens
// when UID isolation is off or an agent is missing from the UID map — can never
// SIGKILL root-owned processes (hive itself, PID 1, the shared tmux server).
const minAgentUID = baseAgentUID

// killAgentProcesses finds all processes owned by the given UID via /proc and
// sends SIGKILL to each. Hung copilot binaries ignore SIGINT, so brute-force
// cleanup is needed to prevent orphan accumulation on the shared SQLite store.
//
// The function is EPERM-safe only for unprivileged callers; hive runs as root,
// so it is NOT inherently a no-op for shared/system UIDs. A floor guard
// (uid >= minAgentUID) plus a self-skip (never SIGKILL our own PID) are the
// real defenses that keep uid < minAgentUID from touching system processes.
func killAgentProcesses(uid int, logger *slog.Logger) int {
	// Floor guard: refuse to sweep by a system-range UID. uid==0 (root) reaches
	// here when UID isolation is disabled or LookupByName missed the agent; as
	// root, matching ownerUID==0 would SIGKILL every root process. This is a real
	// bug signal, so warn loudly and kill nothing.
	if uid < minAgentUID {
		if logger != nil {
			logger.Warn("refusing to kill by uid, would target system/root processes",
				"uid", uid, "min_agent_uid", minAgentUID)
		}
		return 0
	}

	procPath := procRoot
	entries, err := os.ReadDir(procPath)
	if err != nil {
		logger.Warn("failed to read /proc for process cleanup", "uid", uid, "error", err)
		return 0
	}

	killed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		// Belt-and-suspenders: never SIGKILL the hive process itself, mirroring
		// reapAgentCLI's self-skip.
		if pid == os.Getpid() {
			continue
		}

		statusPath := filepath.Join(procPath, entry.Name(), "status")
		f, err := os.Open(statusPath)
		if err != nil {
			continue
		}

		ownerUID := -1
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "Uid:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if parsed, err := strconv.Atoi(fields[1]); err == nil {
						ownerUID = parsed
					}
				}
				break
			}
		}
		_ = f.Close() // read-only /proc status fd; nothing to lose on close error

		if ownerUID != uid {
			continue
		}

		if err := syscall.Kill(pid, syscall.SIGKILL); err == nil {
			killed++
		}
	}
	return killed
}
