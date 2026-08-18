package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeDaemonServiceManager struct {
	installed bool
	enabled   bool
	active    bool
	starts    int
	restarts  int
	startErr  error
	calls     []string
}

func (manager *fakeDaemonServiceManager) Install(_ context.Context, _ daemonServiceSpec) error {
	manager.calls = append(manager.calls, "install")
	manager.installed, manager.enabled = true, true
	return nil
}

func (manager *fakeDaemonServiceManager) Start(_ context.Context, _ daemonServiceSpec) error {
	manager.calls = append(manager.calls, "start")
	if manager.startErr != nil {
		return manager.startErr
	}
	manager.active = true
	manager.starts++
	return nil
}

func (manager *fakeDaemonServiceManager) Inspect(_ context.Context, spec daemonServiceSpec) (daemonServiceStatus, error) {
	manager.calls = append(manager.calls, "inspect")
	return daemonServiceStatus{
		SchemaVersion: daemonServiceSchema, Managed: true, Platform: spec.Platform, Name: spec.Name,
		Installed: manager.installed, Enabled: manager.enabled, Active: manager.active,
		RebootPersistent: true, LoginPersistent: true, RestartOnFailure: true,
		HiveCommit: spec.HiveCommit, ExecutableSHA256: spec.ExecutableSHA256,
	}, nil
}

func (manager *fakeDaemonServiceManager) Uninstall(_ context.Context, _ daemonServiceSpec) error {
	manager.calls = append(manager.calls, "uninstall")
	manager.installed, manager.enabled, manager.active = false, false, false
	return nil
}

func (manager *fakeDaemonServiceManager) forceKillAndRecover() {
	manager.active = false
	if manager.installed && manager.enabled {
		manager.active = true
		manager.restarts++
	}
}

func useFakeDaemonServices(t *testing.T, manager daemonServiceManager) {
	t.Helper()
	prior := daemonServices
	priorRegistryRoot := daemonSchedulerRegistryRoot
	daemonServices = manager
	registryRoot := filepath.Join(t.TempDir(), "scheduler-registry")
	daemonSchedulerRegistryRoot = func() string { return registryRoot }
	t.Cleanup(func() { daemonServices, daemonSchedulerRegistryRoot = prior, priorRegistryRoot })
}

func TestDaemonServiceLifecycleRecoversForcedKillAndUninstalls(t *testing.T) {
	stateDir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager := &fakeDaemonServiceManager{}
	useFakeDaemonServices(t, manager)
	spec, err := activateDaemonService(stateDir, executable, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !manager.installed || !manager.enabled || !manager.active || manager.starts != 1 {
		t.Fatalf("service did not become active: %+v", manager)
	}
	if persisted, exists, err := readDaemonServiceSpec(stateDir); err != nil || !exists || persisted.Name != spec.Name || persisted.Executable != spec.Executable {
		t.Fatalf("service descriptor was not durable: persisted=%+v exists=%t err=%v", persisted, exists, err)
	}

	// Model a hard kill, not a graceful CLI stop. The OS-owned service contract
	// is what restarts the same exact executable/state binding.
	manager.forceKillAndRecover()
	status := inspectDaemonService(stateDir, false)
	if !status.Active || !status.RestartOnFailure || !status.RebootPersistent || manager.restarts != 1 {
		t.Fatalf("forced kill was not recovered by the persistent service: status=%+v manager=%+v", status, manager)
	}

	if err := uninstallDaemonService(stateDir); err != nil {
		t.Fatal(err)
	}
	if manager.installed || manager.enabled || manager.active {
		t.Fatalf("uninstall left service authority active: %+v", manager)
	}
	if _, err := os.Stat(daemonServiceDescriptorPath(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("service descriptor survived uninstall: %v", err)
	}
	if err := uninstallDaemonService(stateDir); err != nil {
		t.Fatalf("repeated uninstall was not idempotent: %v", err)
	}
}

func TestDaemonServiceStartFailureRollsBackRegistration(t *testing.T) {
	stateDir := t.TempDir()
	executable, _ := os.Executable()
	manager := &fakeDaemonServiceManager{startErr: errors.New("start failed")}
	useFakeDaemonServices(t, manager)
	if _, err := activateDaemonService(stateDir, executable, time.Minute); err == nil {
		t.Fatal("service start failure was accepted")
	}
	if manager.installed || manager.enabled || manager.active {
		t.Fatalf("failed start left a persistent registration: %+v", manager)
	}
	if _, err := os.Stat(daemonServiceDescriptorPath(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed start left a descriptor: %v", err)
	}
}

func TestDaemonStatusPreservesDesiredRunningStateDuringCrashRestartDelay(t *testing.T) {
	stateDir := t.TempDir()
	executable, _ := os.Executable()
	manager := &fakeDaemonServiceManager{}
	useFakeDaemonServices(t, manager)
	if _, err := activateDaemonService(stateDir, executable, time.Minute); err != nil {
		t.Fatal(err)
	}
	manager.active = false // hard crash; the fake OS supervisor is in backoff
	status := readIntegratedDaemonStatus(stateDir)
	if !status.Running || status.RuntimeRunning || status.Service == nil || !status.Service.Enabled || status.Service.Active {
		t.Fatalf("status lost persistent desired state during restart delay: %+v", status)
	}
}

func TestDaemonServiceDescriptorFailsClosedOnStateDrift(t *testing.T) {
	stateDir := t.TempDir()
	integratedDir := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(integratedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"schema_version":"hive.integrated-daemon-service.v1","platform":"windows","name":"foreign-service","state_dir":"C:\\foreign","executable":"C:\\foreign\\hive.exe","interval_seconds":900}`)
	if err := os.WriteFile(daemonServiceDescriptorPath(stateDir), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readDaemonServiceSpec(stateDir); err == nil {
		t.Fatal("foreign service descriptor was accepted")
	}
	status := inspectDaemonService(stateDir, false)
	if !status.Managed || status.InspectionError == "" {
		t.Fatalf("drifted descriptor did not fail closed in status: %+v", status)
	}
	manager := &fakeDaemonServiceManager{installed: true, enabled: true, active: true}
	useFakeDaemonServices(t, manager)
	if err := uninstallDaemonService(stateDir); err != nil {
		t.Fatalf("stable-name uninstall could not recover a corrupt descriptor: %v", err)
	}
	if manager.installed || manager.enabled || manager.active {
		t.Fatalf("corrupt-descriptor recovery left service active: %+v", manager)
	}
	if _, err := os.Stat(daemonServiceDescriptorPath(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt descriptor survived recovered uninstall: %v", err)
	}
}
