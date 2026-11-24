package config

import (
	"fmt"
	"strings"
)

// SandboxSetupConfig is the workspace setup step a Job-runtime sandbox kick
// runs in its main container after the node preflight and before the agent
// (hivecommons/hive#19574). It is for work every run needs and the image does
// not carry, such as installing a pinned toolchain from a package mirror with
// a script in the repo, so that it no longer depends on the agent reading the
// repo's instructions and doing it.
//
// The step runs in the Job's own image and environment, with the checked-out
// workspace as its working directory, exactly where the agent then runs:
//
//   - Commands run first. A failure or timeout ends the kick with the reason
//     setup_failed and no node retry: the cause is the script or the mirror,
//     not the node.
//   - HealthCommands run next. A failure means the node cannot run the
//     installed stack: the kick ends as node_unhealthy and hive retries it once
//     on another node after sandbox.preflight's retry_delay_s, the same path a
//     failed preflight takes. The retry runs the whole step again on the same
//     workspace, so setup commands must be safe to repeat.
//
// Like sandbox.preflight, the commands come from this config only, never from a
// kick message. They are part of the Job manifest and their output lands in the
// pod log and the kick log, so a command must not carry a credential on its
// command line.
//
// Setup is off unless Commands or HealthCommands is non-empty.
type SandboxSetupConfig struct {
	Commands       []string `yaml:"commands,omitempty" json:"commands,omitempty"`
	HealthCommands []string `yaml:"health_commands,omitempty" json:"health_commands,omitempty"`
	// TimeoutS bounds each command. Zero uses DefaultSandboxSetupTimeoutS.
	TimeoutS int `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	// TotalTimeoutS bounds both lists together. Zero uses
	// DefaultSandboxSetupTotalTimeoutS.
	TotalTimeoutS int `yaml:"total_timeout_s,omitempty" json:"total_timeout_s,omitempty"`
}

// Defaults and bounds for sandbox.setup. An install step is slower than a
// health probe, so the defaults are longer than preflight's; the list bounds
// are preflight's.
const (
	DefaultSandboxSetupTimeoutS      = 900
	DefaultSandboxSetupTotalTimeoutS = 1800
	MaxSandboxSetupCommands          = MaxSandboxPreflightCommands
	MaxSandboxSetupCommandBytes      = MaxSandboxPreflightCommandBytes
)

// Enabled reports whether the step has anything to run.
func (s SandboxSetupConfig) Enabled() bool {
	return len(s.Commands) > 0 || len(s.HealthCommands) > 0
}

// SandboxSetup returns the effective setup for the agent: the per-agent block
// when present (empty lists disable it for that agent), else the global one.
// Timeouts left at zero fall back to the global block, then to the defaults.
func (a *AgentConfig) SandboxSetup(global AgentSandboxConfig) SandboxSetupConfig {
	var g SandboxSetupConfig
	if global.Setup != nil {
		g = *global.Setup
	}
	out := g
	if a != nil && a.Sandbox != nil && a.Sandbox.Setup != nil {
		out = *a.Sandbox.Setup
		if out.TimeoutS == 0 {
			out.TimeoutS = g.TimeoutS
		}
		if out.TotalTimeoutS == 0 {
			out.TotalTimeoutS = g.TotalTimeoutS
		}
	}
	out.Commands = append([]string(nil), out.Commands...)
	out.HealthCommands = append([]string(nil), out.HealthCommands...)
	if out.TimeoutS == 0 {
		out.TimeoutS = DefaultSandboxSetupTimeoutS
	}
	if out.TotalTimeoutS == 0 {
		out.TotalTimeoutS = DefaultSandboxSetupTotalTimeoutS
	}
	return out
}

// ValidateSandboxSetup rejects a setup that could not run: an empty or
// oversized command, negative timeouts, an unbounded list. A nil block is
// valid (setup off, or inherited).
func ValidateSandboxSetup(s *SandboxSetupConfig) error {
	if s == nil {
		return nil
	}
	for _, list := range []struct {
		name string
		cmds []string
	}{{"commands", s.Commands}, {"health_commands", s.HealthCommands}} {
		if len(list.cmds) > MaxSandboxSetupCommands {
			return fmt.Errorf("setup.%s takes at most %d commands, got %d", list.name, MaxSandboxSetupCommands, len(list.cmds))
		}
		for i, c := range list.cmds {
			switch {
			case strings.TrimSpace(c) == "":
				return fmt.Errorf("setup.%s[%d] must not be empty", list.name, i)
			case len(c) > MaxSandboxSetupCommandBytes:
				return fmt.Errorf("setup.%s[%d] is longer than %d bytes", list.name, i, MaxSandboxSetupCommandBytes)
			case strings.ContainsRune(c, 0):
				return fmt.Errorf("setup.%s[%d] contains a NUL byte", list.name, i)
			}
		}
	}
	for _, f := range []struct {
		name string
		v    int
	}{{"timeout_s", s.TimeoutS}, {"total_timeout_s", s.TotalTimeoutS}} {
		if f.v < 0 {
			return fmt.Errorf("setup.%s must not be negative, got %d", f.name, f.v)
		}
	}
	return nil
}
