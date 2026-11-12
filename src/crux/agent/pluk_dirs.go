package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/hivecommons/hive/crux/sharedfile"
)

// plukRunDir is where pluk's run state and per-session logs live.
//
// A var (not const) as a TEST SEAM, matching the agentTokenCacheDir
// convention: `pluk` is on PATH on any host running a live hive, so a test
// that launches an agent would otherwise try to create /var/run/pluk and
// session logs on the live host (#4737/#4738). TestMain points this at the
// per-run temp tree. Production value is unchanged.
var plukRunDir = "/var/run/pluk"

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
// following chmod are needed: O_CREATE is umask-filtered, and an existing file
// from an earlier run under a tighter umask has to be widened back to the shared
// mode rather than inherited as-is.
//
// The logs dir is group-writable by every agent uid (0770+setgid, no sticky
// bit), so anything found at path may have been planted by an agent: the open
// never follows a symlink, the descriptor is checked to be a regular file,
// and the owner and mode are applied through that descriptor, never by path.
// A path-based chmod/chown here would let one agent widen any dev-owned file
// to the shared group by linking its own session log at it (#17966).
func ensurePlukLogFile(runDir, session string) (string, error) {
	path := plukSessionLogPath(runDir, session)

	f, err := openPlukLogNoFollow(path)
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, sharedfile.ErrNotRegular) {
		// A log left behind by another uid (a pane shell's `>>` under a tight
		// umask, or an earlier run as a different user) cannot be opened or
		// chmodded by us, and a symlink or other non-regular entry at the path
		// is not a log at all. The shared setgid logs dir is group-writable, so
		// the entry can be replaced. Replacing beats leaving the session's log
		// unreadable to peers, or following a planted link.
		if rmErr := os.Remove(path); rmErr == nil {
			f, err = openPlukLogNoFollow(path)
		}
	}
	if err != nil {
		return "", fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()

	if os.Geteuid() == 0 {
		if err := f.Chown(DevUID, NodeGID); err != nil {
			return "", fmt.Errorf("chowning %s to %d:%d: %w", path, DevUID, NodeGID, err)
		}
	}
	if err := f.Chmod(plukLogFileMode); err != nil {
		return "", fmt.Errorf("chmoding %s to %s: %w", path, plukLogFileMode, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("closing %s: %w", path, err)
	}

	return path, nil
}

// openPlukLogNoFollow opens (creating if absent) the regular file at path for
// append without following a symlink there. A symlink is reported as
// sharedfile.ErrNotRegular (the kernel answers ELOOP), as is any other
// non-regular file found behind the descriptor. O_NONBLOCK keeps a planted
// FIFO from parking the launch path on open (it fails with ENXIO instead);
// the kernel ignores it for a regular file.
func openPlukLogNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, plukLogFileMode)
	if err != nil {
		// ELOOP: a symlink. ENXIO: a FIFO with no reader (O_NONBLOCK). Neither
		// is a log; both are reported as not-regular so the caller replaces them.
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENXIO) {
			return nil, fmt.Errorf("%w: %s (%v)", sharedfile.ErrNotRegular, path, err)
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%w: %s is %s", sharedfile.ErrNotRegular, path, info.Mode().Type())
	}
	return f, nil
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
		if err := chmodPlukDir(dir, plukSharedDirMode); err != nil {
			return fmt.Errorf("chmoding %s to %s: %w", dir, plukSharedDirMode, err)
		}
	}
	return nil
}

// plukChmod is a test seam for simulating EPERM.
var plukChmod = os.Chmod

// chmodPlukDir widens dir to mode. A directory left by an earlier run under
// another uid rejects chmod with EPERM; that is benign when it already grants
// the group access the shared mode is meant to provide.
func chmodPlukDir(dir string, mode os.FileMode) error {
	err := plukChmod(dir, mode)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	info, statErr := os.Stat(dir)
	if statErr == nil && info.IsDir() && info.Mode().Perm()&mode.Perm() == mode.Perm() {
		return nil
	}
	return err
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
	patternsFlag := ""
	if _, ok := hivePlukPatternBackends[backend]; ok {
		patternsFlag = " --patterns-dir=" + shellQuote(plukPatternsDir)
	}
	return fmt.Sprintf("PLUK_RUN_DIR=%s %s watch %s --cli=%s --include-raw%s >> %s",
		shellQuote(plukRunDir),
		shellQuote(plukPath),
		shellQuote(session),
		shellQuote(backend),
		patternsFlag,
		shellQuote(logFile),
	)
}

// plukPatternsDir holds pattern sets Hive ships for backends the pinned pluk
// has none for. It is a var so tests can point it at a temp dir.
var plukPatternsDir = "/opt/hive/pluk-patterns"

// hivePlukPatternBackends lists backends whose pattern file is shipped in
// plukPatternsDir and must be passed to pluk via --patterns-dir.
var hivePlukPatternBackends = map[string]struct{}{"omp": {}}

var plukNoPatternsWarned sync.Map

// warnNoPlukPatternsOnce returns true only the first time it is called for a
// backend, so a pattern-less backend does not emit a WARN on every agent attach.
func warnNoPlukPatternsOnce(backend string) bool {
	_, loaded := plukNoPatternsWarned.LoadOrStore(backend, struct{}{})
	return !loaded
}

// logPlukAttached reports a successful pipe-pane attach. A backend with no
// pattern set warns once per process; later attaches for it log at debug.
func (m *Manager) logPlukAttached(plukPath, backend, agentName, logFile string) {
	if plukHasPatterns(plukPath, backend) {
		m.logger.Info("pluk publisher attached", "agent", agentName, "cli", backend, "log", logFile)
		return
	}
	if warnNoPlukPatternsOnce(backend) {
		m.logger.Warn(fmt.Sprintf("no pluk pattern set for cli=%s; only raw_output will be recorded", backend),
			"agent", agentName, "log", logFile)
		return
	}
	m.logger.Debug("no pluk pattern set; only raw_output will be recorded", "agent", agentName, "cli", backend, "log", logFile)
}

// plukHasPatterns reports whether pluk will classify events for backend: either
// Hive ships a pattern file for it, or the installed pluk package bundles one.
func plukHasPatterns(plukPath, backend string) bool {
	name := backend + ".patterns"
	if _, ok := hivePlukPatternBackends[backend]; ok {
		if _, err := os.Stat(filepath.Join(plukPatternsDir, name)); err == nil {
			return true
		}
	}
	resolved, err := filepath.EvalSymlinks(plukPath)
	if err != nil {
		return false
	}
	// <pkg>/dist/cli.js or <pkg>/bin/pluk: the patterns dir sits at the package root.
	dir := filepath.Dir(resolved)
	for i := 0; i < 3; i++ {
		if _, err := os.Stat(filepath.Join(dir, "patterns", name)); err == nil {
			return true
		}
		dir = filepath.Dir(dir)
	}
	return false
}
