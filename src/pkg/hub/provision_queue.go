package hub

import (
	"os"
	"strconv"
	"sync"
)

// Provisioning queue.
//
// Provisions used to launch as unbounded goroutines straight from the HTTP
// handlers. Each provision shells out repeatedly (manifest apply, OCI FSS
// export creation, rollout waits), so a bulk request or replenish burst
// stampedes both the hub and the target cluster/OCI tenancy. This queue
// bounds total provisioning concurrency AND fairness per cluster: a burst
// aimed at one cluster cannot starve provisions bound for another, and the
// per-cluster cap doubles as the OCI FSS creation rate limit.
//
// Jobs are FIFO overall; a worker takes the OLDEST job whose target cluster
// is below the per-cluster in-flight cap and skips past full clusters.
// Callers keep the provisionWG contract (Add at enqueue, Done at completion)
// so tests can still drain all provisioning work before swapping the
// package-level saas*Dir variables.

const (
	// defaultProvisionWorkers bounds total concurrent provisions hub-wide.
	defaultProvisionWorkers = 4
	// defaultProvisionPerCluster bounds concurrent provisions per target
	// cluster — the effective rate limit on OCI FSS export creation and
	// per-cluster kubectl apply storms.
	defaultProvisionPerCluster = 2
)

type provisionJob struct {
	clusterID string
	run       func()
}

type provisionQueueT struct {
	mu       sync.Mutex
	cond     *sync.Cond
	jobs     []provisionJob
	inFlight map[string]int
	spawned  int
	started  bool
}

var provisionQueue = newProvisionQueue()

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return def
}

// provisionWorkerCount / provisionPerClusterCap resolve the queue bounds:
// dashboard-saved Scale Controls value first, env var as the initial
// default. The per-cluster cap is consulted on every job take (live); the
// worker count applies at pool start and can GROW live via ensureWorkers —
// shrinking waits for a hub restart.
func provisionWorkerCount() int {
	return settingOrEnv(getScaleSettings().ProvisionWorkers, "HIVE_PROVISION_WORKERS", defaultProvisionWorkers)
}

func provisionPerClusterCap() int {
	return settingOrEnv(getScaleSettings().ProvisionPerCluster, "HIVE_PROVISION_PER_CLUSTER", defaultProvisionPerCluster)
}

func newProvisionQueue() *provisionQueueT {
	q := &provisionQueueT{inFlight: make(map[string]int)}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// startLocked launches the worker pool on first use. Lazy so package init
// stays side-effect free and tests that never provision spawn nothing.
func (q *provisionQueueT) startLocked() {
	if q.started {
		return
	}
	q.started = true
	for ; q.spawned < provisionWorkerCount(); q.spawned++ {
		go q.worker()
	}
}

// ensureWorkers grows the running pool up to the current configured worker
// count. Called after an admin raises the bound in Scale Controls. A no-op
// until the pool has started (first enqueue starts it at the right size).
func (q *provisionQueueT) ensureWorkers() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.started {
		return
	}
	for ; q.spawned < provisionWorkerCount(); q.spawned++ {
		go q.worker()
	}
}

// enqueue schedules fn to run on the worker pool, tagged with its target
// cluster for fairness. The caller must have done provisionWG.Add(1); the
// queue calls provisionWG.Done() when fn completes. The queue itself is
// unbounded (records are already capped by quotas/max_hives upstream) — what
// is bounded is EXECUTION.
func (q *provisionQueueT) enqueue(clusterID string, fn func()) {
	q.mu.Lock()
	q.startLocked()
	q.jobs = append(q.jobs, provisionJob{clusterID: clusterID, run: fn})
	q.mu.Unlock()
	q.cond.Signal()
}

// depth reports queued (not yet running) jobs, for observability.
func (q *provisionQueueT) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

// takeLocked pops the oldest job whose cluster is under the per-cluster cap,
// or returns false when every queued job targets a saturated cluster.
func (q *provisionQueueT) takeLocked() (provisionJob, bool) {
	cap := provisionPerClusterCap()
	for i, j := range q.jobs {
		if q.inFlight[j.clusterID] < cap {
			q.jobs = append(q.jobs[:i], q.jobs[i+1:]...)
			q.inFlight[j.clusterID]++
			return j, true
		}
	}
	return provisionJob{}, false
}

func (q *provisionQueueT) worker() {
	for {
		q.mu.Lock()
		var job provisionJob
		for {
			var ok bool
			if job, ok = q.takeLocked(); ok {
				break
			}
			q.cond.Wait()
		}
		q.mu.Unlock()

		func() {
			defer func() {
				q.mu.Lock()
				q.inFlight[job.clusterID]--
				q.mu.Unlock()
				// A slot freed can unblock jobs for this cluster AND let other
				// workers re-scan; wake everyone rather than one.
				q.cond.Broadcast()
				provisionWG.Done()
			}()
			job.run()
		}()
	}
}

// enqueueProvision is the call-site helper: pairs the provisionWG.Add with
// queue submission so no site can forget the Done bookkeeping.
func enqueueProvision(clusterID string, fn func()) {
	provisionWG.Add(1)
	provisionQueue.enqueue(clusterID, fn)
}
