package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

func TestLoadAuthoritativeVisualWorkContractHonorsExplicitStateDir(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	writeVisualWorkContractFixture(t, first, "owner/first")
	writeVisualWorkContractFixture(t, second, "owner/second")

	t.Setenv("HIVE_STATE_DIR", second)
	installed, exists, err := loadAuthoritativeVisualWorkContract()
	if err != nil || !exists || installed.Repository != "owner/second" || !sameSpecialistRuntimePath(installed.StateDir, second) {
		t.Fatalf("explicit state selection = %+v exists=%t err=%v", installed, exists, err)
	}
}

func TestLoadAuthoritativeVisualWorkContractUsesCurrentStateRegistry(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(t.TempDir(), "current")
	writeVisualWorkContractFixture(t, stateDir, "owner/current")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HIVE_STATE_DIR", "")
	if err := integrated.RememberCurrentState(filepath.Join(home, ".hive"), stateDir); err != nil {
		t.Fatal(err)
	}

	installed, exists, err := loadAuthoritativeVisualWorkContract()
	if err != nil || !exists || installed.Repository != "owner/current" || !sameSpecialistRuntimePath(installed.StateDir, stateDir) {
		t.Fatalf("current state selection = %+v exists=%t err=%v", installed, exists, err)
	}
}

func TestLoadAuthoritativeVisualWorkContractIsDormantAfterManagedSelectionRetirement(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".hive")
	stateDir := filepath.Join(t.TempDir(), "uninstalled")
	writeVisualWorkContractFixture(t, stateDir, "owner/uninstalled")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HIVE_STATE_DIR", "")
	if err := integrated.RememberCurrentState(root, stateDir); err != nil {
		t.Fatal(err)
	}
	if removed, err := integrated.ForgetCurrentState(root, stateDir); err != nil || !removed {
		t.Fatalf("retire exact managed selection: removed=%t err=%v", removed, err)
	}
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatal(err)
	}

	installed, exists, err := loadAuthoritativeVisualWorkContract()
	if err != nil || exists || installed.Repository != "" {
		t.Fatalf("managed uninstall remained selected: installed=%+v exists=%t err=%v", installed, exists, err)
	}
}

func TestLoadAuthoritativeVisualWorkContractTreatsEmptyScaffoldAsDormant(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := integrated.NewStore(filepath.Join(stateDir, "integrated")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_STATE_DIR", stateDir)

	installed, exists, err := loadAuthoritativeVisualWorkContract()
	if err != nil || exists || installed.Repository != "" {
		t.Fatalf("empty scaffold = %+v exists=%t err=%v", installed, exists, err)
	}
}

func TestLoadAuthoritativeVisualWorkContractDoesNotCreateExplicitState(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("HIVE_STATE_DIR", stateDir)

	installed, exists, err := loadAuthoritativeVisualWorkContract()
	if err != nil || exists || installed.Repository != "" {
		t.Fatalf("uninstalled explicit state = %+v exists=%t err=%v", installed, exists, err)
	}
	entries, readErr := os.ReadDir(stateDir)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("read-only contract load created state: entries=%v err=%v", entries, readErr)
	}
}

func TestLoadAuthoritativeVisualWorkContractTreatsRemovedExplicitStateAsDormant(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "removed-after-uninstall")
	t.Setenv("HIVE_STATE_DIR", stateDir)

	installed, exists, err := loadAuthoritativeVisualWorkContract()
	if err != nil || exists || installed.Repository != "" {
		t.Fatalf("removed explicit state = %+v exists=%t err=%v", installed, exists, err)
	}
	if _, statErr := os.Lstat(stateDir); !os.IsNotExist(statErr) {
		t.Fatalf("read-only dormant load created removed state: %v", statErr)
	}
}

func TestLoadAuthoritativeVisualWorkContractRejectsNonemptyStateWithoutContract(t *testing.T) {
	stateDir := t.TempDir()
	integratedDir := filepath.Join(stateDir, "integrated")
	if _, err := integrated.NewStore(integratedDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(integratedDir, "audit.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_STATE_DIR", stateDir)

	if _, exists, err := loadAuthoritativeVisualWorkContract(); err == nil || exists || !os.IsNotExist(err) {
		t.Fatalf("nonempty state without contract was accepted: exists=%t err=%v", exists, err)
	}
}

func TestLoadAuthoritativeVisualWorkContractRejectsEmbeddedStateDrift(t *testing.T) {
	selected := filepath.Join(t.TempDir(), "selected")
	writeVisualWorkContractFixture(t, selected, "owner/repo")
	store, err := integrated.NewStore(filepath.Join(selected, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	installed.StateDir = filepath.Join(t.TempDir(), "foreign")
	if err := store.Save(installed); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HIVE_STATE_DIR", selected)
	if _, exists, err := loadAuthoritativeVisualWorkContract(); err == nil || exists || !strings.Contains(err.Error(), "state identity drifted") {
		t.Fatalf("drifted embedded state was accepted: exists=%t err=%v", exists, err)
	}
}

func writeVisualWorkContractFixture(t *testing.T, stateDir, repository string) {
	t.Helper()
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(integrated.Config{Repository: repository, RepositoryID: "123", StateDir: stateDir}); err != nil {
		t.Fatal(err)
	}
}
