// Package sandbox launches rootless Podman work with credential-free Git
// workspaces and an optional opaque selected-provider capability for Cline.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux/cline"
	"github.com/hivecommons/hive/crux/posture"
)

const (
	DefaultWorkspaceMount = "/workspace"
	PodmanBinary          = "podman"
	NetworkModeNone       = "none"
	NetworkModeRestricted = "restricted"
)

var credentialEnvNames = map[string]struct{}{
	"GITHUB_TOKEN": {}, "GH_TOKEN": {}, "HIVE_GITHUB_TOKEN": {}, "COPILOT_GITHUB_TOKEN": {},
	"GIT_ASKPASS": {}, "SSH_ASKPASS": {},
	"HIVE_CLINE_PROVIDERS_B64": {},
}

type LaunchSpec struct {
	Image          string
	Name           string
	Workspace      string
	WorkspaceMount string
	WorkDir        string
	Command        []string
	Env            map[string]string
	EnvAllowlist   []string
	NetworkMode    string
	NetworkNone    bool
	ReadOnly       bool
	// PidsLimit caps processes in the container; zero means DefaultPidsLimit.
	PidsLimit     int
	ClineProvider *cline.ProviderFile
	// Relays asks the launcher to supply hive's sandbox relays (RelayNames)
	// on PATH inside the sandbox (#18512). Set for agent kicks only.
	Relays bool
	// RelayHostDir is the host directory the Podman launcher bind-mounts
	// read-only at RelayMountPath. PodmanLauncher.Run stages it when Relays
	// is set; PodmanArgs mounts it and wraps the command when it is non-empty.
	RelayHostDir string
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// Node is the node the run's pod was scheduled on, when the launcher can
	// tell (the Job runtime); empty otherwise.
	Node string
	// FailReason is set when the run failed for a reason that is the
	// infrastructure's, not the agent's: FailReasonNodeUnhealthy or
	// FailReasonWaitingForNode. Empty for every other outcome.
	FailReason string
}

type Launcher interface {
	Run(ctx context.Context, spec LaunchSpec) (Result, error)
}

type PodmanLauncher struct{ Binary string }

func (l PodmanLauncher) Run(ctx context.Context, spec LaunchSpec) (res Result, runErr error) {
	if spec.ClineProvider != nil {
		defer func() {
			res.Stdout = spec.ClineProvider.Redact(res.Stdout)
			res.Stderr = spec.ClineProvider.Redact(res.Stderr)
			if err := spec.ClineProvider.Close(); err != nil {
				runErr = errors.Join(runErr, err)
			}
		}()
		spec.Name = "hive-cline-" + spec.ClineProvider.ID()
	}
	bin := l.Binary
	if bin == "" {
		bin = PodmanBinary
	}
	if _, err := exec.LookPath(bin); err != nil {
		return Result{}, fmt.Errorf("sandbox: podman is required but was not found in PATH: %w", err)
	}
	if spec.Relays && spec.RelayHostDir == "" {
		dir, cleanup, err := StageRelays()
		if err != nil {
			return Result{}, err
		}
		defer cleanup()
		spec.RelayHostDir = dir
	}
	args, err := PodmanArgs(spec)
	if err != nil {
		return Result{}, err
	}
	if spec.ClineProvider != nil {
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), clineContainerCleanupTimeout)
			defer cancel()
			cleanup := exec.CommandContext(cleanupCtx, bin, "rm", "--force", "--ignore", spec.Name)
			cleanup.Env = spec.ClineProvider.SanitizeEnv(SanitizedEnv(os.Environ(), spec.EnvAllowlist, spec.Env))
			if err := cleanup.Run(); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("sandbox: Cline container cleanup failed; remove %s before retrying", spec.Name))
				res.ExitCode = -1
			}
		}()
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = SanitizedEnv(os.Environ(), spec.EnvAllowlist, spec.Env)
	if spec.ClineProvider != nil {
		cmd.Env = spec.ClineProvider.SanitizeEnv(cmd.Env)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	res = Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

// DefaultPidsLimit bounds forks inside a Podman sandbox (#19457).
const DefaultPidsLimit = 4096

func PodmanArgs(spec LaunchSpec) ([]string, error) {
	if strings.TrimSpace(spec.Image) == "" {
		return nil, errors.New("sandbox: image is required")
	}
	if strings.TrimSpace(spec.Workspace) == "" {
		return nil, errors.New("sandbox: workspace is required")
	}
	mount := spec.WorkspaceMount
	if mount == "" {
		mount = DefaultWorkspaceMount
	}
	workdir := spec.WorkDir
	if workdir == "" {
		workdir = mount
	}
	args := []string{"run", "--rm"}
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	networkMode := strings.TrimSpace(spec.NetworkMode)
	if networkMode == "" && spec.NetworkNone {
		networkMode = NetworkModeNone
	}
	if networkMode != "" {
		args = append(args, "--network="+networkMode)
	}
	if spec.ReadOnly {
		args = append(args, "--read-only")
	}
	args = append(args, "--userns=keep-id", "--cap-drop=all", "--security-opt=no-new-privileges")
	pids := spec.PidsLimit
	if pids <= 0 {
		pids = DefaultPidsLimit
	}
	args = append(args, "--pids-limit="+strconv.Itoa(pids))
	if posture.RequireHardened(os.Getenv) {
		// Not a secret, so the value may sit on the command line; the
		// sandbox entry point fails the kick on any containment gap.
		args = append(args, "--env", posture.RequireHardenedEnv+"=1")
	}
	args = append(args, "-v", filepath.Clean(spec.Workspace)+":"+mount+":Z")
	if spec.ClineProvider != nil {
		extra, err := clineProviderArgs(spec, mount, workdir)
		if err != nil {
			return nil, err
		}
		args = append(args, extra...)
	}
	command := spec.Command
	if spec.RelayHostDir != "" {
		args = append(args, "-v", filepath.Clean(spec.RelayHostDir)+":"+RelayMountPath+":ro,Z")
		command = RelayCommand(command)
	}
	args = append(args, "--workdir", workdir)
	// Name only: podman takes the value from its own environment, which Run
	// sets from the same spec.Env. A value on the command line would be
	// readable by every user on the host through /proc/<pid>/cmdline, and
	// some values (the review spool key, #14986) are per-run secrets.
	for k := range spec.Env {
		if !isCredentialName(k) && (spec.ClineProvider == nil || k != "HOME") {
			args = append(args, "--env", k)
		}
	}
	args = append(args, spec.Image)
	args = append(args, command...)
	return args, nil
}

func SanitizedEnv(base []string, allowlist []string, explicit map[string]string) []string {
	allowed := map[string]struct{}{}
	for _, key := range allowlist {
		if !isCredentialName(key) {
			allowed[key] = struct{}{}
		}
	}
	out := make([]string, 0, len(allowed)+len(explicit))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || isCredentialName(key) {
			continue
		}
		if _, ok := allowed[key]; ok {
			out = append(out, entry)
		}
	}
	for key, value := range explicit {
		if !isCredentialName(key) {
			out = append(out, key+"="+value)
		}
	}
	return out
}

func Available() bool { _, err := exec.LookPath(PodmanBinary); return err == nil }

func isCredentialName(name string) bool {
	_, ok := credentialEnvNames[name]
	return ok || strings.Contains(name, "TOKEN") || strings.Contains(name, "SECRET")
}

// IsCredentialName reports whether an environment variable name is one the
// sandbox refuses to pass into a workload. Exported so other launchers (the
// Kubernetes Job launcher in pkg/kubejob) apply exactly the same rule rather
// than a copy that can drift.
func IsCredentialName(name string) bool { return isCredentialName(name) }
