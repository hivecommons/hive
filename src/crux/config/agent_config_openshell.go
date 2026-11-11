package config

import (
	"strings"
	"sync"
)

var (
	openshellPreflightMu sync.RWMutex
	openshellPreflight   func([]SandboxOpenShellConfig) []string
)

// SetOpenShellPreflight registers the cluster-prerequisite check
// AgentSandboxGateWarnings runs for openshell-runtime agents. This package
// (and pkg/config above it) cannot import the OpenShell SDK or crux/sandbox,
// so the code that owns the gateway client plugs itself in here. Passing nil
// unregisters it.
func SetOpenShellPreflight(f func([]SandboxOpenShellConfig) []string) {
	openshellPreflightMu.Lock()
	defer openshellPreflightMu.Unlock()
	openshellPreflight = f
}

// OpenShellPreflightWarnings runs the registered preflight against the given
// openshell blocks; with none registered it reports nothing.
func OpenShellPreflightWarnings(cfgs []SandboxOpenShellConfig) []string {
	openshellPreflightMu.RLock()
	f := openshellPreflight
	openshellPreflightMu.RUnlock()
	if f == nil || len(cfgs) == 0 {
		return nil
	}
	return f(cfgs)
}

// SandboxOpenShellConfig is everything the OpenShell runtime needs beyond the
// image (ADR-0026 phase 1). Hive talks to an OpenShell gateway over its gRPC
// API; the gateway owns the sandbox pod, the Landlock/seccomp confinement and
// the egress proxy, so most of what the Job runtime spells out as a pod
// template is a gateway concern here.
//
// The workspace is shared the same way the Job runtime shares it: the
// sandbox mounts the SAME PersistentVolumeClaim hive's /data lives on, at a
// subPath pointing at the per-kick workspace. The OpenShell SDK's file
// transport is single-file and not available on every gateway, so a copy
// in/out was not an option. This needs the gateway's `allow_driver_config`
// setting on and the claim labelled `openshell.ai/sandbox-attachable`.
//
// AllowHosts is the egress allowlist handed to the gateway as a network
// policy: the hosts hive's own MITM proxy (ADR-0002) would let a Podman
// sandbox reach — github.com, api.github.com — plus whatever the operator
// adds (a model gateway, the hub). Nothing else is reachable from the
// sandbox. Credentials follow the Job rule: none of hive's enter it.
type SandboxOpenShellConfig struct {
	// GatewayURL is the OpenShell gateway's gRPC address (host:port, or an
	// http(s):// URL whose scheme picks TLS). Required when the runtime is
	// selected.
	GatewayURL string `yaml:"gateway_url,omitempty" json:"gateway_url,omitempty"`
	// Namespace is the OpenShell workspace sandboxes are created in. It maps
	// to the OpenShell notion of a workspace, not a Kubernetes namespace.
	// Default "default".
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	// PolicyProfile names a gateway-side policy profile to apply alongside
	// hive's egress policy. Empty means hive's derived policy only.
	PolicyProfile string `yaml:"policy_profile,omitempty" json:"policy_profile,omitempty"`
	// TimeoutS bounds one sandbox from create to delete. Non-positive uses
	// the sandbox timeout (agent_sandbox.timeout_s) hive already applies to
	// the kick.
	TimeoutS int `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	// TokenEnv names the environment variable holding the gateway bearer
	// token. Empty means the gateway is reached unauthenticated (dev only).
	TokenEnv string `yaml:"token_env,omitempty" json:"token_env,omitempty"`
	// InsecureTLS skips gateway certificate verification. Dev only.
	InsecureTLS bool `yaml:"insecure_tls,omitempty" json:"insecure_tls,omitempty"`
	// WorkspaceClaim / WorkspaceClaimMount mirror SandboxJobConfig: the PVC
	// hive's sandbox workspace root lives on, and where hive sees it.
	WorkspaceClaim      string `yaml:"workspace_claim,omitempty" json:"workspace_claim,omitempty"`
	WorkspaceClaimMount string `yaml:"workspace_claim_mount,omitempty" json:"workspace_claim_mount,omitempty"`
	// AllowHosts are additional egress hosts (host or host:port) the
	// sandbox may reach, on top of the GitHub hosts hive always allows.
	AllowHosts []string `yaml:"allow_hosts,omitempty" json:"allow_hosts,omitempty"`
	// Binaries are the in-image executables the egress policy applies to.
	// OpenShell matches network rules per binary; a rule with no binaries
	// allows nothing. Empty uses the launcher's default set.
	Binaries []string `yaml:"binaries,omitempty" json:"binaries,omitempty"`
	// RuntimeClassName selects a sandbox RuntimeClass (gVisor, Kata) on the
	// cluster; empty uses the gateway default.
	RuntimeClassName string `yaml:"runtime_class_name,omitempty" json:"runtime_class_name,omitempty"`
	// NodeSelector and Tolerations pass through to the sandbox pod via the
	// gateway's Kubernetes driver config, like the Job runtime's.
	NodeSelector map[string]string      `yaml:"node_selector,omitempty" json:"node_selector,omitempty"`
	Tolerations  []SandboxJobToleration `yaml:"tolerations,omitempty" json:"tolerations,omitempty"`
}

// SandboxOpenShell returns the effective OpenShell runtime settings. The
// gateway, workspace, token, TLS and claim fields are cluster facts, so a
// per-agent block falls back to the global one for each it leaves empty;
// scheduling fields (node selector, tolerations, runtime class) and the
// extra egress hosts are per-agent choices and replace the global ones
// wholesale when the per-agent block sets them.
func (a *AgentConfig) SandboxOpenShell(global AgentSandboxConfig) SandboxOpenShellConfig {
	var g SandboxOpenShellConfig
	if global.OpenShell != nil {
		g = *global.OpenShell
	}
	if a == nil || a.Sandbox == nil || a.Sandbox.OpenShell == nil {
		return g
	}
	out := *a.Sandbox.OpenShell
	fallback := func(dst *string, src string) {
		if strings.TrimSpace(*dst) == "" {
			*dst = src
		}
	}
	fallback(&out.GatewayURL, g.GatewayURL)
	fallback(&out.Namespace, g.Namespace)
	fallback(&out.PolicyProfile, g.PolicyProfile)
	fallback(&out.TokenEnv, g.TokenEnv)
	fallback(&out.WorkspaceClaim, g.WorkspaceClaim)
	fallback(&out.WorkspaceClaimMount, g.WorkspaceClaimMount)
	fallback(&out.RuntimeClassName, g.RuntimeClassName)
	if out.TimeoutS == 0 {
		out.TimeoutS = g.TimeoutS
	}
	if !out.InsecureTLS {
		out.InsecureTLS = g.InsecureTLS
	}
	if len(out.AllowHosts) == 0 {
		out.AllowHosts = g.AllowHosts
	}
	if len(out.Binaries) == 0 {
		out.Binaries = g.Binaries
	}
	if len(out.NodeSelector) == 0 {
		out.NodeSelector = g.NodeSelector
	}
	if len(out.Tolerations) == 0 {
		out.Tolerations = g.Tolerations
	}
	return out
}
