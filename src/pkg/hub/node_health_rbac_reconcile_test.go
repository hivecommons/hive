package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const legacyTenantNodeRole = `{"metadata":{"resourceVersion":"42"},"rules":[{"apiGroups":[""],"resources":["nodes"],"verbs":["get","list"]},{"apiGroups":[""],"resources":["nodes/proxy"],"verbs":["get"]},{"apiGroups":[""],"resources":["pods"],"verbs":["list"]},{"apiGroups":["metrics.k8s.io"],"resources":["nodes"],"verbs":["list"]}]}`

func TestNodeHealthRBACPatchRevokesLegacyGrants(t *testing.T) {
	patch, err := nodeHealthRBACPatch([]byte(legacyTenantNodeRole))
	if err != nil || patch == "" {
		t.Fatalf("patch = %q, err = %v", patch, err)
	}
	var ops []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(patch), &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 || ops[0].Op != "test" || ops[0].Path != "/metadata/resourceVersion" || string(ops[0].Value) != `"42"` || ops[1].Path != "/rules" || ops[1].Op != "add" {
		t.Fatalf("unsafe patch: %s", patch)
	}
	var rules []struct {
		APIGroups []string `json:"apiGroups"`
		Resources []string `json:"resources"`
		Verbs     []string `json:"verbs"`
	}
	if err := json.Unmarshal(ops[1].Value, &rules); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("unexpected rules: %s", ops[1].Value)
	}
	for _, rule := range rules {
		if len(rule.Resources) != 1 || rule.Resources[0] != "nodes" {
			t.Fatalf("cross-tenant resource access: %+v", rule)
		}
		for _, verb := range rule.Verbs {
			if verb != "get" && verb != "list" {
				t.Fatalf("unexpected write permission: %+v", rule)
			}
		}
	}
	converged := `{"metadata":{"resourceVersion":"43"},"rules":` + string(ops[1].Value) + `}`
	if patch, err := nodeHealthRBACPatch([]byte(converged)); err != nil || patch != "" {
		t.Fatalf("converged role should be unchanged: %q, %v", patch, err)
	}
}

func TestTenantNodeHealthTemplateMatchesReconciliation(t *testing.T) {
	for _, document := range strings.Split(k8sManifestTemplate, "\n---") {
		if !strings.Contains(document, "kind: ClusterRole\n") || !strings.Contains(document, "name: hive-node-health-reader-{{.Namespace}}") {
			continue
		}
		var role struct {
			Rules []map[string][]string `yaml:"rules"`
		}
		if err := yaml.Unmarshal([]byte(document), &role); err != nil {
			t.Fatal(err)
		}
		rules, err := json.Marshal(role.Rules)
		if err != nil {
			t.Fatal(err)
		}
		patch, err := nodeHealthRBACPatch([]byte(`{"metadata":{"resourceVersion":"1"},"rules":` + string(rules) + `}`))
		if err != nil || patch != "" {
			t.Fatalf("provisioning and reconciliation rules differ: patch=%s err=%v", patch, err)
		}
		return
	}
	t.Fatal("tenant node health ClusterRole missing from template")
}

func TestNodeHealthRBACPatchRejectsUnreadableRole(t *testing.T) {
	for _, raw := range []string{"", "{}", `{"metadata":{"resourceVersion":"1"},"aggregationRule":{}}`} {
		if _, err := nodeHealthRBACPatch([]byte(raw)); err == nil {
			t.Errorf("expected error for %q", raw)
		}
	}
}

func TestReconcileNodeHealthRBACExistingTenants(t *testing.T) {
	for _, status := range []string{"", "available", "assigned", "running", "error", "provisioning"} {
		t.Run(status, func(t *testing.T) {
			s := netAdminSweepServer(t)
			logPath := installNetAdminKubectl(t, legacyTenantNodeRole, 0, 0)
			if err := saveSaaSHive(&SaaSHive{ID: "hosted-rbac", Status: status}); err != nil {
				t.Fatal(err)
			}
			s.reconcileNodeHealthRBACIfDue()
			logged := readKubectlLog(t, logPath)
			name := "hive-node-health-reader-" + hiveHostedNamespacePrefix + "hosted-rbac"
			if !strings.Contains(logged, "get clusterrole "+name) || !strings.Contains(logged, "patch clusterrole "+name) || !strings.Contains(logged, "/metadata/resourceVersion") {
				t.Fatalf("existing tenant role not safely narrowed: %s", logged)
			}
			if strings.Contains(logged, "nodes/proxy") || strings.Contains(logged, `"pods"`) {
				t.Fatalf("patch retains dangerous grants: %s", logged)
			}
			s.reconcileNodeHealthRBACIfDue()
			if got := readKubectlLog(t, logPath); got != logged {
				t.Fatal("sweep was not throttled")
			}
		})
	}
}

func TestReconcileNodeHealthRBACRetryAndNoBlindPatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		raw       string
		getExit   int
		patchExit int
		wantPatch bool
	}{
		{"read failure", legacyTenantNodeRole, 1, 0, false},
		{"invalid role", "{}", 0, 0, false},
		{"converged", `{"metadata":{"resourceVersion":"42"},"rules":` + tenantNodeHealthRules + `}`, 0, 0, false},
		{"patch failure", legacyTenantNodeRole, 0, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := netAdminSweepServer(t)
			logPath := installNetAdminKubectl(t, tc.raw, tc.getExit, tc.patchExit)
			if err := saveSaaSHive(&SaaSHive{ID: "hosted-rbac"}); err != nil {
				t.Fatal(err)
			}
			s.reconcileNodeHealthRBACIfDue()
			logged := readKubectlLog(t, logPath)
			if strings.Contains(logged, "patch clusterrole") != tc.wantPatch {
				t.Fatalf("unexpected patch behavior: %s", logged)
			}
			s.lastNodeHealthRBACReconcile = time.Now().Add(-nodeHealthRBACReconcileInterval - time.Second)
			s.reconcileNodeHealthRBACIfDue()
			if strings.Count(readKubectlLog(t, logPath), "get clusterrole") != 2 {
				t.Fatal("role not retried on next sweep")
			}
		})
	}
}

func TestReconcileNodeHealthRBACSkipsPushReportedCluster(t *testing.T) {
	s := netAdminSweepServer(t)
	cluster := s.clusters[defaultClusterID]
	cluster.PullOnly = true
	s.clusters[defaultClusterID] = cluster
	logPath := installNetAdminKubectl(t, legacyTenantNodeRole, 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-rbac"}); err != nil {
		t.Fatal(err)
	}
	s.reconcileNodeHealthRBACIfDue()
	if got := readKubectlLog(t, logPath); got != "" {
		t.Fatalf("attempted kubectl into push-reported cluster: %s", got)
	}
}
