package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/integrated"
)

const longSetupProofRepository = "DavidDiaz0317/hive-visual-hive-install-proof-20260713-234243"

func useTemporaryHiveHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("HIVE_STATE_DIR", "")
	t.Setenv("HIVE_ID", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	return root
}

func useTemporaryHostedHiveState(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	prior := hostedIntegratedStateRoot
	hostedIntegratedStateRoot = filepath.Join(parent, "integrated")
	t.Cleanup(func() { hostedIntegratedStateRoot = prior })
	t.Setenv("HIVE_STATE_DIR", "")
	t.Setenv("HIVE_ID", "hosted-owner-repository")
	t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1")
	return parent
}

func TestHostedRepositoryStateUsesPersistentDataRoot(t *testing.T) {
	parent := useTemporaryHostedHiveState(t)
	selected, err := repositoryIntegratedStateDir("owner/repository")
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := filepath.Join(parent, "integrated", "repos") + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(selected), wantPrefix) {
		t.Fatalf("hosted repository state escaped persistent data root: got %q want prefix %q", selected, wantPrefix)
	}
	if strings.Contains(filepath.ToSlash(selected), "/.hive/") {
		t.Fatalf("hosted repository state fell back to ephemeral home: %q", selected)
	}
}

func TestHostedRepositoryStateRejectsMissingOrSymlinkedDataRoot(t *testing.T) {
	t.Run("missing parent", func(t *testing.T) {
		parent := useTemporaryHostedHiveState(t)
		hostedIntegratedStateRoot = filepath.Join(parent, "missing", "integrated")
		if _, err := repositoryIntegratedStateDir("owner/repository"); err == nil || !strings.Contains(err.Error(), "persistent state parent") {
			t.Fatalf("missing hosted parent was not rejected: %v", err)
		}
	})
	t.Run("symlinked root", func(t *testing.T) {
		parent := useTemporaryHostedHiveState(t)
		target := filepath.Join(parent, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, hostedIntegratedStateRoot); err != nil {
			if os.IsPermission(err) {
				t.Skipf("symlink creation unavailable: %v", err)
			}
			t.Fatal(err)
		}
		if _, err := repositoryIntegratedStateDir("owner/repository"); err == nil || !strings.Contains(err.Error(), "must be a real directory") {
			t.Fatalf("symlinked hosted root was not rejected: %v", err)
		}
	})
}

func TestExplicitStateDirectoryStillOverridesHostedRoot(t *testing.T) {
	parent := useTemporaryHostedHiveState(t)
	explicit := filepath.Join(parent, "explicit", "repository-state")
	t.Setenv("HIVE_STATE_DIR", explicit)
	if got := defaultIntegratedStateDir(); filepath.Clean(got) != filepath.Clean(explicit) {
		t.Fatalf("explicit state directory lost precedence: got %q want %q", got, explicit)
	}
	if got := integratedStateRoot(); filepath.Clean(got) != filepath.Clean(explicit) {
		t.Fatalf("shared hosted state root lost explicit precedence: got %q want %q", got, explicit)
	}
	if err := validateHostedIntegratedStateRoot(); err != nil {
		t.Fatalf("persistent explicit state directory was rejected: %v", err)
	}
}

func TestHostedExplicitStateDirectoryRejectsEphemeralPath(t *testing.T) {
	useTemporaryHostedHiveState(t)
	t.Setenv("HIVE_STATE_DIR", t.TempDir())
	if err := validateHostedIntegratedStateRoot(); err == nil || !strings.Contains(err.Error(), "must remain on persistent") {
		t.Fatalf("ephemeral explicit state path was accepted: %v", err)
	}
}

func TestHostedContainerReplacementKeepsPersistentRepositorySelection(t *testing.T) {
	useTemporaryHostedHiveState(t)
	stateDir, err := repositoryIntegratedStateDir("owner/repository")
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(stateDir, "checkout")
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(integrated.Config{
		Repository: "owner/repository", RepositoryID: "123", DefaultBranch: "main",
		StateDir: stateDir, CheckoutDir: checkout, VisualHive: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := integrated.RememberCurrentState(integratedStateRoot(), stateDir); err != nil {
		t.Fatal(err)
	}

	firstHome := t.TempDir()
	t.Setenv("HOME", firstHome)
	t.Setenv("USERPROFILE", firstHome)
	first, exists, err := loadAuthoritativeVisualWorkContract()
	if err != nil || !exists {
		t.Fatalf("container A persistent contract: exists=%t err=%v", exists, err)
	}

	secondHome := t.TempDir()
	t.Setenv("HOME", secondHome)
	t.Setenv("USERPROFILE", secondHome)
	second, exists, err := loadAuthoritativeVisualWorkContract()
	if err != nil || !exists {
		t.Fatalf("container B persistent contract: exists=%t err=%v", exists, err)
	}
	if first.StateDir != stateDir || second.StateDir != stateDir || defaultIntegratedStateDir() != stateDir {
		t.Fatalf("ephemeral home replacement changed state selection: first=%q second=%q default=%q want=%q", first.StateDir, second.StateDir, defaultIntegratedStateDir(), stateDir)
	}
	for _, home := range []string{firstHome, secondHome} {
		if _, err := os.Stat(filepath.Join(home, ".hive")); !os.IsNotExist(err) {
			t.Fatalf("hosted state leaked into ephemeral home %s: %v", home, err)
		}
	}
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

func TestDefaultRepositoryStatePathPreservesReadOnlyPlanResidue(t *testing.T) {
	useTemporaryHiveHome(t)
	selected, err := repositoryIntegratedStateDir(longSetupProofRepository)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureTestDirectory(filepath.Join(selected, "integrated", "checkout")); err != nil {
		t.Fatal(err)
	}
	recovered, err := repositoryIntegratedStateDir(longSetupProofRepository)
	if err != nil {
		t.Fatal(err)
	}
	if recovered == selected || recovered != selected+"-managed" {
		t.Fatalf("markerless read-only plan residue was reused: initial=%q recovered=%q", selected, recovered)
	}
	if _, err := os.Stat(selected); err != nil {
		t.Fatalf("read-only plan residue was not preserved: %v", err)
	}
}

func ensureTestDirectory(path string) error {
	return os.MkdirAll(path, 0o700)
}
