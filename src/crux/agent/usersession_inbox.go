package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux"
)

// Inbox seed files (POST /api/sessions/{id}/seed). The dashboard sends Inbox
// items into a session as markdown files under <cwd>/.hive/inbox, where cwd is
// the session's shared workspace checkout (or its own work dir when it has
// none). Like an upload, the file is written AS THE SESSION'S UID through the
// su-exec helper, never as the hive's uid into a directory the session
// controls: a symlink planted by any session uid only reaches what that uid
// can already reach, and the script refuses one at .hive or .hive/inbox.
const (
	// userSessionInboxDir is the seed directory relative to the session cwd.
	userSessionInboxDir = ".hive/inbox"
	// userSessionInboxFileMode is the seed file mode: the session uid reads
	// and writes it, the sessions group (every session of the workspace)
	// reads it, nobody else.
	userSessionInboxFileMode = "0640"
	// userSessionInboxSymlinkExit is the script's exit status for a symlink
	// at .hive or .hive/inbox.
	userSessionInboxSymlinkExit     = 76
	userSessionInboxSymlinkExitText = "76"
)

// userSessionInboxScript (run as the session uid:sessions gid): $1 the
// session cwd, $2 the file name, $3 the sessions gid; the content arrives on
// stdin. Directories are created group-writable (umask 007) so every session
// of a shared workspace can add its own files (the workspace's setgid bit
// keeps the sessions group); a `.gitignore` of "*" keeps the transcripts out
// of commits. The file is written to an exclusive temp name, set to 0640 with
// the sessions group and renamed over any earlier copy, so a re-send replaces
// it and a reader never sees half a file.
const userSessionInboxScript = `set -e
ws=$1; name=$2; g=$3
d="$ws/` + userSessionInboxDir + `"
umask 007
for p in "$ws/.hive" "$d"; do
  if [ -L "$p" ]; then exit ` + userSessionInboxSymlinkExitText + `; fi
done
mkdir -p -- "$d"
for p in "$ws/.hive" "$d"; do
  if [ -L "$p" ]; then exit ` + userSessionInboxSymlinkExitText + `; fi
done
if [ ! -e "$d/.gitignore" ]; then printf '*\n' > "$d/.gitignore" || true; fi
t="$d/.$name.$$.tmp"
set -C
cat > "$t"
set +C
chgrp -- "$g" "$t"
chmod ` + userSessionInboxFileMode + ` -- "$t"
mv -f -- "$t" "$d/$name"`

// userSessionInboxCwd validates the working directory a seed is written
// under: "" is the session's own work dir; anything else must be exactly a
// workspace checkout path (<workspace root>/<org>/<repo>).
func userSessionInboxCwd(id, workspace string) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return userSessionWorkDir(id), nil
	}
	clean := filepath.Clean(workspace)
	rel, err := filepath.Rel(userSessionWorkspaceRoot(), clean)
	if err != nil {
		return "", fmt.Errorf("workspace %q is not a session workspace", workspace)
	}
	org, name, ok := strings.Cut(filepath.ToSlash(rel), "/")
	if !ok || !userSessionRepoPartPattern.MatchString(org) || !userSessionRepoPartPattern.MatchString(name) || clean != userSessionWorkspaceDir(org, name) {
		return "", fmt.Errorf("workspace %q is not a session workspace", workspace)
	}
	return clean, nil
}

// StoreInboxFile writes data as <cwd>/.hive/inbox/<fileName>, as the session
// uid, owned by that uid and the sessions group with mode 0640, and returns
// its absolute path. workspace is the session's workspace checkout ("" for a
// session without one). fileName must be a plain name.
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
	out, err := r.priv(ctx, userSessionSpec(uid), bytes.NewReader(data), "sh", "-c", userSessionInboxScript, "sh", cwd, fileName, strconv.Itoa(userSessionGID()))
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == userSessionInboxSymlinkExit {
			return "", crux.ErrUserSessionInboxSymlink
		}
		return "", outputErr("storing inbox file", err, out)
	}
	return filepath.Join(cwd, userSessionInboxDir, fileName), nil
}
