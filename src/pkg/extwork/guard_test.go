package extwork_test

import (
	"os/exec"
	"strings"
	"testing"
)

const (
	modulePrefix     = "github.com/hivecommons/hive/"
	flueAdapterPkg   = modulePrefix + "pkg/extwork/flue"
	extworkPkg       = modulePrefix + "pkg/extwork"
	hiveBinary       = "./cmd/hive"
	flueBuildTag     = "extwork_flue"
	minClosurePkgs   = 50
	moduleRootFromPk = "../.."
)

func goListDeps(t *testing.T, tags string) map[string]bool {
	t.Helper()
	args := []string{"list", "-deps"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, hiveBinary)
	cmd := exec.Command("go", args...)
	cmd.Dir = moduleRootFromPk
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	deps := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		deps[strings.TrimSpace(line)] = true
	}
	if len(deps) < minClosurePkgs {
		t.Fatalf("dependency closure has only %d packages; go list is broken and this guard would pass vacuously", len(deps))
	}
	return deps
}

// TestFlueAdapterNotLinkedUnlessEnabled is the removal-sensitive guard #8361
// asks for: the Flue adapter must be absent from the hive binary's dependency
// closure by default and present only under the extwork_flue build tag.
//
// Both directions are checked so the test cannot pass vacuously: the tagged
// build is the positive control proving the closure computation sees the
// adapter when it really is linked, and the engine-neutral pkg/extwork must be
// reachable in both builds (the dashboard wires the seams even when no engine
// is compiled in).
func TestFlueAdapterNotLinkedUnlessEnabled(t *testing.T) {
	plain := goListDeps(t, "")
	if plain[flueAdapterPkg] {
		t.Fatalf("%s is linked into %s without the %s build tag; the adapter must be opt-in at build time", flueAdapterPkg, hiveBinary, flueBuildTag)
	}
	if !plain[extworkPkg] {
		t.Fatalf("%s is not reachable from %s; the engine-neutral seams must be wired regardless of engine", extworkPkg, hiveBinary)
	}
	tagged := goListDeps(t, flueBuildTag)
	if !tagged[flueAdapterPkg] {
		t.Fatalf("positive control failed: %s is not linked even with -tags %s, so the negative check above proves nothing", flueAdapterPkg, flueBuildTag)
	}
}
