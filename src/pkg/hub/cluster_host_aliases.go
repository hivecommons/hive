package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
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
//
// It is spoke.HostAlias so the same value rides the heartbeat to spokes the
// hub cannot kubectl into, which converge their own Deployment (#18637).
type ClusterHostAlias = spoke.HostAlias

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

// decodeLiveHostAliases parses raw jsonpath output (possibly empty). ok is
// false when the output is not a hostAliases list.
func decodeLiveHostAliases(liveRaw []byte) (live []ClusterHostAlias, ok bool) {
	if len(strings.TrimSpace(string(liveRaw))) == 0 {
		return nil, true
	}
	if err := json.Unmarshal(liveRaw, &live); err != nil {
		return nil, false
	}
	return live, true
}

// hostAliasesConverged reports whether the live hostAliases (raw jsonpath
// output, possibly empty) already equal mergeHostAliases(live, want): every
// desired IP carries exactly the desired hostnames and no desired hostname
// points elsewhere. Order and hostname case are ignored; entries for unrelated
// hosts are tolerated so an operator's hand-applied alias is never clobbered.
func hostAliasesConverged(liveRaw []byte, want []ClusterHostAlias) bool {
	if len(want) == 0 {
		return true
	}
	live, ok := decodeLiveHostAliases(liveRaw)
	if !ok {
		return false
	}
	return spoke.HostAliasesConverged(live, want)
}

// mergeHostAliases returns want merged over live (see spoke.MergeHostAliases):
// unrelated live entries are kept, desired IPs carry exactly the desired
// hostnames. Output is sorted by IP so the patch is deterministic.
func mergeHostAliases(liveRaw []byte, want []ClusterHostAlias) []ClusterHostAlias {
	live, _ := decodeLiveHostAliases(liveRaw)
	return spoke.MergeHostAliases(live, want)
}

// hostAliasesForHeartbeat returns the host_aliases of the cluster the hub's
// own SaaS record places a hive on (never the spoke's self-report), for
// delivery on the heartbeat. nil when the cluster is unknown or declares none.
func (s *HubServer) hostAliasesForHeartbeat(saasClusterID string) []ClusterHostAlias {
	id := strings.TrimSpace(saasClusterID)
	if id == "" {
		return nil
	}
	c, ok := s.clusters[id]
	if !ok || len(c.HostAliases) == 0 {
		return nil
	}
	return c.HostAliases
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
	// Hives the hub cannot kubectl into, per cluster. They are converged by
	// the spoke itself from HeartbeatResponse.HostAliases; the sweep says so
	// once per cluster instead of skipping them silently (#18637).
	viaHeartbeat := map[string]int{}
	unreachable := map[string]int{}
	for _, h := range listSaaSHives() {
		if !netAdminSweepEligible(h.Status) {
			continue
		}
		cluster := s.clusterForHive(&h)
		if cluster == nil {
			continue
		}
		want, ok := wantByCluster[cluster.ID]
		if !ok {
			continue
		}
		if !cluster.KubectlReachable() {
			viaHeartbeat[cluster.ID]++
			continue
		}
		if s.clusterRecentlyUnreachable(cluster.ID) {
			unreachable[cluster.ID]++
			continue
		}
		ns := hiveHostedNamespacePrefix + h.ID

		ctx, cancel := context.WithTimeout(context.Background(), hostAliasesKubectlTimeout)
		getCmd := kubectlForClusterContext(ctx, cluster, "get", "deployment", "hive",
			"-n", ns, "-o", "jsonpath={"+strings.ReplaceAll(podHostAliasesPath, "/", ".")+"}")
		out, err := getCmd.Output()
		cancel()
		if err != nil {
			s.logger.Warn("host_aliases reconcile: could not read hive deployment — aliases not applied, will retry next sweep",
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
	for id, n := range viaHeartbeat {
		s.logger.Info("host_aliases reconcile: push-reported cluster, hub cannot patch; spokes converge from the heartbeat",
			"cluster", id, "hives", n)
	}
	for id, n := range unreachable {
		s.logger.Warn("host_aliases reconcile: cluster recently unreachable — aliases not applied by the hub this sweep (spokes still converge from the heartbeat)",
			"cluster", id, "hives", n)
	}
	if deferred > 0 {
		s.logger.Info("host_aliases reconcile: rate limit reached, remaining hives deferred to next cycle",
			"patched", patched, "deferred", deferred,
			"max_per_cycle", hostAliasesMaxPatchesPerCycle,
			"next_cycle_in", hostAliasesReconcileInterval.String())
	}
}
