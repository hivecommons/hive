//go:build !windows

package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

var errSpekHubFenceBusy = errors.New("Spektacular worktree is owned by another process")

type spekHubFenceContextKey struct{}

// flock is released only when all copies of this open file description close.
// The CLI inherits a copy so a hub crash does not release its ownership. Keep
// the lock file outside work/: git may create/remove that entire directory.
//
// The sweep unlinks the lock of a finished run while holding it, so a lock
// taken on a file that is no longer the one at path is dropped and retried:
// otherwise two processes could each hold a different inode for one run.
func acquireSpekHubFence(path string) (*os.File, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				return nil, errSpekHubFenceBusy
			}
			return nil, fmt.Errorf("lock Spektacular worktree: %w", err)
		}
		held, herr := f.Stat()
		cur, cerr := os.Stat(path)
		if herr == nil && cerr == nil && os.SameFile(held, cur) {
			return f, nil
		}
		f.Close()
	}
	return nil, errSpekHubFenceBusy
}
func spekHubInheritFence(cmd *exec.Cmd, ctx context.Context) {
	if f, ok := ctx.Value(spekHubFenceContextKey{}).(*os.File); ok {
		cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	}
}
