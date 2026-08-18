// Package sandbox launches agent work in credential-free rootless Podman sandboxes.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

type Launcher interface {
	Run(ctx context.Context, spec LaunchSpec) (Result, error)
}

type PodmanLauncher struct{ Binary string }

func (l PodmanLauncher) Run(ctx context.Context, spec LaunchSpec) (Result, error) {
	bin := l.Binary
	if bin == "" {
		bin = PodmanBinary
	}
	if _, err := exec.LookPath(bin); err != nil {
		return Result{}, fmt.Errorf("sandbox: podman is required but was not found in PATH: %w", err)
	}
	args, err := PodmanArgs(spec)
	if err != nil {
		return Result{}, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = SanitizedEnv(os.Environ(), spec.EnvAllowlist, spec.Env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

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
	args = append(args, "--userns=keep-id")
	args = append(args, "-v", filepath.Clean(spec.Workspace)+":"+mount+":Z")
	args = append(args, "--workdir", workdir)
	for k, v := range spec.Env {
		if !isCredentialName(k) {
			args = append(args, "--env", k+"="+v)
		}
	}
	args = append(args, spec.Image)
	args = append(args, spec.Command...)
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
