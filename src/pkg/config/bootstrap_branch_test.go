package config

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// installPathScripts are every script a fresh install runs (or is told to run)
// that clones or pulls the hive repo. Fresh installs must provision mainline
// v4 — never the retired v2 branch, which still exists on origin and therefore
// clones "successfully" while silently missing all v4 work (#4646).
var installPathScripts = []string{
	filepath.Join("..", "..", "..", "bin", "hive-setup.sh"),
	filepath.Join("..", "..", "..", "bin", "hive-prereq-check.sh"),
	filepath.Join("..", "..", "deploy", "bootstrap-lxc.sh"),
}

// retiredBranchRef matches a git branch reference to the retired v2 line in
// clone/pull/checkout positions: `--branch v2`, `-b v2`, `origin v2`,
// `checkout v2` — quoted or not. Deliberately NOT a bare \bv2\b scan: workflow
// FILENAMES and check names like v2-ci.yml / "v2 Tests" are historical and
// must never be renamed (branch protection keys on them), and prose may
// legitimately mention v2.
var retiredBranchRef = regexp.MustCompile(`(--branch|-b|origin|checkout)[= ]+["']?v2\b`)

func TestInstallScriptsNeverCloneRetiredV2(t *testing.T) {
	for _, script := range installPathScripts {
		body, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		if loc := retiredBranchRef.Find(body); loc != nil {
			t.Errorf("%s references the retired v2 branch as an install source (%q) — "+
				"fresh installs must default to v4 (#4646)", script, loc)
		}
	}
}
