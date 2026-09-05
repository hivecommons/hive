// Cluster inventory and health for the SaaS surface: cluster ID
// helpers, the clusters list endpoint, and the cluster health
// aggregation (node capacity/usage, disk, GPU) with its parsers.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// --- Cluster Helpers ---

// clusterIDForSaaSHive returns the cluster ID for a SaaS hive, defaulting to
// the default cluster when the field is empty (backward compatibility).
func clusterIDForSaaSHive(sh SaaSHive) string {
	if sh.ClusterID != "" {
		return sh.ClusterID
	}
	return defaultClusterID
}

// ensureClusterIDForClaim stamps a non-blank cluster_id onto a hive that is
// about to be persisted by a claim/assign. This is the data-integrity guard
// for the observed bug where CLAIMED hives lost cluster_id in their meta.json
// and then fell back to defaultClusterID (the hub-reachable cluster) in clusterForHive — which
// mis-routed App/host resolution for hives that actually run on the heartbeat-only cluster.
//
// Precedence, most-trusted first:
//  1. The hive's OWN non-blank ClusterID (the placeholder already belongs to a
//     cluster — always the most authoritative source; never override it).
//  2. poolFallback — the pool the claim was drawn from, when the caller knows
//     it (e.g. handleApproveProvision picks a pool by auth_method), but only
//     when it names a cluster the hub actually has.
//  3. defaultClusterID — last resort, matching clusterForHive's own fallback.
//
// The result is always non-blank, so with omitempty on the json tag it still
// serializes to a concrete "cluster_id" value rather than vanishing.
func (s *HubServer) ensureClusterIDForClaim(h *SaaSHive, poolFallback string) {
	if h.ClusterID != "" {
		return
	}
	if poolFallback != "" {
		if _, ok := s.clusters[poolFallback]; ok {
			h.ClusterID = poolFallback
			return
		}
	}
	h.ClusterID = defaultClusterID
}

// clusterNameForID returns the human-readable name for a cluster ID.
// Returns empty string when the cluster is not found.
func (s *HubServer) clusterNameForID(clusterID string) string {
	if c, ok := s.clusters[clusterID]; ok {
		return c.Name
	}
	return ""
}

// --- Cluster List API ---

// ClusterListEntry is the JSON response for the clusters list endpoint.
type ClusterListEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	HasGPU bool   `json:"has_gpu"`
	Arch   string `json:"arch"`
	// GitHubHost is the bare hostname of the GitHub instance hives on this
	// cluster default to ("github.com" for public GitHub). Shown in the
	// create-hive modal so an admin can see which GitHub a hive will target.
	GitHubHost string `json:"github_host,omitempty"`
	// AppInstallURL is the GitHub App install link for THIS cluster's GitHub
	// host and app slug. A GitHub Enterprise cluster must never be handed a
	// public github.com link: the install request would land on the wrong
	// GitHub and the GHE org admin would never see it.
	AppInstallURL string `json:"app_install_url,omitempty"`
}

// clusterGitHubConfig projects a cluster's GitHub settings onto the
// config.GitHubConfig that owns URL construction, so the hub and the spoke
// build install links from exactly one implementation.
func clusterGitHubConfig(c *ClusterConfig) config.GitHubConfig {
	if c == nil {
		return config.GitHubConfig{}
	}
	base := c.GitHubBaseURL
	// The cluster stores "" for public GitHub; config.GitHubConfig uses the
	// same convention, so pass it through untouched. Carry the api_url too so the
	// derived config's HostLabel()/IsGHE()/AppInstallURL() resolve the forge
	// base-or-api: a GHE cluster that records only an api_url (blank base_url —
	// the common state) is still recognised as GHE, not mislabelled github.com.
	return config.GitHubConfig{BaseURL: base, APIURL: c.GitHubAPIURL, AppSlug: c.GitHubAppSlug}
}

// githubHostLabel renders a GitHub base URL as a bare hostname for display.
// Empty (public GitHub) becomes "github.com" rather than an empty chip.
func githubHostLabel(baseURL string) string {
	h := strings.TrimSpace(baseURL)
	if h == "" {
		return "github.com"
	}
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	return strings.TrimRight(h, "/")
}

// GitHubHostLabel is the exported form of githubHostLabel, for the spoke to
// normalize its own configured GitHub base URL before reporting it over the
// heartbeat. Sharing one implementation keeps the value the spoke sends and
// the value the hub renders from drifting into two different spellings.
func GitHubHostLabel(baseURL string) string { return githubHostLabel(baseURL) }

func (s *HubServer) handleListClusters(w http.ResponseWriter, r *http.Request) {
	var entries []ClusterListEntry
	for _, c := range s.clusters {
		gh := clusterGitHubConfig(&c)
		entries = append(entries, ClusterListEntry{
			ID:            c.ID,
			Name:          c.Name,
			HasGPU:        c.HasGPU,
			Arch:          c.Arch,
			GitHubHost:    gh.HostLabel(),
			AppInstallURL: gh.AppInstallURL(),
		})
	}
	// Sort for deterministic API output.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ID < entries[j].ID
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

// --- Cluster Health ---

const clusterHealthCacheTTL = 30 * time.Second

// CPU and memory bar thresholds are NOT declared here. The hub serves the
// cluster-health panel raw percentages and the panel colours them with its own
// CLUSTER_CPU_WARN_PCT / CLUSTER_CPU_DANGER_PCT / CLUSTER_MEM_* constants, so a
// Go-side copy would be a second set of numbers that nothing reads and nobody
// updates together. The disk thresholds below are different: they are anchored
// to kubelet behaviour rather than taste and are pinned by a test, so they have
// a reason to exist on this side.
//
// Disk thresholds are anchored to kubelet's own behaviour rather than to
// round numbers, so a coloured bar means something concrete is about to
// happen on the node:
//
//	evictionHard: nodefs.available<10%  -> kubelet starts evicting pods at
//	                                       90% used, so that is the danger line.
//	imageGCHighThresholdPercent: 85     -> kubelet begins garbage-collecting
//	                                       images at 85% used. That is the
//	                                       first automatic reaction to disk
//	                                       filling up, which makes it the
//	                                       right moment to warn an operator
//	                                       while there is still headroom.
//
// clusterHealthDiskWarnPct is the disk usage percentage at which kubelet's
// image garbage collection kicks in (imageGCHighThresholdPercent).
const clusterHealthDiskWarnPct = 85

// clusterHealthDiskDangerPct is the disk usage percentage at which kubelet's
// hard eviction threshold fires (nodefs.available<10%).
const clusterHealthDiskDangerPct = 90

// millicoresPerCore converts cores to millicores.
const millicoresPerCore = 1000

// kiToBytes converts Ki units to bytes.
const kiToBytes = 1024

// miToBytes converts Mi units to bytes.
const miToBytes = 1024 * 1024

// giToBytes converts Gi units to bytes.
const giToBytes = 1024 * 1024 * 1024

// bytesPerMB converts bytes to megabytes.
const bytesPerMB = 1024 * 1024

// percentMultiplier converts a ratio to a percentage.
const percentMultiplier = 100

type ClusterHealthNode struct {
	Name          string `json:"name"`
	CPUCores      int    `json:"cpu_cores"`
	CPUUsedMillis int64  `json:"cpu_used_millicores"`
	CPUPercent    int    `json:"cpu_percent"`
	MemTotalMB    int64  `json:"mem_total_mb"`
	MemUsedMB     int64  `json:"mem_used_mb"`
	MemPercent    int    `json:"mem_percent"`
	DiskPressure  bool   `json:"disk_pressure"`
	// Disk fields describe the node filesystem (nodefs) that kubelet applies
	// its eviction thresholds to. They are pointers because live disk usage
	// comes from the kubelet stats/summary endpoint, which a hub may not be
	// able to reach; nil means "unknown" and must render as dashes rather
	// than as 0 (which would read as healthy).
	DiskTotalMB *int64 `json:"disk_total_mb,omitempty"`
	DiskUsedMB  *int64 `json:"disk_used_mb,omitempty"`
	DiskPercent *int   `json:"disk_percent,omitempty"`
	Pods        int    `json:"pods"`
	PodCapacity int    `json:"pod_capacity"`
	// HiveCount is the number of distinct hive-hosted-* namespaces with a
	// running pod on this node (namespaces, not pods, so a hive briefly
	// running two pods during a rollout is counted once).
	HiveCount  int      `json:"hive_count"`
	Conditions []string `json:"conditions"`
}

// hiveHostedNamespacePrefix is the namespace prefix used for SaaS-provisioned
// hives; pods in these namespaces identify hives running on a node.
const hiveHostedNamespacePrefix = "hive-hosted-"

type ClusterHealthSummary struct {
	TotalNodes    int `json:"total_nodes"`
	TotalCPUCores int `json:"total_cpu_cores"`
	TotalCPUPct   int `json:"total_cpu_percent"`
	TotalMemGB    int `json:"total_mem_gb"`
	TotalMemPct   int `json:"total_mem_percent"`
	// Disk totals cover only the nodes that reported live disk usage. They
	// are pointers so a cluster with no reachable kubelet stats endpoint
	// omits them entirely instead of reporting a misleading 0%.
	TotalDiskGB  *int `json:"total_disk_gb,omitempty"`
	TotalDiskPct *int `json:"total_disk_percent,omitempty"`
	HiveCount    int  `json:"hive_count"`
	// HiveCapacityRemaining estimates how many MORE hives the cluster can
	// hold: per Ready, schedulable node, the per-hive request footprint
	// (see hive_capacity.go) bin-packed into allocatable-minus-requested
	// capacity, summed across nodes. Pointer so it is omitted entirely when
	// pod request data was unavailable (nil = no data, 0 = cluster full).
	HiveCapacityRemaining *int `json:"hive_capacity_remaining,omitempty"`
}

// GPUSummary reports aggregate GPU counts for a cluster.
type GPUSummary struct {
	TotalGPUs       int `json:"total_gpus"`
	AllocatableGPUs int `json:"allocatable_gpus"`
}

// PerClusterHealth holds health data for a single cluster.
type PerClusterHealth struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	Nodes      []ClusterHealthNode  `json:"nodes"`
	Summary    ClusterHealthSummary `json:"summary"`
	GPUSummary *GPUSummary          `json:"gpu_summary,omitempty"`
	HiveCount  int                  `json:"hive_count"`
	Error      string               `json:"error,omitempty"`
	DataSource string               `json:"data_source,omitempty"` // "heartbeat" when data comes from spoke heartbeat instead of kubectl
	DataStale  bool                 `json:"data_stale,omitempty"`  // true when heartbeat data is older than heartbeatHealthStaleness
	DataAge    string               `json:"data_age,omitempty"`    // human-readable age or collection timestamp
	// StuckPods reports hive-namespace pods stuck Terminating — the residue of
	// nodes disappearing without draining (#5328 item 3). Nil means the hub
	// could not determine it (unreachable cluster, pull-only pool, failed
	// listing); a non-nil report with Total 0 means it looked and the cluster
	// is clean. Those must not render alike: 27 orphans accumulated for three
	// weeks precisely because nothing distinguished "none" from "nobody
	// checked". See orphaned_pod_visibility.go.
	StuckPods *StuckPodReport `json:"stuck_pods,omitempty"`
	// LeakedNamespaces reports hive-hosted-* namespaces the cluster holds that
	// this hub has no hive record for. Nil means unknown; non-nil with Total 0
	// means the hub checked and found none.
	LeakedNamespaces *LeakedNamespaceReport `json:"leaked_namespaces,omitempty"`
}

type ClusterHealthResponse struct {
	// Flat fields for backward compatibility (aggregate across all clusters).
	Nodes   []ClusterHealthNode  `json:"nodes"`
	Summary ClusterHealthSummary `json:"summary"`
	// Per-cluster breakdown.
	Clusters []PerClusterHealth `json:"clusters,omitempty"`
}

var (
	clusterHealthCache     *ClusterHealthResponse
	clusterHealthCacheTime time.Time
	clusterHealthCacheMu   sync.Mutex
)

func (s *HubServer) handleClusterHealth(w http.ResponseWriter, r *http.Request) {
	clusterHealthCacheMu.Lock()
	if clusterHealthCache != nil && time.Since(clusterHealthCacheTime) < clusterHealthCacheTTL {
		cached := clusterHealthCache
		clusterHealthCacheMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cached)
		return
	}
	clusterHealthCacheMu.Unlock()

	resp, err := buildClusterHealth(s)
	if err != nil {
		s.logger.Error("cluster health failed", "error", err)
		http.Error(w, `{"error":"failed to gather cluster health"}`, http.StatusInternalServerError)
		return
	}

	clusterHealthCacheMu.Lock()
	clusterHealthCache = resp
	clusterHealthCacheTime = time.Now()
	clusterHealthCacheMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

const (
	// defaultClusterHealthQueryTimeout limits how long we wait for in-cluster
	// health queries when the cluster config does not override it.
	defaultClusterHealthQueryTimeout = 30 * time.Second
	// remoteClusterHealthQueryTimeout gives remote clusters more time because
	// they traverse an external network path from the hub pod.
	remoteClusterHealthQueryTimeout = 60 * time.Second
)

// gpuResourceKey is the extended resource name for NVIDIA GPUs on Kubernetes nodes.
const gpuResourceKey = "nvidia.com/gpu"

func buildClusterHealth(s *HubServer) (*ClusterHealthResponse, error) {
	// Count hives per cluster, deduplicated by hive ID. A hosted hive appears
	// both as a SaaS record and as a registry entry with a ClusterID (they
	// share the same ID), so counting both sources naively doubles the count.
	hiveIDsByCluster := make(map[string]map[string]bool)
	allHiveIDs := make(map[string]bool)
	addHive := func(clusterID, hiveID string) {
		if hiveIDsByCluster[clusterID] == nil {
			hiveIDsByCluster[clusterID] = make(map[string]bool)
		}
		hiveIDsByCluster[clusterID][hiveID] = true
		allHiveIDs[hiveID] = true
	}
	saasHives, saasHivesReadable := listSaaSHivesWithReadStatus()
	for _, sh := range saasHives {
		addHive(clusterIDForSaaSHive(sh), sh.ID)
	}
	s.mu.RLock()
	for _, h := range s.registry.Hives {
		if h.ClusterID != "" {
			addHive(h.ClusterID, h.ID)
		} else {
			// Self-hosted hives without a cluster still count globally.
			allHiveIDs[h.ID] = true
		}
	}
	s.mu.RUnlock()
	hiveCounts := make(map[string]int, len(hiveIDsByCluster))
	for cid, ids := range hiveIDsByCluster {
		hiveCounts[cid] = len(ids)
	}
	totalHiveCount := len(allHiveIDs)
	var knownHostedNamespaces map[string]struct{}
	if saasHivesReadable {
		knownHostedNamespaces = hostedNamespacesForHiveIDs(allHiveIDs)
	} else if s.logger != nil {
		s.logger.Warn("leaked-namespace detection disabled: SaaS hive directory could not be read")
	}

	// Query all clusters in parallel.
	type clusterResult struct {
		health PerClusterHealth
		err    error
	}
	type clusterQuery struct {
		cluster ClusterConfig
		ch      chan clusterResult
	}
	results := make(map[string]clusterQuery)
	for _, c := range s.clusters {
		ch := make(chan clusterResult, 1)
		results[c.ID] = clusterQuery{cluster: c, ch: ch}
		go func(cluster ClusterConfig) {
			health, err := buildSingleClusterHealth(&cluster, hiveCounts[cluster.ID], knownHostedNamespaces, s.logger)
			ch <- clusterResult{health: health, err: err}
		}(c)
	}

	// Collect results with timeout.
	var allNodes []ClusterHealthNode
	var perCluster []PerClusterHealth
	var aggCPUCores int
	var aggCPUUsed int64
	var aggCPUAlloc int64
	var aggMemAlloc int64
	var aggMemUsed int64

	for cID, query := range results {
		timeout := clusterHealthQueryTimeoutFor(&query.cluster)
		select {
		case res := <-query.ch:
			if res.err != nil {
				if errors.Is(res.err, errClusterPullOnly) {
					s.logger.Info("cluster health: pull-only cluster, using the health its spokes report over the heartbeat", "cluster", cID)
				} else {
					s.logger.Warn("cluster health query failed", "cluster", cID, "error", res.err)
				}
				// Fall back to heartbeat-reported health if available.
				if hbHealth := s.getHeartbeatHealthForCluster(cID); hbHealth != nil {
					pch := convertHeartbeatToPerClusterHealth(cID, s.clusterNameForID(cID), hbHealth, hiveCounts[cID])
					perCluster = append(perCluster, pch)
					allNodes = append(allNodes, pch.Nodes...)
					aggCPUCores += pch.Summary.TotalCPUCores
					aggCPUUsed += int64(pch.Summary.TotalCPUPct) * int64(pch.Summary.TotalCPUCores) * millicoresPerCore / percentMultiplier
					aggCPUAlloc += int64(pch.Summary.TotalCPUCores) * millicoresPerCore
					aggMemAlloc += int64(pch.Summary.TotalMemGB) * giToBytes
					aggMemUsed += int64(pch.Summary.TotalMemPct) * int64(pch.Summary.TotalMemGB) * giToBytes / percentMultiplier
					s.logger.Info("cluster health: using heartbeat fallback", "cluster", cID)
					continue
				}
				perCluster = append(perCluster, PerClusterHealth{
					ID:    cID,
					Name:  s.clusterNameForID(cID),
					Error: res.err.Error(),
				})
				continue
			}
			pch := res.health
			pch.ID = cID
			pch.Name = s.clusterNameForID(cID)
			perCluster = append(perCluster, pch)
			allNodes = append(allNodes, pch.Nodes...)
			aggCPUCores += pch.Summary.TotalCPUCores
			aggCPUUsed += int64(pch.Summary.TotalCPUPct) * int64(pch.Summary.TotalCPUCores) * millicoresPerCore / percentMultiplier
			aggCPUAlloc += int64(pch.Summary.TotalCPUCores) * millicoresPerCore
			aggMemAlloc += int64(pch.Summary.TotalMemGB) * giToBytes
			aggMemUsed += int64(pch.Summary.TotalMemPct) * int64(pch.Summary.TotalMemGB) * giToBytes / percentMultiplier
		case <-time.After(timeout):
			s.logger.Warn("cluster health query timed out", "cluster", cID, "timeout", timeout.String())
			// Fall back to heartbeat-reported health if available.
			if hbHealth := s.getHeartbeatHealthForCluster(cID); hbHealth != nil {
				pch := convertHeartbeatToPerClusterHealth(cID, s.clusterNameForID(cID), hbHealth, hiveCounts[cID])
				perCluster = append(perCluster, pch)
				allNodes = append(allNodes, pch.Nodes...)
				aggCPUCores += pch.Summary.TotalCPUCores
				aggCPUUsed += int64(pch.Summary.TotalCPUPct) * int64(pch.Summary.TotalCPUCores) * millicoresPerCore / percentMultiplier
				aggCPUAlloc += int64(pch.Summary.TotalCPUCores) * millicoresPerCore
				aggMemAlloc += int64(pch.Summary.TotalMemGB) * giToBytes
				aggMemUsed += int64(pch.Summary.TotalMemPct) * int64(pch.Summary.TotalMemGB) * giToBytes / percentMultiplier
				s.logger.Info("cluster health: using heartbeat fallback after timeout", "cluster", cID)
				continue
			}
			perCluster = append(perCluster, PerClusterHealth{
				ID:    cID,
				Name:  s.clusterNameForID(cID),
				Error: "query timed out",
			})
		}
	}

	// Include heartbeat-only clusters that are NOT in s.clusters but do have
	// heartbeat-reported health data. This handles firewalled spokes whose
	// cluster isn't in the hub's clusters.json.
	clusterSeen := make(map[string]bool, len(results))
	for cID := range results {
		clusterSeen[cID] = true
	}
	s.heartbeatHealthMu.RLock()
	for cID, entry := range s.heartbeatHealth {
		if clusterSeen[cID] || entry == nil || entry.Report == nil {
			continue
		}
		pch := convertHeartbeatToPerClusterHealth(cID, s.clusterNameForID(cID), entry, hiveCounts[cID])
		perCluster = append(perCluster, pch)
		allNodes = append(allNodes, pch.Nodes...)
		aggCPUCores += pch.Summary.TotalCPUCores
		aggCPUUsed += int64(pch.Summary.TotalCPUPct) * int64(pch.Summary.TotalCPUCores) * millicoresPerCore / percentMultiplier
		aggCPUAlloc += int64(pch.Summary.TotalCPUCores) * millicoresPerCore
		aggMemAlloc += int64(pch.Summary.TotalMemGB) * giToBytes
		aggMemUsed += int64(pch.Summary.TotalMemPct) * int64(pch.Summary.TotalMemGB) * giToBytes / percentMultiplier
	}
	s.heartbeatHealthMu.RUnlock()

	// Compute aggregates only after heartbeat-only clusters are included; an
	// earlier pre-inclusion computation was dead (always overwritten here).
	aggCPUPct := 0
	if aggCPUAlloc > 0 {
		aggCPUPct = int(aggCPUUsed * percentMultiplier / aggCPUAlloc)
	}
	aggMemPct := 0
	if aggMemAlloc > 0 {
		aggMemPct = int(aggMemUsed * percentMultiplier / aggMemAlloc)
	}
	aggMemGB := int(aggMemAlloc / giToBytes)

	// Sort clusters by ID for deterministic output.
	sort.Slice(perCluster, func(i, j int) bool {
		return perCluster[i].ID < perCluster[j].ID
	})

	// Every collection path above appends its nodes to allNodes, so the fleet
	// disk total is derived from them directly. Nodes with no disk data are
	// skipped, so an unreachable cluster lowers coverage without skewing the
	// percentage; if no node anywhere reported, disk is omitted entirely.
	aggDiskGB, aggDiskPct := summarizeDisk(allNodes)

	return &ClusterHealthResponse{
		Nodes: allNodes,
		Summary: ClusterHealthSummary{
			TotalNodes:    len(allNodes),
			TotalCPUCores: aggCPUCores,
			TotalCPUPct:   aggCPUPct,
			TotalMemGB:    aggMemGB,
			TotalMemPct:   aggMemPct,
			TotalDiskGB:   aggDiskGB,
			TotalDiskPct:  aggDiskPct,
			HiveCount:     totalHiveCount,
		},
		Clusters: perCluster,
	}, nil
}

func clusterHealthQueryTimeoutFor(cluster *ClusterConfig) time.Duration {
	if cluster != nil && cluster.ClusterHealthTimeoutSeconds > 0 {
		return time.Duration(cluster.ClusterHealthTimeoutSeconds) * time.Second
	}
	if cluster != nil && !cluster.InCluster {
		return remoteClusterHealthQueryTimeout
	}
	return defaultClusterHealthQueryTimeout
}

// errClusterPullOnly marks a health query skipped because the hub cannot reach
// the cluster at all. It is an EXPECTED outcome, not a fault, so callers report
// it as such and use the spokes' own heartbeat-reported health instead.
var errClusterPullOnly = errors.New("cluster is pull-only: not reachable from the hub")

// buildSingleClusterHealth queries a single cluster for node health data.
func buildSingleClusterHealth(cluster *ClusterConfig, hiveCount int, knownHostedNamespaces map[string]struct{}, logger *slog.Logger) (PerClusterHealth, error) {
	if cluster.PullOnly {
		// Node-level health comes from kubectl, which cannot run here. This is
		// not a new failure mode: the caller already falls back to the health
		// the spokes THEMSELVES report over the heartbeat, which is exactly the
		// right source for a pull-only pool. Returning the sentinel routes into
		// that path and keeps it out of the "query failed" warning.
		return PerClusterHealth{}, fmt.Errorf("%w: %s", errClusterPullOnly, cluster.ID)
	}
	timeout := clusterHealthQueryTimeoutFor(cluster)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Run kubectl top nodes
	topCmd := kubectlForClusterContext(ctx, cluster, "--request-timeout", timeout.String(), "top", "nodes", "--no-headers")
	topOut, err := topCmd.CombinedOutput()
	if err != nil {
		return PerClusterHealth{}, fmt.Errorf("kubectl top nodes on %s: exit status 1: %s", cluster.ID, string(topOut))
	}

	// Run kubectl get nodes -o json
	getCmd := kubectlForClusterContext(ctx, cluster, "--request-timeout", timeout.String(), "get", "nodes", "-o", "json")
	getOut, err := getCmd.CombinedOutput()
	if err != nil {
		return PerClusterHealth{}, fmt.Errorf("kubectl get nodes on %s: %w: %s", cluster.ID, err, string(getOut))
	}

	// Parse kubectl get nodes output
	var nodesJSON struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
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
		return PerClusterHealth{}, fmt.Errorf("parse nodes JSON on %s: %w", cluster.ID, err)
	}

	// Build a map of node info from kubectl get.
	type nodeInfo struct {
		cpuAllocatable int64 // millicores
		memAllocatable int64 // bytes
		podCapacity    int
		diskPressure   bool
		ready          bool
		unschedulable  bool // cordoned; excluded from hive capacity estimates
		conditions     []string
		gpuCapacity    int
		gpuAllocatable int
	}
	nodeMap := make(map[string]*nodeInfo)
	var totalGPUCapacity, totalGPUAllocatable int
	for _, item := range nodesJSON.Items {
		ni := &nodeInfo{}
		// Allocatable (not raw capacity) is what the scheduler can place
		// pods against, so hive capacity math below uses these values.
		ni.cpuAllocatable = parseK8sCPU(item.Status.Allocatable["cpu"])
		ni.memAllocatable = parseK8sMemory(item.Status.Allocatable["memory"])
		ni.podCapacity = parseInt(item.Status.Capacity["pods"])
		ni.gpuCapacity = parseInt(item.Status.Capacity[gpuResourceKey])
		ni.gpuAllocatable = parseInt(item.Status.Allocatable[gpuResourceKey])
		ni.unschedulable = item.Spec.Unschedulable
		totalGPUCapacity += ni.gpuCapacity
		totalGPUAllocatable += ni.gpuAllocatable
		for _, cond := range item.Status.Conditions {
			if cond.Type == "DiskPressure" && cond.Status == "True" {
				ni.diskPressure = true
			}
			if cond.Type == "Ready" && cond.Status == "True" {
				ni.ready = true
				ni.conditions = append(ni.conditions, "Ready")
			} else if cond.Type == "Ready" && cond.Status != "True" {
				ni.conditions = append(ni.conditions, "NotReady")
			}
		}
		if len(ni.conditions) == 0 {
			ni.conditions = []string{"Unknown"}
		}
		nodeMap[item.Metadata.Name] = ni
	}

	// Parse kubectl top nodes output.
	// Format: NAME  CPU(cores)  CPU%  MEMORY(bytes)  MEMORY%
	var nodes []ClusterHealthNode
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

		nodes = append(nodes, ClusterHealthNode{
			Name:          name,
			CPUCores:      cpuCores,
			CPUUsedMillis: cpuUsed,
			CPUPercent:    cpuPct,
			MemTotalMB:    memTotalMB,
			MemUsedMB:     memUsedMB,
			MemPercent:    memPct,
			DiskPressure:  ni.diskPressure,
			Pods:          0, // populated below
			PodCapacity:   ni.podCapacity,
			Conditions:    ni.conditions,
		})
	}

	// Collect LIVE node filesystem usage from each kubelet's stats/summary
	// endpoint. This is best-effort per node: a node whose kubelet proxy is
	// unreachable simply keeps nil disk fields and renders as unknown, which
	// must never degrade the rest of this cluster's health data.
	for i := range nodes {
		rawStats, statsErr := kubectlForClusterContext(ctx, cluster,
			"--request-timeout", timeout.String(),
			"get", "--raw", nodeStatsSummaryPath(nodes[i].Name)).Output()
		if statsErr != nil {
			if logger != nil {
				logger.Debug("cluster health: node disk stats unavailable",
					"cluster", cluster.ID, "node", nodes[i].Name, "error", statsErr)
			}
			continue
		}
		if usage, ok := parseNodeStatsSummaryDisk(rawStats); ok {
			applyNodeDiskUsage(&nodes[i], usage)
		}
	}

	// Count running pods per node and sum their container resource REQUESTS
	// (requests, not usage — that is what the scheduler bin-packs against).
	// Listing only Running pods slightly undercounts requests (Pending pods
	// already assigned to a node are missed), so the capacity estimate below
	// can be marginally optimistic.
	var cpuRequestedPerNode, memRequestedPerNode map[string]int64
	podOut, _ := kubectlForCluster(cluster, "get", "pods", "--all-namespaces", "--field-selector=status.phase=Running", "-o", "json").Output()
	if len(podOut) > 0 {
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
	var hiveCapacityRemaining *int
	if cpuRequestedPerNode != nil {
		var totalSlots int64
		for name, ni := range nodeMap {
			totalSlots += hiveSlotsForNode(ni.cpuAllocatable, ni.memAllocatable,
				cpuRequestedPerNode[name], memRequestedPerNode[name], ni.ready, ni.unschedulable)
		}
		slots := int(totalSlots)
		hiveCapacityRemaining = &slots
		// Headroom alert: warn operators before the cluster fills. Fires when
		// fewer than 10% of total estimated slots (current hives + remaining)
		// are left. Cheap to emit here — this path is cached for 30s and only
		// runs on health page loads.
		if total := hiveCount + slots; total > 0 && logger != nil {
			if slots*100 < total*capacityHeadroomWarnPct {
				logger.Warn("cluster hive capacity headroom low",
					"cluster", cluster.ID, "hives", hiveCount,
					"slots_remaining", slots, "headroom_pct", slots*100/total)
			}
		}
	}

	// Build summary.
	var totalCPUCores int
	var totalCPUUsed int64
	var totalCPUAlloc int64
	var totalMemAlloc int64
	var totalMemUsed int64
	for _, n := range nodes {
		totalCPUCores += n.CPUCores
		totalCPUUsed += n.CPUUsedMillis
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
	totalDiskGB, totalDiskPct := summarizeDisk(nodes)

	result := PerClusterHealth{
		Nodes: nodes,
		Summary: ClusterHealthSummary{
			TotalNodes:            len(nodes),
			TotalCPUCores:         totalCPUCores,
			TotalCPUPct:           totalCPUPct,
			TotalMemGB:            totalMemGB,
			TotalMemPct:           totalMemPct,
			TotalDiskGB:           totalDiskGB,
			TotalDiskPct:          totalDiskPct,
			HiveCount:             hiveCount,
			HiveCapacityRemaining: hiveCapacityRemaining,
		},
		HiveCount: hiveCount,
	}

	// Include GPU summary for clusters with GPUs.
	if totalGPUCapacity > 0 {
		result.GPUSummary = &GPUSummary{
			TotalGPUs:       totalGPUCapacity,
			AllocatableGPUs: totalGPUAllocatable,
		}
	}

	// Orphaned Terminating-pod count (#5328 item 3). READ-ONLY: one extra
	// `kubectl get pods` on a path that already lists pods. It needs its own
	// listing because the query above is field-selected to phase=Running and
	// therefore cannot see an orphan by construction.
	//
	// Best-effort: nil on failure, so a cluster the hub could not interrogate
	// reports UNKNOWN rather than a reassuring zero.
	if stuck := collectStuckPods(ctx, cluster, timeout, time.Now()); stuck != nil {
		result.StuckPods = stuck
		// Log when the fleet is actually accumulating orphans. The reaper
		// clears them, so a persistently non-zero count here means orphans are
		// being PRODUCED faster than they age past orphanedPodMinAge — which is
		// the upstream node-lifecycle fault (#5328 item 1), not a reaper
		// problem. Silence on zero keeps a healthy fleet quiet.
		if stuck.Total > 0 && logger != nil {
			logger.Warn("cluster has hive pods stuck terminating — check for ungraceful node loss",
				"cluster", cluster.ID,
				"stuck_pods", stuck.Total,
				"namespaces_affected", stuck.NamespacesAffected)
		}
	}
	if leaked := collectLeakedHostedNamespaces(ctx, cluster, timeout, knownHostedNamespaces, time.Now(), logger); leaked != nil {
		result.LeakedNamespaces = leaked
		if leaked.Total > 0 && logger != nil {
			logger.Warn("cluster holds hive-hosted namespaces with no hive record — leaked provisioning namespaces, nothing will reclaim them",
				"cluster", cluster.ID,
				"leaked_namespaces", leaked.Total)
		}
	}

	return result, nil
}

// getHeartbeatHealthForCluster retrieves the latest heartbeat-reported health
// for a cluster. Returns nil if no data exists or the data is too old.
func (s *HubServer) getHeartbeatHealthForCluster(clusterID string) *HeartbeatHealthEntry {
	s.heartbeatHealthMu.RLock()
	entry, ok := s.heartbeatHealth[clusterID]
	s.heartbeatHealthMu.RUnlock()
	if !ok || entry == nil || entry.Report == nil {
		return nil
	}
	return entry
}

// convertHeartbeatToPerClusterHealth converts heartbeat-reported health data
// into the hub's PerClusterHealth format for display. If the data is older
// than heartbeatHealthStaleness, it is marked with a staleness warning.
func convertHeartbeatToPerClusterHealth(clusterID, clusterName string, entry *HeartbeatHealthEntry, hiveCount int) PerClusterHealth {
	report := entry.Report

	// Convert HeartbeatNodeMetric to ClusterHealthNode.
	nodes := make([]ClusterHealthNode, len(report.Nodes))
	for i, n := range report.Nodes {
		nodes[i] = ClusterHealthNode{
			Name:          n.Name,
			CPUCores:      n.CPUCores,
			CPUUsedMillis: n.CPUUsedMillis,
			CPUPercent:    n.CPUPercent,
			MemTotalMB:    n.MemTotalMB,
			MemUsedMB:     n.MemUsedMB,
			MemPercent:    n.MemPercent,
			DiskPressure:  n.DiskPressure,
			DiskTotalMB:   n.DiskTotalMB,
			DiskUsedMB:    n.DiskUsedMB,
			DiskPercent:   n.DiskPercent,
			Pods:          n.Pods,
			PodCapacity:   n.PodCapacity,
			HiveCount:     n.HiveCount,
			Conditions:    n.Conditions,
		}
	}

	pch := PerClusterHealth{
		ID:    clusterID,
		Name:  clusterName,
		Nodes: nodes,
		Summary: ClusterHealthSummary{
			TotalNodes:    report.Summary.TotalNodes,
			TotalCPUCores: report.Summary.TotalCPUCores,
			TotalCPUPct:   report.Summary.TotalCPUPct,
			TotalMemGB:    report.Summary.TotalMemGB,
			TotalMemPct:   report.Summary.TotalMemPct,
			// nil for spokes that could not read kubelet disk stats.
			TotalDiskGB:  report.Summary.TotalDiskGB,
			TotalDiskPct: report.Summary.TotalDiskPct,
			HiveCount:    hiveCount,
			// nil for spokes running older builds that do not report it.
			HiveCapacityRemaining: report.Summary.HiveCapacityRemaining,
		},
		HiveCount:  hiveCount,
		DataSource: "heartbeat",
	}

	// Mark staleness if heartbeat data is too old.
	age := time.Since(entry.ReceivedAt)
	if age > heartbeatHealthStaleness {
		pch.DataStale = true
		pch.DataAge = fmt.Sprintf("%dm ago", int(age.Minutes()))
	} else if report.CollectedAt != "" {
		pch.DataAge = report.CollectedAt
	}

	// Convert GPU summary.
	if report.GPUSummary != nil {
		pch.GPUSummary = &GPUSummary{
			TotalGPUs:       report.GPUSummary.Total,
			AllocatableGPUs: report.GPUSummary.Total - report.GPUSummary.Allocated,
		}
	}

	return pch
}

// nodeDiskUsage holds live node filesystem usage for one node.
type nodeDiskUsage struct {
	usedBytes     int64
	capacityBytes int64
}

// nodeStatsSummaryPath builds the kubelet stats/summary proxy path for a node.
// This endpoint is the only source of LIVE disk usage: the node object's
// capacity/allocatable["ephemeral-storage"] reports the declared size only and
// says nothing about how full the filesystem actually is.
func nodeStatsSummaryPath(nodeName string) string {
	return "/api/v1/nodes/" + nodeName + "/proxy/stats/summary"
}

// parseNodeStatsSummaryDisk extracts node filesystem usage from a kubelet
// stats/summary response. node.fs is the nodefs that kubelet's
// evictionHard nodefs.available threshold applies to.
func parseNodeStatsSummaryDisk(raw []byte) (nodeDiskUsage, bool) {
	var summary struct {
		Node struct {
			FS struct {
				UsedBytes     *int64 `json:"usedBytes"`
				CapacityBytes *int64 `json:"capacityBytes"`
			} `json:"fs"`
		} `json:"node"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nodeDiskUsage{}, false
	}
	fs := summary.Node.FS
	// Both values are required: without capacity there is no percentage, and
	// a missing usedBytes must not be treated as zero usage.
	if fs.UsedBytes == nil || fs.CapacityBytes == nil || *fs.CapacityBytes <= 0 {
		return nodeDiskUsage{}, false
	}
	return nodeDiskUsage{usedBytes: *fs.UsedBytes, capacityBytes: *fs.CapacityBytes}, true
}

// applyNodeDiskUsage fills the disk fields on a health node from live usage.
// Nodes with no usable stats keep nil disk fields and render as unknown.
func applyNodeDiskUsage(n *ClusterHealthNode, d nodeDiskUsage) {
	totalMB := d.capacityBytes / bytesPerMB
	usedMB := d.usedBytes / bytesPerMB
	pct := int(d.usedBytes * percentMultiplier / d.capacityBytes)
	n.DiskTotalMB = &totalMB
	n.DiskUsedMB = &usedMB
	n.DiskPercent = &pct
}

// summarizeDisk aggregates per-node disk usage into cluster totals, counting
// only nodes that actually reported usage. Returns nil,nil when no node did,
// so the UI omits disk for that cluster rather than showing a false 0%.
func summarizeDisk(nodes []ClusterHealthNode) (*int, *int) {
	var totalBytes, usedBytes int64
	for _, n := range nodes {
		if n.DiskTotalMB == nil || n.DiskUsedMB == nil {
			continue
		}
		totalBytes += *n.DiskTotalMB * bytesPerMB
		usedBytes += *n.DiskUsedMB * bytesPerMB
	}
	if totalBytes <= 0 {
		return nil, nil
	}
	totalGB := int(totalBytes / giToBytes)
	pct := int(usedBytes * percentMultiplier / totalBytes)
	return &totalGB, &pct
}

// parseK8sCPU parses Kubernetes CPU resource strings (e.g. "4", "4000m", "5866711668n").
// Returns millicores.
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

// parseK8sMemory parses Kubernetes memory resource strings (e.g. "16384Ki", "8Gi").
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

// parseTopCPU parses kubectl top CPU values (e.g. "1200m", "2").
func parseTopCPU(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "m") {
		v := parseInt(strings.TrimSuffix(s, "m"))
		return int64(v)
	}
	v := parseInt(s)
	return int64(v) * millicoresPerCore
}

// parseTopMemory parses kubectl top memory values (e.g. "4096Mi", "8Gi").
func parseTopMemory(s string) int64 {
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

// parseInt parses an integer from a string, returning 0 on failure.
func parseInt(s string) int {
	var v int
	_, _ = fmt.Sscanf(s, "%d", &v)
	return v
}
