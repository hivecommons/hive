package hub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

func renderManifestWithHostAliases(t *testing.T, aliases []ClusterHostAlias) string {
	t.Helper()
	tmpl, err := template.New("manifests").Parse(k8sManifestTemplate)
	if err != nil {
		t.Fatalf("template does not parse: %v", err)
	}
	data := map[string]any{
		"ID": "test-hive", "ImageTag": "v2-latest", "ImagePullPolicy": "Always",
		"InitContainerUID": "1001", "InitContainerGID": "1000",
		"TerminalPort": 7681, "DashboardPort": 8080,
		"CPURequest": "500m", "MemRequest": "1Gi", "CPULimit": "2", "MemLimit": "4Gi",
		"Namespace": "hive-test", "StorageSize": "10Gi", "StorageClass": "standard",
		"HeartbeatKey": "hb", "SessionKey": "sk", "SSOPublicKey": "pk", "TerminalKey": "tk",
		"AuthorizedUsers": "alice:owner", "Host": "test-hive.example", "Owner": "alice",
		"HubPublicURL": "https://hub.example", "ProjectName": "test", "PrimaryRepo": "org/repo",
		"AppKeys":     []provisionAppKey{},
		"HostAliases": aliases,
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("template does not execute: %v", err)
	}
	return buf.String()
}

func TestClusterHostAliasesRenderedIntoPodSpec(t *testing.T) {
	m := renderManifestWithHostAliases(t, []ClusterHostAlias{
		{IP: "129.34.104.21", Hostnames: []string{"github.ibm.com", "api.github.ibm.com"}},
	})
	want := "    spec:\n      hostAliases:\n      - ip: \"129.34.104.21\"\n        hostnames:\n        - \"github.ibm.com\"\n        - \"api.github.ibm.com\"\n"
	// The template carries a comment line between spec: and hostAliases:.
	stripped := strings.Join(func() []string {
		var out []string
		for _, l := range strings.Split(m, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			out = append(out, l)
		}
		return out
	}(), "\n")
	if !strings.Contains(stripped, want) {
		t.Fatalf("rendered manifest missing pod hostAliases block:\n%s\n--- got ---\n%s", want, m)
	}
}

func TestClusterHostAliasesOmittedWhenUnset(t *testing.T) {
	for name, aliases := range map[string][]ClusterHostAlias{"nil": nil, "empty": {}} {
		if m := renderManifestWithHostAliases(t, aliases); strings.Contains(m, "hostAliases") {
			t.Errorf("%s: hostAliases must not be rendered when no aliases configured", name)
		}
	}
}

func TestNormalizeClusterHostAliases(t *testing.T) {
	got, err := normalizeClusterHostAliases([]ClusterHostAlias{
		{IP: " 129.34.104.21 ", Hostnames: []string{" GitHub.IBM.com ", "", "api.github.ibm.com"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].IP != "129.34.104.21" ||
		strings.Join(got[0].Hostnames, ",") != "github.ibm.com,api.github.ibm.com" {
		t.Fatalf("unexpected normalization: %+v", got)
	}
	if out, err := normalizeClusterHostAliases(nil); err != nil || out != nil {
		t.Fatalf("nil input should yield nil,nil; got %v,%v", out, err)
	}
	for name, in := range map[string][]ClusterHostAlias{
		"bad ip":       {{IP: "not-an-ip", Hostnames: []string{"a.example"}}},
		"no hostnames": {{IP: "10.0.0.1", Hostnames: []string{" ", ""}}},
		"url not host": {{IP: "10.0.0.1", Hostnames: []string{"https://a.example"}}},
	} {
		if _, err := normalizeClusterHostAliases(in); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestValidateClusterEntriesKeepsClusterOnBadHostAliases(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	got := validateClusterEntries([]ClusterConfig{
		{ID: "bad", InCluster: true, Domain: "x.example",
			HostAliases: []ClusterHostAlias{{IP: "nope", Hostnames: []string{"a"}}}},
		{ID: "good", InCluster: true, Domain: "x.example",
			HostAliases: []ClusterHostAlias{{IP: "10.1.2.3", Hostnames: []string{"A.Example"}}}},
	}, logger)
	bad, ok := got["bad"]
	if !ok {
		t.Fatal("cluster with invalid host_aliases must still be admitted")
	}
	if bad.HostAliases != nil {
		t.Fatalf("invalid host_aliases must be dropped, got %+v", bad.HostAliases)
	}
	if g := got["good"].HostAliases; len(g) != 1 || g[0].Hostnames[0] != "a.example" {
		t.Fatalf("valid host_aliases must be normalized in registry, got %+v", g)
	}
}

func TestHostAliasesConvergedAndMerge(t *testing.T) {
	want := []ClusterHostAlias{{IP: "129.34.104.21", Hostnames: []string{"github.ibm.com", "api.github.ibm.com"}}}

	if hostAliasesConverged(nil, want) {
		t.Error("empty live must not be converged")
	}
	live := []byte(`[{"ip":"129.34.104.21","hostnames":["API.github.ibm.com","github.ibm.com"]},{"ip":"10.0.0.9","hostnames":["other.example"]}]`)
	if !hostAliasesConverged(live, want) {
		t.Error("live superset (any order/case) must be converged")
	}
	partial := []byte(`[{"ip":"129.34.104.21","hostnames":["github.ibm.com"]}]`)
	if hostAliasesConverged(partial, want) {
		t.Error("missing hostname must not be converged")
	}
	if !hostAliasesConverged([]byte(`[]`), nil) {
		t.Error("no desired aliases is always converged")
	}

	merged := mergeHostAliases([]byte(`[{"ip":"10.0.0.9","hostnames":["other.example"]},{"ip":"129.34.104.21","hostnames":["stale.example"]}]`), want)
	if len(merged) != 2 || merged[0].IP != "10.0.0.9" || merged[1].IP != "129.34.104.21" ||
		strings.Join(merged[1].Hostnames, ",") != "github.ibm.com,api.github.ibm.com" {
		t.Fatalf("merge must keep unrelated IPs and replace desired IPs: %+v", merged)
	}

	patch, err := hostAliasesPatchJSON(merged)
	if err != nil {
		t.Fatal(err)
	}
	var ops []struct {
		Op    string             `json:"op"`
		Path  string             `json:"path"`
		Value []ClusterHostAlias `json:"value"`
	}
	if err := json.Unmarshal([]byte(patch), &ops); err != nil {
		t.Fatalf("patch is not valid JSON-Patch: %v\n%s", err, patch)
	}
	if len(ops) != 1 || ops[0].Op != "add" || ops[0].Path != podHostAliasesPath || len(ops[0].Value) != 2 {
		t.Fatalf("unexpected patch: %s", patch)
	}
}

// testGHEHostAliases is the a-ks-wec2 / vllm-d clusters.json shape.
func testGHEHostAliases() []ClusterHostAlias {
	return []ClusterHostAlias{{IP: "129.34.104.21", Hostnames: []string{"github.ibm.com", "api.github.ibm.com"}}}
}

func writeFakeHostAliases(t *testing.T, dir, hiveID, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "env-"+hiveID+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// One sweep over every posture (#18637): a spoke provisioned before #17805
// (no hostAliases) and a pool slot are patched; a converged spoke is not; a
// cluster with no aliases is never touched; a push-reported cluster shaped
// like a-ks-wec2 is not kubectl'd but is reported, and an unreadable
// Deployment logs a WARN naming the spoke and cluster.
func TestReconcileHostAliasesSweep(t *testing.T) {
	withTempHivesDir(t)
	fake := installFakeKubectl(t)
	var logs bytes.Buffer
	s := &HubServer{
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
		clusters: map[string]ClusterConfig{
			"reach":     {ID: "reach", InCluster: true, HostAliases: testGHEHostAliases()},
			"plain":     {ID: "plain", InCluster: true},
			"a-ks-wec2": {ID: "a-ks-wec2", PullOnly: true, KubeconfigPath: "/kc", Context: "a-ks-wec2", HostAliases: testGHEHostAliases()},
		},
	}
	mustSaveHive(t, &SaaSHive{ID: "old", ClusterID: "reach", Status: "assigned"})
	mustSaveHive(t, &SaaSHive{ID: "slot", ClusterID: "reach", Status: "available"})
	mustSaveHive(t, &SaaSHive{ID: "conv", ClusterID: "reach", Status: ""})
	mustSaveHive(t, &SaaSHive{ID: "unread", ClusterID: "reach", Status: "assigned"})
	mustSaveHive(t, &SaaSHive{ID: "plainhive", ClusterID: "plain", Status: "assigned"})
	mustSaveHive(t, &SaaSHive{ID: "wec2hive", ClusterID: "a-ks-wec2", Status: "assigned"})
	writeFakeHostAliases(t, fake, "old", "")
	writeFakeHostAliases(t, fake, "slot", `[{"ip":"10.0.0.9","hostnames":["other.example"]}]`)
	writeFakeHostAliases(t, fake, "conv", `[{"ip":"129.34.104.21","hostnames":["api.github.ibm.com","github.ibm.com"]}]`)
	writeFakeHostAliases(t, fake, "plainhive", "")

	s.reconcileHostAliases()

	inv := fakeKubectlInvocations(t, fake)
	patchedNS := map[string]string{}
	for _, line := range inv {
		if strings.Contains(line, "hive-hosted-plainhive") || strings.Contains(line, "hive-hosted-wec2hive") {
			t.Errorf("sweep must not kubectl this hive: %s", line)
		}
		if strings.Contains(line, " patch deployment hive ") {
			for _, id := range []string{"old", "slot", "conv", "unread"} {
				if strings.Contains(line, "hive-hosted-"+id+" ") {
					patchedNS[id] = line
				}
			}
		}
	}
	if _, ok := patchedNS["old"]; !ok {
		t.Error("spoke provisioned before #17805 must be patched on the next sweep")
	}
	if line, ok := patchedNS["slot"]; !ok || !strings.Contains(line, "other.example") || !strings.Contains(line, "github.ibm.com") {
		t.Errorf("pool slot must be patched and keep its unrelated alias: %q", line)
	}
	if _, ok := patchedNS["conv"]; ok {
		t.Error("converged spoke must not be patched (idempotent)")
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "hive_id=unread") || !strings.Contains(out, "cluster=reach") {
		t.Errorf("unreadable deployment must WARN naming spoke and cluster:\n%s", out)
	}
	if !strings.Contains(out, "push-reported cluster") || !strings.Contains(out, "cluster=a-ks-wec2") {
		t.Errorf("push-reported cluster must be reported, not skipped silently:\n%s", out)
	}
}

func TestReconcileHostAliasesSweep_NoClusterAliasesIsNoop(t *testing.T) {
	withTempHivesDir(t)
	fake := installFakeKubectl(t)
	s := &HubServer{logger: appKeyTestLogger(), clusters: map[string]ClusterConfig{"plain": {ID: "plain", InCluster: true}}}
	mustSaveHive(t, &SaaSHive{ID: "h", ClusterID: "plain", Status: "assigned"})
	s.reconcileHostAliases()
	if inv := fakeKubectlInvocations(t, fake); len(inv) != 0 {
		t.Fatalf("no cluster declares host_aliases: sweep must not run kubectl, got %v", inv)
	}
}

func TestReconcileHostAliasesSweep_RecentlyUnreachableWarns(t *testing.T) {
	withTempHivesDir(t)
	fake := installFakeKubectl(t)
	var logs bytes.Buffer
	s := &HubServer{
		logger:   slog.New(slog.NewTextHandler(&logs, nil)),
		clusters: map[string]ClusterConfig{"r": {ID: "r", InCluster: true, HostAliases: testGHEHostAliases()}},
	}
	mustSaveHive(t, &SaaSHive{ID: "h", ClusterID: "r", Status: "assigned"})
	s.markClusterUnreachable("r")
	s.reconcileHostAliases()
	if inv := fakeKubectlInvocations(t, fake); len(inv) != 0 {
		t.Fatalf("recently unreachable cluster must not be dialled, got %v", inv)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "cluster=r") {
		t.Fatalf("recently unreachable cluster must WARN:\n%s", out)
	}
}

func TestReconcileHostAliasesSweep_PatchFailureWarns(t *testing.T) {
	withTempHivesDir(t)
	fake := installFakeKubectl(t)
	var logs bytes.Buffer
	s := &HubServer{
		logger:   slog.New(slog.NewTextHandler(&logs, nil)),
		clusters: map[string]ClusterConfig{"r": {ID: "r", InCluster: true, HostAliases: testGHEHostAliases()}},
	}
	mustSaveHive(t, &SaaSHive{ID: "h", ClusterID: "r", Status: "assigned"})
	writeFakeHostAliases(t, fake, "h", "")
	if err := os.WriteFile(filepath.Join(fake, "patch-fail-h"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s.reconcileHostAliases()
	if out := logs.String(); !strings.Contains(out, "patch failed") || !strings.Contains(out, "hive_id=h") {
		t.Fatalf("patch failure must WARN naming the spoke:\n%s", out)
	}
}

func TestHostAliasesConvergedDriftAndBadJSON(t *testing.T) {
	want := testGHEHostAliases()
	if hostAliasesConverged([]byte("not json"), want) {
		t.Error("unparseable live hostAliases must not be converged")
	}
	stale := []byte(`[{"ip":"10.1.1.1","hostnames":["github.ibm.com"]},{"ip":"129.34.104.21","hostnames":["github.ibm.com","api.github.ibm.com"]}]`)
	if hostAliasesConverged(stale, want) {
		t.Error("a desired hostname still pinned to another IP is drift")
	}
	merged := mergeHostAliases(stale, want)
	if len(merged) != 1 || merged[0].IP != "129.34.104.21" {
		t.Fatalf("manual drift must converge to exactly the desired aliases: %+v", merged)
	}
}

// The heartbeat carries the aliases of the SaaS record's cluster, so a
// push-reported cluster like a-ks-wec2 gets them; a cluster without aliases,
// an unknown cluster and a hive with no record get none.
func TestHostAliasesForHeartbeat(t *testing.T) {
	s := &HubServer{clusters: map[string]ClusterConfig{
		"a-ks-wec2": {ID: "a-ks-wec2", PullOnly: true, HostAliases: testGHEHostAliases()},
		"plain":     {ID: "plain", InCluster: true},
	}}
	if got := s.hostAliasesForHeartbeat(" a-ks-wec2 "); len(got) != 1 || got[0].IP != "129.34.104.21" {
		t.Fatalf("a-ks-wec2-style cluster must deliver its aliases, got %+v", got)
	}
	for _, id := range []string{"plain", "unknown", ""} {
		if got := s.hostAliasesForHeartbeat(id); got != nil {
			t.Errorf("cluster %q must deliver no aliases, got %+v", id, got)
		}
	}
}

func TestSelfServiceFenceWithholdsHostAliases(t *testing.T) {
	resp := &HeartbeatResponse{OK: true, HostAliases: testGHEHostAliases()}
	withheld := fenceSelfServiceHeartbeatResponse(resp, SelfServiceHiveIDPrefix+"alice-hive")
	if resp.HostAliases != nil || !strings.Contains(strings.Join(withheld, ","), "HostAliases") {
		t.Fatalf("self-service hive must not receive host_aliases: withheld=%v resp=%+v", withheld, resp.HostAliases)
	}
}
