package config

import "testing"

func TestSandboxGitHubReadProxyDefaultOff(t *testing.T) {
	var nilCfg *SandboxGitHubReadProxy
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("nil block must be valid: %v", err)
	}
	if err := (&SandboxGitHubReadProxy{}).Validate(); err != nil {
		t.Fatalf("disabled block must be valid: %v", err)
	}
	if got := nilCfg.Listen(); got != DefaultSandboxGitHubReadProxyListen {
		t.Fatalf("Listen = %q", got)
	}
}

func TestSandboxGitHubReadProxyValidate(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		ok       bool
	}{
		{"http://hive.ns.svc:18445", true},
		{"https://hive.example.com:18445", true},
		{"", false},
		{"hive.ns.svc:18445", false},
		{"ftp://hive:1", false},
		{"http://user:pw@hive:1", false},
	} {
		err := (&SandboxGitHubReadProxy{Enabled: true, Endpoint: tc.endpoint}).Validate()
		if (err == nil) != tc.ok {
			t.Errorf("endpoint %q: err = %v, want ok=%v", tc.endpoint, err, tc.ok)
		}
	}
}

func TestSandboxJobGitHubReadProxyIsHiveWide(t *testing.T) {
	global := AgentSandboxConfig{Job: &SandboxJobConfig{GitHubReadProxy: &SandboxGitHubReadProxy{Enabled: true, Endpoint: "http://h:1"}}}
	agent := &AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{GitHubReadProxy: &SandboxGitHubReadProxy{Enabled: false}}}}
	if got := agent.SandboxJob(global); got.GitHubReadProxy == nil || !got.GitHubReadProxy.Enabled {
		t.Fatalf("a per-agent block must not override the hive-wide relay setting: %+v", got.GitHubReadProxy)
	}
}
