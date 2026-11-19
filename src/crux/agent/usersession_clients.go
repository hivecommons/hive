package agent

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Stale attached clients. A browser terminal is a tmux client that
// ttyd-tmux.sh attaches, as the session's own uid, on a pty ttyd owns. When
// the browser goes away ttyd signals that script, and the script detaches the
// client (see the attach lifecycle in src/deploy/ttyd-tmux.sh). Whenever that
// chain breaks (a ttyd respawn reparents its children, a wrapper is killed, a
// client process is gone but its socket not yet noticed) the client stays
// attached: the dashboard counts it as an attached browser, and tmux sizes the
// window to the smallest of the stale clients. Each probe therefore lists the
// clients, detaches those whose process is dead or no longer descends from the
// terminal attacher, and reports the survivors as the attached count.
const (
	userSessionClientFormat = "#{client_pid} #{client_tty}"
	userSessionClientFields = 2
	// userSessionReapMax bounds the detaches one probe performs, so a
	// pathological server cannot turn a poll into a long tmux conversation.
	userSessionReapMax = 64
	// userSessionAncestryDepth bounds the parent walk from a client process.
	userSessionAncestryDepth = 16
	// userSessionAttacherComm is the terminal attacher's process name as
	// /proc/<pid>/comm reports it.
	userSessionAttacherComm = "ttyd"
	userSessionProcRoot     = "/proc"
)

// clientOrphanFunc reports whether the tmux client process pid has lost its
// terminal attacher. known is false when that cannot be told (no procfs, an
// unreadable or vanishing ancestor); such a client is left alone.
type clientOrphanFunc func(pid int) (orphaned, known bool)

type userSessionClient struct {
	pid int
	tty string
}

// parseUserSessionClients reads `list-clients -F userSessionClientFormat`
// output, skipping anything that is not "<pid> <tty>".
func parseUserSessionClients(out []byte) []userSessionClient {
	var clients []userSessionClient
	for _, line := range bytes.Split(out, []byte("\n")) {
		fields := strings.Fields(string(line))
		if len(fields) != userSessionClientFields {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 || !strings.HasPrefix(fields[1], "/") {
			continue
		}
		clients = append(clients, userSessionClient{pid: pid, tty: fields[1]})
	}
	return clients
}

// liveClients lists the session's attached clients, detaches the orphaned
// ones (bounded by userSessionReapMax) and returns how many remain attached.
// ok is false when the clients could not be listed, or when the runtime has no
// way to judge a client; the caller then keeps tmux's own count.
func (r *UserSessionRuntime) liveClients(uid int, id string) (live int, ok bool) {
	if r.orphaned == nil {
		return 0, false
	}
	out, err := r.tmuxAs(uid, id, "", "list-clients", "-t", UserSessionTmuxName(id), "-F", userSessionClientFormat)
	if err != nil {
		return 0, false
	}
	reaped := 0
	for _, c := range parseUserSessionClients(out) {
		orphaned, known := r.orphaned(c.pid)
		if !known || !orphaned || reaped >= userSessionReapMax {
			live++
			continue
		}
		reaped++
		if _, err := r.tmuxAs(uid, id, "", "detach-client", "-t", c.tty); err != nil {
			r.logger.Warn("user session: detaching a stale tmux client failed", "id", id, "client_pid", c.pid, "tty", c.tty, "error", err)
			live++
			continue
		}
		r.logger.Info("user session: detached a stale tmux client", "id", id, "client_pid", c.pid, "tty", c.tty)
	}
	return live, true
}

// procClientOrphaned is the real clientOrphanFunc, reading /proc.
func procClientOrphaned(pid int) (bool, bool) {
	return procClientOrphanedIn(userSessionProcRoot, pid)
}

// procClientOrphanedIn walks pid's parent chain under the procfs at root. The
// client is orphaned when its process is gone, or when the walk reaches the
// top (ppid 0 or 1) without passing the terminal attacher: ttyd respawned and
// its children were reparented, the wrapper died, or a client was attached by
// hand from outside ttyd. Anything it cannot read is unknown, never orphaned,
// so a host without procfs reaps nothing.
func procClientOrphanedIn(root string, pid int) (orphaned, known bool) {
	if pid <= 1 {
		return false, false
	}
	if _, err := os.Stat(filepath.Join(root, "self")); err != nil {
		return false, false
	}
	cur := pid
	for depth := 0; depth < userSessionAncestryDepth; depth++ {
		ppid, err := procParentPID(root, cur)
		if err != nil {
			if cur == pid && errors.Is(err, fs.ErrNotExist) {
				return true, true
			}
			return false, false
		}
		if ppid <= 1 {
			return true, true
		}
		if procComm(root, ppid) == userSessionAttacherComm {
			return false, true
		}
		cur = ppid
	}
	return false, false
}

// procParentPID reads the PPid field of <root>/<pid>/status.
func procParentPID(root string, pid int) (int, error) {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "PPid:"); ok {
			return strconv.Atoi(strings.TrimSpace(rest))
		}
	}
	return 0, errors.New("no PPid field")
}

// procComm reads <root>/<pid>/comm, the process name; "" when unreadable.
func procComm(root string, pid int) string {
	data, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "comm"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
