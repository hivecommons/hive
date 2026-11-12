package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

const (
	// hostAliasesReconcileInterval throttles the sweep. Like NET_ADMIN and the
	// per-hive env lane this is static remediation: a hive either carries the
	// cluster's hostAliases or it does not, and a converged hive stays
	// converged until clusters.json changes.
	hostAliasesReconcileInterval = 15 * time.Minute

	// hostAliasesKubectlTimeout bounds each per-hive kubectl get/patch so one
	// unreachable cluster cannot stall the whole sweep.
	hostAliasesKubectlTimeout = 15 * time.Second

	// hostAliasesMaxPatchesPerCycle caps PATCHES per sweep. A hostAliases change
	// is a podspec change and rolls the pod; the 2026-11-11 vllm-d rollout
	// showed 28 simultaneous rollouts saturating CRI-O on one worker (5.8 GB
	// image) and liveness-killing neighbours, so convergence is deliberately
	// slow: 3 per 15 min.
	hostAliasesMaxPatchesPerCycle = 3

	// podHostAliasesPath is the JSON-Patch path to the pod-level hostAliases
	// list in the hive Deployment.
	podHostAliasesPath = "/spec/template/spec/hostAliases"
)

// ClusterHostAlias is one Pod hostAliases entry the hosted-hive provisioner
// renders into every hive Deployment on the cluster (and the reconcile sweep
// converges onto existing ones). It exists for GitHub Enterprise hosts that
// are reachable only through a TLS REVERSE proxy pinned to a fixed IP — not a
// CONNECT forward proxy, so upstream_proxy cannot express it (#17801).
type ClusterHostAlias struct {
	IP        string   `json:"ip" yaml:"ip"`
	Hostnames []string `json:"hostnames" yaml:"hostnames"`
}

// normalizeClusterHostAliases validates and canonicalises a cluster's
// host_aliases: the IP must parse, every entry needs at least one hostname,
// hostnames are trimmed and lowercased, blanks dropped. Returns nil for an
// empty input so callers can test `len(out) == 0`.
func normalizeClusterHostAliases(in []ClusterHostAlias) ([]ClusterHostAlias, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]ClusterHostAlias, 0, len(in))
	for i, a := range in {
		ip := strings.TrimSpace(a.IP)
		if net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("host_aliases[%d]: ip %q does not parse", i, a.IP)
		}
		names := make([]string, 0, len(a.Hostnames))
		for _, h := range a.Hostnames {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" {
				continue
			}
			if strings.ContainsAny(h, " \t/:") {
				return nil, fmt.Errorf("host_aliases[%d]: hostname %q is not a bare host", i, h)
			}
			names = append(names, h)
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("host_aliases[%d]: ip %s has no hostnames", i, ip)
		}
		out = append(out, ClusterHostAlias{IP: ip, Hostnames: names})
	}
	return out, nil
}

// hostAliasesPatchJSON builds the JSON-Patch that sets the pod-level
// hostAliases to exactly want. "add" on an existing path replaces it, so one
// op serves both the missing and the drifted case.
func hostAliasesPatchJSON(want []ClusterHostAlias) (string, error) {
	val, err := json.Marshal(want)
	if err != nil {
		return "", err
	}
	patch, err := json.Marshal([]map[string]any{{
		"op": "add", "path": podHostAliasesPath, "value": json.RawMessage(val),
	}})
	if err != nil {
		return "", err
	}
	return string(patch), nil
}

// hostAliasesConverged reports whether the live hostAliases (raw jsonpath
// output, possibly empty) already carry every entry in want. Order and
// hostname case are ignored; EXTRA live entries are tolerated so an operator's
// hand-applied alias for an unrelated host is never clobbered by the sweep.
func hostAliasesConverged(liveRaw []byte, want []ClusterHostAlias) bool {
	if len(want) == 0 {
		return true
	}
	var live []ClusterHostAlias
	if len(strings.TrimSpace(string(liveRaw))) > 0 {
		if err := json.Unmarshal(liveRaw, &live); err != nil {
			return false
		}
	}
	liveSet := map[string]bool{}
	for _, a := range live {
		for _, h := range a.Hostnames {
			liveSet[strings.TrimSpace(a.IP)+"|"+strings.ToLower(strings.TrimSpace(h))] = true
		}
	}
	for _, a := range want {
		for _, h := range a.Hostnames {
			if !liveSet[a.IP+"|"+h] {
				return false
			}
		}
	}
	return true
}

// mergeHostAliases returns want merged over live: live entries for IPs not in
// want are kept (so unrelated hand-applied aliases survive), entries for IPs
// in want are replaced by the desired hostnames. Output is sorted by IP so the
// patch is deterministic.
func mergeHostAliases(liveRaw []byte, want []ClusterHostAlias) []ClusterHostAlias {
	var live []ClusterHostAlias
	if len(strings.TrimSpace(string(liveRaw))) > 0 {
		_ = json.Unmarshal(liveRaw, &live)
	}
	wantIPs := map[string]bool{}
	for _, a := range want {
		wantIPs[a.IP] = true
	}
	out := make([]ClusterHostAlias, 0, len(live)+len(want))
	for _, a := range live {
		if !wantIPs[strings.TrimSpace(a.IP)] {
			out = append(out, a)
		}
	}
	out = append(out, want...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

// reconcileHostAliasesIfDue runs the sweep at most once per
// hostAliasesReconcileInterval. Safe to call from the poller loop every tick;
// same guarding mutex as reconcileNetAdminIfDue (poller-loop-only state).
func (s *HubServer) reconcileHostAliasesIfDue() {
	s.clusterUnreachableMu.Lock()
	due := s.lastHostAliasesReconcile.IsZero() ||
		time.Since(s.lastHostAliasesReconcile) >= hostAliasesReconcileInterval
	if due {
		s.lastHostAliasesReconcile = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	s.reconcileHostAliases()
}

// reconcileHostAliases sweeps every hosted hive whose cluster declares
// host_aliases and patches the Deployment's pod hostAliases where the desired
// entries are absent — at most hostAliasesMaxPatchesPerCycle per cycle, since
// every patch rolls that hive's pod. Clusters WITHOUT host_aliases are never
// touched, so the lane cannot strip anything an operator applied by hand.
func (s *HubServer) reconcileHostAliases() {
	wantByCluster := map[string][]ClusterHostAlias{}
	for id, c := range s.clusters {
		if len(c.HostAliases) > 0 {
			wantByCluster[id] = c.HostAliases
		}
	}
	if len(wantByCluster) == 0 {
		return
	}

	patched := 0
	deferred := 0
	for _, h := range listSaaSHives() {
		if !netAdminSweepEligible(h.Status) {
			continue
		}
		cluster := s.clusterForHive(&h)
		if cluster == nil {
			continue
		}
		want, ok := wantByCluster[cluster.ID]
		if !ok || s.clusterRecentlyUnreachable(cluster.ID) {
			continue
		}
		ns := hiveHostedNamespacePrefix + h.ID

		ctx, cancel := context.WithTimeout(context.Background(), hostAliasesKubectlTimeout)
		getCmd := kubectlForClusterContext(ctx, cluster, "get", "deployment", "hive",
			"-n", ns, "-o", "jsonpath={"+strings.ReplaceAll(podHostAliasesPath, "/", ".")+"}")
		out, err := getCmd.Output()
		cancel()
		if err != nil {
			s.logger.Debug("host_aliases reconcile: could not read hive deployment",
				"hive_id", h.ID, "cluster", cluster.ID, "error", err)
			continue
		}
		s.markClusterReachable(cluster.ID)

		if hostAliasesConverged(out, want) {
			continue
		}
		if patched >= hostAliasesMaxPatchesPerCycle {
			deferred++
			continue
		}
		patch, perr := hostAliasesPatchJSON(mergeHostAliases(out, want))
		if perr != nil {
			s.logger.Warn("host_aliases reconcile: could not build patch",
				"hive_id", h.ID, "cluster", cluster.ID, "error", perr)
			continue
		}
		pctx, pcancel := context.WithTimeout(context.Background(), hostAliasesKubectlTimeout)
		patchCmd := kubectlForClusterContext(pctx, cluster, "patch", "deployment", "hive",
			"-n", ns, "--type=json", "-p", patch)
		pout, err := patchCmd.CombinedOutput()
		pcancel()
		if err != nil {
			s.markClusterUnreachable(cluster.ID)
			s.logger.Warn("host_aliases reconcile: patch failed — will retry next sweep",
				"hive_id", h.ID, "cluster", cluster.ID,
				"output", strings.TrimSpace(string(pout)), "error", err)
			continue
		}
		s.markClusterReachable(cluster.ID)
		patched++
		s.logger.Info("reconciled cluster host_aliases onto hive deployment (rolls the pod once)",
			"hive_id", h.ID, "cluster", cluster.ID, "patched_this_cycle", patched)
	}
	if deferred > 0 {
		s.logger.Info("host_aliases reconcile: rate limit reached, remaining hives deferred to next cycle",
			"patched", patched, "deferred", deferred,
			"max_per_cycle", hostAliasesMaxPatchesPerCycle,
			"next_cycle_in", hostAliasesReconcileInterval.String())
	}
}
