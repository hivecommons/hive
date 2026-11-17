package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Per-session filesystem layout. Every session gets a sandbox directory of its
// own, owned by the session's reserved uid (usersession_priv.go), holding the
// session's HOME, working directory (uploads live beneath it) and TMPDIR:
//
//	<sandbox root>/<id>/        root:<reader group> 0751, never writable by the session
//	<sandbox root>/<id>/home    <session uid>:<reader group> 2750
//	<sandbox root>/<id>/work    <session uid>:<reader group> 2750
//	<sandbox root>/<id>/tmp     <session uid>:<reader group> 2750
//
// A session never shares a HOME with another session or with an agent. Vendor
// LOGINS are NOT symlinked from the shared home any more: a symlink gave the
// session write access to the shared file (and, for settings.json, a hook that
// every agent would run). Instead a fixed allowlist of the minimal login
// files per vendor (userSessionLoginFiles) is COPIED into the session HOME as
// read-only (0400), session-owned files; the Claude login is copied without
// its refresh token, so a session can never rotate the fleet's login
// (usersession_login.go). Nothing that can execute a command
// (settings.json, hooks, MCP/extension config) is ever copied, and ~/.config
// (gh's hosts.yml, goose extensions) is not bridged at all.
const (
	userSessionHomeDirName = "home"
	userSessionWorkDirName = "work"
	userSessionTmpDirName  = "tmp"
	// userSessionsDirEnv relocates the sessions data root (default
	// /data/sessions), shared with the dashboard store. The runtime only
	// uses it to clean up the layout older releases created there.
	userSessionsDirEnv     = "HIVE_SESSIONS_DIR"
	defaultUserSessionsDir = "/data/sessions"
	// legacyUserSessionHomeDir is the sharedAgentHome child older releases
	// used for session homes.
	legacyUserSessionHomeDir = "sessions"

	// userSessionLoginMaxBytes bounds one copied login file.
	userSessionLoginMaxBytes = 1 << 20
	// userSessionLoginFileMode is the mode a copied login file ends up with.
	userSessionLoginFileMode = "0400"
)

// userSessionLoginFiles are the ONLY shared-home files copied into a session,
// relative to the home. Each is a bare credential or account record for one
// vendor. Deliberately absent: .claude/settings.json (hooks run as the CLI),
// .bob/settings*, .gemini/settings.json, ~/.config/{goose,opencode,aider,gh}
// (extension commands, provider config, the GitHub token) and agy's
// antigravity-cli directory (a mutable token+cache tree). A vendor missing
// here simply starts logged out in a session.
var userSessionLoginFiles = []string{
	".claude/.credentials.json",
	".copilot/config.json",
	".gemini/oauth_creds.json",
	".gemini/google_accounts.json",
	".codex/auth.json",
	".local/share/opencode/auth.json",
}

// userSessionMkdirScript creates directories (as the session uid) with the
// group-readable mode the hive's reader group needs; $@ are the directories.
const userSessionMkdirScript = `umask 027; mkdir -p -- "$@"`

// userSessionCopyLoginScript writes stdin to $1 (replacing any previous copy)
// as a read-only file. The bytes go to a temporary beside the destination and
// are renamed over it, so a CLI reading the login never sees it missing or
// half written. set -C makes the redirect fail instead of following a symlink
// or overwriting; a directory at the destination is refused rather than
// moved into.
const userSessionCopyLoginScript = `set -e; umask 077; if [ -d "$1" ] && [ ! -L "$1" ]; then exit 1; fi; t="$1.hive-new"; rm -f -- "$t"; set -C; cat > "$t"; chmod ` + userSessionLoginFileMode + ` -- "$t"; mv -f -- "$t" "$1"`

// userSessionPrepareScript (run as root) creates the sandbox dir root-owned and
// not writable by the session, then the three session-owned children. $1 is
// the sandbox dir, $2 the session uid, $3 the reader gid.
const userSessionPrepareScript = `set -e
d=$1; u=$2; g=$3
mkdir -p -- "$d"
chown root:"$g" -- "$d"
chmod 0751 -- "$d"
for n in ` + userSessionHomeDirName + ` ` + userSessionWorkDirName + ` ` + userSessionTmpDirName + `; do
  mkdir -p -- "$d/$n"
  chown -h "$u:$g" -- "$d/$n"
  chmod 2750 -- "$d/$n"
done`

// userSessionsDir is the data root holding sessions.json (and, in older
// releases, per-session work dirs).
func userSessionsDir() string {
	if v := os.Getenv(userSessionsDirEnv); v != "" {
		return v
	}
	return defaultUserSessionsDir
}

func userSessionHome(id string) string {
	return filepath.Join(userSessionSandboxDir(id), userSessionHomeDirName)
}

func userSessionCodexHome(id string) string {
	return filepath.Join(userSessionHome(id), ".codex")
}

func userSessionWorkDir(id string) string {
	return filepath.Join(userSessionSandboxDir(id), userSessionWorkDirName)
}

func userSessionTmpDir(id string) string {
	return filepath.Join(userSessionSandboxDir(id), userSessionTmpDirName)
}

// legacyUserSessionPaths are the dev-owned directories earlier releases
// created for a session id.
func legacyUserSessionPaths(id string) []string {
	return []string{
		filepath.Join(sharedAgentHome, legacyUserSessionHomeDir, id),
		filepath.Join(userSessionsDir(), id),
	}
}

// prepareSandbox creates the session's sandbox owned by uid, creates the
// directories the CLI expects inside the HOME as that uid, and copies in the
// allowlisted vendor login files.
func (r *UserSessionRuntime) prepareSandbox(id string, uid int) error {
	gid, err := userSessionReaderGID()
	if err != nil {
		return err
	}
	root := userSessionSandboxRoot()
	if err := os.MkdirAll(root, userSessionSandboxRootMode); err != nil {
		return fmt.Errorf("creating session sandbox root %s: %w", root, err)
	}
	if err := os.Chmod(root, userSessionSandboxRootMode); err != nil {
		return fmt.Errorf("securing session sandbox root %s: %w", root, err)
	}
	dir := userSessionSandboxDir(id)
	if out, err := r.privRun(userSessionRootSpec, nil, "sh", "-c", userSessionPrepareScript, "sh", dir, fmt.Sprint(uid), fmt.Sprint(gid)); err != nil {
		return outputErr("creating session sandbox "+dir, err, out)
	}
	home := userSessionHome(id)
	dirs := []string{
		filepath.Join(home, ".config"),
		agentXDGDataHome(home),
		agentXDGStateHome(home),
		userSessionCodexHome(id),
	}
	for _, rel := range userSessionLoginFiles {
		dirs = append(dirs, filepath.Dir(filepath.Join(home, rel)))
	}
	args := append([]string{"sh", "-c", userSessionMkdirScript, "sh"}, dirs...)
	if out, err := r.privRun(userSessionSpec(uid), nil, args...); err != nil {
		return outputErr("creating session dirs", err, out)
	}
	r.copyLoginFiles(id, uid)
	return nil
}

// copyLoginFiles brings the session's login copies up to date. A missing
// source is skipped (that vendor was never signed in fleet-wide); a failed
// copy is logged and skipped so one unreadable login cannot block the session.
//
// The Claude login is not a plain copy: the session gets it without the
// refresh token, and a newer login already in the session HOME is adopted
// into the shared file first (usersession_login.go).
//
// For every other vendor the copy is skipped when the session's file was
// written after the shared one. That is the state of an unchanged copy from
// an earlier start, and also of a login the CLI in the session rewrote itself
// (a token refresh): re-copying an older shared file over it on a resume
// would destroy the only current login. A shared file that changed since
// (a new fleet sign-in) is newer than the copy and is copied.
func (r *UserSessionRuntime) copyLoginFiles(id string, uid int) {
	home := userSessionHome(id)
	for _, rel := range userSessionLoginFiles {
		if rel == userSessionClaudeLoginRel {
			// Reported (log and audit) inside; a failure must not block the
			// session, which then starts on whatever copy it has.
			_ = r.syncClaudeLogin(id, sessionLoginTriggerStart, sessionLoginModeStart)
			continue
		}
		src := filepath.Join(sharedAgentHome, rel)
		dest := filepath.Join(home, rel)
		if sessionLoginCopyIsCurrent(src, dest) {
			continue
		}
		data, ok := readLoginSource(src)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), userSessionCmdTimeout)
		out, err := r.priv(ctx, userSessionSpec(uid), strings.NewReader(data), "sh", "-c", userSessionCopyLoginScript, "sh", dest)
		cancel()
		if err != nil {
			r.logger.Warn("user session login copy failed", "id", id, "file", rel, "error", outputErr("copy", err, out))
		}
	}
}

// sessionLoginCopyIsCurrent reports whether the session already holds a login
// file at dest written after the shared file src, so src must not replace it.
func sessionLoginCopyIsCurrent(src, dest string) bool {
	di, err := os.Lstat(dest)
	if err != nil || !di.Mode().IsRegular() {
		return false
	}
	si, err := os.Stat(src)
	if err != nil {
		return false
	}
	return di.ModTime().After(si.ModTime())
}

// readLoginSource reads one shared login file: a regular file (symlinks are
// resolved, directories refused) no larger than userSessionLoginMaxBytes.
func readLoginSource(path string) (string, bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > userSessionLoginMaxBytes {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, userSessionLoginMaxBytes+1))
	if err != nil || len(b) > userSessionLoginMaxBytes {
		return "", false
	}
	return string(b), true
}

// userSessionEnvPairs are the per-session variables set on the tmux session
// after the inherited credentials were stripped.
func userSessionEnvPairs(id, scrubScript string) [][2]string {
	home := userSessionHome(id)
	pairs := [][2]string{
		{"HOME", home},
		{"CODEX_HOME", userSessionCodexHome(id)},
		{"XDG_CONFIG_HOME", filepath.Join(home, ".config")},
		{"XDG_DATA_HOME", agentXDGDataHome(home)},
		{"XDG_STATE_HOME", agentXDGStateHome(home)},
		{"TMPDIR", userSessionTmpDir(id)},
		{"HIVE_SESSION_ID", id},
	}
	if scrubScript != "" {
		// Sourced by every non-interactive bash the CLI spawns; unsets any
		// GitHub token the CLI re-exports into its tool shells (#4045).
		pairs = append(pairs, [2]string{"BASH_ENV", scrubScript}, [2]string{"ENV", scrubScript})
	}
	return pairs
}
