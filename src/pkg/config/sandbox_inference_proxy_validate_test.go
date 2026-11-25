package config

import (
	"strings"
	"testing"
)

// Config validation (the load and save paths) enforces the inference
// listener's contract (#19619).
func TestConfigValidateSandboxInferenceProxy(t *testing.T) {
	withProxy := func(ip *SandboxInferenceProxy) *Config {
		c := &Config{}
		c.AgentSandbox = AgentSandboxConfig{Job: &SandboxJobConfig{InferenceProxy: ip}}
		return c
	}
	if err := withProxy(&SandboxInferenceProxy{Enabled: true, ListenAddr: ":18446", Endpoint: "https://hive.ns.svc:18446"}).validateJobAffinities(); err != nil {
		t.Fatalf("complete block: %v", err)
	}
	err := withProxy(&SandboxInferenceProxy{Enabled: true, ListenAddr: ":18446", Endpoint: "http://hive.ns.svc:18446"}).validateJobAffinities()
	if err == nil || !strings.Contains(err.Error(), "inference_proxy.endpoint") {
		t.Fatalf("err = %v, want the http endpoint refused", err)
	}
}
