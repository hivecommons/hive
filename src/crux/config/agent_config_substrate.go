package config

import (
	"strings"
	"sync"
)

var (
	substratePreflightMu sync.RWMutex
	substratePreflight   func([]SandboxSubstrateConfig) []string
)

// SetSubstratePreflight registers the cluster-prerequisite check
// AgentSandboxGateWarnings runs for substrate-runtime agents. This package
// (and pkg/config above it) cannot import the Substrate client or
// crux/sandbox, so the code that owns the Control API client plugs itself in
// here. Passing nil unregisters it.
func SetSubstratePreflight(f func([]SandboxSubstrateConfig) []string) {
	substratePreflightMu.Lock()
	defer substratePreflightMu.Unlock()
	substratePreflight = f
}

// SubstratePreflightWarnings runs the registered preflight against the given
// substrate blocks; with none registered it reports nothing.
func SubstratePreflightWarnings(cfgs []SandboxSubstrateConfig) []string {
	substratePreflightMu.RLock()
	f := substratePreflight
	substratePreflightMu.RUnlock()
	if f == nil || len(cfgs) == 0 {
		return nil
	}
	return f(cfgs)
}

// SandboxSubstrateConfig is everything the Agent Substrate runtime needs
// beyond the agent's kick (ADR-0026 phase 2). Hive talks to Substrate's
// Control API (ate-api-server) over gRPC; Substrate owns the gVisor/microVM
// sandbox, the snapshot store and the atenet-egress gateway, so most of what
// the Job runtime spells out as a pod template is an ActorTemplate hive
// writes per kick from these settings.
//
// The workspace is NOT shared the way the Job and OpenShell runtimes share
// it: Substrate cannot mount an existing PersistentVolumeClaim, only a
// per-actor durableDir or a CSI volume it provisions itself. Hive therefore
// copies the per-kick workspace into the actor and back out through the
// bridge the sandbox image runs (crux substrate-bridge), reached over the
// atenet-router mTLS CONNECT tunnel. The local workspace stays the source of
// truth after the kick, so the push broker and PR flow are unchanged.
//
// AllowHosts is the egress allowlist handed to Substrate as the actor's
// EgressPolicy: the hosts hive's own MITM proxy (ADR-0002) would let a Podman
// sandbox reach — github.com, api.github.com — plus whatever the operator
// adds. Nothing else is reachable from the actor; Substrate default-denies.
// Credentials follow the Job rule: none of hive's enter it. Substrate's own
// egress credential injection is complementary and not used by hive.
type SandboxSubstrateConfig struct {
	// Endpoint is the Control API address (host:port, or an http(s):// URL
	// whose scheme picks TLS). Default api.ate-system.svc:443.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// Atespace is the Substrate atespace (its tenant namespace, not a
	// Kubernetes namespace) actors are created in. Default "hive".
	Atespace string `yaml:"atespace,omitempty" json:"atespace,omitempty"`
	// TokenEnv names the environment variable holding a Control API bearer
	// token. Empty reads TokenFile instead.
	TokenEnv string `yaml:"token_env,omitempty" json:"token_env,omitempty"`
	// TokenFile is the projected ServiceAccount token (audience
	// api.ate-system.svc) hive authenticates with. Default
	// /var/run/secrets/ate/token.
	TokenFile string `yaml:"token_file,omitempty" json:"token_file,omitempty"`
	// CAFile is the PEM bundle the Control API serving certificate chains
	// to (the servicedns ClusterTrustBundle projection). Empty uses
	// /var/run/secrets/ate/ca.crt when present, else the system roots.
	CAFile string `yaml:"ca_file,omitempty" json:"ca_file,omitempty"`
	// InsecureTLS skips Control API and router certificate verification.
	// Dev only.
	InsecureTLS bool `yaml:"insecure_tls,omitempty" json:"insecure_tls,omitempty"`
	// TimeoutS bounds one actor from create to delete. Non-positive uses
	// the sandbox timeout (agent_sandbox.timeout_s) hive already applies to
	// the kick.
	TimeoutS int `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	// RouterURL is the atenet-router CONNECT listener hive reaches the
	// actor's bridge through. Default https://atenet-router.ate-system.svc:8444.
	RouterURL string `yaml:"router_url,omitempty" json:"router_url,omitempty"`
	// RouterClientBundle and RouterCAFile are the podCertificate bundle the
	// router authenticates hive with and the CA its serving certificate
	// chains to. Defaults /run/router-client/credential-bundle.pem and
	// /run/router-client/router-ca.pem.
	RouterClientBundle string `yaml:"router_client_bundle,omitempty" json:"router_client_bundle,omitempty"`
	RouterCAFile       string `yaml:"router_ca_file,omitempty" json:"router_ca_file,omitempty"`
	// BridgePort is the port the in-actor bridge listens on. Default 8477.
	BridgePort int `yaml:"bridge_port,omitempty" json:"bridge_port,omitempty"`
	// Image is the digest-pinned sandbox image (…@sha256:…) actors run.
	// Substrate refuses tag-only references, so this overrides the agent
	// sandbox image, which is usually a tag.
	Image string `yaml:"image,omitempty" json:"image,omitempty"`
	// BridgeCommand is the actor entrypoint that serves the bridge. Empty
	// uses the crux binary's substrate-bridge subcommand.
	BridgeCommand []string `yaml:"bridge_command,omitempty" json:"bridge_command,omitempty"`
	// CPUMillicores and MemoryBytes are the ActorTemplate resource limits;
	// Substrate requires both. Defaults 2000 and 4 GiB.
	CPUMillicores int64 `yaml:"cpu_millicores,omitempty" json:"cpu_millicores,omitempty"`
	MemoryBytes   int64 `yaml:"memory_bytes,omitempty" json:"memory_bytes,omitempty"`
	// WorkerSelector pins actors to WorkerPools carrying these labels; the
	// pool decides the sandbox class (gvisor or microvm). Empty means any
	// pool.
	WorkerSelector map[string]string `yaml:"worker_selector,omitempty" json:"worker_selector,omitempty"`
	// SnapshotOnIdle suspends the actor (asynchronous snapshot) after the
	// kick instead of deleting it, so Substrate keeps a snapshot. The
	// snapshot is retained until the agent's next idle snapshot replaces it
	// (at most one per agent per hive process); it is not yet resumed on the
	// next kick, and actors left from before a hive restart are not
	// reclaimed. Default false: a kick is one-shot and snapshots cost
	// object storage.
	SnapshotOnIdle bool `yaml:"snapshot_on_idle,omitempty" json:"snapshot_on_idle,omitempty"`
	// AllowHosts are additional egress hosts (host or host:port, leading
	// *. wildcard allowed) the actor may reach, on top of the GitHub hosts
	// hive always allows.
	AllowHosts []string `yaml:"allow_hosts,omitempty" json:"allow_hosts,omitempty"`
	// PolicyProfile names an operator-side profile; hive records it on the
	// template so pools and in-actor tooling can act on it.
	PolicyProfile string `yaml:"policy_profile,omitempty" json:"policy_profile,omitempty"`
}

// SandboxSubstrate returns the effective Substrate runtime settings. The
// endpoint, atespace, credentials, TLS, router and bridge fields are cluster
// facts, so a per-agent block falls back to the global one for each it
// leaves empty; the image, resources, pool selector, snapshot choice and
// extra egress hosts are per-agent choices and replace the global ones
// wholesale when the per-agent block sets them.
func (a *AgentConfig) SandboxSubstrate(global AgentSandboxConfig) SandboxSubstrateConfig {
	var g SandboxSubstrateConfig
	if global.Substrate != nil {
		g = *global.Substrate
	}
	if a == nil || a.Sandbox == nil || a.Sandbox.Substrate == nil {
		return g
	}
	out := *a.Sandbox.Substrate
	fallback := func(dst *string, src string) {
		if strings.TrimSpace(*dst) == "" {
			*dst = src
		}
	}
	fallback(&out.Endpoint, g.Endpoint)
	fallback(&out.Atespace, g.Atespace)
	fallback(&out.TokenEnv, g.TokenEnv)
	fallback(&out.TokenFile, g.TokenFile)
	fallback(&out.CAFile, g.CAFile)
	fallback(&out.RouterURL, g.RouterURL)
	fallback(&out.RouterClientBundle, g.RouterClientBundle)
	fallback(&out.RouterCAFile, g.RouterCAFile)
	fallback(&out.Image, g.Image)
	fallback(&out.PolicyProfile, g.PolicyProfile)
	if out.TimeoutS == 0 {
		out.TimeoutS = g.TimeoutS
	}
	if out.BridgePort == 0 {
		out.BridgePort = g.BridgePort
	}
	if out.CPUMillicores == 0 {
		out.CPUMillicores = g.CPUMillicores
	}
	if out.MemoryBytes == 0 {
		out.MemoryBytes = g.MemoryBytes
	}
	if !out.InsecureTLS {
		out.InsecureTLS = g.InsecureTLS
	}
	if !out.SnapshotOnIdle {
		out.SnapshotOnIdle = g.SnapshotOnIdle
	}
	if len(out.BridgeCommand) == 0 {
		out.BridgeCommand = g.BridgeCommand
	}
	if len(out.AllowHosts) == 0 {
		out.AllowHosts = g.AllowHosts
	}
	if len(out.WorkerSelector) == 0 {
		out.WorkerSelector = g.WorkerSelector
	}
	return out
}
