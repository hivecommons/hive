package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Per-session uid isolation. A session's tmux server and CLI run as an
// unprivileged uid taken from a RESERVED range that agents (HIVE_UID_BASE,
// default 2001, allocated upward) and the hive itself (dev, 1001) never use.
// The hive reaches that uid the same way it reaches an agent's: the SUID
// su-exec helper (root:hive-launch 4750, exec-able by the hive, not by any
// agent or session uid). The session uid is deliberately NOT in group node
// (every agent is) and NOT in hive-launch, so it cannot read /secrets/*.pem,
// /data/secrets, /data/.hive, the hive's /proc/<pid>/environ, or exec su-exec.
//
// The slot a session occupies is allocated by the dashboard store, persisted in
// the session record and passed in on every start; the uid is base+slot-1. The
// runtime recovers the uid for later calls from the OWNER of the session's home
// directory, so the filesystem stays the single source of truth.
const (
	userSessionUIDBaseEnv  = "HIVE_SESSION_UID_BASE"
	userSessionUIDSlotsEnv = "HIVE_SESSION_UID_SLOTS"
	// defaultUserSessionUIDBase sits far above the agent range (2001+).
	defaultUserSessionUIDBase  = 20001
	defaultUserSessionUIDSlots = 8192
	// userSessionMinUIDBase refuses a base inside the range agents grow into.
	userSessionMinUIDBase = 10000
	// agentUIDBaseEnv is the entrypoint's agent uid base knob.
	agentUIDBaseEnv = "HIVE_UID_BASE"

	userSessionSandboxDirEnv     = "HIVE_SESSIONS_SANDBOX_DIR"
	defaultUserSessionSandboxDir = "/data/session-sandbox"
	// userSessionSandboxRootMode lets the session uid traverse (not list) the
	// sandbox root to reach its own, unguessable-id directory.
	userSessionSandboxRootMode = 0o711

	// userSessionReaderGIDEnv / userSessionReaderGroup name the group that may
	// READ session sandboxes: the hive's own launcher group. Agents and
	// sessions are not members.
	userSessionReaderGIDEnv = "HIVE_SESSIONS_READER_GID"
	userSessionReaderGroup  = "hive-launch"

	userSessionRootSpec = "root"
	userSuExecBinary    = "su-exec"
	// userSessionUploadFullExit is the exit status the upload script uses for
	// "this session already holds the maximum number of files".
	userSessionUploadFullExit     = 75
	userSessionUploadFullExitText = "75"
)

func envPositiveInt(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func userSessionUIDBase() int {
	return envPositiveInt(userSessionUIDBaseEnv, defaultUserSessionUIDBase)
}
func userSessionUIDSlots() int {
	return envPositiveInt(userSessionUIDSlotsEnv, defaultUserSessionUIDSlots)
}

// userSessionUIDInRange reports whether uid is inside the reserved range.
func userSessionUIDInRange(uid int) bool {
	base := userSessionUIDBase()
	return uid >= base && uid < base+userSessionUIDSlots()
}

// userSessionUID maps a 1-based slot to its uid and refuses any configuration
// in which that uid could be an agent's, the proxy's, the hive's or root's.
func userSessionUID(slot int) (int, error) {
	slots := userSessionUIDSlots()
	if slot < 1 || slot > slots {
		return 0, fmt.Errorf("session uid slot %d is outside 1..%d", slot, slots)
	}
	base := userSessionUIDBase()
	if base < userSessionMinUIDBase {
		return 0, fmt.Errorf("%s=%d is below %d, inside the range agent uids are allocated from", userSessionUIDBaseEnv, base, userSessionMinUIDBase)
	}
	if base <= envPositiveInt(agentUIDBaseEnv, baseAgentUID) {
		return 0, fmt.Errorf("%s=%d must be above %s", userSessionUIDBaseEnv, base, agentUIDBaseEnv)
	}
	uid := base + slot - 1
	if uid == 0 || uid == proxyUserUID {
		return 0, fmt.Errorf("session uid %d is reserved", uid)
	}
	for _, path := range []string{PersistedUIDMapPath, UIDMapPath} {
		m, err := LoadUIDMap(path)
		if err != nil {
			continue
		}
		for name, agentUID := range m.Agents {
			if agentUID >= base && agentUID < base+slots {
				return 0, fmt.Errorf("agent %q uses uid %d inside the reserved session range %d..%d", name, agentUID, base, base+slots-1)
			}
		}
	}
	return uid, nil
}

// userSessionSpec is the su-exec identity for a session uid: its own private
// group, never the hive's gid (which every agent shares).
func userSessionSpec(uid int) string { return fmt.Sprintf("%d:%d", uid, uid) }

func userSessionSandboxRoot() string {
	if v := os.Getenv(userSessionSandboxDirEnv); v != "" {
		return v
	}
	return defaultUserSessionSandboxDir
}

func userSessionSandboxDir(id string) string {
	return filepath.Join(userSessionSandboxRoot(), id)
}

// userSessionReaderGID resolves the group allowed to read sandboxes. It fails
// closed: without it the hive could not read transcripts for resume, and
// falling back to a broader group (node) would open them to agents.
func userSessionReaderGID() (int, error) {
	if v := strings.TrimSpace(os.Getenv(userSessionReaderGIDEnv)); v != "" {
		gid, err := strconv.Atoi(v)
		if err != nil || gid <= 0 {
			return 0, fmt.Errorf("%s must be a positive integer", userSessionReaderGIDEnv)
		}
		return gid, nil
	}
	g, err := user.LookupGroup(userSessionReaderGroup)
	if err != nil {
		return 0, fmt.Errorf("per-session isolation needs the %q group or %s: %w", userSessionReaderGroup, userSessionReaderGIDEnv, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil || gid <= 0 {
		return 0, fmt.Errorf("group %q has an unusable gid %q", userSessionReaderGroup, g.Gid)
	}
	return gid, nil
}

// privRunFunc runs argv as identity user ("root" or "uid:gid") through su-exec.
type privRunFunc func(ctx context.Context, user string, stdin io.Reader, argv ...string) ([]byte, error)

// execUserSessionPriv is the real privRunFunc. The child gets a minimal
// environment: nothing from the hive's own (credential-bearing) env.
func execUserSessionPriv(ctx context.Context, user string, stdin io.Reader, argv ...string) ([]byte, error) {
	if user == "" || len(argv) == 0 {
		return nil, errors.New("refusing to run without an explicit identity and command")
	}
	cmd := exec.CommandContext(ctx, userSuExecBinary, append([]string{user}, argv...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.WaitDelay = tmuxCmdWaitDelay
	cmd.Stdin = stdin
	return cmd.CombinedOutput()
}

// privRun is r.priv bound to the standard command timeout.
func (r *UserSessionRuntime) privRun(user string, stdin io.Reader, argv ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), userSessionCmdTimeout)
	defer cancel()
	return r.priv(ctx, user, stdin, argv...)
}

// statPathUID returns the owner uid of a directory, without following a
// symlink at the path itself.
func statPathUID(path string) (int, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !fi.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("owner of %s is not available on this platform", path)
	}
	return int(st.Uid), nil
}

// sessionUID recovers a session's uid from the owner of its home directory and
// refuses anything outside the reserved range, so a tampered or legacy
// directory can never make the runtime act as the hive or an agent.
func (r *UserSessionRuntime) sessionUID(id string) (int, error) {
	uid, err := r.statUID(userSessionHome(id))
	if err != nil {
		return 0, fmt.Errorf("session sandbox unavailable: %w", err)
	}
	if !userSessionUIDInRange(uid) {
		return 0, fmt.Errorf("session home is owned by uid %d, outside the reserved session range", uid)
	}
	return uid, nil
}

// userSessionTmuxSocketDir is the directory tmux keeps the uid's sockets in.
func userSessionTmuxSocketDir(uid int) string {
	root := strings.TrimRight(os.Getenv("TMUX_TMPDIR"), "/")
	if root == "" {
		root = "/tmp"
	}
	return filepath.Join(root, fmt.Sprintf("tmux-%d", uid))
}

// Remove ends the session and deletes everything it owned: the sandbox (home,
// work dir, uploads, tmp) and the uid's tmux socket dir, so a later session
// that is allocated the same uid starts clean. Directories earlier releases
// created for the id are removed too. A failure is returned and the caller
// must keep the session record (and its uid slot) so the removal is retried.
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
	dir := userSessionSandboxDir(id)
	if _, err := os.Lstat(dir); err == nil {
		if out, err := r.privRun(userSessionRootSpec, nil, "rm", "-rf", "--one-file-system", "--", dir); err != nil {
			return outputErr("removing session sandbox", err, out)
		}
	}
	if uidErr == nil {
		sock := userSessionTmuxSocketDir(uid)
		if out, err := r.privRun(userSessionRootSpec, nil, "rm", "-rf", "--one-file-system", "--", sock); err != nil {
			return outputErr("removing session tmux socket dir", err, out)
		}
	}
	for _, p := range legacyUserSessionPaths(id) {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("removing legacy session dir %s: %w", p, err)
		}
	}
	return nil
}
