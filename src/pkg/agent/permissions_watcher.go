package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Permission watcher constants — no magic numbers.
const (
	// PermissionFixInterval is how often the watcher scans for wrong ownership.
	PermissionFixInterval = 10 * time.Second
)

// DevUID and NodeGID are package variables (not constants) so the test suite
// can point the permission fixer at fixture trees owned by the test user
// instead of the live /data agent identities (#4685, #4693).
var (
	// DevUID is the uid of the "dev" user that agents run as.
	DevUID = 1001

	// NodeGID is the gid of the "node" group shared by all agent users.
	NodeGID = 1000
)

const (
	// DirPerms is the minimum permission bits required on directories (u+rwx, g+rwx).
	// Group access is essential because agents run as different users sharing the node group.
	DirPerms = 0o770

	// FilePerms is the minimum permission bits required on files (u+rw, g+rw).
	FilePerms = 0o660

	// bobStateDirGroupRWX is `g+rwx` — the bits a bob state directory (.bob)
	// must grant the shared `node` group so an agent UID can create, replace and
	// traverse the files bob persists there (settings, installation_id, the
	// .bob-errors/ logger target). A dir-owning hive-<agent> user commonly
	// creates these 0755 (owner rwx, group r-x), which leaves the group without
	// write and makes bob log "error saving your latest settings changes" and
	// "Failed to initialize logger" while running degraded. The watcher ORs
	// these bits in to bring such a dir to at least 0770 without ever narrowing
	// the owner or other bits it already has.
	bobStateDirGroupRWX os.FileMode = 0o070
)

// ModeFileGlob matches the per-agent GitHub-mode files the Manager writes
// (/tmp/.hive-mode-<agent>) and the enforcement layer reads: gh-wrapper.sh,
// git-credential-hive.sh, and the MITM proxy. The Manager writes them as the
// hive uid but the shell readers run as per-agent UIDs, so the files must be
// world-readable. A pod running a build that wrote them owner-only (#3172), or
// that wrote them under a restrictive umask (#3882), has 0600 files that no
// agent can read — which killed both `set -e` readers and blocked every gh
// call and push (#3679/#3881). The watcher re-widens them each tick so a
// running pod self-heals without operator intervention.
//
// A var (not const) so tests can point the scan at a writable temp pattern,
// matching WatchedHomeDirs/SharedRepoParent. Production value is unchanged.
var ModeFileGlob = "/tmp/.hive-mode-*"

// CapsFileGlob matches the per-agent capability files (/tmp/.hive-caps-<agent>,
// #4492). They are repaired by the same scan and under the same rules as the
// mode files: written by the hive uid, read on the enforcement path, and
// world-readable so a future out-of-process reader (the shell wrappers already
// read the mode file this way) is not locked out by a narrow umask.
var CapsFileGlob = "/tmp/.hive-caps-*"

// modeFileReadBits is `a+r` — the read access every mode-file consumer needs.
// ORed into the existing perms, never narrowing owner bits, so an
// already-correct 0644 file is left byte-identical and the scan idempotent.
const modeFileReadBits os.FileMode = 0o444

// bobStateDirBase is the directory name bob uses for both its shared-HOME state
// (/data/home/.bob) and its per-agent workdir state (/data/agents/<name>/.bob).
// The watcher recognizes an entry as a bob state dir by this basename so it can
// self-heal the group-write bit the general fixEntry guard would otherwise skip
// (those dirs are owned by hive-<agent>, not DevUID). Kept in sync with
// config.BobStateDirName without importing config here to avoid a cycle.
const bobStateDirBase = ".bob"

// WatchedHomeDirs are the subdirectories under the shared home and data
// volume that tools (Copilot, Claude, etc.) or init containers frequently
// create with root ownership, locking out agent UIDs.
var WatchedHomeDirs = []string{
	"/data/home/.copilot",
	"/data/home/.claude",
	"/data/home/.cache",
	"/data/home/.config",
	"/data/home/.local",
	// bobshell writes its installation-id file and chat-recording tree under
	// $HOME/.bob (verified in bobshell 1.0.6 bundle/bob.js: the state dir is
	// path.join(os.homedir(), ".bob")). Agents run with HOME=/data/home, which
	// the entrypoint creates as root, so without this entry every launch logs
	// "EACCES: permission denied, mkdir '/data/home/.bob'" and bob falls back
	// to an ephemeral installation ID with chat recording disabled.
	//
	// The nested tree (.bob/tmp/<uuid>/chats) needs no separate entries: bob
	// creates those with mkdirSync({recursive:true}) as the AGENT's own uid
	// once .bob itself is traversable+writable by the node group, and
	// fixPermissions walks each root recursively (filepath.Walk) so anything
	// created root-owned underneath is corrected on the next tick anyway.
	"/data/home/.bob",
	"/data/agents",
	// Per-agent bead stores (/data/beads/<agent>) must be group-writable so the
	// dashboard/hub process can mint an issue-sourced epic into an agent's store
	// (e.g. the architect's) even though that dir is owned by the agent's UID.
	// Without this an existing spoke's beads dirs stay 0755 and "Plan this issue"
	// fails with EACCES on beads.json.tmp.
	"/data/beads",
}

// SharedRepoParent is the shared HOME directory into which agents clone the
// target project repos (e.g. /data/home/api-server, /data/home/ui). Each agent's
// per-workspace repo (/data/agents/<agent>/<repo>) is a SYMLINK back to the clone
// here, so all agents share one working tree per repo.
//
// The clone is created dev:node but mode 0755 (setgid `node` gives group `node`,
// but the default umask leaves the group WITHOUT write). That locks every agent
// UID out of writing to the tree: they can read and enumerate issues, but any
// `git checkout -b` / write-a-test-file / `git commit` fails with
// "Permission denied", so ISSUES_AND_PRS agents can open issues but never PRs.
//
// The watcher self-heals this the same way it does .bob: it finds each git-repo
// directory directly under SharedRepoParent (a real, non-symlink child that
// holds a .git) and walks it, bringing dirs to >=0770 and files to >=0660 for
// the shared node group. We scan only repo children — not all of /data/home — to
// avoid walking the large dotdir tree (.cache, .local, node_modules) every tick;
// those are already covered by their own WatchedHomeDirs entries.
//
// A var (not const) so tests can point the scan at a writable temp tree, matching
// GooseLogsDir/WatchedHomeDirs. Production value is unchanged.
var SharedRepoParent = "/data/home"

// GooseLogsDir is the rolling log directory goose 1.37 creates on startup.
// Goose panics if this directory doesn't exist, so the watcher ensures it
// is created at startup with correct permissions.
//
// A var (not const) so tests can point the permissions watcher at a writable
// temp tree (together with WatchedHomeDirs) to exercise ensureWatchedDirs /
// fixPermissions / fixEntry. Production value is unchanged.
var GooseLogsDir = "/data/home/.local/state/goose/logs/cli"

// maxDedupedWarnKeys bounds the failure-dedupe map. The watcher walks agent
// workspaces whose contents churn, so without a cap a long-lived pod with many
// transient unfixable entries would grow the map forever. Hitting the cap
// resets it, which at worst re-logs each still-failing path once at WARN.
const maxDedupedWarnKeys = 4096

// warnDeduper suppresses repeats of an identical per-path repair failure.
//
// The watcher's failure arms fire on EVERY tick for a condition the watcher
// itself cannot clear: an entry it lacks permission to chown/chmod (EPERM
// after the entrypoint's privilege drop) triggers the same warning every
// PermissionFixInterval, indefinitely — six WARN lines every 10 seconds
// dominated a healthy standalone Hive's log (#4488). The deduper keeps the
// first failure per (operation, path) at WARN and demotes identical repeats
// to DEBUG. The entry is cleared when the repair succeeds or the path stops
// needing one, so a NEW failure on the same path warns again; a failure whose
// error text changes also warns again, because it is new information.
type warnDeduper struct {
	mu   sync.Mutex
	seen map[string]string // (op + "\x00" + path) -> last error text
}

// shouldWarn records the failure and reports whether it is new (first time
// this key failed, or the error text changed since last time).
func (d *warnDeduper) shouldWarn(key, errText string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil || len(d.seen) >= maxDedupedWarnKeys {
		d.seen = make(map[string]string)
	}
	if prev, ok := d.seen[key]; ok && prev == errText {
		return false
	}
	d.seen[key] = errText
	return true
}

// clear forgets a key so the next failure on it warns at WARN again. Called
// when a repair succeeds or the path no longer needs one — the condition
// changed, so a future failure is a new finding, not a repeat.
func (d *warnDeduper) clear(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.seen, key)
}

// dedupeKey builds the (operation, path) key. The op is part of the key so a
// path failing both chown and chmod gets one WARN for each distinct problem.
func dedupeKey(op, path string) string { return op + "\x00" + path }

// permWarnDedupe is the watcher's shared dedupe state. Package-level because
// fixEntry keeps its (path, fi, logger) signature; reset by tests via
// resetPermWarnDedupe.
var permWarnDedupe = &warnDeduper{}

// resetPermWarnDedupe restores pristine dedupe state. Tests only.
func resetPermWarnDedupe() { permWarnDedupe = &warnDeduper{} }

// warnDeduped logs a repair failure: WARN the first time this op fails on
// this path with this error, DEBUG on identical repeats. The message text is
// unchanged from the pre-dedupe watcher so operator greps keep working.
func warnDeduped(logger *slog.Logger, op, msg, path string, err error) {
	if permWarnDedupe.shouldWarn(dedupeKey(op, path), err.Error()) {
		logger.Warn(msg,
			"path", path,
			"error", err,
			"note", "identical repeats logged at debug until this changes",
		)
		return
	}
	logger.Debug(msg, "path", path, "error", err)
}

// StartPermissionsWatcher runs a background goroutine that periodically
// scans WatchedHomeDirs and fixes files/directories that were created
// with wrong ownership (e.g., root-owned by Copilot CLI).
//
// It never blocks or panics. Call it once at startup:
//
//	go agent.StartPermissionsWatcher(logger)
func StartPermissionsWatcher(logger *slog.Logger) {
	logger.Info("permissions watcher started",
		"interval", PermissionFixInterval,
		"watched_dirs", WatchedHomeDirs,
		"target_uid", DevUID,
		"target_gid", NodeGID,
	)

	// Ensure watched directories exist with correct ownership on first run.
	ensureWatchedDirs(logger)

	ticker := time.NewTicker(PermissionFixInterval)
	defer ticker.Stop()

	for range ticker.C {
		fixPermissions(logger)
	}
}

// ensureWatchedDirs creates each watched directory if it does not exist
// and sets correct ownership.
func ensureWatchedDirs(logger *slog.Logger) {
	allDirs := append([]string{GooseLogsDir}, WatchedHomeDirs...)
	for _, dir := range allDirs {
		if err := os.MkdirAll(dir, DirPerms|0o070); err != nil {
			warnDeduped(logger, "mkdir", "permissions watcher: failed to create directory", dir, err)
			continue
		}
		permWarnDedupe.clear(dedupeKey("mkdir", dir))
		// Always set ownership on the top-level dir at startup.
		if err := os.Chown(dir, DevUID, NodeGID); err != nil {
			warnDeduped(logger, "chown watched dir", "permissions watcher: failed to chown directory", dir, err)
		} else {
			permWarnDedupe.clear(dedupeKey("chown watched dir", dir))
		}
	}
}

// sharedRepoClones returns the git-repo directories directly under
// SharedRepoParent — a real (non-symlink) child dir whose tree holds a .git.
// These are the project-repo working trees agents share (and symlink into their
// workspaces). Non-repo children (dotdirs, node caches) are skipped so we don't
// walk the whole shared HOME tree on every tick.
//
// SECURITY (CWE-59): children are inspected with os.Lstat and any symlink is
// skipped, so a planted link under /data/home can never redirect the widening
// walk onto a path outside the shared HOME — mirroring the symlink guard in
// fixEntry. Only a genuine directory (created by `git clone`) is returned.
//
// Never errors: an unreadable parent yields an empty list, matching the
// watcher's best-effort contract (it must never abort a tick).
func sharedRepoClones() []string {
	entries, err := os.ReadDir(SharedRepoParent)
	if err != nil {
		return nil
	}
	var repos []string
	for _, e := range entries {
		name := e.Name()
		if len(name) > 0 && name[0] == '.' {
			continue // skip dotdirs — covered by WatchedHomeDirs
		}
		repoPath := filepath.Join(SharedRepoParent, name)
		// Lstat (not Stat) so a symlinked child is seen AS a link and rejected —
		// we must never follow a link out of the shared HOME (CWE-59).
		fi, statErr := os.Lstat(repoPath)
		if statErr != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			continue
		}
		// A real dir that holds a .git is a clone. Lstat the .git too so a
		// planted ".git" symlink can't be used to smuggle a non-repo dir in.
		gitFI, gitErr := os.Lstat(filepath.Join(repoPath, ".git"))
		if gitErr != nil || gitFI.Mode()&os.ModeSymlink != 0 {
			continue
		}
		repos = append(repos, repoPath)
	}
	return repos
}

// fixPermissions walks each watched directory and fixes ownership/mode
// on any file or directory that is wrong. It only logs when it actually
// changes something.
func fixPermissions(logger *slog.Logger) {
	// Widen the shared project-repo clones so agent UIDs (group node) can
	// write/commit/push and therefore open PRs. fixEntry already brings each
	// dir to >=0770 and each file to >=0660 for the node group (and skips
	// symlinks), which is exactly what the clone (created 0755 dev:node) needs.
	fixModeFiles(logger)

	roots := append([]string{}, WatchedHomeDirs...)
	roots = append(roots, sharedRepoClones()...)
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			// Directory doesn't exist yet — create it.
			if os.IsNotExist(err) {
				ensureWatchedDirs(logger)
			}
			continue
		}
		if !info.IsDir() {
			continue
		}

		err = filepath.Walk(root, func(path string, fi os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return nil // skip unreadable entries, don't abort walk
			}
			fixEntry(path, fi, logger)
			return nil
		})
		if err != nil {
			logger.Warn("permissions watcher: walk error",
				"root", root,
				"error", err,
			)
		}
	}
}

// fixModeFiles re-widens the per-agent mode files (ModeFileGlob) to be
// world-readable. This is the running-pod remediation for #3882/#3679: the
// Manager only rewrites a mode file on a level change or kick, so a pod whose
// files are 0600 (written by the #3172 build, or umask-narrowed) stays broken
// until then; this scan repairs them within one tick.
//
// SECURITY: /tmp is sticky and world-writable, so an agent can pre-create a
// name matching the glob. Only regular files owned by the hive uid are
// touched, the only change ever made is adding read bits, and the whole
// check-then-chmod happens on one open descriptor: O_NOFOLLOW refuses a
// planted symlink at open time, and f.Stat/f.Chmod act on that same inode, so
// the pathname cannot be swapped between the check and the chmod (CWE-59 /
// CWE-367 — the same descriptor discipline writeAgentStateFile follows, #3175).
func fixModeFiles(logger *slog.Logger) {
	for _, glob := range []string{ModeFileGlob, CapsFileGlob} {
		paths, err := filepath.Glob(glob)
		if err != nil {
			continue
		}
		for _, path := range paths {
			fixModeFile(path, logger)
		}
	}
}

// fixModeFile applies the fixModeFiles policy to a single path.
func fixModeFile(path string, logger *slog.Logger) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		// ELOOP for a planted symlink, EACCES for a file we do not own: skip.
		return
	}
	defer func() { _ = f.Close() }() // read-only fd; nothing to lose on close error
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return
	}
	perm := fi.Mode().Perm()
	if perm&modeFileReadBits == modeFileReadBits {
		permWarnDedupe.clear(dedupeKey("chmod mode file", path))
		return
	}
	newPerm := perm | modeFileReadBits
	if err := f.Chmod(newPerm); err != nil {
		if permWarnDedupe.shouldWarn(dedupeKey("chmod mode file", path), err.Error()) {
			logger.Warn("permissions watcher: chmod mode file failed; agents cannot read their GitHub mode and gh/pushes will be blocked",
				"path", path,
				"old_mode", perm.String(),
				"want_mode", newPerm.String(),
				"error", err,
				"note", "identical repeats logged at debug until this changes",
			)
		} else {
			logger.Debug("permissions watcher: chmod mode file failed",
				"path", path,
				"error", err,
			)
		}
		return
	}
	permWarnDedupe.clear(dedupeKey("chmod mode file", path))
	logger.Info("permissions watcher: fixed mode file permissions",
		"path", path,
		"old_mode", perm.String(),
		"new_mode", newPerm.String(),
	)
}

// fixEntry checks a single file or directory and corrects ownership/mode
// if needed.
func fixEntry(path string, fi os.FileInfo, logger *slog.Logger) {
	// SECURITY (audit F12, CWE-59): never act on a symlink. This walks the
	// agent HOME dirs, which agents can write to, and both os.Chmod and
	// os.Chown FOLLOW symlinks — so a planted link would redirect this
	// repair loop onto a file outside the tree. filepath.Walk reports
	// entries via Lstat, so the link is visible as a link here; the ownership
	// check below reads the LINK's metadata, not the target's, and so cannot
	// be relied on to catch this. Mirrors ensureWorldWritable's symlink skip.
	if fi.Mode()&os.ModeSymlink != 0 {
		return
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}

	// Fix group ownership if not in the node group — all agents share this group.
	// Also fix root-owned files (uid 0) to the dev user.
	needsChown := false
	newUID := int(stat.Uid)
	if stat.Uid == 0 {
		newUID = DevUID
		needsChown = true
	}
	if stat.Gid != uint32(NodeGID) {
		needsChown = true
	}
	if needsChown {
		if err := os.Chown(path, newUID, NodeGID); err != nil {
			warnDeduped(logger, "chown", "permissions watcher: chown failed", path, err)
		} else {
			permWarnDedupe.clear(dedupeKey("chown", path))
			logger.Warn("permissions watcher: fixed ownership",
				"path", path,
				"was_uid", stat.Uid,
				"was_gid", stat.Gid,
				"new_uid", newUID,
				"new_gid", NodeGID,
			)
		}
	} else {
		// Ownership is correct (or was fixed externally): forget any recorded
		// failure so a future regression on this path warns at WARN again.
		permWarnDedupe.clear(dedupeKey("chown", path))
	}

	// bob state dirs (.bob) are the one class of directory the watcher must
	// widen even though they are owned by a hive-<agent> UID rather than
	// DevUID: bob runs as that agent's UID through the shared `node` group, so
	// a 0755 dir leaves the group without write and bob logs "error saving your
	// latest settings changes" / "Failed to initialize logger" and runs
	// degraded. verifyBobStateDirsWritable only DETECTS this; here we actually
	// fix it, on every tick and at startup, so the fleet self-heals instead of
	// staying degraded until an operator hand-chmods the pod. Handled before
	// the general owner guard below precisely because that guard would skip
	// these non-DevUID dirs. Only the group r/w/x bits are added — owner and
	// other bits are preserved — so an already-770 dir is left untouched.
	if fi.IsDir() && filepath.Base(path) == bobStateDirBase {
		fixBobStateDirGroupWrite(path, fi.Mode(), logger)
	}

	// Only fix permissions on files we own or just chowned.
	// Skipping files owned by other users avoids "operation not permitted"
	// spam when agents create files as their own users.
	if newUID != DevUID && stat.Uid != uint32(DevUID) {
		return
	}

	mode := fi.Mode()
	if fi.IsDir() {
		// Directories need u+rwx to be usable.
		if mode.Perm()&DirPerms != DirPerms {
			newMode := mode.Perm() | DirPerms
			if err := os.Chmod(path, newMode); err != nil {
				warnDeduped(logger, "chmod dir", "permissions watcher: chmod dir failed", path, err)
			} else {
				permWarnDedupe.clear(dedupeKey("chmod dir", path))
				logger.Warn("permissions watcher: fixed directory permissions",
					"path", path,
					"old_mode", mode.Perm().String(),
					"new_mode", newMode.String(),
				)
			}
		} else {
			permWarnDedupe.clear(dedupeKey("chmod dir", path))
		}
	} else {
		// Regular files need u+rw to be readable/writable.
		if mode.Perm()&FilePerms != FilePerms {
			newMode := mode.Perm() | FilePerms
			if err := os.Chmod(path, newMode); err != nil {
				warnDeduped(logger, "chmod file", "permissions watcher: chmod file failed", path, err)
			} else {
				permWarnDedupe.clear(dedupeKey("chmod file", path))
				logger.Warn("permissions watcher: fixed file permissions",
					"path", path,
					"old_mode", mode.Perm().String(),
					"new_mode", newMode.String(),
				)
			}
		} else {
			permWarnDedupe.clear(dedupeKey("chmod file", path))
		}
	}
}

// fixBobStateDirGroupWrite ensures a bob state directory (.bob) grants the
// shared `node` group read+write+execute, so an agent UID reaching it through
// that group can persist bob's settings, installation_id and .bob-errors/
// logger target. It ORs bobStateDirGroupRWX into whatever the dir already has —
// never narrowing owner or other bits — so a dir that is already group-writable
// (>= 0770) is left byte-identical and the watcher stays idempotent.
//
// This is the remediation that verifyBobStateDirsWritable only ever DETECTED:
// production .bob dirs are created 0755 (r-x for the group) by their
// hive-<agent> owner, which is why bob logged "error saving your latest
// settings changes" / "Failed to initialize logger" fleet-wide and ran
// degraded. The chmod runs on every watcher tick and at startup, so the fix
// self-heals rather than requiring a hand-chmod on the pod.
//
// Safe and non-fatal: a chmod we are not permitted to perform (EPERM — we are
// neither the owner nor privileged) is logged and swallowed, exactly like the
// watcher's other chmod/chown arms, so it never blocks or panics.
func fixBobStateDirGroupWrite(path string, mode os.FileMode, logger *slog.Logger) {
	perm := mode.Perm()
	// Nothing to do when the group already has all of r/w/x — this keeps an
	// already-remediated (>= 0770) dir untouched and the walk idempotent.
	if perm&bobStateDirGroupRWX == bobStateDirGroupRWX {
		permWarnDedupe.clear(dedupeKey("chmod bob state dir", path))
		return
	}
	newPerm := perm | bobStateDirGroupRWX
	if err := os.Chmod(path, newPerm); err != nil {
		if permWarnDedupe.shouldWarn(dedupeKey("chmod bob state dir", path), err.Error()) {
			logger.Error("permissions watcher: chmod bob state dir failed; bob will keep reporting 'error saving your latest settings changes' and run degraded",
				"path", path,
				"old_mode", perm.String(),
				"want_mode", newPerm.String(),
				"error", err,
				"note", "identical repeats logged at debug until this changes",
			)
		} else {
			logger.Debug("permissions watcher: chmod bob state dir failed",
				"path", path,
				"error", err,
			)
		}
		return
	}
	permWarnDedupe.clear(dedupeKey("chmod bob state dir", path))
	logger.Info("permissions watcher: fixed bob state dir permissions",
		"path", path,
		"old_mode", perm.String(),
		"new_mode", newPerm.String(),
	)
}
