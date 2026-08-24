package github

import (
	"log/slog"
	"os"
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
const requestDirMode = 0o2775

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
