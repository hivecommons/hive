//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSystemdDaemonUnitIsUserPersistentAndCrashRecovering(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state with % and spaces")
	spec, err := newTestDaemonServiceSpec(stateDir, "/home/operator/Hive Builds/hive", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	unit, err := systemdDaemonUnit(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"WantedBy=default.target", "Restart=always", "RestartSec=5s", "StartLimitIntervalSec=0",
		"KillMode=control-group", "TimeoutStopSec=30s", "NoNewPrivileges=true", "--log-file",
		"--expected-hive-commit \"" + testDaemonHiveCommit + "\"",
		"--expected-executable-sha256 \"" + testDaemonDigest() + "\"",
		`ExecStart="/home/operator/Hive Builds/hive" daemon --state-dir "`,
	} {
		if !strings.Contains(unit, required) {
			t.Fatalf("systemd unit omitted %q:\n%s", required, unit)
		}
	}
	if strings.Contains(unit, "User=root") || strings.Contains(unit, "WantedBy=multi-user.target") {
		t.Fatalf("systemd unit unexpectedly requests system/root authority:\n%s", unit)
	}
	if !strings.Contains(unit, "state with %% and spaces") {
		t.Fatalf("systemd specifier was not escaped:\n%s", unit)
	}
}

func TestSystemdServiceManagerInstallStartInspectAndUninstall(t *testing.T) {
	stateDir := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "config")
	spec, err := newTestDaemonServiceSpec(stateDir, "/home/operator/bin/hive", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	loaded, enabled, active := false, false, false
	lingerEnabled := false
	var loginctlCalls []string
	runner := func(_ context.Context, arguments ...string) ([]byte, error) {
		switch arguments[0] {
		case "daemon-reload":
			loaded = true
		case "enable":
			enabled = true
		case "start":
			active = true
		case "show":
			loadState, unitState, activeState := "not-found", "disabled", "inactive"
			if loaded {
				loadState = "loaded"
			}
			if enabled {
				unitState = "enabled"
			}
			if active {
				activeState = "active"
			}
			fragmentPath := ""
			if loaded {
				fragmentPath = filepath.Join(configDir, "systemd", "user", spec.Name)
			}
			return []byte("LoadState=" + loadState + "\nUnitFileState=" + unitState + "\nActiveState=" + activeState + "\nFragmentPath=" + fragmentPath + "\nDropInPaths=\n"), nil
		case "disable":
			enabled, active = false, false
		}
		return nil, nil
	}
	loginctlRunner := func(_ context.Context, arguments ...string) ([]byte, error) {
		loginctlCalls = append(loginctlCalls, strings.Join(arguments, " "))
		switch arguments[0] {
		case "show-user":
			if got := strings.Join(arguments, " "); got != "show-user 4242 --property=Linger --value" {
				t.Fatalf("loginctl inspection was not bound to the exact numeric UID: %q", got)
			}
			if lingerEnabled {
				return []byte("yes\n"), nil
			}
			return []byte("no\n"), nil
		case "enable-linger":
			if got := strings.Join(arguments, " "); got != "enable-linger 4242" {
				t.Fatalf("loginctl enable was not bound to the exact numeric UID: %q", got)
			}
			lingerEnabled = true
			return nil, nil
		default:
			t.Fatalf("unexpected loginctl invocation: %v", arguments)
			return nil, nil
		}
	}
	manager := &systemdDaemonServiceManager{
		run: runner, runLoginctl: loginctlRunner,
		configDir:  func() (string, error) { return configDir, nil },
		currentUID: func() int { return 4242 },
	}
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(loginctlCalls, "|"), "show-user 4242 --property=Linger --value|enable-linger 4242|show-user 4242 --property=Linger --value"; got != want {
		t.Fatalf("install did not enable and recheck linger exactly: got %q want %q", got, want)
	}
	if err := manager.Start(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Inspect(context.Background(), spec)
	if err != nil || !status.Installed || !status.Enabled || !status.Active || !status.RestartOnFailure || !status.RebootPersistent || !status.LoginPersistent || status.HiveCommit != spec.HiveCommit || status.ExecutableSHA256 != spec.ExecutableSHA256 {
		t.Fatalf("unexpected service status: %+v err=%v", status, err)
	}
	definition := filepath.Join(configDir, "systemd", "user", spec.Name)
	if info, err := os.Stat(definition); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("user unit was not installed: %v", err)
	}
	if err := os.WriteFile(definition, []byte("[Service]\nExecStart=/tmp/foreign\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Inspect(context.Background(), spec); err == nil {
		t.Fatal("drifted systemd unit was reported as production-ready")
	}
	if !lingerEnabled {
		t.Fatal("test precondition failed: linger was not enabled")
	}
	if err := manager.Uninstall(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if !lingerEnabled {
		t.Fatal("uninstall disabled account-wide login linger")
	}
	if _, err := os.Stat(definition); !os.IsNotExist(err) {
		t.Fatalf("user unit survived uninstall: %v", err)
	}
}

func TestSystemdServiceInstallPreservesAlreadyEnabledLinger(t *testing.T) {
	stateDir := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "config")
	spec, err := newTestDaemonServiceSpec(stateDir, "/home/operator/bin/hive", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	showCalls := 0
	manager := &systemdDaemonServiceManager{
		run: func(_ context.Context, _ ...string) ([]byte, error) { return nil, nil },
		runLoginctl: func(_ context.Context, arguments ...string) ([]byte, error) {
			if arguments[0] != "show-user" {
				t.Fatalf("already-enabled linger triggered a mutation: %v", arguments)
			}
			showCalls++
			return []byte("yes\n"), nil
		},
		configDir:  func() (string, error) { return configDir, nil },
		currentUID: func() int { return 1001 },
	}
	if err := manager.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if showCalls != 1 {
		t.Fatalf("already-enabled linger was inspected %d times during install, want 1", showCalls)
	}
}

func TestSystemdServiceStartFailsWhenEnabledLingerDoesNotRecheck(t *testing.T) {
	stateDir := t.TempDir()
	spec, err := newTestDaemonServiceSpec(stateDir, "/home/operator/bin/hive", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	systemctlCalled := false
	showCalls, enableCalls := 0, 0
	manager := &systemdDaemonServiceManager{
		run: func(_ context.Context, _ ...string) ([]byte, error) {
			systemctlCalled = true
			return nil, nil
		},
		runLoginctl: func(_ context.Context, arguments ...string) ([]byte, error) {
			switch arguments[0] {
			case "show-user":
				showCalls++
				return []byte("no\n"), nil
			case "enable-linger":
				enableCalls++
				return nil, nil
			default:
				t.Fatalf("unexpected loginctl invocation: %v", arguments)
				return nil, nil
			}
		},
		currentUID: func() int { return 1002 },
	}
	err = manager.Start(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "remained disabled") {
		t.Fatalf("start did not fail closed on a failed linger recheck: %v", err)
	}
	if systemctlCalled {
		t.Fatal("systemctl start ran despite failed linger persistence verification")
	}
	if showCalls != 2 || enableCalls != 1 {
		t.Fatalf("unexpected linger verification calls: show=%d enable=%d", showCalls, enableCalls)
	}
}

func TestSystemdServiceInspectFailsClosedWhenLoginctlIsMissing(t *testing.T) {
	stateDir := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "config")
	spec, err := newTestDaemonServiceSpec(stateDir, "/home/operator/bin/hive", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	unit, err := systemdDaemonUnit(spec)
	if err != nil {
		t.Fatal(err)
	}
	definition := filepath.Join(configDir, "systemd", "user", spec.Name)
	if err := os.MkdirAll(filepath.Dir(definition), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(definition, []byte(unit), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &systemdDaemonServiceManager{
		run: func(_ context.Context, arguments ...string) ([]byte, error) {
			if arguments[0] != "show" {
				t.Fatalf("unexpected systemctl invocation: %v", arguments)
			}
			return []byte("LoadState=loaded\nUnitFileState=enabled\nActiveState=active\nFragmentPath=" + definition + "\nDropInPaths=\n"), nil
		},
		runLoginctl: func(_ context.Context, _ ...string) ([]byte, error) {
			return nil, errors.New("exec: loginctl: executable file not found")
		},
		configDir:  func() (string, error) { return configDir, nil },
		currentUID: func() int { return 1003 },
	}
	status, err := manager.Inspect(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "inspect systemd login linger for numeric UID 1003") {
		t.Fatalf("missing loginctl did not fail inspection closed: status=%+v err=%v", status, err)
	}
	if status.RebootPersistent {
		t.Fatalf("missing loginctl was reported as reboot-persistent: %+v", status)
	}
}

func TestSystemdServiceInstallFailsBeforeMutationWhenLoginctlIsMissing(t *testing.T) {
	stateDir := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "config")
	spec, err := newTestDaemonServiceSpec(stateDir, "/home/operator/bin/hive", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	systemctlCalled := false
	manager := &systemdDaemonServiceManager{
		run: func(_ context.Context, _ ...string) ([]byte, error) {
			systemctlCalled = true
			return nil, nil
		},
		runLoginctl: func(_ context.Context, _ ...string) ([]byte, error) {
			return nil, errors.New("exec: loginctl: executable file not found")
		},
		configDir:  func() (string, error) { return configDir, nil },
		currentUID: func() int { return 1005 },
	}
	err = manager.Install(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "inspect systemd login linger for numeric UID 1005") {
		t.Fatalf("missing loginctl did not fail install closed: %v", err)
	}
	if systemctlCalled {
		t.Fatal("systemctl was mutated before login linger could be verified")
	}
	definition := filepath.Join(configDir, "systemd", "user", spec.Name)
	if _, statErr := os.Stat(definition); !os.IsNotExist(statErr) {
		t.Fatalf("service definition was written despite missing loginctl: %v", statErr)
	}
}

func TestSystemdServiceInspectRequiresExactLingerYes(t *testing.T) {
	stateDir := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "config")
	spec, err := newTestDaemonServiceSpec(stateDir, "/home/operator/bin/hive", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	unit, err := systemdDaemonUnit(spec)
	if err != nil {
		t.Fatal(err)
	}
	definition := filepath.Join(configDir, "systemd", "user", spec.Name)
	if err := os.MkdirAll(filepath.Dir(definition), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(definition, []byte(unit), 0o600); err != nil {
		t.Fatal(err)
	}
	lingerResponse := "no\n"
	manager := &systemdDaemonServiceManager{
		run: func(_ context.Context, _ ...string) ([]byte, error) {
			return []byte("LoadState=loaded\nUnitFileState=enabled\nActiveState=active\nFragmentPath=" + definition + "\nDropInPaths=\n"), nil
		},
		runLoginctl: func(_ context.Context, _ ...string) ([]byte, error) {
			return []byte(lingerResponse), nil
		},
		configDir:  func() (string, error) { return configDir, nil },
		currentUID: func() int { return 1004 },
	}
	status, err := manager.Inspect(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if status.RebootPersistent {
		t.Fatalf("Linger=no was reported as reboot-persistent: %+v", status)
	}
	lingerResponse = "enabled\n"
	if _, err := manager.Inspect(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "expected exact yes/no response") {
		t.Fatalf("unexpected linger output did not fail closed: %v", err)
	}
}
