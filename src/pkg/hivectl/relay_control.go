package hivectl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

var (
	readRelayPIDFile = os.ReadFile
	runRelayCommand  = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	signalRelayPID = syscall.Kill
)

// RelaySwitchResult describes whether a running contributor relay was found
// and signaled to reload the generated contributor.env projection.
type RelaySwitchResult struct {
	Running bool
	Target  string
}

// ContributorRelay is the existing relay PID record, shared by profile switching
// and the contributor watch view. New fields remain optional for older relays.
type ContributorRelay struct {
	PID              int    `json:"pid"`
	StartedAt        string `json:"started_at,omitempty"`
	EnvFile          string `json:"env_file,omitempty"`
	ContainerRuntime string `json:"container_runtime,omitempty"`
	ContainerName    string `json:"container_name,omitempty"`
	TmuxSession      string `json:"tmux_session,omitempty"`
	Mode             string `json:"mode,omitempty"`
	LogFile          string `json:"log_file,omitempty"`
}

type relayPIDFile = ContributorRelay

// RunningContributor reads the one existing PID record and checks liveness in
// the correct namespace (container PIDs are not host PIDs). It never starts work.
func RunningContributor(ctx context.Context, store *ProfileStore) (*ContributorRelay, error) {
	data, err := readRelayPIDFile(store.RelayPIDPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r ContributorRelay
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse contributor PID record: %w", err)
	}
	if r.ContainerName != "" {
		if r.ContainerRuntime == "" {
			r.ContainerRuntime = "docker"
		}
		if r.ContainerRuntime != "docker" && r.ContainerRuntime != "podman" {
			return nil, fmt.Errorf("unsupported contributor runtime %q", r.ContainerRuntime)
		}
		out, err := runRelayCommand(ctx, r.ContainerRuntime, "inspect", "-f", "{{.State.Running}}", r.ContainerName)
		if err != nil || strings.TrimSpace(string(out)) != "true" {
			return nil, nil
		}
	} else if r.PID <= 0 || signalRelayPID(r.PID, 0) != nil {
		return nil, nil
	}
	return &r, nil
}

// StopContributor stops only the recorded contributor, never unrelated containers.
func StopContributor(ctx context.Context, r ContributorRelay) error {
	if r.ContainerName != "" {
		if r.ContainerRuntime != "docker" && r.ContainerRuntime != "podman" {
			return fmt.Errorf("unsupported contributor runtime")
		}
		out, err := runRelayCommand(ctx, r.ContainerRuntime, "stop", r.ContainerName)
		if err != nil {
			return fmt.Errorf("stop contributor: %w: %s", err, out)
		}
		return nil
	}
	if r.PID <= 0 {
		return fmt.Errorf("invalid contributor PID")
	}
	return signalRelayPID(r.PID, syscall.SIGTERM)
}

// SignalRunningRelay asks the relay recorded by contributor-relay.pid to
// reload its projection. Native relays get SIGUSR1 directly; container-mode
// relays are signaled through docker/podman using the recorded container name.
// A missing or stale pid file reports Running=false without error.
func SignalRunningRelay(ctx context.Context, store *ProfileStore) (RelaySwitchResult, error) {
	data, err := readRelayPIDFile(store.RelayPIDPath())
	if errors.Is(err, os.ErrNotExist) {
		return RelaySwitchResult{}, nil
	}
	if err != nil {
		return RelaySwitchResult{}, fmt.Errorf("read relay pid file %s: %w", store.RelayPIDPath(), err)
	}
	var pidFile relayPIDFile
	if err := json.Unmarshal(data, &pidFile); err != nil {
		return RelaySwitchResult{}, fmt.Errorf("parse relay pid file %s: %w", store.RelayPIDPath(), err)
	}
	if pidFile.ContainerName != "" {
		runtime := strings.TrimSpace(pidFile.ContainerRuntime)
		if runtime == "" {
			runtime = "docker"
		}
		if out, err := runRelayCommand(ctx, runtime, "kill", "--signal", "USR1", pidFile.ContainerName); err != nil {
			if len(out) > 0 {
				return RelaySwitchResult{}, fmt.Errorf("signal relay container %s with %s: %w: %s", pidFile.ContainerName, runtime, err, strings.TrimSpace(string(out)))
			}
			return RelaySwitchResult{}, fmt.Errorf("signal relay container %s with %s: %w", pidFile.ContainerName, runtime, err)
		}
		return RelaySwitchResult{Running: true, Target: fmt.Sprintf("%s container %s", runtime, pidFile.ContainerName)}, nil
	}
	if pidFile.PID <= 0 {
		return RelaySwitchResult{}, fmt.Errorf("relay pid file %s has invalid pid %s", store.RelayPIDPath(), strconv.Itoa(pidFile.PID))
	}
	if err := signalRelayPID(pidFile.PID, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return RelaySwitchResult{}, nil
		}
		return RelaySwitchResult{}, fmt.Errorf("check relay pid %d from %s: %w", pidFile.PID, store.RelayPIDPath(), err)
	}
	if err := signalRelayPID(pidFile.PID, syscall.SIGUSR1); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return RelaySwitchResult{}, nil
		}
		return RelaySwitchResult{}, fmt.Errorf("signal relay pid %d from %s: %w", pidFile.PID, store.RelayPIDPath(), err)
	}
	return RelaySwitchResult{Running: true, Target: fmt.Sprintf("pid %d", pidFile.PID)}, nil
}
