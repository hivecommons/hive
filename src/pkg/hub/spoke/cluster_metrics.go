package spoke

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
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	millicoresPerCore         = 1000
	kiToBytes                 = 1024
	miToBytes                 = 1024 * 1024
	giToBytes                 = 1024 * 1024 * 1024
	bytesPerMB                = 1024 * 1024
	percentMultiplier         = 100
	hiveHostedNamespacePrefix = "hive-hosted-"
	gpuResourceKey            = "nvidia.com/gpu"
)

// metricsCollectionCacheTTL is how long we cache spoke-side cluster metrics
// before running kubectl again. Prevents running kubectl on every heartbeat.
const metricsCollectionCacheTTL = 60 * time.Second

// metricsCollectionTimeout bounds how long we wait for each kubectl command
// during spoke-side metrics collection.
const metricsCollectionTimeout = 10 * time.Second

// nodeHealthWarningInterval prevents a permanently missing/forbidden node API
// from writing the same warning on every heartbeat.
const nodeHealthWarningInterval = time.Hour

// maxNodesInHeartbeat limits the number of nodes reported in a heartbeat
// to prevent oversized payloads on large clusters.
const maxNodesInHeartbeat = 100

var (
	cachedClusterHealth     *HeartbeatClusterHealthReport
	cachedClusterHealthTime time.Time
	cachedClusterHealthMu   sync.Mutex
	lastNodeHealthWarning   = map[string]time.Time{}
	nodeHealthWarningMu     sync.Mutex
)

// CollectClusterHealth gathers node-level CPU, memory, pod, and GPU metrics
// from the in-cluster Kubernetes API on the spoke. Results are cached for
// metricsCollectionCacheTTL to avoid excessive API calls. If metrics-server is
// unavailable, the report still carries node capacity plus NodeHealthError.
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
	// Query nodes first. The core Node API has the capacity/allocatable data
	// needed for useful totals even when metrics-server is absent or forbidden.
	getOut, err := k8sAPIGet("/api/v1/nodes")
	if err != nil {
		reason := nodeHealthErrorReason("nodes API failed", err)
		logNodeHealthWarning(logger, reason)
		return partialClusterHealthReport(reason)
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
		reason := nodeHealthErrorReason("failed to parse nodes JSON", err)
		logNodeHealthWarning(logger, reason)
		return partialClusterHealthReport(reason)
	}

	type nodeInfo struct {
		cpuAllocatable int64 // millicores
		memAllocatable int64 // bytes
		diskCapacity   int64 // bytes; capacity preferred, allocatable fallback
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
	nodeNames := make([]string, 0, len(nodesJSON.Items))
	var totalGPUCapacity, totalGPUAllocatable int
	gpuTypes := map[string]bool{}

	for _, item := range nodesJSON.Items {
		if item.Metadata.Name == "" {
			continue
		}
		ni := &nodeInfo{}
		// Allocatable (not raw capacity) is what the scheduler can place
		// pods against, so hive capacity math below uses these values.
		ni.cpuAllocatable = parseK8sCPU(item.Status.Allocatable["cpu"])
		ni.memAllocatable = parseK8sMemory(item.Status.Allocatable["memory"])
		ni.diskCapacity = parseK8sMemory(item.Status.Capacity["ephemeral-storage"])
		if ni.diskCapacity == 0 {
			ni.diskCapacity = parseK8sMemory(item.Status.Allocatable["ephemeral-storage"])
		}
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
		nodeNames = append(nodeNames, item.Metadata.Name)
	}

	// Query metrics API for live CPU/memory usage. If it is absent, forbidden or
	// malformed, keep capacity totals from the Node API and carry the reason to
	// the hub so the UI can tell operators exactly what to fix.
	metricsByNode := make(map[string]map[string]string)
	nodeHealthError := ""
	topOut, err := k8sAPIGet("/apis/metrics.k8s.io/v1beta1/nodes")
	if err != nil {
		nodeHealthError = nodeHealthErrorReason("metrics API failed", err)
		logNodeHealthWarning(logger, nodeHealthError)
	} else {
		var metricsJSON struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Usage map[string]string `json:"usage"`
			} `json:"items"`
		}
		if err := json.Unmarshal(topOut, &metricsJSON); err != nil {
			nodeHealthError = nodeHealthErrorReason("failed to parse metrics API response", err)
			logNodeHealthWarning(logger, nodeHealthError)
		} else {
			for _, item := range metricsJSON.Items {
				metricsByNode[item.Metadata.Name] = item.Usage
			}
		}
	}

	var nodes []HeartbeatNodeMetric
	for _, name := range nodeNames {
		ni := nodeMap[name]
		cpuCores := int(ni.cpuAllocatable / millicoresPerCore)
		cpuUsed := int64(0)
		cpuPct := 0
		memUsed := int64(0)
		memPct := 0
		if usage, ok := metricsByNode[name]; ok {
			cpuUsed = parseK8sCPU(usage["cpu"])
			memUsed = parseK8sMemory(usage["memory"])
			if ni.cpuAllocatable > 0 {
				cpuPct = int(cpuUsed * percentMultiplier / ni.cpuAllocatable)
			}
			if ni.memAllocatable > 0 {
				memPct = int(memUsed * percentMultiplier / ni.memAllocatable)
			}
		}

		memTotalMB := ni.memAllocatable / bytesPerMB
		memUsedMB := memUsed / bytesPerMB
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
		if ni.diskCapacity > 0 {
			totalMB := ni.diskCapacity / bytesPerMB
			node.DiskTotalMB = &totalMB
		}
		if ni.gpuCapacity > 0 {
			node.GPUs = ni.gpuCapacity
			node.GPUType = ni.gpuType
		}
		// LIVE node filesystem usage from the kubelet stats/summary endpoint.
		// The node object's ephemeral-storage capacity cannot tell us how full the
		// disk is. Best-effort per node: a failure leaves usage/percent nil while
		// retaining capacity above.
		if rawStats, statsErr := k8sAPIGet(nodeStatsSummaryPath(name)); statsErr != nil {
			if logger != nil {
				logger.Debug("spoke metrics: node disk stats unavailable", "node", name, "error", statsErr)
			}
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
	if err != nil {
		if nodeHealthError == "" {
			nodeHealthError = nodeHealthErrorReason("pods API failed", err)
			logNodeHealthWarning(logger, nodeHealthError)
		} else {
			logNodeHealthWarning(logger, nodeHealthErrorReason("pods API failed", err))
		}
	}
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
		} else if nodeHealthError == "" {
			nodeHealthError = "failed to parse pods API response"
			logNodeHealthWarning(logger, nodeHealthError)
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
	// Disk capacity is available from the Node API. Disk percentage is computed
	// only over nodes that reported live usage, so partial coverage stays honest
	// instead of averaging in phantom zeros.
	var totalDiskCapacityBytes, totalDiskUsageBytes, totalDiskUsedBytes int64
	readyNodes := 0

	for _, n := range nodes {
		totalCPUCores += n.CPUCores
		totalCPUUsed += n.CPUUsedMillis
		totalPods += n.Pods
		if n.Ready {
			readyNodes++
		}
		if n.DiskTotalMB != nil {
			totalDiskCapacityBytes += *n.DiskTotalMB * bytesPerMB
			if n.DiskUsedMB != nil {
				totalDiskUsageBytes += *n.DiskTotalMB * bytesPerMB
				totalDiskUsedBytes += *n.DiskUsedMB * bytesPerMB
			}
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

	// TotalDiskGB can come from node capacity alone. TotalDiskPct stays nil
	// when no node reported live usage, so the hub renders the percentage as
	// unavailable rather than displaying a misleading 0%.
	var totalDiskGB, totalDiskPct *int
	if totalDiskCapacityBytes > 0 {
		gb := int(totalDiskCapacityBytes / giToBytes)
		totalDiskGB = &gb
	}
	if totalDiskUsageBytes > 0 {
		pct := int(totalDiskUsedBytes * percentMultiplier / totalDiskUsageBytes)
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
		CollectedAt:     time.Now().UTC().Format(time.RFC3339),
		NodeHealthError: nodeHealthError,
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

func partialClusterHealthReport(reason string) *HeartbeatClusterHealthReport {
	return &HeartbeatClusterHealthReport{
		CollectedAt:     time.Now().UTC().Format(time.RFC3339),
		NodeHealthError: reason,
	}
}

func nodeHealthErrorReason(prefix string, err error) string {
	if err == nil {
		return prefix
	}
	detail := strings.TrimSpace(err.Error())
	if detail == "" {
		return prefix
	}
	const maxNodeHealthErrorDetail = 240
	if len(detail) > maxNodeHealthErrorDetail {
		detail = detail[:maxNodeHealthErrorDetail] + "…"
	}
	return prefix + ": " + detail
}

func logNodeHealthWarning(logger *slog.Logger, reason string) {
	if logger == nil {
		return
	}
	now := time.Now()
	nodeHealthWarningMu.Lock()
	last := lastNodeHealthWarning[reason]
	if now.Sub(last) < nodeHealthWarningInterval {
		nodeHealthWarningMu.Unlock()
		return
	}
	lastNodeHealthWarning[reason] = now
	nodeHealthWarningMu.Unlock()
	logger.Warn("spoke node health partial", "reason", reason)
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

// nodeStatsSummaryPath builds the kubelet stats/summary proxy path for a node.
func nodeStatsSummaryPath(nodeName string) string {
	return "/api/v1/nodes/" + nodeName + "/proxy/stats/summary"
}

type nodeDiskUsage struct {
	capacityBytes int64
	usedBytes     int64
}

func parseNodeStatsSummaryDisk(raw []byte) (nodeDiskUsage, bool) {
	var doc struct {
		Node struct {
			Fs struct {
				CapacityBytes int64 `json:"capacityBytes"`
				UsedBytes     int64 `json:"usedBytes"`
			} `json:"fs"`
		} `json:"node"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nodeDiskUsage{}, false
	}
	if doc.Node.Fs.CapacityBytes <= 0 || doc.Node.Fs.UsedBytes < 0 {
		return nodeDiskUsage{}, false
	}
	return nodeDiskUsage{capacityBytes: doc.Node.Fs.CapacityBytes, usedBytes: doc.Node.Fs.UsedBytes}, true
}

func parseK8sCPU(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "n") {
		v, _ := strconv.ParseInt(strings.TrimSuffix(s, "n"), 10, 64)
		const nanocoresPerMillicore = 1_000_000
		return v / nanocoresPerMillicore
	}
	if strings.HasSuffix(s, "m") {
		v := parseInt(strings.TrimSuffix(s, "m"))
		return int64(v)
	}
	v := parseInt(s)
	return int64(v) * millicoresPerCore
}

func parseK8sMemory(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "Ki") {
		v := parseInt(strings.TrimSuffix(s, "Ki"))
		return int64(v) * kiToBytes
	}
	if strings.HasSuffix(s, "Mi") {
		v := parseInt(strings.TrimSuffix(s, "Mi"))
		return int64(v) * miToBytes
	}
	if strings.HasSuffix(s, "Gi") {
		v := parseInt(strings.TrimSuffix(s, "Gi"))
		return int64(v) * giToBytes
	}
	return int64(parseInt(s))
}

func parseInt(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

func hiveSlotsForNode(cpuAllocatableMillis, memAllocatableBytes, cpuRequestedMillis, memRequestedBytes int64, ready, unschedulable bool) int64 {
	if !ready || unschedulable {
		return 0
	}
	cpuFree := cpuAllocatableMillis - cpuRequestedMillis
	memFree := memAllocatableBytes - memRequestedBytes
	if cpuFree <= 0 || memFree <= 0 {
		return 0
	}
	const perHiveCPUMillis = 1000
	const perHiveMemoryBytes = 2 * giToBytes
	cpuSlots := cpuFree / perHiveCPUMillis
	memSlots := memFree / perHiveMemoryBytes
	if cpuSlots < memSlots {
		return cpuSlots
	}
	return memSlots
}
