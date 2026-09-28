package agent

import (
	"context"
	"os"
	"strings"
	"time"
)

// crashRestartReason is the restart reason recorded when hive relaunches an
// agent whose CLI died on its own (tmux session gone, bare shell, watchdog
// dead-session recovery) rather than by operator or recovery intent.
const crashRestartReason = "crash"

// crashResumeMinUptime is the minimum lifetime of the dead run for the
// relaunch to resume its session. A run that died sooner is treated as a
// launch failure (corrupt or unresumable session, bad model, auth) and comes
// back fresh, so a broken session can never produce a resume→crash loop.
const crashResumeMinUptime = 2 * time.Minute

// crashResumeEnv disables crash-resume fleet-wide when set to "0".
const crashResumeEnv = "HIVE_CRASH_RESUME"

func crashResumeEnabled() bool {
	return os.Getenv(crashResumeEnv) != "0"
}

// crashResumeFlag returns the CLI flag that reopens the agent's most recent
// session for the given backend, or "" when the backend cannot resume safely.
//
// Only per-agent-scoped "most recent" lookups are eligible: claude scopes
// --continue to the working directory (always the agent's own), copilot scopes
// it to $HOME/.copilot, which is per-agent only under the per-UID home layout.
// In the legacy shared-home layout copilot --continue could reopen ANOTHER
// agent's session, so it is skipped there. Backends whose resume is a separate
// subcommand (codex resume, goose session -r) are not rewritten here.
func crashResumeFlag(backend string, uid int) string {
	switch backend {
	case "claude":
		return "--continue"
	case "copilot":
		if uid <= 0 || sharedAgentHomeForced() {
			return ""
		}
		return "--continue"
	}
	return ""
}

// shouldResumeAfterCrash decides, at crash-restart time, whether the relaunch
// should reopen the dead run's session.
func shouldResumeAfterCrash(agent *AgentProcess, backend string, now time.Time) (bool, string) {
	if !crashResumeEnabled() {
		return false, "disabled by " + crashResumeEnv
	}
	if strings.TrimSpace(agent.Config.LaunchCmd) != "" {
		return false, "custom launch_cmd"
	}
	if crashResumeFlag(backend, agent.UID) == "" {
		return false, "backend has no scoped resume flag"
	}
	if agent.StartedAt == nil {
		return false, "no previous run"
	}
	if up := now.Sub(*agent.StartedAt); up < crashResumeMinUptime {
		return false, "previous run too short (" + up.Truncate(time.Second).String() + ")"
	}
	return true, ""
}

// RestartAfterCrash is the relaunch path for an agent whose CLI died on its
// own. Unlike an operator restart it tries to preserve the agent's context:
// the relaunched CLI reopens the most recent session, so an in-flight task
// survives an OOM, a stray SIGKILL of the agent's process tree, or a CLI crash
// instead of coming back to a blank prompt and losing everything it knew.
func (m *Manager) RestartAfterCrash(ctx context.Context, name string) error {
	m.mu.Lock()
	if agent, ok := m.agents[name]; ok {
		backend := agent.effectiveBackend()
		if IsInferenceBackend(backend) {
			backend = "claude"
		}
		resume, why := shouldResumeAfterCrash(agent, backend, time.Now())
		agent.resumeOnLaunch = resume
		if resume {
			m.logger.Info("crash restart will resume previous session", "name", name, "backend", backend)
		} else {
			m.logger.Info("crash restart starts fresh", "name", name, "backend", backend, "reason", why)
		}
	}
	m.mu.Unlock()
	return m.restartWithReason(ctx, name, crashRestartReason, false)
}
