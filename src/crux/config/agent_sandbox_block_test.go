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

// TestValidateSandboxJobTransport (#19694 review): a typo such as "Copy" is
// refused where the config loads, not read as the claim transport and failed
// only when a kick runs.
func TestValidateSandboxJobTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  *SandboxJobConfig
		want string
	}{
		{"nil", nil, ""},
		{"empty", &SandboxJobConfig{}, ""},
		{"claim", &SandboxJobConfig{WorkspaceTransport: "claim"}, ""},
		{"copy with a limit", &SandboxJobConfig{WorkspaceTransport: "copy", WorkspaceSizeLimit: "20Gi"}, ""},
		{"capitalised copy", &SandboxJobConfig{WorkspaceTransport: "Copy"}, "workspace_transport"},
		{"unknown", &SandboxJobConfig{WorkspaceTransport: "rsync"}, "workspace_transport"},
		{"bad limit", &SandboxJobConfig{WorkspaceTransport: "copy", WorkspaceSizeLimit: "lots"}, "workspace_size_limit"},
		{"runtime class", &SandboxJobConfig{RuntimeClassName: "kata-qemu"}, ""},
		{"bad runtime class", &SandboxJobConfig{RuntimeClassName: "Kata VM"}, "runtime_class_name"},
		{"overlong runtime class", &SandboxJobConfig{RuntimeClassName: strings.Repeat("a", maxRuntimeClassNameLen+1)}, "runtime_class_name"},
	} {
		err := ValidateSandboxJobTransport(tc.job)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want it to name %q", tc.name, err, tc.want)
		}
	}
}

// TestSandboxJobRuntimeClassName (#19694 review): job.runtime_class_name is
// overridden by its env var when that is set, defaults to empty (the cluster
// default), and a per-agent job block that leaves it empty inherits the
// global one rather than dropping the spoke's runtime class.
func TestSandboxJobRuntimeClassName(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == SandboxJobRuntimeClassEnv {
				return v
			}
			return ""
		}
	}
	if got := (SandboxJobConfig{}).EffectiveRuntimeClassName(env("")); got != "" {
		t.Errorf("default runtime class = %q, want empty", got)
	}
	if got := (SandboxJobConfig{RuntimeClassName: " kata "}).EffectiveRuntimeClassName(env("")); got != "kata" {
		t.Errorf("configured runtime class = %q, want kata", got)
	}
	if got := (SandboxJobConfig{RuntimeClassName: "kata"}).EffectiveRuntimeClassName(env(" gvisor ")); got != "gvisor" {
		t.Errorf("env-overridden runtime class = %q, want gvisor", got)
	}
	if got := (SandboxJobConfig{RuntimeClassName: "kata"}).EffectiveRuntimeClassName(nil); got != "kata" {
		t.Errorf("runtime class with nil getenv = %q, want kata", got)
	}
	global := AgentSandboxConfig{Job: &SandboxJobConfig{RuntimeClassName: "kata"}}
	inherit := &AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{ServiceAccount: "sa"}}}
	if got := inherit.SandboxJob(global).RuntimeClassName; got != "kata" {
		t.Errorf("per-agent block runtime class = %q, want the global kata inherited", got)
	}
	own := &AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{RuntimeClassName: "gvisor"}}}
	if got := own.SandboxJob(global).RuntimeClassName; got != "gvisor" {
		t.Errorf("per-agent block runtime class = %q, want its own gvisor", got)
	}
}
