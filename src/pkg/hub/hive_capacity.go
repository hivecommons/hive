package hub

// capacityHeadroomWarnPct is the remaining-slots percentage below which the
// hub logs a low-headroom warning for a cluster (see buildSingleClusterHealth).
const capacityHeadroomWarnPct = 10

// Per-hive scheduling footprint used for "room for ~N more hives" capacity
// estimates on the Cluster Health section. These are derived at package init
// from the provisioning template's resources.requests (cpuRequest and
// memRequest in saas_provision.go, rendered into the hive Deployment YAML),
// so the capacity math can never drift from what provisioning actually
// requests for the main "hive" container. Init containers are ignored: they
// carry no resource requests in the template.
var (
	// hiveCPURequestMillis is the per-hive CPU request in millicores (parsed from cpuRequest).
	hiveCPURequestMillis = parseK8sCPU(cpuRequest)
	// hiveMemRequestBytes is the per-hive memory request in bytes (parsed from memRequest).
	hiveMemRequestBytes = parseK8sMemory(memRequest)
)

// hiveSlotsForNode returns how many additional hive pods fit on a single node,
// bin-packing the per-hive request footprint into the node's unrequested
// allocatable capacity — the same arithmetic the scheduler uses (resource
// REQUESTS of already-scheduled pods, not live usage). Nodes that are not
// Ready or are cordoned (spec.unschedulable) contribute zero slots because
// the scheduler will not place new hive pods there.
func hiveSlotsForNode(cpuAllocatableMillis, memAllocatableBytes, cpuRequestedMillis, memRequestedBytes int64, ready, unschedulable bool) int64 {
	if !ready || unschedulable {
		return 0
	}
	// Guard against a malformed footprint (would divide by zero).
	if hiveCPURequestMillis <= 0 || hiveMemRequestBytes <= 0 {
		return 0
	}
	freeCPU := cpuAllocatableMillis - cpuRequestedMillis
	freeMem := memAllocatableBytes - memRequestedBytes
	if freeCPU <= 0 || freeMem <= 0 {
		return 0
	}
	cpuSlots := freeCPU / hiveCPURequestMillis
	memSlots := freeMem / hiveMemRequestBytes
	if memSlots < cpuSlots {
		return memSlots
	}
	return cpuSlots
}

// clusterHiveCount counts the hosted-hive records (assigned AND pooled
// placeholders — both consume a namespace, PVC and route) that live on the
// given cluster. SaaS records are the authoritative source: every
// hub-provisioned hive has one, stamped with its ClusterID at provision time.
// Records with an empty ClusterID predate multi-cluster and are counted
// against the default cluster, matching how every kubectl path routes them.
func clusterHiveCount(clusterID string) int {
	n := 0
	for _, h := range listSaaSHives() {
		cid := h.ClusterID
		if cid == "" {
			cid = defaultClusterID
		}
		if cid == clusterID {
			n++
		}
	}
	return n
}

// clusterAtMaxHives reports whether the cluster has reached its configured
// max_hives ceiling (0 = unlimited), returning the current count for the
// error message / log line. This is the hard per-cluster provisioning gate;
// callers must check it before creating a new SaaS record on the cluster.
func clusterAtMaxHives(cluster *ClusterConfig) (bool, int) {
	if cluster == nil || cluster.MaxHives <= 0 {
		return false, 0
	}
	n := clusterHiveCount(cluster.ID)
	return n >= cluster.MaxHives, n
}
