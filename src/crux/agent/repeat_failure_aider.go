package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// aider route for the repeat-failure check (hivecommons/hive#14550, C1/C3/C14;
// issue #14752).
//
// In tmux aider runs each command as `$SHELL -i -c <cmd>`. An interactive
// bash ignores BASH_ENV but reads ~/.bashrc, so while the check is on Hive
// replaces the agent's ~/.bashrc bridge (a symlink to the shared rc file) with
// a per-agent file that sources the shared rc file and then, only in a `-c`
// shell, the Hive-owned BASH_ENV script of the neutral route, rendered with
// the aider_bashrc route. SHELL is exported as /bin/bash so aider picks bash.
// The shared rc file is never written.
//
// Only Hive-managed per-UID homes get the file (never contributor local mode
// or HIVE_SHARED_AGENT_HOME=1). Turning the check off removes the file and
// restores the bridge at the next launch, and removes the script; a leftover
// line names a missing script and does nothing.
//
// Not covered: without a tty aider runs commands through subprocess with
// shell=True (/bin/sh -c), which reads no rc file.

const (
	// RepeatFailureRouteAiderBashrc names the aider route in a record's Route.
	RepeatFailureRouteAiderBashrc = "aider_bashrc"
	// repeatFailureAiderBashrcHeader marks a ~/.bashrc as Hive's, so cleanup
	// never removes an rc file Hive did not write.
	repeatFailureAiderBashrcHeader = "# Hive repeat-failure aider route (hivecommons/hive#14550)."
	repeatFailureAiderBashrcMode   = 0o644
	repeatFailureAiderBackend      = "aider"
)

// repeatFailureAiderBashrcActive reports whether the aider route applies to a
// launch: the check is on, the backend is aider and the agent has its own
// Hive-managed home (the same conditions setupInteractiveHome provisions).
func (m *Manager) repeatFailureAiderBashrcActive(agent *AgentProcess, backend string) bool {
	if agent == nil || agent.UID <= 0 || sharedAgentHomeForced() {
		return false
	}
	if strings.ToLower(strings.TrimSpace(backend)) != repeatFailureAiderBackend {
		return false
	}
	return m.repeatFailureEnabled()
}

// renderRepeatFailureAiderBashrc renders the per-agent ~/.bashrc: the shared
// rc file first, then the guarded line for `-c` shells.
func renderRepeatFailureAiderBashrc(sharedRC, script string) []byte {
	rc := shellQuote(sharedRC)
	s := shellQuote(script)
	var b strings.Builder
	b.WriteString(repeatFailureAiderBashrcHeader + "\n")
	b.WriteString("# Written by Hive while the check is on, in place of the link to the shared rc file; do not edit.\n")
	b.WriteString("if [ -r " + rc + " ]; then . " + rc + "; fi\n")
	b.WriteString("if [[ -n ${BASH_EXECUTION_STRING-} && -r " + s + " ]]; then . " + s + "; fi\n")
	return []byte(b.String())
}

// isRepeatFailureAiderBashrc reports whether rc is a regular file Hive wrote
// for the aider route.
func isRepeatFailureAiderBashrc(rc string) (bool, []byte) {
	info, err := os.Lstat(rc)
	if err != nil || !info.Mode().IsRegular() {
		return false, nil
	}
	data, err := os.ReadFile(rc)
	if err != nil || !bytes.HasPrefix(data, []byte(repeatFailureAiderBashrcHeader)) {
		return false, nil
	}
	return true, data
}

// provisionRepeatFailureAiderBashrc writes the per-agent ~/.bashrc when the
// aider route applies, or removes the one Hive wrote when it no longer does so
// bridgeInteractiveHome restores the link. A real rc file Hive did not write
// is never replaced. Best-effort like the rest of home provisioning.
func (m *Manager) provisionRepeatFailureAiderBashrc(agent *AgentProcess, home, backend string) {
	rc := filepath.Join(home, ".bashrc")
	ours, existing := isRepeatFailureAiderBashrc(rc)
	if !m.repeatFailureAiderBashrcActive(agent, backend) {
		if ours {
			if err := os.Remove(rc); err != nil && !os.IsNotExist(err) {
				m.logger.Warn("repeat-failure: removing the aider ~/.bashrc failed", "agent", agent.Name, "path", rc, "error", err)
			}
		}
		return
	}
	content := renderRepeatFailureAiderBashrc(filepath.Join(sharedAgentHome, ".bashrc"), repeatFailureBashEnvPath(home))
	if ours && bytes.Equal(existing, content) {
		return
	}
	if !ours {
		info, err := os.Lstat(rc)
		switch {
		case err == nil && info.Mode()&os.ModeSymlink == 0:
			m.logger.Warn("repeat-failure: ~/.bashrc is not Hive's; aider route not installed", "agent", agent.Name, "path", rc)
			return
		case err != nil && !os.IsNotExist(err):
			m.logger.Warn("repeat-failure: inspecting ~/.bashrc failed; aider route not installed", "agent", agent.Name, "path", rc, "error", err)
			return
		}
	}
	tmp := rc + ".hive-new"
	_ = os.Remove(tmp)
	if err := writeFileNoFollow(tmp, content, repeatFailureAiderBashrcMode); err != nil {
		_ = os.Remove(tmp)
		m.logger.Warn("repeat-failure: writing the aider ~/.bashrc failed", "agent", agent.Name, "path", rc, "error", err)
		return
	}
	_ = os.Chown(tmp, agent.UID, -1)
	if err := os.Rename(tmp, rc); err != nil {
		_ = os.Remove(tmp)
		m.logger.Warn("repeat-failure: installing the aider ~/.bashrc failed", "agent", agent.Name, "path", rc, "error", err)
	}
}
