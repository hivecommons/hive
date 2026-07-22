package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestInstallerTransitionDurablyStopsAndRestartsExactOwnedSchedulers(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state-root")
	registryRoot := filepath.Join(stateRoot, "schedulers")
	ownedInstall := filepath.Join(root, "installed")
	stateDir := filepath.Join(stateRoot, "repositories", "owner--repo")
	if err := os.MkdirAll(filepath.Join(stateDir, "integrated"), 0o700); err != nil {
		t.Fatal(err)
	}
	hiveName := "hive"
	if runtime.GOOS == "windows" {
		hiveName = "hive.exe"
	}
	runtimeExecutable := filepath.Join(ownedInstall, hiveName)
	if err := os.MkdirAll(ownedInstall, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeExecutable, []byte("exact restored runtime\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec, err := newDaemonServiceSpecWithIdentity(stateDir, runtimeExecutable, 17*time.Minute, testDaemonHiveCommit, testDaemonDigest())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonServiceSpec(spec); err != nil {
		t.Fatal(err)
	}
	manager := &fakeDaemonServiceManager{installed: true, enabled: true, active: true}
	useFakeDaemonServices(t, manager)
	priorStateRoot, priorRegistryRoot, priorStart := installerTransitionStateRoot, daemonSchedulerRegistryRoot, runInstallerSchedulerStart
	installerTransitionStateRoot = func() string { return stateRoot }
	daemonSchedulerRegistryRoot = func() string { return registryRoot }
	starts := 0
	runInstallerSchedulerStart = func(_ context.Context, executable string, arguments []string) ([]byte, error) {
		starts++
		if !sameInstallerPath(executable, runtimeExecutable) || len(arguments) < 6 || arguments[0] != "start" || arguments[1] != "--state-dir" || !sameInstallerPath(arguments[2], stateDir) {
			t.Fatalf("unexpected exact restart: executable=%s arguments=%v", executable, arguments)
		}
		return []byte(`{"running":true}`), nil
	}
	t.Cleanup(func() {
		installerTransitionStateRoot, daemonSchedulerRegistryRoot, runInstallerSchedulerStart = priorStateRoot, priorRegistryRoot, priorStart
	})
	if err := writeDaemonServiceRegistry(spec); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "installer-transition.json")
	journal, err := prepareInstallerTransition(journalPath, ownedInstall)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Phase != "prepared" || len(journal.Entries) != 1 || journal.Entries[0].Phase != "stopped" || manager.installed || manager.active {
		t.Fatalf("prepare did not durably quiesce exact scheduler: journal=%+v manager=%+v", journal, manager)
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("prepare journal was not durable: %v", err)
	}
	journal, err = restartInstallerTransition(journalPath, runtimeExecutable, "rollback")
	if err != nil {
		t.Fatal(err)
	}
	if journal.Phase != "rollback-complete" || starts != 1 {
		t.Fatalf("rollback did not restart exactly once: journal=%+v starts=%d", journal, starts)
	}
	if _, err := os.Stat(journalPath); !os.IsNotExist(err) {
		t.Fatalf("completed installer journal survived: %v", err)
	}
}

func TestInstallerTransitionLeavesSchedulerOwnedByAnotherInstallUntouched(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state-root")
	stateDir := filepath.Join(stateRoot, "repositories", "owner--repo")
	ownedInstall := filepath.Join(root, "installed")
	foreignInstall := filepath.Join(root, "foreign-hive")
	if err := os.MkdirAll(filepath.Join(stateDir, "integrated"), 0o700); err != nil {
		t.Fatal(err)
	}
	hiveName := "hive"
	if runtime.GOOS == "windows" {
		hiveName = "hive.exe"
	}
	spec, err := newDaemonServiceSpecWithIdentity(stateDir, filepath.Join(foreignInstall, hiveName), time.Minute, testDaemonHiveCommit, testDaemonDigest())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDaemonServiceSpec(spec); err != nil {
		t.Fatal(err)
	}
	priorStateRoot, priorRegistryRoot := installerTransitionStateRoot, daemonSchedulerRegistryRoot
	installerTransitionStateRoot = func() string { return stateRoot }
	daemonSchedulerRegistryRoot = func() string { return filepath.Join(stateRoot, "schedulers") }
	t.Cleanup(func() { installerTransitionStateRoot, daemonSchedulerRegistryRoot = priorStateRoot, priorRegistryRoot })
	journal, err := prepareInstallerTransition(filepath.Join(root, "journal.json"), ownedInstall)
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.Entries) != 0 {
		t.Fatalf("foreign scheduler was included in exact install transaction: %+v", journal)
	}
	if _, err := os.Stat(daemonServiceDescriptorPath(stateDir)); err != nil {
		t.Fatalf("foreign scheduler descriptor was modified: %v", err)
	}
}
