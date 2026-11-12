package config

import (
	"fmt"
	"sort"
	"strings"

	cruxconfig "github.com/hivecommons/hive/crux/config"
)

func sortedRuntimeNames() []string {
	names := make([]string, 0, len(ValidSandboxRuntimes))
	for n := range ValidSandboxRuntimes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// MaxBobSessionLabelLen bounds bob reporting labels:
// governor.bob.session_prefix + <agent> and agents.<name>.bob.session_label.
const MaxBobSessionLabelLen = 64

// MaxBobDisplayNameLen bounds the legacy agents.<name>.bob_display_name alias.
const MaxBobDisplayNameLen = MaxBobSessionLabelLen

// ValidateBobSessionLabel checks an optional bob reporting label component.
// Empty is valid. Non-empty values must be at most MaxBobSessionLabelLen bytes
// of ASCII letters, digits, '-', '_' or '.', so the label is safe in env vars,
// file names, Bob --instance-id, and report keys.
func ValidateBobSessionLabel(field, v string) error {
	if v == "" {
		return nil
	}
	if len(v) > MaxBobSessionLabelLen {
		return fmt.Errorf("%s %q is longer than %d characters", field, v, MaxBobSessionLabelLen)
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("%s %q contains %q (allowed: letters, digits, '-', '_', '.')", field, v, r)
		}
	}
	return nil
}

// ValidateBobDisplayName checks the legacy flat bob reporting label.
func ValidateBobDisplayName(v string) error {
	return ValidateBobSessionLabel("bob_display_name", v)
}

// ValidateKickTemplateName gates the SHAPE of a kick_template value: it is a
// bare file name that the scheduler looks up under the policy directories and
// the embedded defaults, never a path. Empty is fine (convention lookup). A
// value carrying a path separator or ".." is refused at load/save time — the
// scheduler joins it under /data/policies and the cloned examples dir, so a
// path would read outside them. Whether the NAME resolves to a file is a
// separate, softer question (a template can legitimately be created later by
// the prompt editor), answered with a warning by
// scheduler.WarnDanglingKickTemplates and shown in the prompt editor
// (hivecommons/hive#7390).
func ValidateKickTemplateName(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if strings.ContainsAny(v, `/\`) || strings.Contains(v, "..") {
		return fmt.Errorf("invalid kick_template %q: must be a bare file name such as scanner-holdgated.md, not a path", v)
	}
	return nil
}

const cadenceTargetSeparator = "|"

// CadenceTargetKey returns the persisted governor cadence/last-kick key for an
// agent, optionally scoped to a repo. Empty repo preserves the legacy agent key.
func CadenceTargetKey(agent, repo string) string {
	if repo == "" {
		return agent
	}
	return agent + cadenceTargetSeparator + repo
}

// SplitCadenceTargetKey separates a governor cadence/last-kick key into agent
// and repo. Keys without a repo are the legacy aggregate shape.
func SplitCadenceTargetKey(key string) (agent, repo string) {
	agent, repo, ok := strings.Cut(key, cadenceTargetSeparator)
	if !ok {
		return key, ""
	}
	return agent, repo
}

// AgentSandboxGateWarnings reports the sandbox misconfigurations that the
// two-gate opt-in otherwise makes SILENT. Empty means nothing to say.
//
// SandboxEnabled (above) requires BOTH agent_sandbox.enabled AND a per-agent
// sandbox.enabled: true. That second gate is deliberate — a sandboxed agent
// runs a completely different execution model (no tmux CLI at all; every kick
// is a podman run against the primary repo), and startSandboxKickLocked in
// pkg/agent has NO fallback to tmux: an agent flipped on without a resolvable
// image fails every kick outright. So the gate is not something to quietly
// collapse.
//
// What it must not do is stay invisible. The dashboard's Security tab writes
// only the GLOBAL flag (handleGovernorSecurity), and it is the only sandbox
// control the UI offers — so an owner can turn "agent sandbox" on, be told the
// setting was updated, and have every agent keep running unconfined. The
// response's own sandboxedAgents field stays 0 and nothing explains why.
//
// #4918 is what that silence costs. An agent doing correct work on an assigned
// third-party repo ran that repo's test suite, a hook escaped its stubs, and
// `rpm-ostree kargs` reached the operator's real deployment. An operator who
// had flipped the sandbox toggle on would reasonably believe they were covered.
//
// These are WARNINGS, not validation errors: every state described here is a
// legal config, and refusing to boot on it would turn a diagnostic into an
// outage.
func AgentSandboxGateWarnings(cfg *Config) []string {
	return AgentSandboxGateReport(cfg).Warnings
}

// AgentSandboxGateReport is AgentSandboxGateWarnings plus the preflight
// findings kept apart per runtime, so callers can attribute them to the agents
// on that runtime without parsing strings. Warnings is the same flat list.
func AgentSandboxGateReport(cfg *Config) SandboxGateReport {
	if cfg == nil || !cfg.AgentSandbox.Enabled {
		// The sandbox is off globally. That is the documented default and the
		// posture the docs describe; it is not a misconfiguration.
		return SandboxGateReport{}
	}

	var optedIn, noImage, noClaim, noGateway, noDigest, badRuntime []string
	var openshellBlocks []SandboxOpenShellConfig
	var substrateBlocks []SandboxSubstrateConfig
	for name, a := range cfg.Agents {
		if !a.SandboxEnabled(cfg.AgentSandbox) {
			continue
		}
		optedIn = append(optedIn, name)
		if strings.TrimSpace(a.SandboxImage(cfg.AgentSandbox)) == "" {
			noImage = append(noImage, name)
		}
		switch rt := a.SandboxRuntime(cfg.AgentSandbox); {
		case !ValidSandboxRuntimes[rt]:
			badRuntime = append(badRuntime, name+"="+rt)
		case rt == SandboxRuntimeJob && strings.TrimSpace(a.SandboxJob(cfg.AgentSandbox).WorkspaceClaim) == "":
			noClaim = append(noClaim, name)
		case rt == SandboxRuntimeOpenShell:
			os := a.SandboxOpenShell(cfg.AgentSandbox)
			if strings.TrimSpace(os.GatewayURL) == "" {
				noGateway = append(noGateway, name)
			}
			if strings.TrimSpace(os.WorkspaceClaim) == "" {
				noClaim = append(noClaim, name)
			}
			if strings.TrimSpace(os.GatewayURL) != "" {
				openshellBlocks = append(openshellBlocks, os)
			}
		case rt == SandboxRuntimeSubstrate:
			sb := a.SandboxSubstrate(cfg.AgentSandbox)
			// Substrate refuses tag-only images; the agent image is usually a
			// tag, so the substrate block must carry the digest-pinned one.
			if !strings.Contains(sb.Image, "@sha256:") {
				noDigest = append(noDigest, name)
			}
			substrateBlocks = append(substrateBlocks, sb)
		}
	}
	sort.Strings(optedIn)
	sort.Strings(noImage)
	sort.Strings(noClaim)
	sort.Strings(noGateway)
	sort.Strings(noDigest)
	sort.Strings(badRuntime)

	var out []string
	if len(optedIn) == 0 {
		out = append(out, fmt.Sprintf(
			"agent_sandbox.enabled is true but NO agent is opted in, so the sandbox is inert and all %d agent(s) still run unconfined on the tmux path — "+
				"the per-agent gate is separate: set `sandbox: {enabled: true}` on each agent that should be sandboxed (#4918)",
			len(cfg.Agents)))
		return SandboxGateReport{Warnings: out}
	}
	if len(noImage) > 0 {
		out = append(out, fmt.Sprintf(
			"agent(s) %s are sandbox-opted-in but resolve no sandbox image; sandboxed kicks have no tmux fallback and will fail outright — "+
				"set agent_sandbox.image (or the per-agent sandbox.image)",
			strings.Join(noImage, ", ")))
	}
	if len(badRuntime) > 0 {
		out = append(out, fmt.Sprintf(
			"agent(s) %s name a sandbox runtime hive does not have; sandboxed kicks will fail outright — "+
				"sandbox.runtime must be one of %q (#6311)",
			strings.Join(badRuntime, ", "), sortedRuntimeNames()))
	}
	if len(noClaim) > 0 {
		out = append(out, fmt.Sprintf(
			"agent(s) %s use the job or openshell sandbox runtime but resolve no workspace_claim; the sandbox has no way to see the workspace and every kick will fail — "+
				"set agent_sandbox.job.workspace_claim / agent_sandbox.openshell.workspace_claim (or the per-agent one) to the PVC hive's data lives on (#6311)",
			strings.Join(noClaim, ", ")))
	}
	if len(noGateway) > 0 {
		out = append(out, fmt.Sprintf(
			"agent(s) %s use the openshell sandbox runtime but resolve no openshell.gateway_url; every kick will fail — "+
				"set agent_sandbox.openshell.gateway_url (or the per-agent sandbox.openshell.gateway_url) to the OpenShell gateway's gRPC address (ADR-0026)",
			strings.Join(noGateway, ", ")))
	}
	if len(noDigest) > 0 {
		out = append(out, fmt.Sprintf(
			"agent(s) %s use the substrate sandbox runtime but resolve no digest-pinned substrate.image; Substrate refuses tag-only images and every kick will fail — "+
				"set agent_sandbox.substrate.image (or the per-agent sandbox.substrate.image) to the sandbox image's …@sha256:… reference (ADR-0026 phase 2)",
			strings.Join(noDigest, ", ")))
	}
	if len(optedIn) < len(cfg.Agents) {
		out = append(out, fmt.Sprintf(
			"agent_sandbox.enabled is true but only %d of %d agent(s) are opted in (%s); the rest still run unconfined on the tmux path (#4918)",
			len(optedIn), len(cfg.Agents), strings.Join(optedIn, ", ")))
	}
	// Last, because it may reach the network (bounded and cached): the static
	// warnings above must not wait on a gateway.
	openshellWarnings := cruxconfig.OpenShellPreflightWarnings(openshellBlocks)
	substrateWarnings := cruxconfig.SubstratePreflightWarnings(substrateBlocks)
	out = append(out, openshellWarnings...)
	out = append(out, substrateWarnings...)
	return SandboxGateReport{Warnings: out, OpenShell: openshellWarnings, Substrate: substrateWarnings}
}

// CadenceIsOperatorOwned reports whether an operator explicitly set the
// cadence for agent in the given governor mode, which makes it immune to pack
// reconciliation — the same contract ModelIsOperatorOwned provides for models.
func (g *GovernorConfig) CadenceIsOperatorOwned(mode, agent string) bool {
	return g.CadenceOwners[mode][agent] == FieldOwnerOperator
}

// ClaimCadenceOwnership marks the (mode, agent) cadence as operator-owned so
// no subsequent pack apply reconciles it back to the pack default.
func (g *GovernorConfig) ClaimCadenceOwnership(mode, agent string) {
	if g.CadenceOwners == nil {
		g.CadenceOwners = make(map[string]map[string]string)
	}
	if g.CadenceOwners[mode] == nil {
		g.CadenceOwners[mode] = make(map[string]string)
	}
	g.CadenceOwners[mode][agent] = FieldOwnerOperator
}

// ReleaseCadenceOwnership drops the ownership marker for (mode, agent). Called
// when the cadence entry itself is removed (e.g. the agent left the roster) so
// a stale claim cannot outlive the value it protected.
func (g *GovernorConfig) ReleaseCadenceOwnership(mode, agent string) {
	owners, ok := g.CadenceOwners[mode]
	if !ok {
		return
	}
	delete(owners, agent)
	if len(owners) == 0 {
		delete(g.CadenceOwners, mode)
	}
}
