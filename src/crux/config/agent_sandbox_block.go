package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SandboxBlockProblem reports why an agent's effective sandbox block — its own
// sandbox fields, then the agent_sandbox defaults — cannot run a kick, or ""
// when the block is complete. It checks only what config alone decides (the
// same config checks the manager makes before a kick), so the dashboard can
// refuse to turn the sandbox on for an agent whose every kick would fail
// (hivecommons/hive#18525).
func (a *AgentConfig) SandboxBlockProblem(global AgentSandboxConfig) string {
	if strings.TrimSpace(a.SandboxImage(global)) == "" {
		return "no sandbox image: neither the agent's sandbox.image nor agent_sandbox.image is set"
	}
	runtime := a.SandboxRuntime(global)
	if !ValidSandboxRuntimes[runtime] {
		return fmt.Sprintf("sandbox runtime %q is not one hive has (%s, %s, %s or %s)", runtime, SandboxRuntimePodman, SandboxRuntimeJob, SandboxRuntimeOpenShell, SandboxRuntimeSubstrate)
	}
	if runtime == SandboxRuntimeJob && strings.TrimSpace(a.SandboxJob(global).WorkspaceClaim) == "" {
		return "the job sandbox runtime needs job.workspace_claim, in the agent's sandbox.job or in agent_sandbox.job"
	}
	return ""
}

// PrefilledSandboxBlock returns the agent's own sandbox block with the image
// and runtime pinned to what they resolve to now (the agent's value, else the
// agent_sandbox default) and enabled set to true. Every other field is left
// as the agent wrote it, so it keeps inheriting the global default.
func (a *AgentConfig) PrefilledSandboxBlock(global AgentSandboxConfig) *AgentSandboxOverride {
	out := &AgentSandboxOverride{}
	if a != nil && a.Sandbox != nil {
		out = cloneSandboxOverride(a.Sandbox)
	}
	out.Image = a.SandboxImage(global)
	out.Runtime = a.SandboxRuntime(global)
	on := true
	out.Enabled = &on
	return out
}

// CopiedSandboxBlock returns src's sandbox block for another agent to run
// with: src's own fields (deep-copied) with the image and runtime pinned to
// what they resolve to for src, enabled set to true. The publish block is
// dropped, because its verify commands are written for src's repository and
// work, not the receiving agent's; the receiving agent's own publish block,
// passed as keepPublish, is kept instead.
func CopiedSandboxBlock(src *AgentConfig, global AgentSandboxConfig, keepPublish *SandboxPublishConfig) *AgentSandboxOverride {
	out := &AgentSandboxOverride{}
	if src != nil && src.Sandbox != nil {
		out = cloneSandboxOverride(src.Sandbox)
	}
	out.Image = src.SandboxImage(global)
	out.Runtime = src.SandboxRuntime(global)
	out.Publish = nil
	if keepPublish != nil {
		p := *keepPublish
		p.Verify = append([]string(nil), keepPublish.Verify...)
		out.Publish = &p
	}
	on := true
	out.Enabled = &on
	return out
}

// cloneSandboxOverride deep-copies a sandbox block. Every field carries a
// JSON tag, so a round trip copies the nested maps and slices too.
func cloneSandboxOverride(in *AgentSandboxOverride) *AgentSandboxOverride {
	out := &AgentSandboxOverride{}
	if in == nil {
		return out
	}
	raw, err := json.Marshal(in)
	if err != nil {
		cp := *in
		return &cp
	}
	if err := json.Unmarshal(raw, out); err != nil {
		cp := *in
		return &cp
	}
	return out
}
