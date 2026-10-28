package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// kickLogSourceSandbox prefixes the source: header line of a sandboxed
	// kick's archive — "sandbox-job" or "sandbox-podman" — so the kick
	// history page shows what the body is: the container's stdout/stderr
	// hive collected when the run ended, not a tmux capture.
	kickLogSourceSandbox = "sandbox"
	// kickLogReasonSandbox is the <reason> component of a sandboxed kick's
	// archive filename.
	kickLogReasonSandbox = "sandbox"
)

// sandboxKickLogBody renders a sandboxed run's captured output as the body of
// a kick log: a short outcome block, then stdout and stderr verbatim. The
// transcript is already token-scrubbed by the executor; this adds no new
// text from the agent.
func sandboxKickLogBody(runtime string, res SandboxKickResult, runErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "runtime: %s\nexit_code: %d\ncommits: %d\n", runtime, res.Sandbox.ExitCode, res.CommitCount)
	if res.Branch != "" {
		fmt.Fprintf(&b, "branch: %s\n", res.Branch)
	}
	if res.Workspace != "" {
		fmt.Fprintf(&b, "workspace: %s\n", res.Workspace)
	}
	if res.PR != nil && res.PR.URL != "" {
		fmt.Fprintf(&b, "pr: %s\n", res.PR.URL)
	}
	if res.PRWithheld != "" {
		fmt.Fprintf(&b, "pr_withheld: %s\n", res.PRWithheld)
	}
	if res.ReportPath != "" {
		fmt.Fprintf(&b, "agent_report: %s\n", res.ReportPath)
	}
	if res.ReportStatus != "" {
		fmt.Fprintf(&b, "agent_report_status: %s\n", res.ReportStatus)
	}
	if res.TimedOut {
		b.WriteString("timed_out: true\n")
	}
	if runErr != nil {
		fmt.Fprintf(&b, "error: %s\n", runErr.Error())
	}
	b.WriteString("\n---- stdout ----\n")
	b.WriteString(res.Sandbox.Stdout)
	if !strings.HasSuffix(res.Sandbox.Stdout, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n---- stderr ----\n")
	b.WriteString(res.Sandbox.Stderr)
	if !strings.HasSuffix(res.Sandbox.Stderr, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// sandboxKickLogArchive is a sandboxed kick's log, built under m.mu (the
// header reads agent state) and written after the lock is released.
type sandboxKickLogArchive struct {
	dir     string
	id      string
	content string
}

// prepareSandboxKickLogLocked builds the archive for a sandboxed run so the
// caller can write it off-lock. The filename is keyed on the kick's own start
// time like every other archive, so the kick history page orders sandboxed
// and tmux kicks the same way. Returns nil when archiving is off.
//
// This is the only durable copy of a Job's output: the Job (and its pod log)
// is deleted on success or GC'd at its TTL, and the workspace transcript
// (.hive/sandbox-transcript.log) lives under the sandbox root the workspace
// sweep removes after two hours. Before this archive existed, "full log" on
// a job-runtime agent was a 404 as soon as either of those had happened.
func (m *Manager) prepareSandboxKickLogLocked(agent *AgentProcess, runtime string, started time.Time, res SandboxKickResult, runErr error) *sandboxKickLogArchive {
	if m.kickLogRetention <= 0 || m.kickLogDir == "" {
		return nil
	}
	body := sandboxKickLogBody(runtime, res, runErr)
	source := kickLogSourceSandbox
	if runtime != "" {
		source += "-" + runtime
	}
	header := m.kickLogHeaderLocked(agent, "archived", time.Now(), kickLogReasonSandbox, source)
	id := started.UTC().Format(kickLogTimestampFormat) + "-" + kickLogReasonSandbox + kickLogSuffix
	return &sandboxKickLogArchive{dir: m.agentKickLogDir(agent.Name), id: id, content: header + body}
}

// writeSandboxKickLog persists a prepared archive and prunes the directory.
// Callers must NOT hold m.mu; pruning reads the directory.
func (m *Manager) writeSandboxKickLog(name string, a *sandboxKickLogArchive) error {
	if a == nil {
		return nil
	}
	if err := os.MkdirAll(a.dir, kickLogDirMode); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(a.dir, a.id), []byte(a.content), kickLogFileMode); err != nil {
		return err
	}
	m.logger.Info("archived sandbox kick log", "name", name, "file", a.id, "bytes", len(a.content))
	m.pruneKickLogs(a.dir)
	m.notifyKickObserver(name, KickObserverEventArchived, kickLogReasonSandbox)
	return nil
}

// latestKickLog returns the newest archive in dir and its id, or ("", "",
// nil) when the directory has none.
func latestKickLog(dir string) (id, content string, err error) {
	infos, err := readKickLogDir(dir)
	if err != nil || len(infos) == 0 {
		return "", "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, infos[0].ID))
	if err != nil {
		return "", "", err
	}
	return infos[0].ID, string(data), nil
}
