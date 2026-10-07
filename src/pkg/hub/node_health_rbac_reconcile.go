package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

const nodeHealthRBACReconcileInterval = 15 * time.Minute

// This allowlist intentionally excludes nodes/proxy (kubelet exec access) and
// pods (cross-tenant pod specs). Missing telemetry is safer than tenant access
// to other tenants' workloads. Keep in sync with k8sManifestTemplate.
const tenantNodeHealthRules = `[{"apiGroups":[""],"resources":["nodes"],"verbs":["get","list"]},{"apiGroups":["metrics.k8s.io"],"resources":["nodes"],"verbs":["list"]}]`

// nodeHealthRBACPatch replaces the rules, including any legacy overbroad grants.
// A resourceVersion test makes concurrent edits fail rather than overwriting
// them blindly. Never patch an unreadable or aggregated role.
func nodeHealthRBACPatch(raw []byte) (string, error) {
	var role struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Rules           any             `json:"rules"`
		AggregationRule json.RawMessage `json:"aggregationRule"`
	}
	if err := json.Unmarshal(raw, &role); err != nil {
		return "", err
	}
	if role.Metadata.ResourceVersion == "" || (len(role.AggregationRule) > 0 && string(role.AggregationRule) != "null") {
		return "", fmt.Errorf("missing resourceVersion or unexpected aggregated tenant role")
	}
	var want any
	if err := json.Unmarshal([]byte(tenantNodeHealthRules), &want); err != nil {
		return "", err
	}
	if reflect.DeepEqual(role.Rules, want) {
		return "", nil
	}
	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/resourceVersion", "value": role.Metadata.ResourceVersion},
		{"op": "add", "path": "/rules", "value": want},
	})
	return string(patch), err
}

func (s *HubServer) reconcileNodeHealthRBACIfDue() {
	s.clusterUnreachableMu.Lock()
	due := s.lastNodeHealthRBACReconcile.IsZero() || time.Since(s.lastNodeHealthRBACReconcile) >= nodeHealthRBACReconcileInterval
	if due {
		s.lastNodeHealthRBACReconcile = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	for _, h := range listSaaSHives() {
		cluster := s.clusterForHive(&h)
		if cluster == nil || !cluster.KubectlReachable() || s.clusterRecentlyUnreachable(cluster.ID) {
			continue
		}
		ns := hostedNamespaceForHive(&h)
		if ns == "" {
			continue
		}
		// Read/patch only this tenant's dedicated role; never touch a shared
		// operator role or recreate a role deleted during deprovisioning.
		s.reconcileTenantNodeHealthRole(cluster, "hive-node-health-reader-"+ns)
	}
}

func (s *HubServer) reconcileTenantNodeHealthRole(cluster *ClusterConfig, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	raw, err := kubectlForClusterContext(ctx, cluster, "get", "clusterrole", name, "-o", "json").Output()
	cancel()
	if err != nil {
		s.logger.Warn("node health RBAC reconcile: cannot read tenant role; will retry", "cluster", cluster.ID, "role", name, "error", err)
		return
	}
	patch, err := nodeHealthRBACPatch(raw)
	if err != nil {
		s.logger.Warn("node health RBAC reconcile: invalid tenant role", "cluster", cluster.ID, "role", name, "error", err)
		return
	}
	if patch == "" {
		return
	}
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	out, err := kubectlForClusterContext(ctx, cluster, "patch", "clusterrole", name, "--type=json", "-p", patch).CombinedOutput()
	cancel()
	if err != nil {
		s.logger.Warn("node health RBAC reconcile: patch failed; will retry", "cluster", cluster.ID, "role", name, "error", err, "output", strings.TrimSpace(string(out)))
		return
	}
	s.logger.Info("narrowed tenant node health ClusterRole", "cluster", cluster.ID, "role", name)
}
