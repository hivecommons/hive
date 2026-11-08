package runs

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/kubejob"
	"github.com/hivecommons/hive/crux/sandbox"
)

// Capabilities is what a backend can enforce. A request for anything it
// cannot is refused before the run starts.
type Capabilities struct {
	NetworkNone       bool
	NetworkRestricted bool
}

// Backend pairs a sandbox.Launcher with its capabilities and the teardown
// that guarantees a cancelled or timed-out run leaves nothing behind.
type Backend struct {
	Name         string
	Launcher     sandbox.Launcher
	Capabilities Capabilities
	// Teardown, when set, is run after a cancel or timeout with the launch
	// name. The podman launcher's client process dies with the context but
	// the container it started does not, so podman removes it here; the
	// Job launcher deletes its Job itself (propagationPolicy Background)
	// and leaves Teardown nil.
	Teardown func(ctx context.Context, name string) error
}

// Supports reports whether the backend can enforce network mode mode; the
// returned string names what is missing when it cannot.
func (b Backend) Supports(network string) (string, bool) {
	switch network {
	case sandbox.NetworkModeNone:
		if !b.Capabilities.NetworkNone {
			return fmt.Sprintf("backend %s cannot enforce network %q", b.Name, network), false
		}
	case sandbox.NetworkModeRestricted:
		if !b.Capabilities.NetworkRestricted {
			return fmt.Sprintf("backend %s cannot enforce network %q", b.Name, network), false
		}
	default:
		return fmt.Sprintf("unknown network mode %q", network), false
	}
	return "", true
}

// BackendFromConfig selects and builds the backend cfg names.
func BackendFromConfig(cfg Config) (*Backend, error) {
	switch cfg.Backend {
	case BackendPodman:
		bin := cfg.PodmanBinary
		if bin == "" {
			bin = sandbox.PodmanBinary
		}
		return &Backend{
			Name:         BackendPodman,
			Launcher:     sandbox.PodmanLauncher{Binary: bin},
			Capabilities: Capabilities{NetworkNone: true, NetworkRestricted: cfg.NetworkRestricted},
			Teardown:     podmanTeardown(bin),
		}, nil
	case BackendJob:
		if cfg.Job == nil {
			return nil, fmt.Errorf("%w: backend %q needs a job block", ErrConfig, BackendJob)
		}
		opts := jobOptions(*cfg.Job)
		// The default is the contained run's, not the launcher's: a Config
		// that skipped Defaults still isolates.
		opts.Network = cfg.JobNetwork
		if opts.Network == "" {
			opts.Network = kubejob.NetworkNone
		}
		if err := opts.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrConfig, err)
		}
		return &Backend{
			Name:     BackendJob,
			Launcher: &kubejob.Launcher{Options: opts},
			// "none" is claimable when the launcher isolates each run with
			// its own NetworkPolicy (job_network none) or the operator
			// asserts one of theirs does; "restricted" never is.
			Capabilities: Capabilities{NetworkNone: opts.Network == kubejob.NetworkNone || cfg.JobNetworkPolicy},
		}, nil
	}
	return nil, fmt.Errorf("%w: unknown backend %q", ErrConfig, cfg.Backend)
}

// podmanTeardown removes the named container whether or not it is still
// running; --ignore makes an already-removed container a success.
func podmanTeardown(bin string) func(context.Context, string) error {
	return func(ctx context.Context, name string) error {
		out, err := exec.CommandContext(ctx, bin, "rm", "--force", "--ignore", name).CombinedOutput()
		if err != nil {
			return fmt.Errorf("podman rm %s: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// jobOptions mirrors the agent sandbox's config-to-launcher mapping
// (agent.JobOptionsFromConfig) without importing the agent manager.
func jobOptions(job config.SandboxJobConfig) kubejob.Options {
	opts := kubejob.Options{
		WorkspaceClaim:      job.WorkspaceClaim,
		WorkspaceClaimMount: job.WorkspaceClaimMount,
		NodeSelector:        job.NodeSelector,
		ServiceAccount:      job.ServiceAccount,
		SchedulerName:       job.SchedulerName,
		EnvFromSecrets:      job.EnvFromSecrets,
		TTLSeconds:          job.TTLSeconds,
		Resources:           kubejob.Resources{Limits: job.Resources.Limits, Requests: job.Resources.Requests},
	}
	for _, t := range job.Tolerations {
		opts.Tolerations = append(opts.Tolerations, kubejob.Toleration{Key: t.Key, Operator: t.Operator, Value: t.Value, Effect: t.Effect})
	}
	for _, v := range job.Volumes {
		opts.ExtraVolumes = append(opts.ExtraVolumes, kubejob.VolumeMount{Name: v.Name, Claim: v.Claim, MountPath: v.MountPath, ReadOnly: v.ReadOnly})
	}
	return opts
}
