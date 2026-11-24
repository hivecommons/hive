package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The sandbox inference listener is default off (#19619): with no block, or a
// disabled one, boot wires nothing and binds nothing.
func TestWireSandboxInferenceRelayDefaultOff(t *testing.T) {
	withJob := func(job *config.SandboxJobConfig) *config.Config {
		c := &config.Config{}
		c.AgentSandbox = config.AgentSandboxConfig{Job: job}
		return c
	}
	cfgs := []*config.Config{
		{},
		withJob(&config.SandboxJobConfig{}),
		withJob(&config.SandboxJobConfig{InferenceProxy: &config.SandboxInferenceProxy{Enabled: false, Endpoint: "http://h:1"}}),
	}
	for i, cfg := range cfgs {
		b := &boot{cfg: cfg, logger: quietLogger()}
		b.wireSandboxInferenceRelay() // nil proxy and nil manager: must be a silent no-op
		if b.agentMgr != nil {
			t.Fatalf("case %d wired a manager", i)
		}
	}
}
