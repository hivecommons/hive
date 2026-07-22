//go:build windows

package integrated

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func windowsShortPathName(path string) (string, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	length, err := windows.GetShortPathName(pointer, &buffer[0], uint32(len(buffer)))
	if err != nil {
		return "", err
	}
	if length >= uint32(len(buffer)) {
		buffer = make([]uint16, length+1)
		length, err = windows.GetShortPathName(pointer, &buffer[0], uint32(len(buffer)))
		if err != nil {
			return "", err
		}
	}
	return windows.UTF16ToString(buffer[:length]), nil
}

func TestOrdinaryDirectoryChainAcceptsShortNameAliasWithoutWeakeningReparseChecks(t *testing.T) {
	target := filepath.Join(t.TempDir(), "ordinary directory", "nested state")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	short, err := windowsShortPathName(target)
	if err != nil {
		t.Skipf("Windows short paths are unavailable: %v", err)
	}
	if strings.EqualFold(filepath.Clean(short), filepath.Clean(target)) {
		t.Skip("the test volume did not assign a distinct 8.3 path")
	}
	if err := ensureOrdinaryDirectoryChain(short, 0o700); err != nil {
		t.Fatalf("ordinary 8.3 alias was treated as a reparse point: %v", err)
	}
}

func createTestJunction(t *testing.T, link, target string) {
	t.Helper()
	if output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("directory junctions are unavailable: %v: %s", err, output)
	}
}

func TestWindowsJunctionAncestorsAreRejectedBeforeStateOrGitWrites(t *testing.T) {
	t.Run("state-integrated", func(t *testing.T) {
		state, outside := t.TempDir(), t.TempDir()
		sentinel := filepath.Join(outside, "sentinel.txt")
		if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		createTestJunction(t, filepath.Join(state, "integrated"), outside)
		if err := validateSetupStateRoot(state, "owner/repo"); err == nil {
			t.Fatal("state integrated junction was accepted")
		}
		if data, err := os.ReadFile(sentinel); err != nil || string(data) != "unchanged" {
			t.Fatalf("outside sentinel changed: %q err=%v", data, err)
		}
	})

	t.Run("checkout-parent", func(t *testing.T) {
		state, outside := t.TempDir(), t.TempDir()
		integrated := filepath.Join(state, "integrated")
		if err := os.MkdirAll(integrated, 0o700); err != nil {
			t.Fatal(err)
		}
		createTestJunction(t, filepath.Join(integrated, "checkouts"), outside)
		checkout := filepath.Join(integrated, "checkouts", "owner--repo")
		if _, err := validateManagedCheckoutBeforeGit(checkout, "owner/repo"); err == nil {
			t.Fatal("checkout parent junction was accepted")
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatalf("checkout validation wrote through junction: entries=%v err=%v", entries, err)
		}
	})
}

func TestLegacyManagedCheckoutMigrationRejectsNestedJunction(t *testing.T) {
	_, checkout, proof := createLegacyManagedCheckout(t)
	outside := t.TempDir()
	junction := filepath.Join(checkout, ".git", "objects", "attacker")
	createTestJunction(t, junction, outside)
	if _, err := validateManagedCheckoutBeforeGitWithLegacy(checkout, "owner/repo", &proof); err == nil {
		t.Fatal("markerless legacy checkout with a nested Git junction was migrated")
	}
	if _, err := os.Lstat(filepath.Join(checkout, ".git", managedCheckoutOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("junction-containing checkout received an ownership marker: %v", err)
	}
}
