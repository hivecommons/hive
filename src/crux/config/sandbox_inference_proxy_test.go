package config

import (
	"strings"
	"testing"
)

// The sandbox inference listener follows the read relay's contract (#19619):
// default off, no default bind, https endpoint without credentials.
func TestSandboxInferenceProxyValidate(t *testing.T) {
	var nilCfg *SandboxInferenceProxy
	if err := nilCfg.Validate(); err != nil {
		t.Fatalf("nil block must be valid: %v", err)
	}
	if nilCfg.Listen() != "" {
		t.Fatal("there is no default listen address")
	}
	if err := (&SandboxInferenceProxy{Endpoint: "http://h:1"}).Validate(); err != nil {
		t.Fatalf("a disabled block must be valid however it looks: %v", err)
	}
	for _, tc := range []struct {
		name, listen, endpoint, wantErr string
	}{
		{"complete", ":18446", "https://hive-inference.ns.svc:18446", ""},
		{"no listen_addr", "", "https://h:1", "agent_sandbox.job.inference_proxy.listen_addr"},
		{"listen_addr without port", "18446", "https://h:1", "agent_sandbox.job.inference_proxy.listen_addr"},
		{"no endpoint", ":18446", "", "agent_sandbox.job.inference_proxy.endpoint"},
		{"cleartext endpoint", ":18446", "http://h:1", "https"},
		{"credentialed endpoint", ":18446", "https://u:p@h:1", "credentials"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (&SandboxInferenceProxy{Enabled: true, ListenAddr: tc.listen, Endpoint: tc.endpoint}).Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSandboxJobInferenceProxyIsHiveWide(t *testing.T) {
	global := AgentSandboxConfig{Job: &SandboxJobConfig{InferenceProxy: &SandboxInferenceProxy{Enabled: true, ListenAddr: ":18446", Endpoint: "https://h:1"}}}
	agent := &AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{InferenceProxy: &SandboxInferenceProxy{Enabled: false}}}}
	if got := agent.SandboxJob(global); got.InferenceProxy == nil || !got.InferenceProxy.Enabled {
		t.Fatalf("a per-agent block must not override the hive-wide listener setting: %+v", got.InferenceProxy)
	}
}
