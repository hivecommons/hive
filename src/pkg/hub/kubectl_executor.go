package hub

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultKubectlMaxPerCluster is the per-cluster concurrent kubectl process
// cap when neither the dashboard Scale Controls value nor
// HIVE_KUBECTL_MAX_PER_CLUSTER is set.
const defaultKubectlMaxPerCluster = 8

// kubectlSlotWaitTimeout bounds how long a caller waits for a per-cluster
// slot before giving up, so one hung command cannot starve the pool forever.
const kubectlSlotWaitTimeout = 30 * time.Second

// kubectlDefaultTimeout bounds the lifetime of a kubectl process started via
// kubectlForCluster. Callers that need a tighter bound use
// kubectlForClusterContext with their own deadline.
const kubectlDefaultTimeout = 5 * time.Minute

// kubectlInflight records a running command so pool exhaustion can name the
// oldest one.
type kubectlInflight struct {
	args    string
	started time.Time
}

var (
	kubectlSlotsMu   sync.Mutex
	kubectlSlotsCond = sync.NewCond(&kubectlSlotsMu)
	kubectlInUse     = map[string]int{}
	kubectlInflights = map[string]map[uint64]kubectlInflight{}
	kubectlNextID    uint64

	kubectlPoolExhausted atomic.Int64
)

// KubectlPoolExhaustedTotal returns how many slot waits gave up because the
// per-cluster pool stayed full (exported for the metrics surface).
func KubectlPoolExhaustedTotal() int64 { return kubectlPoolExhausted.Load() }

// kubectlMaxPerCluster returns the per-cluster concurrency cap: the
// dashboard-saved Scale Controls value first, HIVE_KUBECTL_MAX_PER_CLUSTER
// as the initial default. Consulted on every slot acquisition, so a saved
// change takes effect live.
func kubectlMaxPerCluster() int {
	return settingOrEnv(getScaleSettings().KubectlPerCluster, "HIVE_KUBECTL_MAX_PER_CLUSTER", defaultKubectlMaxPerCluster)
}

// acquireKubectlSlot waits for a per-cluster execution slot and returns its
// release func. The wait ends with an error when ctx is done or
// kubectlSlotWaitTimeout expires. desc names the command for exhaustion logs.
func acquireKubectlSlot(ctx context.Context, clusterID, desc string) (release func(), err error) {
	waitCtx, cancel := context.WithTimeout(ctx, kubectlSlotWaitTimeout)
	defer cancel()
	// sync.Cond cannot select on a context, so wake waiters when it ends.
	stop := context.AfterFunc(waitCtx, func() {
		kubectlSlotsMu.Lock()
		kubectlSlotsMu.Unlock() //nolint:staticcheck // barrier so the wakeup cannot be missed
		kubectlSlotsCond.Broadcast()
	})
	defer stop()

	kubectlSlotsMu.Lock()
	for kubectlInUse[clusterID] >= kubectlMaxPerCluster() {
		if err := waitCtx.Err(); err != nil {
			kubectlPoolExhausted.Add(1)
			oldest, age := oldestKubectlInflightLocked(clusterID)
			kubectlSlotsMu.Unlock()
			slog.Warn("kubectl pool exhausted", "cluster", clusterID, "in_use", kubectlMaxPerCluster(),
				"oldest_command", oldest, "oldest_age", age.Round(time.Second).String(), "error", err)
			return nil, fmt.Errorf("kubectl slot wait for cluster %q: %w", clusterID, err)
		}
		kubectlSlotsCond.Wait()
	}
	kubectlInUse[clusterID]++
	kubectlNextID++
	id := kubectlNextID
	if kubectlInflights[clusterID] == nil {
		kubectlInflights[clusterID] = map[uint64]kubectlInflight{}
	}
	kubectlInflights[clusterID][id] = kubectlInflight{args: desc, started: time.Now()}
	kubectlSlotsMu.Unlock()
	return func() {
		kubectlSlotsMu.Lock()
		kubectlInUse[clusterID]--
		delete(kubectlInflights[clusterID], id)
		kubectlSlotsMu.Unlock()
		kubectlSlotsCond.Broadcast()
	}, nil
}

// oldestKubectlInflightLocked returns the longest-running command for a
// cluster. Caller holds kubectlSlotsMu.
func oldestKubectlInflightLocked(clusterID string) (string, time.Duration) {
	var oldest kubectlInflight
	for _, f := range kubectlInflights[clusterID] {
		if oldest.started.IsZero() || f.started.Before(oldest.started) {
			oldest = f
		}
	}
	if oldest.started.IsZero() {
		return "", 0
	}
	return oldest.args, time.Since(oldest.started)
}

// kubectlCmd wraps exec.Cmd so that the blocking execution methods acquire a
// per-cluster slot first. Field access (Args, Stdin, Env, ...) is promoted
// from the embedded Cmd, so existing call sites work unchanged. Callers must
// use Output/CombinedOutput/Run — Start/Wait on the embedded Cmd would bypass
// the bound.
type kubectlCmd struct {
	*exec.Cmd
	clusterID string
	ctx       context.Context
	cancel    context.CancelFunc // releases the default timeout; may be nil
}

// slot acquires the cluster slot, describing the command by its subcommand
// (args can carry secrets via env/stdin, never argv values we would not log).
func (c *kubectlCmd) slot() (func(), error) {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	desc := "kubectl"
	if len(c.Args) > 1 {
		desc = strings.Join(c.Args[1:], " ")
	}
	return acquireKubectlSlot(ctx, c.clusterID, desc)
}

func (c *kubectlCmd) Output() ([]byte, error) {
	if c.cancel != nil {
		defer c.cancel()
	}
	release, err := c.slot()
	if err != nil {
		return nil, err
	}
	defer release()
	return c.Cmd.Output()
}

func (c *kubectlCmd) CombinedOutput() ([]byte, error) {
	if c.cancel != nil {
		defer c.cancel()
	}
	release, err := c.slot()
	if err != nil {
		return nil, err
	}
	defer release()
	return c.Cmd.CombinedOutput()
}

func (c *kubectlCmd) Run() error {
	if c.cancel != nil {
		defer c.cancel()
	}
	release, err := c.slot()
	if err != nil {
		return err
	}
	defer release()
	return c.Cmd.Run()
}
