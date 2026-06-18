package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
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
	// Run kubectl top nodes (requires metrics-server).
	topOut, err := runKubectlLocal("top", "nodes", "--no-headers")
	if err != nil {
		logger.Debug("spoke metrics: kubectl top nodes failed (metrics-server may not be installed)", "error", err)
		return nil
	}

	// Run kubectl get nodes for capacity, allocatable, conditions, GPU resources.
	getOut, err := runKubectlLocal("get", "nodes", "-o", "json")
	if err != nil {
		logger.Debug("spoke metrics: kubectl get nodes failed", "error", err)
		return nil
	}

	// Parse node metadata from kubectl get nodes.
	var nodesJSON struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
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
		conditions     []string
		diskPressure   bool
	}
	nodeMap := make(map[string]*nodeInfo)
	var totalGPUCapacity, totalGPUAllocatable int
	gpuTypes := map[string]bool{}

	for _, item := range nodesJSON.Items {
		ni := &nodeInfo{}
		ni.cpuAllocatable = parseK8sCPU(item.Status.Allocatable["cpu"])
		ni.memAllocatable = parseK8sMemory(item.Status.Allocatable["memory"])
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

	// Parse kubectl top nodes output.
	// Format: NAME  CPU(cores)  CPU%  MEMORY(bytes)  MEMORY%
	var nodes []HeartbeatNodeMetric
	lines := strings.Split(strings.TrimSpace(string(topOut)), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		const topFieldCount = 5
		if len(fields) < topFieldCount {
			continue
		}
		name := fields[0]
		cpuUsed := parseTopCPU(fields[1])
		memUsed := parseTopMemory(fields[3])

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
		nodes = append(nodes, node)
	}

	// Count running pods per node.
	podOut, err := runKubectlLocal("get", "pods", "--all-namespaces", "--field-selector=status.phase=Running", "-o", "json")
	if err == nil && len(podOut) > 0 {
		var podsJSON struct {
			Items []struct {
				Spec struct {
					NodeName string `json:"nodeName"`
				} `json:"spec"`
			} `json:"items"`
		}
		if json.Unmarshal(podOut, &podsJSON) == nil {
			podCounts := make(map[string]int)
			for _, p := range podsJSON.Items {
				podCounts[p.Spec.NodeName]++
			}
			for i := range nodes {
				nodes[i].Pods = podCounts[nodes[i].Name]
			}
		}
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
	readyNodes := 0

	for _, n := range nodes {
		totalCPUCores += n.CPUCores
		totalCPUUsed += n.CPUUsedMillis
		totalPods += n.Pods
		if n.Ready {
			readyNodes++
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

	report := &HeartbeatClusterHealthReport{
		Nodes: nodes,
		Summary: HeartbeatClusterSummary{
			TotalNodes:    len(nodes),
			ReadyNodes:    readyNodes,
			TotalCPUCores: totalCPUCores,
			TotalCPUPct:   totalCPUPct,
			TotalMemGB:    totalMemGB,
			TotalMemPct:   totalMemPct,
			TotalPods:     totalPods,
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

// runKubectlLocal runs kubectl in-cluster (no kubeconfig needed) and returns
// stdout. Used by spokes to collect local cluster metrics.
func runKubectlLocal(args ...string) ([]byte, error) {
	cmd := exec.Command("kubectl", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
