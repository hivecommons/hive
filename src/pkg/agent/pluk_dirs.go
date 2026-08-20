package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	plukRunDir = "/var/run/pluk"
)

// pluk runs from per-agent tmux servers. Agent UIDs differ, but they all share
// the node group, so setgid keeps new entries group-owned without opening the
// directory to unrelated container users.
var plukSharedDirMode = os.FileMode(0o770) | os.ModeSetgid

// plukLogFileMode matches plukSharedDirMode's intent one level down. hive-panes
// is run BY one agent to read every OTHER agent's log, so a session log written
// by uid A has to be readable by uid B; the shared node group is what makes that
// work, exactly as it does for the directory.
//
// The file has to be created here rather than left to the shell. pipe-pane's
// `>>` would create it under whatever umask the agent's pane shell carries, and
// a 0077 umask yields a 0600 log that no peer can read — the same class of
// silent breakage permissions_watcher.go exists to repair for agent config
// (#3882). Creating it up front with an explicit mode means `>>` only ever
// appends to a file that is already correct.
var plukLogFileMode = os.FileMode(0o660)

// plukSessionLogPath is where pluk's events for one tmux session are appended.
// It mirrors what pluk's own `attach` builds — <run-dir>/logs/<session>.jsonl —
// and is the path src/deploy/hive-panes.sh globs and
// src/pkg/dashboard/inception_watcher.go tails.
func plukSessionLogPath(runDir, session string) string {
	return filepath.Join(runDir, "logs", session+".jsonl")
}

// ensurePlukLogFile creates (or leaves alone) the session's JSONL log with a
// mode peer agents can read, and returns its path. Both the O_CREATE mode and a
// following Chmod are needed: O_CREATE is umask-filtered, and an existing file
// from an earlier run under a tighter umask has to be widened back to the shared
// mode rather than inherited as-is.
func ensurePlukLogFile(runDir, session string) (string, error) {
	path := plukSessionLogPath(runDir, session)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, plukLogFileMode)
	if err != nil {
		return "", fmt.Errorf("creating %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("closing %s: %w", path, err)
	}

	if os.Geteuid() == 0 {
		if err := os.Chown(path, DevUID, NodeGID); err != nil {
			return "", fmt.Errorf("chowning %s to %d:%d: %w", path, DevUID, NodeGID, err)
		}
	}
	if err := os.Chmod(path, plukLogFileMode); err != nil {
		return "", fmt.Errorf("chmoding %s to %s: %w", path, plukLogFileMode, err)
	}

	return path, nil
}

func ensurePlukRunDirs(runDir string) error {
	for _, name := range []string{"logs", "commands"} {
		dir := filepath.Join(runDir, name)
		if err := os.MkdirAll(dir, plukSharedDirMode); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(dir, DevUID, NodeGID); err != nil {
				return fmt.Errorf("chowning %s to %d:%d: %w", dir, DevUID, NodeGID, err)
			}
		}
		if err := os.Chmod(dir, plukSharedDirMode); err != nil {
			return fmt.Errorf("chmoding %s to %s: %w", dir, plukSharedDirMode, err)
		}
	}
	return nil
}

// plukPipePaneCmd builds the shell command tmux pipe-pane runs for one agent
// session (kubestellar/hive#4285).
//
// `pluk watch` classifies its stdin and writes the resulting events to STDOUT.
// It opens no file of its own — the published package contains no
// createWriteStream, appendFile, writeFile or openSync in any module — and
// pipe-pane only feeds the pane into the command's stdin; it does nothing with
// the command's stdout. So without the `>>` here the events are produced and
// discarded, /var/run/pluk/logs stays empty, and both consumers
// (src/deploy/hive-panes.sh, src/pkg/dashboard/inception_watcher.go) wait on
// files that never appear.
//
// The redirect and --include-raw were lost together in #1759, which replaced
// the Go `pluk-publish` binary — a publisher, which wrote the log itself — with
// the TypeScript `pluk watch`. That migration translated the flags
// (`--session X --cli Y` became `watch X --cli=Y`) but not the file-writing half
// the rewrite had moved into the shell. hive-panes.sh predates #1759 and has
// only ever read raw_output, so restoring both is restoring the contract it was
// written against, not picking a new one.
//
// --include-raw is load-bearing, not cosmetic: watch defaults includeRaw to
// false, and raw_output is the ONLY event type hive-panes.sh consumes. Pluk's
// own `attach` passes it by default (opt out with --no-raw).
//
// PLUK_RUN_DIR is inert for `watch` on 0.8.0 — only subscribe, send and
// sessions read it — but it is what pluk's own attach exports, so a later pluk
// that does consult it resolves to the hive run dir instead of /tmp/pluk-run.
//
// Every operand is shell-quoted. The whole string is handed to a shell by tmux,
// and the log path now embeds the session name, so a name carrying a space or a
// shell metacharacter would otherwise split the redirect target.
func plukPipePaneCmd(plukPath, session, backend, logFile string) string {
	return fmt.Sprintf("PLUK_RUN_DIR=%s %s watch %s --cli=%s --include-raw >> %s",
		shellQuote(plukRunDir),
		shellQuote(plukPath),
		shellQuote(session),
		shellQuote(backend),
		shellQuote(logFile),
	)
}
