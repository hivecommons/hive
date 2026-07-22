package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/integrated"
)

const specialistRuntimeLeaseSchema = "hive.specialist-runtime-lease.v1"

var errSpecialistRuntimeLeaseHeld = errors.New("another Hive process owns the persistent specialist runtime")

type specialistRuntimeLease struct {
	SchemaVersion  string    `json:"schema_version"`
	PID            int       `json:"pid"`
	Repository     string    `json:"repository"`
	ConfigSHA256   string    `json:"config_sha256"`
	ProviderSHA256 string    `json:"provider_sha256"`
	AcquiredAt     time.Time `json:"acquired_at"`
}

type integratedSpecialistRuntime struct {
	Manager      *agent.Manager
	WorkDir      string
	configSHA256 string
	lease        *os.File
	closeOnce    sync.Once
	closeErr     error
}

func newIntegratedSpecialistRuntime(stateDir string, durable integrated.Config) (*integratedSpecialistRuntime, error) {
	if durable.ExecutionMode == integrated.ExecutionHosted || (durable.Automation != integrated.AutomationRepairPR && durable.Automation != integrated.AutomationAutoMerge) {
		return nil, nil
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	durableStateDir, err := filepath.Abs(durable.StateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve durable specialist state path: %w", err)
	}
	if !sameSpecialistRuntimePath(stateDir, durableStateDir) {
		return nil, fmt.Errorf("specialist runtime state path mismatch: command=%s durable=%s", stateDir, durableStateDir)
	}
	if !strings.EqualFold(strings.TrimSpace(durable.Provider), "codex") {
		return nil, fmt.Errorf("persistent Hive specialists require the Codex provider, got %q", durable.Provider)
	}
	model, err := specialistModelFromProviderArgs(durable.ProviderArgs)
	if err != nil {
		return nil, err
	}
	configSHA256, err := specialistRuntimeConfigDigest(durable, stateDir, model)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(stateDir, "integrated", "specialists")
	workDir := filepath.Join(root, "agents")
	lease, err := claimSpecialistRuntimeLease(root, durable.Repository, configSHA256)
	if err != nil {
		return nil, err
	}
	keepLease := false
	defer func() {
		if !keepLease {
			releaseSpecialistRuntimeLease(lease)
		}
	}()
	agents := specialistAgentConfigs(model)
	org := strings.SplitN(strings.TrimSpace(durable.Repository), "/", 2)[0]
	manager, err := agent.NewSpecialistManager(agents, slog.New(slog.NewTextHandler(io.Discard, nil)), agent.ProjectContext{
		Org: org, Repos: []string{durable.Repository}, ACMMLevel: durable.ACMMLevel, PRsAllowed: false,
	}, agent.SpecialistManagerOptions{WorkDir: workDir, CodexCommand: durable.ProviderCommand, CodexHome: resolveIntegratedCodexHome()})
	if err != nil {
		return nil, fmt.Errorf("construct persistent Hive specialist runtime: %w", err)
	}
	providerSHA256 := manager.SpecialistProviderSHA256()
	if providerSHA256 == "" {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = manager.ShutdownSpecialists(cleanupCtx)
		cancel()
		return nil, errors.New("persistent Hive specialist runtime has no sealed provider identity")
	}
	if err := persistSpecialistRuntimeLease(lease, specialistRuntimeLease{
		SchemaVersion: specialistRuntimeLeaseSchema, PID: os.Getpid(), Repository: durable.Repository,
		ConfigSHA256: configSHA256, ProviderSHA256: providerSHA256, AcquiredAt: time.Now().UTC(),
	}); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = manager.ShutdownSpecialists(cleanupCtx)
		cancel()
		return nil, err
	}
	keepLease = true
	return &integratedSpecialistRuntime{Manager: manager, WorkDir: workDir, configSHA256: configSHA256, lease: lease}, nil
}

func resolveIntegratedCodexHome() string {
	if configured := strings.TrimSpace(os.Getenv("CODEX_HOME")); configured != "" {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func specialistAgentConfigs(model string) map[string]config.AgentConfig {
	agents := make(map[string]config.AgentConfig, 5)
	for _, role := range []agent.SpecialistRole{
		agent.SpecialistQuality,
		agent.SpecialistCIMaintainer,
		agent.SpecialistSecurity,
		agent.SpecialistArchitect,
		agent.SpecialistScanner,
	} {
		name := string(role)
		agents[name] = config.AgentConfig{
			ID: name, Backend: "codex", Model: model, Enabled: true,
			ClearOnKick: true, CLIPinned: true, Role: name, Mode: "ADVISORY", OnDemand: true,
		}
	}
	return agents
}

func (runtimeState *integratedSpecialistRuntime) matches(stateDir string, durable integrated.Config) error {
	if runtimeState == nil {
		return errors.New("persistent specialist runtime is not configured")
	}
	model, err := specialistModelFromProviderArgs(durable.ProviderArgs)
	if err != nil {
		return err
	}
	digest, err := specialistRuntimeConfigDigest(durable, stateDir, model)
	if err != nil {
		return err
	}
	if digest != runtimeState.configSHA256 {
		return errors.New("persistent specialist runtime configuration changed; restart Hive before another cycle")
	}
	return nil
}

func (runtimeState *integratedSpecialistRuntime) Close() error {
	if runtimeState == nil {
		return nil
	}
	runtimeState.closeOnce.Do(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if runtimeState.Manager != nil {
			runtimeState.closeErr = runtimeState.Manager.ShutdownSpecialists(cleanupCtx)
		}
		if runtimeState.closeErr == nil {
			releaseSpecialistRuntimeLease(runtimeState.lease)
			runtimeState.lease = nil
		}
	})
	return runtimeState.closeErr
}

func specialistModelFromProviderArgs(arguments []string) (string, error) {
	model := ""
	for index := 0; index < len(arguments); index++ {
		argument := strings.TrimSpace(arguments[index])
		var value string
		switch {
		case argument == "--model":
			index++
			if index >= len(arguments) {
				return "", errors.New("Codex --model provider argument is missing its value")
			}
			value = strings.TrimSpace(arguments[index])
		case strings.HasPrefix(argument, "--model="):
			value = strings.TrimSpace(strings.TrimPrefix(argument, "--model="))
		default:
			return "", fmt.Errorf("persistent specialists support only the Codex --model provider argument, got %q", argument)
		}
		if value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return "", errors.New("Codex specialist model must be one non-empty argument")
		}
		if model != "" {
			return "", errors.New("Codex specialist model was configured more than once")
		}
		model = value
	}
	if model == "" {
		return "", errors.New("persistent Hive specialists require one explicit Codex --model provider argument")
	}
	return model, nil
}

func specialistRuntimeConfigDigest(durable integrated.Config, stateDir, model string) (string, error) {
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(struct {
		Repository      string                   `json:"repository"`
		StateDir        string                   `json:"state_dir"`
		Provider        string                   `json:"provider"`
		ProviderCommand string                   `json:"provider_command"`
		ProviderArgs    []string                 `json:"provider_args"`
		Model           string                   `json:"model"`
		Automation      integrated.Automation    `json:"automation"`
		ExecutionMode   integrated.ExecutionMode `json:"execution_mode"`
		ACMMLevel       int                      `json:"acmm_level"`
	}{
		Repository: durable.Repository, StateDir: filepath.Clean(stateDir), Provider: strings.ToLower(strings.TrimSpace(durable.Provider)),
		ProviderCommand: filepath.Clean(durable.ProviderCommand), ProviderArgs: append([]string(nil), durable.ProviderArgs...), Model: model,
		Automation: durable.Automation, ExecutionMode: durable.ExecutionMode, ACMMLevel: durable.ACMMLevel,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func claimSpecialistRuntimeLease(root, repository, configSHA256 string) (*os.File, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(root); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("specialist runtime root is not a real directory")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(root, "runtime.lease")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open specialist runtime lease: %w", err)
	}
	locked, err := tryLockDaemonLease(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("claim specialist runtime lease: %w", err)
	}
	if !locked {
		_ = file.Close()
		return nil, errSpecialistRuntimeLeaseHeld
	}
	if err := persistSpecialistRuntimeLease(file, specialistRuntimeLease{
		SchemaVersion: specialistRuntimeLeaseSchema, PID: os.Getpid(), Repository: repository,
		ConfigSHA256: configSHA256, AcquiredAt: time.Now().UTC(),
	}); err != nil {
		releaseSpecialistRuntimeLease(file)
		return nil, err
	}
	return file, nil
}

func persistSpecialistRuntimeLease(file *os.File, lease specialistRuntimeLease) error {
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return err
	}
	if err := file.Truncate(0); err == nil {
		_, err = file.Seek(0, 0)
	}
	if err == nil {
		_, err = file.Write(append(data, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		return fmt.Errorf("persist specialist runtime lease: %w", err)
	}
	return nil
}

func releaseSpecialistRuntimeLease(file *os.File) {
	if file == nil {
		return
	}
	_ = unlockDaemonLease(file)
	_ = file.Close()
}

func sameSpecialistRuntimePath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}
