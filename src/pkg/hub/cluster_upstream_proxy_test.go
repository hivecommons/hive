package hub

import (
	"bytes"
	"strings"
	"testing"
	"text/template"
)

// renderManifestWithUpstreamProxy renders the real spoke template with the
// upstream-proxy data keys the provisioner derives from ClusterConfig.
func renderManifestWithUpstreamProxy(t *testing.T, up *ClusterUpstreamProxy) string {
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
		"AppKeys":              []provisionAppKey{},
		"HasUpstreamProxy":     up.enabled(),
		"UpstreamProxyURL":     up.url(),
		"UpstreamProxyHosts":   up.hosts(),
		"UpstreamProxyNoProxy": up.noProxy(),
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("template does not execute: %v", err)
	}
	return buf.String()
}

func TestClusterUpstreamProxyInjectsEnv(t *testing.T) {
	up := &ClusterUpstreamProxy{
		URL:     " http://proxy.corp.example:3128 ",
		Hosts:   []string{"ghe.corp.example", " *.ghe.corp.example ", ""},
		NoProxy: []string{"10.0.0.0/8"},
	}
	m := renderManifestWithUpstreamProxy(t, up)
	for _, want := range []string{
		"- name: HIVE_UPSTREAM_PROXY_URL\n          value: \"http://proxy.corp.example:3128\"",
		"- name: HIVE_UPSTREAM_PROXY_HOSTS\n          value: \"ghe.corp.example,*.ghe.corp.example\"",
		"- name: HIVE_NO_PROXY\n          value: \"10.0.0.0/8\"",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("rendered manifest missing:\n%s", want)
		}
	}
}

func TestClusterUpstreamProxyOmittedWhenUnset(t *testing.T) {
	for name, up := range map[string]*ClusterUpstreamProxy{
		"nil":       nil,
		"empty-url": {Hosts: []string{"ghe.corp.example"}},
	} {
		t.Run(name, func(t *testing.T) {
			m := renderManifestWithUpstreamProxy(t, up)
			if strings.Contains(m, "HIVE_UPSTREAM_PROXY") || strings.Contains(m, "HIVE_NO_PROXY") {
				t.Fatal("upstream proxy env must not be emitted when the cluster has none")
			}
		})
	}
	// URL alone: hosts/no_proxy lines are omitted rather than emitted empty.
	m := renderManifestWithUpstreamProxy(t, &ClusterUpstreamProxy{URL: "http://p:3128"})
	if !strings.Contains(m, "HIVE_UPSTREAM_PROXY_URL") || strings.Contains(m, "HIVE_UPSTREAM_PROXY_HOSTS") || strings.Contains(m, "HIVE_NO_PROXY") {
		t.Fatalf("URL-only cluster should emit only HIVE_UPSTREAM_PROXY_URL")
	}
}
