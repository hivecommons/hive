package hub

import (
	"os"
	"os/exec"
	"strconv"
	"sync"
)

// Bounded kubectl execution.
//
// Every hub → cluster interaction shells out to kubectl. At fleet scale the
// unbounded fan-out (health collection, reconcilers, provisioning, upgrade
// sweeps all spawning subprocesses concurrently) can exhaust hub pod CPU,
// file descriptors and the target API server's client budget. This executor
// caps concurrent kubectl processes PER CLUSTER with a semaphore acquired at
// execution time (Output/CombinedOutput/Run), so command CONSTRUCTION stays
// cheap and call sites are unchanged.

// defaultKubectlMaxPerCluster is the per-cluster concurrent kubectl process
// cap when HIVE_KUBECTL_MAX_PER_CLUSTER is unset or invalid.
const defaultKubectlMaxPerCluster = 8

var (
	kubectlSlotCapOnce sync.Once
	kubectlSlotCap     int

	kubectlSlotsMu sync.Mutex
	kubectlSlots   = map[string]chan struct{}{}
)

// kubectlMaxPerCluster returns the per-cluster concurrency cap, overridable
// via HIVE_KUBECTL_MAX_PER_CLUSTER (values < 1 fall back to the default).
func kubectlMaxPerCluster() int {
	kubectlSlotCapOnce.Do(func() {
		kubectlSlotCap = defaultKubectlMaxPerCluster
		if v := os.Getenv("HIVE_KUBECTL_MAX_PER_CLUSTER"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 1 {
				kubectlSlotCap = n
			}
		}
	})
	return kubectlSlotCap
}

// acquireKubectlSlot blocks until a per-cluster execution slot is free and
// returns its release func. Blocking is acceptable: callers already block on
// the subprocess itself, and slots are always released on command completion.
func acquireKubectlSlot(clusterID string) (release func()) {
	kubectlSlotsMu.Lock()
	sem, ok := kubectlSlots[clusterID]
	if !ok {
		sem = make(chan struct{}, kubectlMaxPerCluster())
		kubectlSlots[clusterID] = sem
	}
	kubectlSlotsMu.Unlock()
	sem <- struct{}{}
	return func() { <-sem }
}

// kubectlCmd wraps exec.Cmd so that the blocking execution methods acquire a
// per-cluster slot first. Field access (Args, Stdin, Env, ...) is promoted
// from the embedded Cmd, so existing call sites work unchanged. Callers must
// use Output/CombinedOutput/Run — Start/Wait on the embedded Cmd would bypass
// the bound.
type kubectlCmd struct {
	*exec.Cmd
	clusterID string
}

func (c *kubectlCmd) Output() ([]byte, error) {
	release := acquireKubectlSlot(c.clusterID)
	defer release()
	return c.Cmd.Output()
}

func (c *kubectlCmd) CombinedOutput() ([]byte, error) {
	release := acquireKubectlSlot(c.clusterID)
	defer release()
	return c.Cmd.CombinedOutput()
}

func (c *kubectlCmd) Run() error {
	release := acquireKubectlSlot(c.clusterID)
	defer release()
	return c.Cmd.Run()
}
