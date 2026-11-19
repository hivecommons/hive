package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/sharedfile"
)

// Removing a session's files.
//
// The failure this closes (#18767). The sandbox was deleted with the
// 10-second userSessionCmdTimeout that bounds a tmux call. A HOME holding a
// CLI's caches takes longer than that to delete (a minute was seen), so the
// timeout fired, the cancel of the root child could not run (see
// userSessionRootKillScript), `rm` finished by itself, and the caller was
// handed the cancel's error for a removal that had succeeded: the files were
// gone and the session record was kept.
//
// Two rules follow. A removal has its own, generous deadline. And what decides
// whether a tree was removed is the tree: a command that reports an error
// after the directory is gone has removed it.

// userSessionRemoveTimeout bounds deleting one session tree (the sandbox, the
// tmux socket dir). It is a ceiling for a wedged filesystem, not an estimate.
const userSessionRemoveTimeout = 15 * time.Minute

// removeTree deletes dir as root. An error from a command that left nothing
// behind is logged and dropped: the directory is gone, which is what was
// asked for.
func (r *UserSessionRuntime) removeTree(what, dir string) error {
	out, err := r.privRunFor(userSessionRemoveTimeout, userSessionRootSpec, nil, "rm", "-rf", "--one-file-system", "--", dir)
	if err == nil {
		return nil
	}
	if _, serr := os.Lstat(dir); errors.Is(serr, fs.ErrNotExist) {
		r.logger.Info("user session removal: the command reported an error, but the directory is gone; treated as removed", "what", what, "path", dir, "error", err)
		return nil
	}
	return outputErr(what, err, out)
}

// Remove ends the session and deletes everything it owned: its worktree of the
// shared workspace clone (uncommitted work is auto-saved onto its branch first,
// see userSessionWorktreeRemoveScript), the sandbox (home, work dir, uploads,
// tmp) and the uid's tmux socket dir, so a later session that is allocated the
// same uid starts clean. Directories earlier releases created for the id are
// removed too. It is safe to call again after any failure, and on a session
// that is already partly or wholly gone. A failure is returned and the caller
// must keep the session record (and its uid slot) so the removal is retried.
//
// The order puts what can refuse first and what cannot be undone last: the
// login check removes nothing, the worktree step keeps the work on a branch
// (a resume gets the worktree back from it), and only then is the sandbox
// deleted.
func (r *UserSessionRuntime) Remove(id string) error {
	if err := checkUserSessionID(id); err != nil {
		return err
	}
	uid, uidErr := r.sessionUID(id)
	if uidErr == nil {
		if err := r.killAs(uid, id); err != nil {
			r.logger.Warn("user session kill before removal failed", "id", id, "error", err)
		}
	}
	// The session's CLI is gone (killed above), so its login can no longer
	// change. If that login is the newest one on this hive, it goes into the
	// shared file BEFORE the sandbox is deleted: removing a session must never
	// remove the only valid login. "Nothing to adopt" (no file, an expired
	// copy, a copy without a refresh token, a file that is not a credential)
	// is not a failure and costs two stats and one small read.
	// (The CLI creates files and directories with private modes; the hive
	// must be able to look at the login path to judge it.)
	r.makeHomeReadable(id)
	if err := keepSessionForLogin(r.syncClaudeLogin(id, sessionLoginTriggerRemove, sessionLoginModeRemove)); err != nil {
		return err
	}
	// The same for a Codex or OpenCode login the session's CLI refreshed
	// (usersession_login_rotating.go).
	for _, l := range rotatingSessionLogins {
		if err := keepSessionForLogin(r.syncRotatingLogin(id, sessionLoginTriggerRemove, l, sessionLoginModeRemove)); err != nil {
			return err
		}
	}
	r.workspaceMu.Lock()
	err := r.removeWorktrees(id)
	r.workspaceMu.Unlock()
	if err != nil {
		return fmt.Errorf("removing session worktree: %w", err)
	}
	dir := userSessionSandboxDir(id)
	if _, err := os.Lstat(dir); err == nil {
		if err := r.removeTree("removing session sandbox", dir); err != nil {
			return err
		}
	}
	if uidErr == nil {
		if err := r.removeTree("removing session tmux socket dir", userSessionTmuxSocketDir(uid)); err != nil {
			return err
		}
	}
	for _, p := range legacyUserSessionPaths(id) {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("removing legacy session dir %s: %w", p, err)
		}
	}
	return nil
}

// keepSessionForLogin turns the outcome of a login check made before a
// removal into the error that stops it, nil when the removal may go on.
func keepSessionForLogin(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sharedfile.ErrTooLarge):
		// Over the size of any credentials file: not a login, so there is
		// nothing to keep. The check logged it.
		return nil
	case errors.Is(err, crux.ErrUserSessionHoldsLogin):
		return err
	default:
		return fmt.Errorf("this session's sign-in could not be checked, so its files are kept: %w", err)
	}
}

// Removed reports whether nothing of session id is left on disk: no sandbox,
// no worktrees and none of the directories earlier releases made. It answers
// false when that cannot be told (the sandbox root itself is missing or
// unreadable, as on a volume that is not mounted), so a caller that drops
// records of removed sessions never drops one whose files it merely cannot
// see.
func (r *UserSessionRuntime) Removed(id string) bool {
	if checkUserSessionID(id) != nil {
		return false
	}
	if fi, err := os.Stat(userSessionSandboxRoot()); err != nil || !fi.IsDir() {
		return false
	}
	paths := append([]string{userSessionSandboxDir(id), userSessionWorktreesDir(id)}, legacyUserSessionPaths(id)...)
	for _, p := range paths {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}
