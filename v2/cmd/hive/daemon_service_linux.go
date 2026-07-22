//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type systemdDaemonServiceManager struct {
	run         func(context.Context, ...string) ([]byte, error)
	runLoginctl func(context.Context, ...string) ([]byte, error)
	configDir   func() (string, error)
	currentUID  func() int
}

func newPlatformDaemonServiceManager() daemonServiceManager {
	return &systemdDaemonServiceManager{
		run:         runSystemdUserCommand,
		runLoginctl: runLoginctlCommand,
		configDir:   os.UserConfigDir,
		currentUID:  os.Getuid,
	}
}

func platformDaemonServiceName(stateDir string) string {
	return "hive-integrated-" + stableDaemonServiceSuffix(stateDir) + ".service"
}

func systemdDaemonUnit(spec daemonServiceSpec) (string, error) {
	if err := validateDaemonServiceSpec(spec); err != nil {
		return "", err
	}
	execStart := strings.Join([]string{
		systemdQuoteArgument(spec.Executable),
		"daemon",
		"--state-dir",
		systemdQuoteArgument(spec.StateDir),
		"--interval",
		fmt.Sprintf("%ds", spec.IntervalSeconds),
		"--expected-hive-commit",
		systemdQuoteArgument(spec.HiveCommit),
		"--expected-executable-sha256",
		systemdQuoteArgument(spec.ExecutableSHA256),
		"--log-file",
		systemdQuoteArgument(filepath.Join(spec.StateDir, "integrated", "daemon.log")),
	}, " ")
	return `[Unit]
Description=Hive production testing scheduler (` + stableDaemonServiceSuffix(spec.StateDir) + `)
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStart=` + execStart + `
Restart=always
RestartSec=5s
KillMode=control-group
TimeoutStopSec=30s
NoNewPrivileges=true

[Install]
WantedBy=default.target
`, nil
}

func systemdQuoteArgument(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.ReplaceAll(value, "\n", "\\n")
	value = strings.ReplaceAll(value, "\r", "\\r")
	value = strings.ReplaceAll(value, "\t", "\\t")
	return `"` + value + `"`
}

func (manager *systemdDaemonServiceManager) definitionPath(spec daemonServiceSpec) (string, error) {
	configDir, err := manager.configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "systemd", "user", spec.Name), nil
}

func (manager *systemdDaemonServiceManager) Install(ctx context.Context, spec daemonServiceSpec) error {
	unit, err := systemdDaemonUnit(spec)
	if err != nil {
		return err
	}
	if err := manager.ensureLinger(ctx); err != nil {
		return err
	}
	definitionPath, err := manager.definitionPath(spec)
	if err != nil {
		return fmt.Errorf("resolve systemd user unit directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(definitionPath), 0o700); err != nil {
		return err
	}
	temporary := definitionPath + fmt.Sprintf(".%d.tmp", os.Getpid())
	if err := os.WriteFile(temporary, []byte(unit), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, definitionPath); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if output, err := manager.run(ctx, "daemon-reload"); err != nil {
		_ = os.Remove(definitionPath)
		return serviceCommandError("reload systemd user units", output, err)
	}
	if output, err := manager.run(ctx, "enable", spec.Name); err != nil {
		_, _ = manager.run(ctx, "disable", spec.Name)
		_ = os.Remove(definitionPath)
		_, _ = manager.run(ctx, "daemon-reload")
		return serviceCommandError("enable systemd user service", output, err)
	}
	return nil
}

func (manager *systemdDaemonServiceManager) Start(ctx context.Context, spec daemonServiceSpec) error {
	if err := validateDaemonServiceSpec(spec); err != nil {
		return err
	}
	if err := manager.ensureLinger(ctx); err != nil {
		return err
	}
	if output, err := manager.run(ctx, "start", spec.Name); err != nil {
		return serviceCommandError("start systemd user service", output, err)
	}
	return nil
}

func (manager *systemdDaemonServiceManager) Inspect(ctx context.Context, spec daemonServiceSpec) (daemonServiceStatus, error) {
	definitionPath, pathErr := manager.definitionPath(spec)
	status := daemonServiceStatus{
		SchemaVersion: daemonServiceSchema, Platform: "linux", Name: spec.Name,
		StartupMode: "systemd-user-default-target-with-logind-linger", DefinitionPath: definitionPath,
		HiveCommit: spec.HiveCommit, ExecutableSHA256: spec.ExecutableSHA256,
	}
	if pathErr != nil {
		return status, pathErr
	}
	properties, err := manager.properties(ctx, spec)
	if err != nil {
		return status, err
	}
	status.Installed = properties["LoadState"] == "loaded"
	status.Enabled = properties["UnitFileState"] == "enabled"
	status.Active = properties["ActiveState"] == "active" || properties["ActiveState"] == "activating"
	if !status.Installed {
		return status, nil
	}
	expectedUnit, err := systemdDaemonUnit(spec)
	if err != nil {
		return status, err
	}
	installedUnit, err := os.ReadFile(definitionPath)
	if err != nil {
		return status, fmt.Errorf("read installed systemd user service: %w", err)
	}
	if string(installedUnit) != expectedUnit || filepath.Clean(properties["FragmentPath"]) != filepath.Clean(definitionPath) || strings.TrimSpace(properties["DropInPaths"]) != "" {
		return status, fmt.Errorf("registered systemd user service drifted from Hive's exact executable and repository state binding")
	}
	status.RestartOnFailure = true
	lingerEnabled, err := manager.lingerEnabled(ctx)
	if err != nil {
		return status, err
	}
	status.RebootPersistent = lingerEnabled
	status.LoginPersistent = true
	return status, nil
}

func (manager *systemdDaemonServiceManager) uid() (string, error) {
	currentUID := manager.currentUID
	if currentUID == nil {
		currentUID = os.Getuid
	}
	uid := currentUID()
	if uid < 0 {
		return "", fmt.Errorf("resolve current numeric UID for systemd login linger: invalid UID %d", uid)
	}
	return strconv.Itoa(uid), nil
}

func (manager *systemdDaemonServiceManager) loginctl(ctx context.Context, arguments ...string) ([]byte, error) {
	runner := manager.runLoginctl
	if runner == nil {
		runner = runLoginctlCommand
	}
	return runner(ctx, arguments...)
}

func (manager *systemdDaemonServiceManager) lingerEnabled(ctx context.Context) (bool, error) {
	uid, err := manager.uid()
	if err != nil {
		return false, err
	}
	return manager.lingerEnabledForUID(ctx, uid)
}

func (manager *systemdDaemonServiceManager) lingerEnabledForUID(ctx context.Context, uid string) (bool, error) {
	output, err := manager.loginctl(ctx, "show-user", uid, "--property=Linger", "--value")
	if err != nil {
		return false, serviceCommandError("inspect systemd login linger for numeric UID "+uid, output, err)
	}
	switch strings.TrimSpace(string(output)) {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	default:
		return false, fmt.Errorf("inspect systemd login linger for numeric UID %s: expected exact yes/no response, received %q", uid, strings.TrimSpace(string(output)))
	}
}

func (manager *systemdDaemonServiceManager) ensureLinger(ctx context.Context) error {
	uid, err := manager.uid()
	if err != nil {
		return err
	}
	enabled, err := manager.lingerEnabledForUID(ctx, uid)
	if err != nil {
		return err
	}
	if enabled {
		return nil
	}
	output, err := manager.loginctl(ctx, "enable-linger", uid)
	if err != nil {
		return serviceCommandError("enable systemd login linger for numeric UID "+uid, output, err)
	}
	enabled, err = manager.lingerEnabledForUID(ctx, uid)
	if err != nil {
		return fmt.Errorf("verify systemd login linger for numeric UID %s after enable: %w", uid, err)
	}
	if !enabled {
		return fmt.Errorf("systemd login linger for numeric UID %s remained disabled after loginctl enable-linger; Hive cannot guarantee restart after reboot", uid)
	}
	return nil
}

func (manager *systemdDaemonServiceManager) properties(ctx context.Context, spec daemonServiceSpec) (map[string]string, error) {
	output, err := manager.run(ctx, "show", spec.Name, "--property=LoadState,UnitFileState,ActiveState,FragmentPath,DropInPaths")
	if err != nil {
		return nil, serviceCommandError("inspect systemd user service", output, err)
	}
	properties := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			properties[key] = value
		}
	}
	return properties, nil
}

func (manager *systemdDaemonServiceManager) Uninstall(ctx context.Context, spec daemonServiceSpec) error {
	properties, err := manager.properties(ctx, spec)
	if err != nil {
		return err
	}
	if properties["LoadState"] == "loaded" {
		if output, disableErr := manager.run(ctx, "disable", "--now", spec.Name); disableErr != nil {
			return serviceCommandError("disable systemd user service", output, disableErr)
		}
	}
	definitionPath, err := manager.definitionPath(spec)
	if err != nil {
		return err
	}
	if err := os.Remove(definitionPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if output, reloadErr := manager.run(ctx, "daemon-reload"); reloadErr != nil {
		return serviceCommandError("reload systemd user units", output, reloadErr)
	}
	_, _ = manager.run(ctx, "reset-failed", spec.Name)
	return nil
}

func runSystemdUserCommand(ctx context.Context, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, arguments...)...)
	command.Stdin = nil
	command.Env = daemonEnvironment(os.Environ())
	return command.CombinedOutput()
}

func runLoginctlCommand(ctx context.Context, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "loginctl", arguments...)
	command.Stdin = nil
	command.Env = daemonEnvironment(os.Environ())
	return command.CombinedOutput()
}
