package config

import (
	"strings"
	"testing"
)

func TestSandboxGitHubReadProxyDefaultOff(t *testing.T) {
	var nilCfg *SandboxGitHubReadProxy
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("nil block must be valid: %v", err)
	}
	if err := (&SandboxGitHubReadProxy{}).Validate(); err != nil {
		t.Fatalf("disabled block must be valid: %v", err)
	}
	// Disabled stays valid however the other keys look: the feature is off.
	if err := (&SandboxGitHubReadProxy{Endpoint: "http://h:1"}).Validate(); err != nil {
		t.Fatalf("disabled block must be valid: %v", err)
	}
}

// There is no default bind: enabling without listen_addr is a config error
// that names the key.
func TestSandboxGitHubReadProxyHasNoDefaultListen(t *testing.T) {
	if got := (*SandboxGitHubReadProxy)(nil).Listen(); got != "" {
		t.Fatalf("Listen = %q, want no default", got)
	}
	err := (&SandboxGitHubReadProxy{Enabled: true, Endpoint: "https://hive.ns.svc:8443"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "agent_sandbox.job.github_read_proxy.listen_addr") {
		t.Fatalf("err = %v, want one naming listen_addr", err)
	}
	if err := (&SandboxGitHubReadProxy{Enabled: true, ListenAddr: "   ", Endpoint: "https://h:1"}).Validate(); err == nil {
		t.Fatal("a blank listen_addr must be refused")
	}
	if err := (&SandboxGitHubReadProxy{Enabled: true, ListenAddr: "8443", Endpoint: "https://h:1"}).Validate(); err == nil {
		t.Fatal("listen_addr without a port must be refused")
	}
}

func TestSandboxGitHubReadProxyValidate(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		ok       bool
	}{
		{"https://hive.ns.svc:8443", true},
		{"https://10.20.30.40:8443", true},
		{"http://hive.ns.svc:8443", false}, // cleartext is refused
		{"", false},
		{"hive.ns.svc:8443", false},
		{"ftp://hive:1", false},
		{"https://user:pw@hive:1", false},
	} {
		err := (&SandboxGitHubReadProxy{Enabled: true, ListenAddr: ":8443", Endpoint: tc.endpoint}).Validate()
		if (err == nil) != tc.ok {
			t.Errorf("endpoint %q: err = %v, want ok=%v", tc.endpoint, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "agent_sandbox.job.github_read_proxy.endpoint") {
			t.Errorf("endpoint %q: error %q does not name the key", tc.endpoint, err)
		}
	}
	err := (&SandboxGitHubReadProxy{Enabled: true, ListenAddr: ":8443", Endpoint: "http://h:1"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("http endpoint error = %v, want one saying https is required", err)
	}
}

func TestSandboxJobGitHubReadProxyIsHiveWide(t *testing.T) {
	global := AgentSandboxConfig{Job: &SandboxJobConfig{GitHubReadProxy: &SandboxGitHubReadProxy{Enabled: true, ListenAddr: ":8443", Endpoint: "https://h:1"}}}
	agent := &AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{GitHubReadProxy: &SandboxGitHubReadProxy{Enabled: false}}}}
	if got := agent.SandboxJob(global); got.GitHubReadProxy == nil || !got.GitHubReadProxy.Enabled {
		t.Fatalf("a per-agent block must not override the hive-wide relay setting: %+v", got.GitHubReadProxy)
	}
}
