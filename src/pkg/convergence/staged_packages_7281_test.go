package convergence_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// modulePrefix is the import path every package in this module shares.
const modulePrefix = "github.com/hivecommons/hive/"

// stagedMarker is the sentence each staged convergence subpackage carries in
// its package doc. It is matched verbatim, so removing or reordering the words
// while wiring a package up will fail the test rather than silently pass.
const stagedMarker = "deliberately not wired into any hive binary"

// stagedConvergencePackages are the convergence subpackages that are compiled
// and maintained but reachable from no binary (#7281).
//
// This list is the claim under test, not a free-form annotation: every entry
// must be BOTH unwired and marked, and every convergence subpackage missing
// from the list must be genuinely reachable. Both directions are checked, so
// the list cannot drift away from reality in either direction.
//
// On v5, mutation and proof are wired through cmd/hive/mutationwire.go
// (#6064), outcome through cmd/hive/publicationwire.go (#8353), and the audit
// campaign through the dashboard runs API (#8460 gap 6). No convergence
// subpackages are staged.
var stagedConvergencePackages = []string{}

// TestStagedConvergencePackagesAreMarkedAndUnwired turns #7281's finding into
// an enforced invariant.
//
// The problem it fixes is not "these packages are unused" — staging work is
// legitimate, and pkg/turn does exactly that on purpose. The problem is that a
// reader could not tell STAGED work from ABANDONED work, because the code
// carried no marker either way. A doc comment alone would just rot, so the
// marker is made load-bearing:
//
//   - A package that is unwired MUST say so. Otherwise 2,580 lines look like
//     live production code that simply nobody happens to call.
//   - A package that says so MUST actually still be unwired. Otherwise the
//     first person to wire it up leaves behind a doc comment that actively
//     lies, which is worse than the silence this test was written to fix.
//
// The second direction is what makes this a guard rather than a pin: wiring
// mutation/outcome/proof into a binary is a perfectly good outcome, and this
// test does not forbid it — it just forces the doc comment to be corrected in
// the same change.
func TestStagedConvergencePackagesAreMarkedAndUnwired(t *testing.T) {
	reachable := packagesReachableFromBinaries(t)

	// Direction 1: every staged package is genuinely unreachable and marked.
	for _, pkg := range stagedConvergencePackages {
		importPath := modulePrefix + pkg
		if reachable[importPath] {
			t.Errorf("%s is reachable from a binary but its package doc still claims to be staged.\n"+
				"Wiring it up is fine — but remove the %q marker from its doc.go in the same change, "+
				"or the comment becomes an active lie about production reachability.", pkg, stagedMarker)
		}
		if doc := packageDoc(t, pkg); !strings.Contains(doc, stagedMarker) {
			t.Errorf("%s is unreachable from every binary but carries no staged marker.\n"+
				"Add %q to its package doc (see pkg/turn for the convention) so a reader can tell "+
				"staged work from abandoned work, or wire the package up.", pkg, stagedMarker)
		}
	}

	// Direction 2: no OTHER convergence package is quietly unreachable. This
	// is what stops the next dark subpackage from going unnoticed the way
	// these three did.
	staged := make(map[string]bool, len(stagedConvergencePackages))
	for _, pkg := range stagedConvergencePackages {
		staged[modulePrefix+pkg] = true
	}
	for _, importPath := range convergencePackages(t) {
		if staged[importPath] || reachable[importPath] {
			continue
		}
		t.Errorf("%s is reachable from no binary and is not on the staged list.\n"+
			"Either wire it up, or add it to stagedConvergencePackages and give it a staged "+
			"package doc — unexplained dark code is the exact failure #7281 reported.",
			strings.TrimPrefix(importPath, modulePrefix))
	}
}

// packagesReachableFromBinaries returns the transitive dependency closure of
// every binary and test harness in the module, which is the real definition of
// "can run in production" — an importer that is itself dark does not count.
func packagesReachableFromBinaries(t *testing.T) map[string]bool {
	t.Helper()
	out := goList(t, "-deps", "../../cmd/...", "../../test/...")
	reachable := make(map[string]bool, len(out))
	for _, p := range out {
		reachable[p] = true
	}
	// Fail closed: an empty or tiny closure would make direction 1 pass
	// vacuously while proving nothing at all.
	if len(reachable) < 50 {
		t.Fatalf("dependency closure has only %d packages, which cannot be right; "+
			"the go list invocation is broken and this test would pass vacuously", len(reachable))
	}
	if !reachable[modulePrefix+"pkg/convergence"] {
		t.Fatalf("pkg/convergence itself is missing from the closure, so the closure is not "+
			"being computed correctly; refusing to draw conclusions from it (%d packages)", len(reachable))
	}
	return reachable
}

// convergencePackages lists every package in the convergence cluster.
func convergencePackages(t *testing.T) []string {
	t.Helper()
	pkgs := goList(t, "./...")
	var out []string
	for _, p := range pkgs {
		if strings.HasPrefix(p, modulePrefix+"pkg/convergence") {
			out = append(out, p)
		}
	}
	if len(out) < len(stagedConvergencePackages)+1 {
		t.Fatalf("found only %d convergence packages (%v), expected at least the parent plus "+
			"the %d staged ones; the listing is broken", len(out), out, len(stagedConvergencePackages))
	}
	return out
}

// isModuleCacheFailure reports whether go tool output describes a failure to
// download modules or write the module cache, rather than a real listing error.
func isModuleCacheFailure(out string) bool {
	for _, marker := range []string{
		"writing go.mod cache",
		"module cache not writable",
		"module lookup disabled",
		"proxy.golang.org",
		"dial tcp",
		"no such host",
	} {
		if strings.Contains(out, marker) {
			return true
		}
	}
	return false
}

// TestIsModuleCacheFailure pins the classifier so a rewording of a real
// listing error cannot silently start skipping the guard.
func TestIsModuleCacheFailure(t *testing.T) {
	env := []string{
		"go: writing go.mod cache: mkdir /ro/cache: permission denied",
		"go: module lookup disabled by GOFLAGS=-mod=vendor",
		"go: example.com/dep@v1.0.0: Get \"https://proxy.golang.org/...\": dial tcp: lookup proxy.golang.org: no such host",
	}
	for _, out := range env {
		if !isModuleCacheFailure(out) {
			t.Errorf("expected environment failure to be recognized: %q", out)
		}
	}
	real := []string{
		"go: no such package ../../cmd/nonexistent",
		"pkg/convergence/mutation/ledger.go:10:2: undefined: frobnicate",
	}
	for _, out := range real {
		if isModuleCacheFailure(out) {
			t.Errorf("real listing error must not be classified as environmental: %q", out)
		}
	}
}

// packageDoc returns the package doc comment text for a package in this module.
func packageDoc(t *testing.T, pkg string) string {
	t.Helper()
	dir := filepath.Join("..", "..", filepath.FromSlash(pkg))
	cmd := exec.Command("go", "doc", "-all", "./"+filepath.ToSlash(dir))
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go doc %s: %v\n%s", pkg, err, out)
	}
	return string(out)
}

func goList(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Computing the closure of cmd/... and test/... needs the full module
		// graph, which `go list` may have to download even when this package
		// itself compiles fine. In sandboxes with a read-only module cache or
		// no network that is an environment limitation, not a finding about
		// the staged packages — skip instead of failing the whole package.
		if isModuleCacheFailure(string(out)) {
			t.Skipf("go list %v needs the module cache/network, unavailable here: %v\n%s", args, err, out)
		}
		t.Fatalf("go list %v: %v\n%s", args, err, out)
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			pkgs = append(pkgs, line)
		}
	}
	return pkgs
}
