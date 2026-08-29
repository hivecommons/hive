// Package proclock provides an exclusive, self-releasing process singleton
// lock built on flock(2).
//
// WHY THIS EXISTS (#2453, #2496)
//
// A spoke pod was observed running TWO hive processes at once: the registry's
// startedAt alternated between two values beat to beat, 4-5 heartbeat senders
// shared one pod name, and one process held a stale App-auth snapshot while
// the other was healthy — the dashboard flipped state on every beat. The
// in-process StartHeartbeat guard (#2462) is scoped to a single process and
// is structurally blind to this class: each process passes its own guard.
//
// The only guard that works across processes is a kernel object both
// processes contend on. flock gives exactly that, with the crucial property
// that the lock is released automatically when the holding process dies —
// however it dies (SIGKILL included). There is no stale-pidfile problem: a
// lock file left on disk by a dead process is simply an unlocked file.
//
// The lock file must live on CONTAINER-LOCAL storage (/var/run, /tmp), never
// on the shared /data PVC: during a rolling update (maxSurge=1) the old and
// new PODS legitimately overlap, and on an RWX PVC a shared lock would
// deadlock the surge pod against the terminating one. Same-pod duplicates —
// the fault this exists to stop — always share the container filesystem.
package proclock

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

// lockFileMode is world-readable so the holder PID in the file is legible to
// anything diagnosing the container; the file content is only a PID.
const lockFileMode = 0o644

// maxHolderBytes bounds how much of the lock file is read back for the error
// message. The content is a PID and a newline; anything longer is garbage.
const maxHolderBytes = 64

// Lock is an acquired exclusive process lock. It holds the lock file open for
// the life of the process; the kernel releases the flock when the file
// descriptor closes, which happens at the latest when the process dies.
type Lock struct {
	file *os.File
	path string
}

// Acquire takes a non-blocking exclusive flock on path, creating the file if
// needed. On success it records the caller's PID in the file (for the NEXT
// contender's error message) and returns a Lock that must be kept for the
// process lifetime. When the lock is already held — by another process, or by
// another open descriptor in this one — it returns an error naming the
// holder's PID when one is legible.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, lockFileMode)
	if err != nil {
		return nil, fmt.Errorf("open singleton lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readHolder(f)
		_ = f.Close() // best-effort cleanup; the flock error is what's returned
		if holder != "" {
			return nil, fmt.Errorf("singleton lock %s is held by process %s: %w", path, holder, err)
		}
		return nil, fmt.Errorf("singleton lock %s is held by another process: %w", path, err)
	}
	// Best-effort PID record: the lock itself is the flock, not this content,
	// so a failed write must not fail the acquisition.
	if err := f.Truncate(0); err == nil {
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid()) // best-effort per the comment above
			_ = f.Sync()                               // best-effort per the comment above
		}
	}
	return &Lock{file: f, path: path}, nil
}

// Path returns the lock file location, for logging.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Release drops the lock explicitly. Production code never needs to call it —
// process death releases the flock — but tests acquiring and re-acquiring in
// one process do.
func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN) // best-effort; process death releases it anyway
	_ = l.file.Close()                                   // best-effort; process death releases it anyway
	l.file = nil
}

// readHolder returns the PID recorded in the lock file by its current holder,
// or "" when none is legible.
func readHolder(f *os.File) string {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	buf := make([]byte, maxHolderBytes)
	n, _ := f.Read(buf)
	holder := strings.TrimSpace(string(buf[:n]))
	// Only a plausible PID is worth naming; anything else is noise.
	for _, r := range holder {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return holder
}
