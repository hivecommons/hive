package config

import (
	"strings"
	"testing"
)

func TestSandboxBlockProblem(t *testing.T) {
	tests := []struct {
		name   string
		agent  AgentConfig
		global AgentSandboxConfig
		want   string
	}{
		{name: "no image anywhere", want: "no sandbox image"},
		{name: "global image, default podman", global: AgentSandboxConfig{Image: "img"}},
		{name: "agent image", agent: AgentConfig{Sandbox: &AgentSandboxOverride{Image: "img"}}},
		{name: "bad runtime", global: AgentSandboxConfig{Image: "img", Runtime: "vm"}, want: `runtime "vm"`},
		{name: "job without claim", global: AgentSandboxConfig{Image: "img", Runtime: SandboxRuntimeJob}, want: "workspace_claim"},
		{name: "job with global claim", global: AgentSandboxConfig{Image: "img", Runtime: SandboxRuntimeJob, Job: &SandboxJobConfig{WorkspaceClaim: "data"}}},
		{name: "job copy transport needs no claim", global: AgentSandboxConfig{Image: "img", Runtime: SandboxRuntimeJob, Job: &SandboxJobConfig{WorkspaceTransport: SandboxJobWorkspaceCopy}}},
		{name: "agent job block inherits the copy transport", agent: AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{ServiceAccount: "sa"}}}, global: AgentSandboxConfig{Image: "img", Runtime: SandboxRuntimeJob, Job: &SandboxJobConfig{WorkspaceTransport: SandboxJobWorkspaceCopy}}},
		{name: "job claim transport still needs a claim", global: AgentSandboxConfig{Image: "img", Runtime: SandboxRuntimeJob, Job: &SandboxJobConfig{WorkspaceTransport: SandboxJobWorkspaceClaim}}, want: "workspace_claim"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.agent.SandboxBlockProblem(tt.global)
			if tt.want == "" && got != "" {
				t.Fatalf("problem = %q, want none", got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("problem = %q, want it to mention %q", got, tt.want)
			}
		})
	}
}

func TestPrefilledSandboxBlockPinsDefaults(t *testing.T) {
	global := AgentSandboxConfig{Image: "global-img", Runtime: SandboxRuntimeJob, TimeoutS: 600}
	a := AgentConfig{Sandbox: &AgentSandboxOverride{EnvAllowlist: []string{"FOO"}}}
	got := a.PrefilledSandboxBlock(global)
	if got.Image != "global-img" || got.Runtime != SandboxRuntimeJob {
		t.Fatalf("prefill = %+v, want global image and runtime pinned", got)
	}
	if got.Enabled == nil || !*got.Enabled {
		t.Fatal("prefill must enable the block")
	}
	if len(got.EnvAllowlist) != 1 || got.EnvAllowlist[0] != "FOO" {
		t.Fatalf("own fields lost: %+v", got.EnvAllowlist)
	}
	got.EnvAllowlist[0] = "BAR"
	if a.Sandbox.EnvAllowlist[0] != "FOO" {
		t.Fatal("prefill aliased the agent's slice")
	}

	var bare AgentConfig
	if got := bare.PrefilledSandboxBlock(AgentSandboxConfig{Image: "i"}); got.Image != "i" || got.Runtime != SandboxRuntimePodman {
		t.Fatalf("bare prefill = %+v", got)
	}
}

func TestCopiedSandboxBlockDropsPublish(t *testing.T) {
	src := AgentConfig{Sandbox: &AgentSandboxOverride{
		Image:   "spyre-img",
		Runtime: SandboxRuntimeJob,
		Job:     &SandboxJobConfig{WorkspaceClaim: "data", NodeSelector: map[string]string{"accel": "spyre"}},
		Publish: &SandboxPublishConfig{Verify: []string{"pytest -x"}},
	}}
	got := CopiedSandboxBlock(&src, AgentSandboxConfig{}, nil)
	if got.Image != "spyre-img" || got.Runtime != SandboxRuntimeJob || got.Job == nil || got.Job.NodeSelector["accel"] != "spyre" {
		t.Fatalf("copy = %+v", got)
	}
	if got.Publish != nil {
		t.Fatalf("copy kept the source's publish block: %+v", got.Publish)
	}
	if got.Enabled == nil || !*got.Enabled {
		t.Fatal("copy must enable the block")
	}
	got.Job.NodeSelector["accel"] = "other"
	if src.Sandbox.Job.NodeSelector["accel"] != "spyre" {
		t.Fatal("copy aliased the source's job block")
	}

	keep := &SandboxPublishConfig{Verify: []string{"make check"}, VerifyTimeoutS: 30}
	got = CopiedSandboxBlock(&src, AgentSandboxConfig{}, keep)
	if got.Publish == nil || got.Publish.Verify[0] != "make check" || got.Publish.VerifyTimeoutS != 30 {
		t.Fatalf("receiver publish not kept: %+v", got.Publish)
	}

	if got := cloneSandboxOverride(nil); got == nil {
		t.Fatal("clone(nil) = nil, want empty block")
	}
}
