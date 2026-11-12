package hub

import (
	"bytes"
	"encoding/json"
	"log/slog"
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
