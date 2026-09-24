package main

import (
	"path/filepath"
	"testing"
)

// The GitHub App diagnosis and classification logic moved to pkg/apphealth
// (#7238). That extraction turned two package-level vars — spokeAppKeyPath and
// spokeProvisionedAppKeyPath — into an argument, and the thin wrappers left
// behind here build that argument via appKeyPaths(0).
//
// This guard pins the one property the move could silently destroy.
//
// Those identifiers are vars rather than consts for a single documented reason
// (see the comment above their declaration): tests repoint them at a temp dir.
// Before the extraction that worked because every reader dereferenced the var
// directly. Now there is an intermediary, and an entirely reasonable-looking
// "optimisation" — hoisting appKeyPaths(0) into a package-level var, or caching
// it in a sync.Once — would capture the production paths at init time and make
// every later reassignment invisible.
//
// The failure that would cause is the nasty kind: nothing panics and nothing
// fails to compile. The diagnosis would simply pre-flight /var/... instead of
// the test's temp key, find no key there, and return key-missing without ever
// reaching the API classification the test is about. Tests that still pass for
// the wrong reason are worse than tests that break, so this asserts the
// read-at-call-time behaviour explicitly instead of trusting it.
func TestAppKeyPathsReadsVarsAtCallTime(t *testing.T) {
	origPVC, origProvisioned := appKeys.DataKeyPath, appKeys.ProvisionedKeyPath
	origDataDir, origProvisionedDir := appKeys.DataDir, appKeys.ProvisionedDir
	t.Cleanup(func() {
		appKeys.DataKeyPath, appKeys.ProvisionedKeyPath = origPVC, origProvisioned
		appKeys.DataDir, appKeys.ProvisionedDir = origDataDir, origProvisionedDir
	})

	// Call once before touching anything, so a cached implementation has every
	// opportunity to memoise the production paths.
	before := appKeyPaths(0)
	if before.Spoke != origPVC || before.Provisioned != origProvisioned {
		t.Fatalf("appKeyPaths(0) does not reflect the vars as declared: got %+v, want {%s %s}",
			before, origPVC, origProvisioned)
	}

	dir := t.TempDir()
	wantSpoke := filepath.Join(dir, "pvc-app.pem")
	wantProvisioned := filepath.Join(dir, "provisioned-app.pem")
	appKeys.DataKeyPath, appKeys.ProvisionedKeyPath = wantSpoke, wantProvisioned

	got := appKeyPaths(0)
	if got.Spoke != wantSpoke {
		t.Errorf("Spoke = %q, want %q — appKeyPaths(0) must read appKeys.DataKeyPath at call time, "+
			"not capture it at init", got.Spoke, wantSpoke)
	}
	if got.Provisioned != wantProvisioned {
		t.Errorf("Provisioned = %q, want %q — appKeyPaths(0) must read "+
			"appKeys.ProvisionedKeyPath at call time, not capture it at init",
			got.Provisioned, wantProvisioned)
	}

	// The two paths are distinct inputs with different meanings (PVC delivery
	// vs read-only Secret mount) and pkg/github consults them in order. A
	// wrapper that filled both fields from one var would pass everything above
	// if the test used a single path, so they are deliberately different here.
	if got.Spoke == got.Provisioned {
		t.Error("appKeyPaths(0) collapsed two distinct key paths into one value")
	}

	appKeys.DataDir = filepath.Join(dir, "data")
	appKeys.ProvisionedDir = filepath.Join(dir, "secrets")
	withAppID := appKeyPaths(3568013)
	wantExtra := []string{
		filepath.Join(appKeys.DataDir, "gh-app-key-3568013.pem"),
		filepath.Join(appKeys.ProvisionedDir, "gh-app-key-3568013.pem"),
	}
	if len(withAppID.Extra) != len(wantExtra) {
		t.Fatalf("Extra = %#v, want %#v", withAppID.Extra, wantExtra)
	}
	for i := range wantExtra {
		if withAppID.Extra[i] != wantExtra[i] {
			t.Errorf("Extra[%d] = %q, want %q", i, withAppID.Extra[i], wantExtra[i])
		}
	}
}
