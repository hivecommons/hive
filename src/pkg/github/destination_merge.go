package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	gh "github.com/google/go-github/v72/github"
)

var ErrDestinationMergeBusy = errors.New("destination merge already in progress")

var destinationMergeDirectory atomic.Value
var destinationMergeLocks sync.Map

type destinationMergeLease struct{ key, dir string }
type destinationMergeLeaseKey struct{}

// ConfigureDestinationMerges is called before starting production relays.
// All writers of a destination must share this durable directory. An empty
// directory is an isolated process-local guard for test clients.
func ConfigureDestinationMerges(dir string) { destinationMergeDirectory.Store(dir) }

func DestinationMergeDirectoryForPreparation(dir string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dir), "destination-merges")
}

func DestinationMergeDirectory() string {
	dir, _ := destinationMergeDirectory.Load().(string)
	return dir
}

func VerifyDestinationMerge(ctx context.Context, client *gh.Client, owner, repo string, number int, head string) error {
	lease, ok := ctx.Value(destinationMergeLeaseKey{}).(destinationMergeLease)
	if !ok {
		return nil
	}
	pr, _, err := client.PullRequests.Get(WithRESTCaller(ctx, "hive:merge_destination_recheck"), owner, repo, number)
	if err != nil {
		return err
	}
	if !DestinationMergeHeld(ctx, lease.dir, owner+"/"+repo, pr.GetBase().GetRef()) ||
		(head != "" && head != pr.GetHead().GetSHA()) {
		return errors.New("merge head or destination changed under lease")
	}
	return nil
}

func DestinationMergeHeld(ctx context.Context, dir, repo, base string) bool {
	held, ok := ctx.Value(destinationMergeLeaseKey{}).(destinationMergeLease)
	return ok && held == (destinationMergeLease{key: strings.ToLower(strings.TrimSpace(repo)) + ":" + strings.TrimSpace(base), dir: dir})
}

// AcquireDestinationMerge holds a destination across its final admission
// reads and mutation. It never waits on another merge while retaining stale
// admission evidence. Nested calls on the same leased context borrow it.
func AcquireDestinationMerge(ctx context.Context, dir, repo, base string) (context.Context, func(), error) {
	key := strings.ToLower(strings.TrimSpace(repo)) + ":" + strings.TrimSpace(base)
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(base) == "" {
		return ctx, nil, errors.New("merge destination missing")
	}
	lease := destinationMergeLease{key: key, dir: dir}
	if held, ok := ctx.Value(destinationMergeLeaseKey{}).(destinationMergeLease); ok {
		if held == lease {
			return ctx, func() {}, nil
		}
		return ctx, nil, errors.New("merge destination changed under lease")
	}
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	value, _ := destinationMergeLocks.LoadOrStore(dir+"\n"+key, &sync.Mutex{})
	lock, ok := value.(*sync.Mutex)
	if !ok {
		return ctx, nil, errors.New("invalid destination merge lock")
	}
	if !lock.TryLock() {
		return ctx, nil, ErrDestinationMergeBusy
	}
	unlock := lock.Unlock
	if dir != "" {
		const directoryMode = 0o755
		const fileMode = 0o644
		if err := os.MkdirAll(dir, directoryMode); err != nil {
			unlock()
			return ctx, nil, err
		}
		sum := sha256.Sum256([]byte(key))
		f, err := os.OpenFile(filepath.Join(dir, hex.EncodeToString(sum[:])+".lock"), os.O_CREATE|os.O_RDWR, fileMode)
		if err != nil {
			unlock()
			return ctx, nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			unlock()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				err = ErrDestinationMergeBusy
			}
			return ctx, nil, err
		}
		unlock = func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			lock.Unlock()
		}
	}
	return context.WithValue(ctx, destinationMergeLeaseKey{}, lease), unlock, nil
}
