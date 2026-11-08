// Package runs is the contained-run service of the Crux work services (S2
// in hivecommons/crux#114): run a command the caller does not trust in a
// sandbox with no network and no credentials, bounded in time and output,
// and report the real exit code — or, when the exit is not knowable, say
// so. Records live in runstate (S7) so a caller that lost its connection
// can ask again by ID.
//
// sandbox.Launcher is the execution seam; Backend pairs a launcher with
// what it can enforce. A request for a limit the selected backend cannot
// enforce is refused with an *UnsupportedError, never run weakened.
package runs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"time"

	"github.com/hivecommons/hive/crux/checkout"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/kubejob"
)

// Backend names, Config.Backend.
const (
	BackendPodman = "podman"
	BackendJob    = "job"
)

// Limits and defaults.
const (
	DefaultTimeout        = 10 * time.Minute
	DefaultMaxTimeout     = 2 * time.Hour
	DefaultMaxOutputBytes = 1 << 20
	// DefaultMaxOutputCap bounds what a caller may ask for; the full output
	// is kept as an artifact regardless, so the cap protects the response
	// and the record, not the evidence.
	DefaultMaxOutputCap = 64 << 20
	maxConfigBytes      = 1 << 20
	// TeardownTimeout bounds the container removal after a cancel or
	// timeout, run on its own context because the run's is already dead.
	TeardownTimeout = 30 * time.Second
)

// ErrConfig wraps every configuration error.
var ErrConfig = errors.New("runs: invalid configuration")

// Config is the operator's choice of backend and bounds, loaded by
// LoadConfig from the JSON file `crux serve --runs-config` names.
// Selection is configuration, not per request.
type Config struct {
	// Backend is BackendPodman (default) or BackendJob.
	Backend string `json:"backend,omitempty"`
	// Image is the container image every run uses. Required.
	Image string `json:"image"`
	// PodmanBinary overrides the podman executable (tests).
	PodmanBinary string `json:"podman_binary,omitempty"`
	// Job holds the Kubernetes Job settings when Backend is BackendJob;
	// the same shape as the agent sandbox's job block.
	Job *config.SandboxJobConfig `json:"job,omitempty"`
	// JobNetwork is how a Job's pod reaches the network: "none" (default)
	// has the launcher create a per-run deny-all NetworkPolicy before the
	// Job and refuse the run if it cannot; "host" runs the pod with
	// hostNetwork; "default" leaves the pod on the cluster network. Only
	// "none" lets the backend claim the network "none" capability on its
	// own — and only as far as the cluster's CNI enforces NetworkPolicy.
	JobNetwork string `json:"job_network,omitempty"`
	// JobNetworkPolicy is the operator's assertion that a NetworkPolicy of
	// their own in the Job namespace already denies egress for pods
	// labelled app.kubernetes.io/managed-by=hive-kubejob. It lets a
	// job_network of "host" or "default" still claim network "none".
	JobNetworkPolicy bool `json:"job_network_policy,omitempty"`
	// NetworkRestricted is the operator's assertion that a container
	// network named "restricted" exists on the podman host; without it a
	// request for network "restricted" is refused.
	NetworkRestricted bool `json:"network_restricted,omitempty"`
	// RepoAllowlist is the set of repository URL prefixes the ad-hoc
	// worktree form may clone. Empty refuses every worktree request.
	RepoAllowlist []string `json:"repo_allowlist,omitempty"`
	// WorkspaceRoots is the set of host directories a plain workspace_path
	// may lie under. Empty refuses every workspace_path request.
	WorkspaceRoots []string `json:"workspace_roots,omitempty"`
	// DefaultTimeoutMs and MaxTimeoutMs bound timeout_ms; zero takes the
	// package defaults.
	DefaultTimeoutMs int64 `json:"default_timeout_ms,omitempty"`
	MaxTimeoutMs     int64 `json:"max_timeout_ms,omitempty"`
	// DefaultMaxOutputBytes and MaxOutputBytesCap bound max_output_bytes;
	// zero takes the package defaults.
	DefaultMaxOutputBytes int64 `json:"default_max_output_bytes,omitempty"`
	MaxOutputBytesCap     int64 `json:"max_output_bytes_cap,omitempty"`
}

// LoadConfig reads and validates the JSON configuration at path.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	defer f.Close()
	return loadConfig(f)
}

func loadConfig(r io.Reader) (Config, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxConfigBytes+1))
	if err != nil || len(data) > maxConfigBytes {
		return Config{}, fmt.Errorf("%w: configuration size", ErrConfig)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var cfg Config
	if err := d.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	cfg.Defaults()
	return cfg, cfg.Validate()
}

// Defaults fills the zero fields.
func (c *Config) Defaults() {
	if c.Backend == "" {
		c.Backend = BackendPodman
	}
	if c.Backend == BackendJob && c.JobNetwork == "" {
		c.JobNetwork = kubejob.NetworkNone
	}
	if c.DefaultTimeoutMs <= 0 {
		c.DefaultTimeoutMs = DefaultTimeout.Milliseconds()
	}
	if c.MaxTimeoutMs <= 0 {
		c.MaxTimeoutMs = DefaultMaxTimeout.Milliseconds()
	}
	if c.DefaultMaxOutputBytes <= 0 {
		c.DefaultMaxOutputBytes = DefaultMaxOutputBytes
	}
	if c.MaxOutputBytesCap <= 0 {
		c.MaxOutputBytesCap = DefaultMaxOutputCap
	}
}

// Validate reports the errors that would otherwise surface as a failed run.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Image) == "" {
		return fmt.Errorf("%w: image is required", ErrConfig)
	}
	switch c.Backend {
	case BackendPodman:
	case BackendJob:
		if c.Job == nil {
			return fmt.Errorf("%w: backend %q needs a job block", ErrConfig, BackendJob)
		}
		switch c.JobNetwork {
		case kubejob.NetworkNone, kubejob.NetworkHost, kubejob.NetworkDefault:
		default:
			return fmt.Errorf("%w: job_network must be %q, %q or %q", ErrConfig, kubejob.NetworkNone, kubejob.NetworkHost, kubejob.NetworkDefault)
		}
	default:
		return fmt.Errorf("%w: backend must be %q or %q", ErrConfig, BackendPodman, BackendJob)
	}
	if c.DefaultTimeoutMs > c.MaxTimeoutMs {
		return fmt.Errorf("%w: default_timeout_ms exceeds max_timeout_ms", ErrConfig)
	}
	if c.DefaultMaxOutputBytes > c.MaxOutputBytesCap {
		return fmt.Errorf("%w: default_max_output_bytes exceeds max_output_bytes_cap", ErrConfig)
	}
	for _, root := range c.WorkspaceRoots {
		if !strings.HasPrefix(root, "/") {
			return fmt.Errorf("%w: workspace_roots entries must be absolute: %q", ErrConfig, root)
		}
	}
	// An allowlist entry is compared to the caller's URL as a string, so it
	// must be canonical for the same reason the request must be (the
	// checkout service applies the same rule to its allowlist).
	for _, repo := range c.RepoAllowlist {
		if err := checkout.ValidateRepoURL(strings.TrimSpace(repo)); err != nil {
			return fmt.Errorf("%w: repo_allowlist entry %q: %v", ErrConfig, repo, err)
		}
	}
	return nil
}
