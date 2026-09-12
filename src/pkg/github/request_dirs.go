package github

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// requestDirMode is the mode every agent-facing request queue must end up with.
//
// Agents (UID >= 2001, in the shared "node" group) DROP request files here —
// hive-open-pr / hive-open-issue run AS the agent. MkdirAll is masked by umask
// to 0755 (not group-writable), so an agent's write gets EACCES and its PR or
// issue silently fails to open. Group-write + setgid (like /data/beads) makes
// the dir writable by every agent and makes agent-written files inherit the
// node group. The forge-check still holds: each watcher reads a file's OWNING
// UID, which is the agent that wrote it — group-writability lets agents write,
// it does not let one agent forge another's ownership.
//
// The sticky bit (as on /tmp) is required alongside group-write: without it,
// write permission on the directory is delete permission on every entry, so
// any agent could unlink or replace a peer's queued request regardless of
// file ownership. With it, only a file's owner may unlink or rename it —
// drop-box semantics are preserved.
//
// NOTE: os.Chmod ignores raw 0o2000/0o1000 bits — setgid and sticky MUST be
// expressed as os.ModeSetgid / os.ModeSticky or they are silently dropped
// (the previous raw-octal 0o2775 chmod never actually set setgid).
const requestDirMode = 0o775 | os.ModeSetgid | os.ModeSticky

// ensureRequestDir creates one request queue and opens it to the agent group.
// Returns false when the directory cannot be created, which is the only state
// in which the corresponding watcher must disable itself.
func ensureRequestDir(logger *slog.Logger, kind, dir string) bool {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		if logger != nil {
			logger.Warn(kind+"-request queue: cannot create request dir",
				slog.String("dir", dir), slog.String("error", err.Error()))
		}
		return false
	}
	if err := os.Chmod(dir, requestDirMode); err != nil && logger != nil {
		logger.Warn(kind+"-request queue: could not set group-writable perms; agents may be unable to queue requests",
			slog.String("dir", dir), slog.String("error", err.Error()))
	}
	return true
}

// PrepareRequestDirs creates the PR and issue request queues WITHOUT starting
// their watchers.
//
// The watchers are deliberately gated on a usable GitHub App — with no App
// there is no bot to author as, and the design says requests should "simply
// accumulate rather than opening under a wrong identity". Accumulating,
// however, requires somewhere to accumulate IN, and the queues used to be
// created inside that same gate. So on a hive whose App is not usable at boot
// the directories did not exist at all, and `hive-open-pr` / `hive-open-issue`
// hard-failed in the agent's shell: the finding was discarded rather than
// queued, and the failure was visible only to whoever read that agent's pane.
//
// This is not a rare corner. App setup routinely completes AFTER boot — the
// operator saves the installation ID from the dashboard, /gh-setup persists it,
// or auto-discovery finds it on a later poll. Every one of those leaves a hive
// that reports healthy App auth while its agents cannot queue a single write.
//
// Creating the queues unconditionally costs two empty directories and makes the
// documented behavior real: requests wait on disk until a watcher runs.
func PrepareRequestDirs(logger *slog.Logger) {
	ensureRequestDir(logger, "pr", prRequestDir())
	ensureRequestDir(logger, "issue", issueRequestDir())
}

// inFlightGrace is how long a request file whose contents do not parse is
// assumed to still be mid-write rather than genuinely malformed. See
// quarantinable.
const inFlightGrace = 5 * time.Second

// writeRequestFile publishes one request/result file atomically.
//
// os.WriteFile opens with O_CREATE|O_TRUNC and writes separately, so a watcher
// scan landing in between sees the final ".json" name holding zero bytes. That
// content does not unmarshal, and every watcher's response to unparseable JSON
// is to quarantine the file as ".bad" — so a perfectly valid request was
// destroyed purely because the scan won the race. Writing to a temp name the
// scanners ignore and rename(2)-ing it into place makes the entry appear only
// once it is complete; rename is atomic within a directory.
//
// The ".tmp" suffix is load-bearing: every scanner requires a ".json" suffix,
// so an in-progress "<name>.json.tmp" is invisible to them.
func writeRequestFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp) // no-op once the rename succeeded
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// CreateTemp makes 0600; request queues are group-readable by design.
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	tmp = ""
	return nil
}

// quarantinable reports whether a file that failed to parse should be treated
// as permanently bad (renamed aside) rather than retried on the next tick.
//
// Our own writers are atomic, but the request queues are drop-boxes: an agent
// may append with a plain shell redirect, which has the same torn-read window.
// Quarantining is destructive and unrecoverable, so it is only correct once the
// file has stopped changing. A file that is empty, or was modified within
// inFlightGrace, is assumed to still be in flight and is left alone; a stale
// unparseable file is genuinely malformed.
func quarantinable(path string, now time.Time) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false // vanished; nothing to quarantine
	}
	if st.Size() == 0 {
		return false
	}
	return now.Sub(st.ModTime()) >= inFlightGrace
}
