package main

import (
	"path/filepath"
	"testing"
)

// The GitHub App diagnosis and classification logic moved to pkg/apphealth
// (#7238). That extraction turned two package-level vars — spokeAppKeyPath and
// spokeProvisionedAppKeyPath — into an argument, and the thin wrappers left
// behind here build that argument via appKeyPaths().
//
// This guard pins the one property the move could silently destroy.
//
// Those identifiers are vars rather than consts for a single documented reason
// (see the comment above their declaration): tests repoint them at a temp dir.
// Before the extraction that worked because every reader dereferenced the var
// directly. Now there is an intermediary, and an entirely reasonable-looking
// "optimisation" — hoisting appKeyPaths() into a package-level var, or caching
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
	origPVC, origProvisioned := spokeAppKeyPath, spokeProvisionedAppKeyPath
	t.Cleanup(func() {
		spokeAppKeyPath, spokeProvisionedAppKeyPath = origPVC, origProvisioned
	})

	// Call once before touching anything, so a cached implementation has every
	// opportunity to memoise the production paths.
	before := appKeyPaths()
	if before.Spoke != origPVC || before.Provisioned != origProvisioned {
		t.Fatalf("appKeyPaths() does not reflect the vars as declared: got %+v, want {%s %s}",
			before, origPVC, origProvisioned)
	}

	dir := t.TempDir()
	wantSpoke := filepath.Join(dir, "pvc-app.pem")
	wantProvisioned := filepath.Join(dir, "provisioned-app.pem")
	spokeAppKeyPath, spokeProvisionedAppKeyPath = wantSpoke, wantProvisioned

	got := appKeyPaths()
	if got.Spoke != wantSpoke {
		t.Errorf("Spoke = %q, want %q — appKeyPaths() must read spokeAppKeyPath at call time, "+
			"not capture it at init", got.Spoke, wantSpoke)
	}
	if got.Provisioned != wantProvisioned {
		t.Errorf("Provisioned = %q, want %q — appKeyPaths() must read "+
			"spokeProvisionedAppKeyPath at call time, not capture it at init",
			got.Provisioned, wantProvisioned)
	}

	// The two paths are distinct inputs with different meanings (PVC delivery
	// vs read-only Secret mount) and pkg/github consults them in order. A
	// wrapper that filled both fields from one var would pass everything above
	// if the test used a single path, so they are deliberately different here.
	if got.Spoke == got.Provisioned {
		t.Error("appKeyPaths() collapsed two distinct key paths into one value")
	}
}
