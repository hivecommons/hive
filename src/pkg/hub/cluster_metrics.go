package hub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// metricsCollectionCacheTTL is how long we cache spoke-side cluster metrics
// before running kubectl again. Prevents running kubectl on every heartbeat.
const metricsCollectionCacheTTL = 60 * time.Second

// metricsCollectionTimeout bounds how long we wait for each kubectl command
// during spoke-side metrics collection.
const metricsCollectionTimeout = 10 * time.Second

// maxNodesInHeartbeat limits the number of nodes reported in a heartbeat
// to prevent oversized payloads on large clusters.
const maxNodesInHeartbeat = 100

var (
	cachedClusterHealth     *HeartbeatClusterHealthReport
	cachedClusterHealthTime time.Time
	cachedClusterHealthMu   sync.Mutex
)

// CollectClusterHealth gathers node-level CPU, memory, pod, and GPU metrics
// by running kubectl in-cluster on the spoke. Results are cached for
// metricsCollectionCacheTTL to avoid excessive kubectl calls.
// Returns nil if metrics-server is not installed or kubectl fails.
func CollectClusterHealth(logger *slog.Logger) *HeartbeatClusterHealthReport {
	cachedClusterHealthMu.Lock()
	if cachedClusterHealth != nil && time.Since(cachedClusterHealthTime) < metricsCollectionCacheTTL {
		cached := cachedClusterHealth
		cachedClusterHealthMu.Unlock()
		return cached
	}
	cachedClusterHealthMu.Unlock()

	report := collectClusterHealthUncached(logger)

	cachedClusterHealthMu.Lock()
	cachedClusterHealth = report
	cachedClusterHealthTime = time.Now()
	cachedClusterHealthMu.Unlock()

	return report
}

func collectClusterHealthUncached(logger *slog.Logger) *HeartbeatClusterHealthReport {
	// Query metrics API for node resource usage (requires metrics-server).
	topOut, err := k8sAPIGet("/apis/metrics.k8s.io/v1beta1/nodes")
	if err != nil {
		logger.Debug("spoke metrics: metrics API failed (metrics-server may not be installed)", "error", err)
		return nil
	}

	// Query nodes API for capacity, allocatable, conditions, GPU resources.
	getOut, err := k8sAPIGet("/api/v1/nodes")
	if err != nil {
		logger.Debug("spoke metrics: nodes API failed", "error", err)
		return nil
	}

	// Parse node metadata from kubectl get nodes.
	var nodesJSON struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
				Capacity    map[string]string `json:"capacity"`
				Conditions  []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(getOut, &nodesJSON); err != nil {
		logger.Debug("spoke metrics: failed to parse nodes JSON", "error", err)
		return nil
	}

	type nodeInfo struct {
		cpuAllocatable int64 // millicores
		memAllocatable int64 // bytes
		podCapacity    int
		gpuCapacity    int
		gpuAllocatable int
		gpuType        string
		ready          bool
		unschedulable  bool // cordoned; excluded from hive capacity estimates
		conditions     []string
		diskPressure   bool
	}
	nodeMap := make(map[string]*nodeInfo)
	var totalGPUCapacity, totalGPUAllocatable int
	gpuTypes := map[string]bool{}

	for _, item := range nodesJSON.Items {
		ni := &nodeInfo{}
		// Allocatable (not raw capacity) is what the scheduler can place
		// pods against, so hive capacity math below uses these values.
		ni.cpuAllocatable = parseK8sCPU(item.Status.Allocatable["cpu"])
		ni.memAllocatable = parseK8sMemory(item.Status.Allocatable["memory"])
		ni.unschedulable = item.Spec.Unschedulable
		ni.podCapacity = parseInt(item.Status.Capacity["pods"])
		ni.gpuCapacity = parseInt(item.Status.Capacity[gpuResourceKey])
		ni.gpuAllocatable = parseInt(item.Status.Allocatable[gpuResourceKey])
		totalGPUCapacity += ni.gpuCapacity
		totalGPUAllocatable += ni.gpuAllocatable

		// Detect GPU type from common node labels.
		if gpuLabel, ok := item.Metadata.Labels["nvidia.com/gpu.product"]; ok && gpuLabel != "" {
			ni.gpuType = gpuLabel
			gpuTypes[gpuLabel] = true
		}

		for _, cond := range item.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				ni.ready = true
				ni.conditions = append(ni.conditions, "Ready")
			} else if cond.Type == "Ready" && cond.Status != "True" {
				ni.conditions = append(ni.conditions, "NotReady")
			}
			if cond.Type == "DiskPressure" && cond.Status == "True" {
				ni.diskPressure = true
			}
		}
		if len(ni.conditions) == 0 {
			ni.conditions = []string{"Unknown"}
		}
		nodeMap[item.Metadata.Name] = ni
	}

	// Parse metrics API JSON response.
	var metricsJSON struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Usage map[string]string `json:"usage"`
		} `json:"items"`
	}
	if err := json.Unmarshal(topOut, &metricsJSON); err != nil {
		logger.Debug("spoke metrics: failed to parse metrics API response", "error", err)
		return nil
	}

	var nodes []HeartbeatNodeMetric
	for _, item := range metricsJSON.Items {
		name := item.Metadata.Name
		cpuUsed := parseK8sCPU(item.Usage["cpu"])
		memUsed := parseK8sMemory(item.Usage["memory"])

		ni, ok := nodeMap[name]
		if !ok {
			continue
		}

		cpuCores := int(ni.cpuAllocatable / millicoresPerCore)
		cpuPct := 0
		if ni.cpuAllocatable > 0 {
			cpuPct = int(cpuUsed * percentMultiplier / ni.cpuAllocatable)
		}

		memTotalMB := ni.memAllocatable / bytesPerMB
		memUsedMB := memUsed / bytesPerMB
		memPct := 0
		if ni.memAllocatable > 0 {
			memPct = int(memUsed * percentMultiplier / ni.memAllocatable)
		}

		node := HeartbeatNodeMetric{
			Name:          name,
			CPUCores:      cpuCores,
			CPUUsedMillis: cpuUsed,
			CPUPercent:    cpuPct,
			MemTotalMB:    memTotalMB,
			MemUsedMB:     memUsedMB,
			MemPercent:    memPct,
			PodCapacity:   ni.podCapacity,
			Ready:         ni.ready,
			Conditions:    ni.conditions,
			DiskPressure:  ni.diskPressure,
		}
		if ni.gpuCapacity > 0 {
			node.GPUs = ni.gpuCapacity
			node.GPUType = ni.gpuType
		}
		// LIVE node filesystem usage from the kubelet stats/summary endpoint.
		// The node object's ephemeral-storage capacity is only the declared
		// size and cannot tell us how full the disk is. Best-effort per node:
		// a failure leaves the disk fields nil (rendered as unknown).
		if rawStats, statsErr := k8sAPIGet(nodeStatsSummaryPath(name)); statsErr != nil {
			logger.Debug("spoke metrics: node disk stats unavailable", "node", name, "error", statsErr)
		} else if usage, ok := parseNodeStatsSummaryDisk(rawStats); ok {
			totalMB := usage.capacityBytes / bytesPerMB
			usedMB := usage.usedBytes / bytesPerMB
			diskPct := int(usage.usedBytes * percentMultiplier / usage.capacityBytes)
			node.DiskTotalMB = &totalMB
			node.DiskUsedMB = &usedMB
			node.DiskPercent = &diskPct
		}
		nodes = append(nodes, node)
	}

	// Count running pods per node and sum their container resource REQUESTS
	// (requests, not usage — that is what the scheduler bin-packs against).
	// Listing only Running pods slightly undercounts requests (Pending pods
	// already assigned to a node are missed), so the capacity estimate below
	// can be marginally optimistic.
	var cpuRequestedPerNode, memRequestedPerNode map[string]int64
	podOut, err := k8sAPIGet("/api/v1/pods?fieldSelector=status.phase%3DRunning")
	if err == nil && len(podOut) > 0 {
		var podsJSON struct {
			Items []struct {
				Metadata struct {
					Namespace string `json:"namespace"`
				} `json:"metadata"`
				Spec struct {
					NodeName   string `json:"nodeName"`
					Containers []struct {
						Resources struct {
							Requests map[string]string `json:"requests"`
						} `json:"resources"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"items"`
		}
		if json.Unmarshal(podOut, &podsJSON) == nil {
			podCounts := make(map[string]int)
			cpuRequestedPerNode = make(map[string]int64)
			memRequestedPerNode = make(map[string]int64)
			// hiveNamespacesPerNode tracks distinct hive-hosted-* namespaces per
			// node so each hive is counted once even with multiple pods.
			hiveNamespacesPerNode := make(map[string]map[string]bool)
			for _, p := range podsJSON.Items {
				podCounts[p.Spec.NodeName]++
				for _, c := range p.Spec.Containers {
					cpuRequestedPerNode[p.Spec.NodeName] += parseK8sCPU(c.Resources.Requests["cpu"])
					memRequestedPerNode[p.Spec.NodeName] += parseK8sMemory(c.Resources.Requests["memory"])
				}
				if strings.HasPrefix(p.Metadata.Namespace, hiveHostedNamespacePrefix) {
					if hiveNamespacesPerNode[p.Spec.NodeName] == nil {
						hiveNamespacesPerNode[p.Spec.NodeName] = make(map[string]bool)
					}
					hiveNamespacesPerNode[p.Spec.NodeName][p.Metadata.Namespace] = true
				}
			}
			for i := range nodes {
				nodes[i].Pods = podCounts[nodes[i].Name]
				nodes[i].HiveCount = len(hiveNamespacesPerNode[nodes[i].Name])
			}
		}
	}

	// Estimate remaining hive capacity: bin-pack the per-hive request
	// footprint into each Ready, schedulable node's free (allocatable minus
	// requested) capacity. Only computed when the pod listing above parsed,
	// since without per-node requests the estimate would be meaningless.
	// Unlike the hub kubectl path, no approximation from usage is needed
	// here: the in-cluster pod API returns full container requests.
	var hiveCapacityRemaining *int
	if cpuRequestedPerNode != nil {
		var totalSlots int64
		for name, ni := range nodeMap {
			totalSlots += hiveSlotsForNode(ni.cpuAllocatable, ni.memAllocatable,
				cpuRequestedPerNode[name], memRequestedPerNode[name], ni.ready, ni.unschedulable)
		}
		slots := int(totalSlots)
		hiveCapacityRemaining = &slots
	}

	// Cap the number of nodes to prevent oversized payloads.
	if len(nodes) > maxNodesInHeartbeat {
		nodes = nodes[:maxNodesInHeartbeat]
	}

	// Build summary.
	var totalCPUCores int
	var totalCPUUsed int64
	var totalCPUAlloc int64
	var totalMemAlloc int64
	var totalMemUsed int64
	var totalPods int
	// Disk totals accumulate only over nodes that actually reported usage, so
	// partial coverage stays honest instead of averaging in phantom zeros.
	var totalDiskBytes, totalDiskUsedBytes int64
	readyNodes := 0

	for _, n := range nodes {
		totalCPUCores += n.CPUCores
		totalCPUUsed += n.CPUUsedMillis
		totalPods += n.Pods
		if n.Ready {
			readyNodes++
		}
		if n.DiskTotalMB != nil && n.DiskUsedMB != nil {
			totalDiskBytes += *n.DiskTotalMB * bytesPerMB
			totalDiskUsedBytes += *n.DiskUsedMB * bytesPerMB
		}
		if ni, ok := nodeMap[n.Name]; ok {
			totalCPUAlloc += ni.cpuAllocatable
			totalMemAlloc += ni.memAllocatable
		}
		totalMemUsed += n.MemUsedMB * bytesPerMB
	}

	totalCPUPct := 0
	if totalCPUAlloc > 0 {
		totalCPUPct = int(totalCPUUsed * percentMultiplier / totalCPUAlloc)
	}
	totalMemPct := 0
	if totalMemAlloc > 0 {
		totalMemPct = int(totalMemUsed * percentMultiplier / totalMemAlloc)
	}
	totalMemGB := int(totalMemAlloc / giToBytes)

	// nil when no node reported disk usage, so the hub omits disk for this
	// cluster rather than displaying a misleading 0%.
	var totalDiskGB, totalDiskPct *int
	if totalDiskBytes > 0 {
		gb := int(totalDiskBytes / giToBytes)
		pct := int(totalDiskUsedBytes * percentMultiplier / totalDiskBytes)
		totalDiskGB = &gb
		totalDiskPct = &pct
	}

	report := &HeartbeatClusterHealthReport{
		Nodes: nodes,
		Summary: HeartbeatClusterSummary{
			TotalNodes:            len(nodes),
			ReadyNodes:            readyNodes,
			TotalCPUCores:         totalCPUCores,
			TotalCPUPct:           totalCPUPct,
			TotalMemGB:            totalMemGB,
			TotalMemPct:           totalMemPct,
			TotalDiskGB:           totalDiskGB,
			TotalDiskPct:          totalDiskPct,
			TotalPods:             totalPods,
			HiveCapacityRemaining: hiveCapacityRemaining,
		},
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// Include GPU summary if the cluster has GPUs.
	if totalGPUCapacity > 0 {
		types := make([]string, 0, len(gpuTypes))
		for t := range gpuTypes {
			types = append(types, t)
		}
		report.GPUSummary = &HeartbeatGPUSummary{
			Total:     totalGPUCapacity,
			Allocated: totalGPUCapacity - totalGPUAllocatable,
			Types:     types,
		}
	}

	return report
}

// These are vars (not consts) purely so tests can redirect the in-cluster K8s
// API endpoint and service-account file paths at an httptest server / temp
// dir. Production never reassigns them.
var (
	k8sAPIServer  = "https://kubernetes.default.svc"
	k8sTokenPath  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	k8sCACertPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

func k8sTLSConfig() (*tls.Config, error) {
	caCert, err := os.ReadFile(k8sCACertPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read k8s CA cert at %s, refusing to connect with TLS verification disabled: %w", k8sCACertPath, err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("k8s CA cert at %s did not contain any PEM certificates, refusing to connect with an empty cert pool", k8sCACertPath)
	}

	return &tls.Config{RootCAs: pool}, nil
}

func k8sAPIGet(path string) ([]byte, error) {
	token, err := os.ReadFile(k8sTokenPath)
	if err != nil {
		return nil, fmt.Errorf("reading service account token: %w", err)
	}

	tlsConfig, err := k8sTLSConfig()
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Timeout:   metricsCollectionTimeout,
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}

	ctx, cancel := context.WithTimeout(context.Background(), metricsCollectionTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k8sAPIServer+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("k8s API %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", path, err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("k8s API %s: HTTP %d: %s", path, resp.StatusCode, string(body[:minInt(len(body), 200)]))
	}
	return body, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
