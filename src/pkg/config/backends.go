package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

func LaunchCmdDeclaredBackend(cmd string) string {
	bin := LaunchCmdExecutable(cmd)
	if bin == "" {
		return ""
	}
	if bin == "agent-launch.sh" {
		fields := strings.Fields(strings.TrimSpace(cmd))
		i := 0
		for i < len(fields) && strings.Contains(fields[i], "=") && !strings.HasPrefix(fields[i], "-") {
			i++
		}
		for j := i + 1; j < len(fields); j++ {
			if fields[j] == "--backend" && j+1 < len(fields) {
				return fields[j+1]
			}
		}
		return ""
	}
	if IsCLIBackend(bin) || IsInferenceBackend(bin) {
		return bin
	}
	return ""
}

// LaunchCmdExecutable returns the command's first executable basename after
// leading KEY=value environment assignments.
func LaunchCmdExecutable(cmd string) string {
	fields := strings.Fields(strings.TrimSpace(cmd))
	if len(fields) == 0 {
		return ""
	}
	i := 0
	for i < len(fields) && strings.Contains(fields[i], "=") && !strings.HasPrefix(fields[i], "-") {
		i++
	}
	if i >= len(fields) {
		return ""
	}
	return filepath.Base(fields[i])
}

// ReconcileLaunchCmdForBackend returns the launch_cmd an agent should keep
// once its backend is set to backend, and whether that means dropping the
// one it had. backend is the operator-owned field; launch_cmd is derived from
// it (the dashboard fills the default per backend when it is empty), so when
// the two disagree — judged by the same ValidateLaunchCmdBackend gate the
// loader applies — launch_cmd is the stale half and is cleared. A launch_cmd
// that already agrees with the new backend, or has no opinion (an unknown
// wrapper script), is kept untouched.
//
// Every path that changes an agent's backend must call this before
// persisting. The dashboard "switch backend" control did not, and persisted a
// `backend: codex` next to a `launch_cmd: /usr/bin/bob …`; the running
// process kept its in-memory state so nothing failed until the next restart,
// when the loader rejected the config and the hive crash-looped for 18 hours.
func (g GovernorConfig) ReconcileLaunchCmdForBackend(backend, launchCmd string) (string, bool) {
	if strings.TrimSpace(launchCmd) == "" {
		return launchCmd, false
	}
	if g.ValidateLaunchCmdBackend(backend, launchCmd) == nil {
		return launchCmd, false
	}
	return "", true
}

func (g GovernorConfig) ValidateLaunchCmdBackend(backend, launchCmd string) error {
	declared := LaunchCmdDeclaredBackend(launchCmd)
	if declared == "" || backend == "" {
		if backend != "" {
			if bin := LaunchCmdExecutable(launchCmd); bin != "" && g.isGatewayName(bin) {
				return fmt.Errorf("backend %q routes through the claude CLI but launch_cmd %q launches gateway %q — clear launch_cmd or point it at claude",
					backend, launchCmd, bin)
			}
		}
		return nil
	}
	if IsCLIBackend(backend) {
		if strings.EqualFold(declared, backend) {
			return nil
		}
		return fmt.Errorf("backend %q contradicts launch_cmd %q (it launches %q): the agent would be launched as %s but health-checked and diagnosed as %s, then relaunched as \"hung\" forever — set backend to %s or fix launch_cmd",
			backend, launchCmd, declared, declared, backend, declared)
	}
	if IsInferenceBackend(backend) || g.isGatewayName(backend) {
		if declared == "claude" {
			return nil
		}
		return fmt.Errorf("backend %q routes through the claude CLI but launch_cmd %q launches %q — clear launch_cmd or point it at claude",
			backend, launchCmd, declared)
	}
	if strings.EqualFold(declared, backend) {
		return nil
	}
	return nil
}

// ClearInvalidInferenceLaunchCmds removes persisted launch_cmd values that
// launch an inference backend or configured gateway binary. Those backends are
// model routes; the process must be the claude CLI.
func (g GovernorConfig) ClearInvalidInferenceLaunchCmds(agents map[string]AgentConfig) []string {
	var cleared []string
	for name, agent := range agents {
		bin := LaunchCmdExecutable(agent.LaunchCmd)
		if bin == "" {
			continue
		}
		if IsInferenceBackend(bin) || g.isGatewayName(bin) {
			agent.LaunchCmd = ""
			agents[name] = agent
			cleared = append(cleared, name)
		}
	}
	sort.Strings(cleared)
	return cleared
}

// agentSourceLabel renders an agent's name for a validation error, naming the
// per-agent overlay file it came from when there is one (#6024).
//
// "agent supervisor" alone is ambiguous: hive.yaml, the ConfigMap seed, the
// dashboard overlay and /data/agent-configs/<name>.yaml all land in the same
// agent map, so an operator reading the crash message has no way to know which
// file to edit - and the overlay directory is the one they are least likely to
// look in. "agent supervisor (from /data/agent-configs/supervisor.yaml)" turns
// an 8-hour hunt into a single edit.
func agentSourceLabel(name, sourceFile string) string {
	if sourceFile == "" {
		return name
	}
	return fmt.Sprintf("%s (from %s)", name, sourceFile)
}

// isGatewayName reports whether backend names a configured model gateway,
// matched case-insensitively to mirror ResolveGateway.
func (g GovernorConfig) isGatewayName(backend string) bool {
	for _, gw := range g.ResolvedGateways() {
		if gw.Name != "" && strings.EqualFold(gw.Name, backend) {
			return true
		}
	}
	return false
}

// BackendAllowed reports whether this spoke's backends allow/deny list
// permits placing an agent on backend.
func (c *Config) BackendAllowed(backend string) bool {
	return c.Backends.Allowed(backend)
}
