package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/crux"
)

// Inbox seed files (POST /api/sessions/{id}/seed). The dashboard sends Inbox
// items into a session as markdown files under <cwd>/.hive/inbox, where cwd is
// the session's worktree of the shared workspace clone (or its own work dir
// when it has none). Like an upload, the file is written AS THE SESSION'S UID
// through the su-exec helper, never as the hive's uid into a directory the
// session controls: a symlink planted by any session uid only reaches what
// that uid can already reach, and the script refuses one at .hive or
// .hive/inbox.
const (
	// userSessionInboxDir is the seed directory relative to the session cwd.
	userSessionInboxDir = ".hive/inbox"
	// userSessionInboxSymlinkExit is the script's exit status for a symlink
	// at .hive or .hive/inbox, or a directory or symlink at the target name.
	userSessionInboxSymlinkExit     = 76
	userSessionInboxSymlinkExitText = "76"
)

// userSessionInboxScript (run as the session uid:sessions gid): $1 the
// session cwd, $2 the file name; the content arrives on stdin. Directories are
// created group-writable (umask 007) so every session of a shared workspace
// can add its own files; a `.gitignore` of "*" keeps the transcripts out of
// commits. The file is created exclusively (noclobber, which refuses a
// symlink) under umask 027, so it is 0640 from birth with the group the
// directory hands down (the sessions group in the setgid workspace), then
// renamed over any earlier copy. Nothing is chmod-ed or chgrp-ed by path
// afterwards: in a group-writable directory a peer session could swap the
// name for a symlink between the write and such a call. A symlink at .hive or
// .hive/inbox, or a directory at the target name, is refused.
const userSessionInboxScript = `set -e
ws=$1; name=$2
d="$ws/` + userSessionInboxDir + `"
umask 007
for p in "$ws/.hive" "$d"; do
  if [ -L "$p" ]; then exit ` + userSessionInboxSymlinkExitText + `; fi
done
mkdir -p -- "$d"
for p in "$ws/.hive" "$d"; do
  if [ -L "$p" ]; then exit ` + userSessionInboxSymlinkExitText + `; fi
done
set -C
if [ ! -e "$d/.gitignore" ] && [ ! -L "$d/.gitignore" ]; then printf '*\n' > "$d/.gitignore" 2>/dev/null || true; fi
umask 027
t="$d/.$name.$$.tmp"
cat > "$t"
if [ -d "$d/$name" ] || [ -L "$d/$name" ]; then rm -f -- "$t"; exit ` + userSessionInboxSymlinkExitText + `; fi
mv -f -- "$t" "$d/$name"`

// userSessionInboxCwd validates the working directory a seed is written
// under: "" is the session's own work dir; anything else must be exactly this
// session's worktree (<workspace root>/.sessions/<id>/<org>/<repo>) or, for a
// session recorded before per-session worktrees, the shared clone
// (<workspace root>/<org>/<repo>) it still runs in.
func userSessionInboxCwd(id, workspace string) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return userSessionWorkDir(id), nil
	}
	clean := filepath.Clean(workspace)
	org, name := filepath.Base(filepath.Dir(clean)), filepath.Base(clean)
	if !userSessionRepoPartPattern.MatchString(org) || !userSessionRepoPartPattern.MatchString(name) ||
		(clean != userSessionWorktreeDir(id, org, name) && clean != userSessionWorkspaceDir(org, name)) {
		return "", fmt.Errorf("workspace %q is not a session workspace", workspace)
	}
	return clean, nil
}

// StoreInboxFile writes data as <cwd>/.hive/inbox/<fileName>, as the session
// uid, owned by that uid with mode 0640 and the directory's group (the
// sessions group in the setgid worktree), and returns its absolute path.
// workspace is the session's worktree ("" for a session without one).
// fileName must be a plain name.
func (r *UserSessionRuntime) StoreInboxFile(id, workspace, fileName string, data []byte) (string, error) {
	if err := checkUserSessionID(id); err != nil {
		return "", err
	}
	if len(fileName) > userSessionUploadNameMax || !userSessionUploadNamePattern.MatchString(fileName) {
		return "", fmt.Errorf("invalid inbox file name")
	}
	cwd, err := userSessionInboxCwd(id, workspace)
	if err != nil {
		return "", err
	}
	uid, err := r.sessionUID(id)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), userSessionUploadTimeout)
	defer cancel()
	out, err := r.priv(ctx, userSessionSpec(uid), bytes.NewReader(data), "sh", "-c", userSessionInboxScript, "sh", cwd, fileName)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == userSessionInboxSymlinkExit {
			return "", crux.ErrUserSessionInboxSymlink
		}
		return "", outputErr("storing inbox file", err, out)
	}
	return filepath.Join(cwd, userSessionInboxDir, fileName), nil
}
