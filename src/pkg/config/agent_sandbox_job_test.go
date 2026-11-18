package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sandbox.runtime: job (#6311) — precedence, merge semantics, and the boot
// warnings that keep a misconfigured job runtime from failing silently.

func TestSandboxRuntimePrecedence(t *testing.T) {
	var nilAgent *AgentConfig
	if got := nilAgent.SandboxRuntime(AgentSandboxConfig{}); got != SandboxRuntimePodman {
		t.Errorf("nil agent, nothing set = %q, want podman", got)
	}
	a := &AgentConfig{}
	if got := a.SandboxRuntime(AgentSandboxConfig{Runtime: " job "}); got != SandboxRuntimeJob {
		t.Errorf("global job = %q", got)
	}
	a.Sandbox = &AgentSandboxOverride{Runtime: "podman"}
	if got := a.SandboxRuntime(AgentSandboxConfig{Runtime: "job"}); got != SandboxRuntimePodman {
		t.Errorf("per-agent podman must beat global job, got %q", got)
	}
	a.Sandbox = &AgentSandboxOverride{Runtime: "lambda"}
	if got := a.SandboxRuntime(AgentSandboxConfig{}); got != "lambda" {
		t.Errorf("unknown runtime must be returned as written for the manager to reject, got %q", got)
	}
}

func TestSandboxJobMergeSemantics(t *testing.T) {
	global := AgentSandboxConfig{Job: &SandboxJobConfig{
		WorkspaceClaim: "hive-data", WorkspaceClaimMount: "/data", TTLSeconds: 900,
		NodeSelector:   map[string]string{"pool": "general"},
		EnvFromSecrets: []string{"global-secret"},
	}}
	var nilAgent *AgentConfig
	if got := nilAgent.SandboxJob(global); got.WorkspaceClaim != "hive-data" || got.NodeSelector["pool"] != "general" {
		t.Errorf("nil agent must get the global block, got %+v", got)
	}
	if got := (&AgentConfig{}).SandboxJob(AgentSandboxConfig{}); got.WorkspaceClaim != "" {
		t.Errorf("no job block anywhere must be empty, got %+v", got)
	}
	a := &AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{
		NodeSelector: map[string]string{"accelerator": "spyre"},
		Resources:    SandboxJobResources{Limits: map[string]string{"ibm.com/spyre_pf": "2"}},
	}}}
	got := a.SandboxJob(global)
	if got.WorkspaceClaim != "hive-data" || got.WorkspaceClaimMount != "/data" || got.TTLSeconds != 900 {
		t.Errorf("claim fields and TTL must fall back to global, got %+v", got)
	}
	if got.NodeSelector["accelerator"] != "spyre" || got.NodeSelector["pool"] != "" {
		t.Errorf("pod-template fields must be replaced wholesale, got %+v", got.NodeSelector)
	}
	if len(got.EnvFromSecrets) != 0 {
		t.Errorf("per-agent block without secrets must not inherit global secrets (credential scope is per agent), got %v", got.EnvFromSecrets)
	}
	if got.Resources.Limits["ibm.com/spyre_pf"] != "2" {
		t.Errorf("resources not carried: %+v", got.Resources)
	}
	a.Sandbox.Job.WorkspaceClaim = "other-claim"
	a.Sandbox.Job.TTLSeconds = 60
	got = a.SandboxJob(global)
	if got.WorkspaceClaim != "other-claim" || got.TTLSeconds != 60 {
		t.Errorf("per-agent claim/TTL must win when set, got %+v", got)
	}
}

func TestSandboxJobYAMLRoundTrip(t *testing.T) {
	src := `
agent_sandbox:
  enabled: true
  runtime: job
  job:
    workspace_claim: hive-data
agents:
  deps:
    sandbox:
      enabled: true
      image: registry.example/dev:latest
      job:
        node_selector: { accelerator: spyre }
        resources:
          limits: { "ibm.com/spyre_pf": "2" }
        env_from_secrets: [ artifact-registry ]
        tolerations:
          - { key: spyre, operator: Exists, effect: NoSchedule }
        volumes:
          - { name: cache, claim: compile-cache, mount_path: /cache, read_only: true }
        ttl_seconds: 600
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	a := cfg.Agents["deps"]
	if got := a.SandboxRuntime(cfg.AgentSandbox); got != SandboxRuntimeJob {
		t.Errorf("runtime = %q", got)
	}
	job := a.SandboxJob(cfg.AgentSandbox)
	if job.WorkspaceClaim != "hive-data" || job.NodeSelector["accelerator"] != "spyre" || job.Resources.Limits["ibm.com/spyre_pf"] != "2" {
		t.Errorf("job block = %+v", job)
	}
	if len(job.Tolerations) != 1 || job.Tolerations[0].Effect != "NoSchedule" {
		t.Errorf("tolerations = %+v", job.Tolerations)
	}
	if len(job.Volumes) != 1 || job.Volumes[0].Claim != "compile-cache" || !job.Volumes[0].ReadOnly {
		t.Errorf("volumes = %+v", job.Volumes)
	}
	if job.TTLSeconds != 600 || job.EnvFromSecrets[0] != "artifact-registry" {
		t.Errorf("ttl/secrets = %+v", job)
	}
	if warnings := AgentSandboxGateWarnings(&cfg); len(warnings) != 0 {
		t.Errorf("a complete job config must raise no warnings, got %q", warnings)
	}
}

func TestGateWarnsJobRuntimeWithoutClaim(t *testing.T) {
	yes := true
	cfg := &Config{
		AgentRuntimeConfig: AgentRuntimeConfig{
			AgentSandbox: AgentSandboxConfig{Enabled: true, Image: "img"},
			Agents: map[string]AgentConfig{
				"deps": {Sandbox: &AgentSandboxOverride{Enabled: &yes, Runtime: SandboxRuntimeJob}},
			},
		},
	}
	got := AgentSandboxGateWarnings(cfg)
	if len(got) != 1 || !strings.Contains(got[0], "deps") || !strings.Contains(got[0], "workspace_claim") {
		t.Errorf("warnings = %q, want one naming deps and job.workspace_claim", got)
	}
}

func TestGateWarnsUnknownRuntime(t *testing.T) {
	yes := true
	cfg := &Config{
		AgentRuntimeConfig: AgentRuntimeConfig{
			AgentSandbox: AgentSandboxConfig{Enabled: true, Image: "img"},
			Agents: map[string]AgentConfig{
				"deps": {Sandbox: &AgentSandboxOverride{Enabled: &yes, Runtime: "lambda"}},
			},
		},
	}
	got := AgentSandboxGateWarnings(cfg)
	if len(got) != 1 || !strings.Contains(got[0], "deps=lambda") || !strings.Contains(got[0], "job") || !strings.Contains(got[0], "podman") {
		t.Errorf("warnings = %q, want one naming deps=lambda and the valid runtimes", got)
	}
}

func TestSandboxJobImagePullSecretsMergeAndValidation(t *testing.T) {
	global := AgentSandboxConfig{Job: &SandboxJobConfig{WorkspaceClaim: "hive-data", ImagePullSecrets: []string{"reg-a"}}}
	inherit := &AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{ServiceAccount: "sa"}}}
	if got := inherit.SandboxJob(global).ImagePullSecrets; len(got) != 1 || got[0] != "reg-a" {
		t.Errorf("inherited = %v, want [reg-a]", got)
	}
	own := &AgentConfig{Sandbox: &AgentSandboxOverride{Job: &SandboxJobConfig{ImagePullSecrets: []string{"reg-b"}}}}
	if got := own.SandboxJob(global).ImagePullSecrets; len(got) != 1 || got[0] != "reg-b" {
		t.Errorf("own = %v, want [reg-b]", got)
	}
	if err := ValidateImagePullSecrets([]string{"reg-a", "x.y"}); err != nil {
		t.Errorf("valid names refused: %v", err)
	}
	for _, bad := range []string{"user:pass", "UPPER", "a b", ""} {
		if err := ValidateImagePullSecrets([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := ValidateImagePullSecrets(make([]string, MaxJobImagePullSecrets+1)); err == nil {
		t.Error("over-long list accepted")
	}
}

func TestSandboxJobImagePullSecretsYAMLRoundTrip(t *testing.T) {
	var job SandboxJobConfig
	if err := yaml.Unmarshal([]byte("service_account: sa\nimage_pull_secrets: [reg-a, reg-b]\n"), &job); err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var back SandboxJobConfig
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.ImagePullSecrets) != 2 || back.ImagePullSecrets[1] != "reg-b" {
		t.Errorf("round trip = %v", back.ImagePullSecrets)
	}
}

func TestGateWarnsServiceAccountWithoutImagePullSecrets(t *testing.T) {
	yes := true
	mk := func(pull []string) *Config {
		return &Config{AgentRuntimeConfig: AgentRuntimeConfig{
			AgentSandbox: AgentSandboxConfig{Enabled: true, Image: "img", Runtime: SandboxRuntimeJob,
				Job: &SandboxJobConfig{WorkspaceClaim: "hive-data", ServiceAccount: "job-sa", ImagePullSecrets: pull}},
			Agents: map[string]AgentConfig{"deps": {Sandbox: &AgentSandboxOverride{Enabled: &yes}}},
		}}
	}
	got := AgentSandboxGateWarnings(mk(nil))
	if len(got) != 1 || !strings.Contains(got[0], "image_pull_secrets") {
		t.Errorf("warnings = %q, want one naming image_pull_secrets", got)
	}
	if got := AgentSandboxGateWarnings(mk([]string{"reg-a"})); len(got) != 0 {
		t.Errorf("configured list must raise no warning, got %q", got)
	}
	if got := AgentSandboxGateWarnings(mk([]string{"user:pass"})); len(got) != 1 || !strings.Contains(got[0], "invalid") {
		t.Errorf("invalid name warnings = %q", got)
	}
}
