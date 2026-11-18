package config

import (
	"fmt"
	"strings"
)

// SandboxPreflightConfig is the node health check a Job-runtime sandbox kick
// runs in its main container before the agent starts (hivecommons/hive#18811).
// A node whose accelerator card is unusable fails the kick in seconds with
// the distinct reason node_unhealthy instead of minutes into the agent's run,
// and hive retries once on a different node.
//
// Each command is run with `sh -c` in the Job's own image, workspace and
// environment, after hive's relay check. The commands come from this config
// only, never from a kick message, exactly like sandbox.publish.verify. They
// are part of the Job manifest and their output lands in the kick transcript,
// so a command must not carry a credential on its command line.
//
// Preflight is off unless Commands is non-empty.
type SandboxPreflightConfig struct {
	Commands []string `yaml:"commands,omitempty" json:"commands,omitempty"`
	// TimeoutS bounds each command. Zero uses DefaultSandboxPreflightTimeoutS.
	TimeoutS int `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	// TotalTimeoutS bounds all commands together. Zero uses
	// DefaultSandboxPreflightTotalTimeoutS.
	TotalTimeoutS int `yaml:"total_timeout_s,omitempty" json:"total_timeout_s,omitempty"`
	// RetryDelayS is how long hive waits before re-creating the Job on
	// another node after node_unhealthy. Zero retries immediately.
	RetryDelayS int `yaml:"retry_delay_s,omitempty" json:"retry_delay_s,omitempty"`
}

// Defaults and bounds for sandbox.preflight and job.schedule_timeout_s.
const (
	DefaultSandboxPreflightTimeoutS      = 120
	DefaultSandboxPreflightTotalTimeoutS = 300
	// DefaultSandboxScheduleTimeoutS is how long a kick's pod may stay
	// Unschedulable before the kick fails with waiting_for_node.
	DefaultSandboxScheduleTimeoutS = 600

	// MaxSandboxPreflightCommands and MaxSandboxPreflightCommandBytes bound
	// operator-supplied preflight lists.
	MaxSandboxPreflightCommands     = 32
	MaxSandboxPreflightCommandBytes = 4096
)

// SandboxPreflight returns the effective preflight for the agent: the
// per-agent block when present (an empty commands list disables it for that
// agent), else the global one. Timeouts left at zero fall back to the global
// block, then to the defaults.
func (a *AgentConfig) SandboxPreflight(global AgentSandboxConfig) SandboxPreflightConfig {
	var g SandboxPreflightConfig
	if global.Preflight != nil {
		g = *global.Preflight
	}
	out := g
	if a != nil && a.Sandbox != nil && a.Sandbox.Preflight != nil {
		out = *a.Sandbox.Preflight
		if out.TimeoutS == 0 {
			out.TimeoutS = g.TimeoutS
		}
		if out.TotalTimeoutS == 0 {
			out.TotalTimeoutS = g.TotalTimeoutS
		}
		if out.RetryDelayS == 0 {
			out.RetryDelayS = g.RetryDelayS
		}
	}
	out.Commands = append([]string(nil), out.Commands...)
	if out.TimeoutS == 0 {
		out.TimeoutS = DefaultSandboxPreflightTimeoutS
	}
	if out.TotalTimeoutS == 0 {
		out.TotalTimeoutS = DefaultSandboxPreflightTotalTimeoutS
	}
	return out
}

// ValidateSandboxPreflight rejects a preflight that could not run: an empty
// or oversized command, negative timeouts, an unbounded list. A nil block is
// valid (preflight off).
func ValidateSandboxPreflight(p *SandboxPreflightConfig) error {
	if p == nil {
		return nil
	}
	if len(p.Commands) > MaxSandboxPreflightCommands {
		return fmt.Errorf("preflight.commands takes at most %d commands, got %d", MaxSandboxPreflightCommands, len(p.Commands))
	}
	for i, c := range p.Commands {
		switch {
		case strings.TrimSpace(c) == "":
			return fmt.Errorf("preflight.commands[%d] must not be empty", i)
		case len(c) > MaxSandboxPreflightCommandBytes:
			return fmt.Errorf("preflight.commands[%d] is longer than %d bytes", i, MaxSandboxPreflightCommandBytes)
		case strings.ContainsRune(c, 0):
			return fmt.Errorf("preflight.commands[%d] contains a NUL byte", i)
		}
	}
	for _, f := range []struct {
		name string
		v    int
	}{{"timeout_s", p.TimeoutS}, {"total_timeout_s", p.TotalTimeoutS}, {"retry_delay_s", p.RetryDelayS}} {
		if f.v < 0 {
			return fmt.Errorf("preflight.%s must not be negative, got %d", f.name, f.v)
		}
	}
	return nil
}

// ValidateSandboxJobScheduleTimeout rejects a negative job.schedule_timeout_s.
func ValidateSandboxJobScheduleTimeout(job *SandboxJobConfig) error {
	if job != nil && job.ScheduleTimeoutS < 0 {
		return fmt.Errorf("job.schedule_timeout_s must not be negative, got %d", job.ScheduleTimeoutS)
	}
	return nil
}
