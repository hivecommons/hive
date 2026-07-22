package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

const longSetupProofRepository = "DavidDiaz0317/hive-visual-hive-install-proof-20260713-234243"

func useTemporaryHiveHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("HIVE_STATE_DIR", "")
	return root
}

func TestDefaultRepositoryStatePathIsBoundedStableAndCollisionResistant(t *testing.T) {
	home := useTemporaryHiveHome(t)
	first, err := repositoryIntegratedStateDir(longSetupProofRepository)
	if err != nil {
		t.Fatal(err)
	}
	again, err := repositoryIntegratedStateDir(longSetupProofRepository)
	if err != nil {
		t.Fatal(err)
	}
	other, err := repositoryIntegratedStateDir("AnotherOwner/hive-visual-hive-install-proof-20260713-234243")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("default repository state path is not deterministic: %q != %q", first, again)
	}
	if first == other {
		t.Fatalf("different repositories collided at %q", first)
	}
	if !strings.HasPrefix(filepath.Clean(first), filepath.Join(home, ".hive", "repos")+string(filepath.Separator)) {
		t.Fatalf("bounded state path escaped the Hive repository root: %q", first)
	}
	if len(filepath.Base(first)) > 37 || strings.Contains(strings.ToLower(first), "daviddiaz0317--hive-visual-hive-install-proof") {
		t.Fatalf("default state path is not bounded or still repeats the repository slug: %q", first)
	}
	packPath := filepath.Join(first, "integrated", "checkout", ".git", "objects", "pack", "pack-"+strings.Repeat("f", 40)+".keep")
	controlledSuffix, err := filepath.Rel(home, packPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(controlledSuffix) >= 150 {
		t.Fatalf("Hive-controlled state and Git pack suffix is not bounded: %d %q", len(controlledSuffix), controlledSuffix)
	}
	simulatedWindowsPath := filepath.Join(`C:\Users\newuser`, controlledSuffix)
	if len(simulatedWindowsPath) >= 220 {
		t.Fatalf("bounded path is not comfortably below the legacy Windows limit for an ordinary home: %d %q", len(simulatedWindowsPath), simulatedWindowsPath)
	}
}

func TestDefaultRepositoryStatePathKeepsExistingLegacyInstallationReadable(t *testing.T) {
	useTemporaryHiveHome(t)
	legacy := legacyRepositoryIntegratedStateDir(longSetupProofRepository)
	store, err := integrated.NewStore(filepath.Join(legacy, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(legacy, "integrated", "checkouts", strings.ReplaceAll(strings.ToLower(longSetupProofRepository), "/", "-"))
	if err := store.Save(integrated.Config{Repository: longSetupProofRepository, RepositoryID: "123", StateDir: legacy, CheckoutDir: checkout}); err != nil {
		t.Fatal(err)
	}
	selected, err := repositoryIntegratedStateDir(longSetupProofRepository)
	if err != nil {
		t.Fatal(err)
	}
	if selected != legacy {
		t.Fatalf("existing legacy state was not selected: got %q want %q", selected, legacy)
	}
}

func TestDefaultRepositoryStatePathIgnoresUnownedPartialLegacyDirectory(t *testing.T) {
	useTemporaryHiveHome(t)
	legacy := legacyRepositoryIntegratedStateDir(longSetupProofRepository)
	if err := ensureTestDirectory(filepath.Join(legacy, "integrated")); err != nil {
		t.Fatal(err)
	}
	selected, err := repositoryIntegratedStateDir(longSetupProofRepository)
	if err != nil {
		t.Fatal(err)
	}
	if selected == legacy || !strings.Contains(filepath.ToSlash(selected), "/.hive/repos/") {
		t.Fatalf("unowned partial legacy path was reused instead of bounded state: %q", selected)
	}
}

func ensureTestDirectory(path string) error {
	return os.MkdirAll(path, 0o700)
}
