package config

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// SandboxJobToleration mirrors the subset of a Kubernetes toleration an
// operator writes by hand.
type SandboxJobToleration struct {
	Key      string `yaml:"key,omitempty" json:"key,omitempty"`
	Operator string `yaml:"operator,omitempty" json:"operator,omitempty"`
	Value    string `yaml:"value,omitempty" json:"value,omitempty"`
	Effect   string `yaml:"effect,omitempty" json:"effect,omitempty"`
}

// SandboxJobResources mirrors Kubernetes resource requirements with string
// quantities so a device limit (`<vendor>.com/<device>: "2"`) passes through.
type SandboxJobResources struct {
	Limits   map[string]string `yaml:"limits,omitempty" json:"limits,omitempty"`
	Requests map[string]string `yaml:"requests,omitempty" json:"requests,omitempty"`
}

// SandboxJobVolume is an additional PVC mounted into the Job (a compile
// cache, a model store).
type SandboxJobVolume struct {
	Name      string `yaml:"name,omitempty" json:"name,omitempty"`
	Claim     string `yaml:"claim" json:"claim"`
	MountPath string `yaml:"mount_path" json:"mount_path"`
	ReadOnly  bool   `yaml:"read_only,omitempty" json:"read_only,omitempty"`
}

// SandboxGitHubReadProxy configures the sandbox GitHub read relay (#18865).
// Default off. With enabled: true, hive listens (TLS) on listen_addr and every
// sandbox.runtime: job kick gets HTTPS_PROXY pointing at endpoint plus a
// per-kick relay key. The relay attaches a read-only, repo-scoped GitHub App
// token to GET/HEAD requests for api.github.com, codeload.github.com,
// raw.githubusercontent.com and the git fetch paths of github.com. Writes are
// refused and the token never enters the Job.
type SandboxGitHubReadProxy struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// ListenAddr is the host:port hive binds. There is deliberately NO
	// default: binding an unrequested port on every interface is a decision
	// the operator makes. Required when enabled.
	ListenAddr string `yaml:"listen_addr,omitempty" json:"listen_addr,omitempty"`
	// Endpoint is the https://host:port Job pods use to reach the relay, for
	// example the in-namespace Service name. Required when enabled and must
	// be https: the listener serves TLS with a certificate issued by hive's
	// proxy CA for this host. It carries no credentials (hive adds a per-kick
	// key itself).
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// NoProxy lists extra hosts or domains a Job reaches directly, added to
	// the built-in cluster-local list (localhost, .svc, .cluster.local).
	NoProxy []string `yaml:"no_proxy,omitempty" json:"no_proxy,omitempty"`
}

// Listen returns the trimmed bind address; empty when unset (there is no
// default).
func (c *SandboxGitHubReadProxy) Listen() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.ListenAddr)
}

// Validate reports why an enabled block cannot work. A disabled or nil block
// is always valid.
func (c *SandboxGitHubReadProxy) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	listen := c.Listen()
	if listen == "" {
		return errors.New("agent_sandbox.job.github_read_proxy.listen_addr is required when enabled (there is no default; set the host:port hive should bind, for example :8443)")
	}
	if _, port, err := net.SplitHostPort(listen); err != nil || port == "" {
		return errors.New("agent_sandbox.job.github_read_proxy.listen_addr must be host:port (the host may be empty to bind every interface)")
	}
	raw := strings.TrimSpace(c.Endpoint)
	if raw == "" {
		return errors.New("agent_sandbox.job.github_read_proxy.endpoint is required when enabled (the https://host:port Job pods use to reach hive)")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return errors.New("agent_sandbox.job.github_read_proxy.endpoint must be an https://host:port URL")
	}
	if u.Scheme != "https" {
		return errors.New("agent_sandbox.job.github_read_proxy.endpoint must use https:// (the per-kick key must not cross the pod network in cleartext; hive serves TLS with a certificate from its proxy CA)")
	}
	if u.User != nil {
		return errors.New("agent_sandbox.job.github_read_proxy.endpoint must not carry credentials; hive adds a per-kick key itself")
	}
	return nil
}
