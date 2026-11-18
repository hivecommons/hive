package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The sandbox GitHub read relay is default off (#18865): with no block, or a
// disabled one, boot wires nothing and binds nothing.
func TestWireSandboxGitHubReadRelayDefaultOff(t *testing.T) {
	withJob := func(job *config.SandboxJobConfig) *config.Config {
		c := &config.Config{}
		c.AgentSandbox = config.AgentSandboxConfig{Job: job}
		return c
	}
	cfgs := []*config.Config{
		{},
		withJob(&config.SandboxJobConfig{}),
		withJob(&config.SandboxJobConfig{GitHubReadProxy: &config.SandboxGitHubReadProxy{Enabled: false, Endpoint: "http://h:1"}}),
	}
	for i, cfg := range cfgs {
		b := &boot{cfg: cfg, logger: quietLogger()}
		b.wireSandboxGitHubReadRelay() // nil proxy and nil manager: must be a silent no-op
		if b.agentMgr != nil {
			t.Fatalf("case %d wired a manager", i)
		}
	}
}

func TestSandboxReadTokenNeedsAppAuth(t *testing.T) {
	b := &boot{cfg: &config.Config{}, logger: quietLogger()}
	if tok, err := b.sandboxReadToken(); err == nil || tok != "" {
		t.Fatalf("token = %q, err = %v; want an error and no token", tok, err)
	}
}
