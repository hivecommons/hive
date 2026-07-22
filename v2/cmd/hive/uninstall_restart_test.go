package main

import (
	"path/filepath"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

func TestUninstallSchedulerRestartRemainsReachableBeforeDurablePause(t *testing.T) {
	stateDir := t.TempDir()
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	config := integrated.Config{Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main"}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if !shouldRestartManagementScheduler("uninstall", stateDir) {
		t.Fatal("failed uninstall preflight would leave an unpaused installation stopped")
	}
	config.Paused = true
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	if shouldRestartManagementScheduler("uninstall", stateDir) {
		t.Fatal("partially prepared uninstall would restart a durably paused installation")
	}
}
