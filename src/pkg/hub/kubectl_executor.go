package hub

import (
	"os/exec"
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
// cap when neither the dashboard Scale Controls value nor
// HIVE_KUBECTL_MAX_PER_CLUSTER is set.
const defaultKubectlMaxPerCluster = 8

var (
	kubectlSlotsMu   sync.Mutex
	kubectlSlotsCond = sync.NewCond(&kubectlSlotsMu)
	kubectlInUse     = map[string]int{}
)

// kubectlMaxPerCluster returns the per-cluster concurrency cap: the
// dashboard-saved Scale Controls value first, HIVE_KUBECTL_MAX_PER_CLUSTER
// as the initial default. Consulted on every slot acquisition, so a saved
// change takes effect live.
func kubectlMaxPerCluster() int {
	return settingOrEnv(getScaleSettings().KubectlPerCluster, "HIVE_KUBECTL_MAX_PER_CLUSTER", defaultKubectlMaxPerCluster)
}

// acquireKubectlSlot blocks until a per-cluster execution slot is free and
// returns its release func. Blocking is acceptable: callers already block on
// the subprocess itself, and slots are always released on command completion.
func acquireKubectlSlot(clusterID string) (release func()) {
	kubectlSlotsMu.Lock()
	for kubectlInUse[clusterID] >= kubectlMaxPerCluster() {
		kubectlSlotsCond.Wait()
	}
	kubectlInUse[clusterID]++
	kubectlSlotsMu.Unlock()
	return func() {
		kubectlSlotsMu.Lock()
		kubectlInUse[clusterID]--
		kubectlSlotsMu.Unlock()
		kubectlSlotsCond.Broadcast()
	}
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
