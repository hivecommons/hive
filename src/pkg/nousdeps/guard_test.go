package nousdeps

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These guards test INVARIANTS of the repository, not behaviour (the same
// idea as src/deploy/test_supply_chain_pins.sh, which CI also runs):
//
//   - the Nous framework stays pinned to a full commit in both places that
//     install it, and neither place checks out a branch;
//   - bin/nous-runner.sh runs `hive nous-setup` before run_campaign.py and
//     performs no fetch of its own, so every runtime download goes through
//     the allowlist and ends up in the dependency record.
//
// A PR that re-introduces a floating ref or a bare curl/pip/git-clone in the
// runner fails here rather than quietly shipping.

const (
	repoRelDockerfile = "../../Dockerfile"
	repoRelInstall    = "../../../bin/nous-install.sh"
	repoRelRunner     = "../../../bin/nous-runner.sh"
)

var (
	fullSHARe       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	dockerNousPinRe = regexp.MustCompile(`(?m)^\s*ARG\s+NOUS_COMMIT\s*=\s*"?'?([^"'\s]+)`)
	installPinRe    = regexp.MustCompile(`(?m)^NOUS_PIN_SHA="\$\{HIVE_NOUS_PIN:-([^}]+)\}"`)
	// unpinnedFetchRe catches the tools that fetch from the network. The
	// runner must not call any of them: setup does, under the allowlist.
	unpinnedFetchRe = regexp.MustCompile(`(?m)^\s*(curl|wget|pip3?\s+install|uv\s+pip|git\s+clone|git\s+fetch|git\s+pull)\b`)
)

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Clean(rel)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("%s not available from the package dir (%v); the shell guard in src/deploy covers it in CI", rel, err)
	}
	return string(data)
}

func TestGuardNousImagePinIsFullCommit(t *testing.T) {
	df := readRepoFile(t, repoRelDockerfile)
	m := dockerNousPinRe.FindStringSubmatch(df)
	if m == nil {
		t.Fatal("Dockerfile has no `ARG NOUS_COMMIT=` — the Nous image pin was lost")
	}
	if !fullSHARe.MatchString(m[1]) {
		t.Fatalf("Dockerfile NOUS_COMMIT=%q is not a full 40-hex commit", m[1])
	}
	if !strings.Contains(df, `git -C /opt/nous checkout "${NOUS_COMMIT}"`) {
		t.Fatal("Dockerfile no longer checks out ${NOUS_COMMIT} after cloning Nous")
	}
	if strings.Contains(df, "git clone --branch") || strings.Contains(df, "git clone -b ") {
		t.Fatal("Dockerfile clones Nous by branch; the branch is a moving target")
	}
}

func TestGuardNousInstallScriptPinIsFullCommit(t *testing.T) {
	sh := readRepoFile(t, repoRelInstall)
	m := installPinRe.FindStringSubmatch(sh)
	if m == nil {
		t.Fatal("bin/nous-install.sh has no NOUS_PIN_SHA default")
	}
	if !fullSHARe.MatchString(m[1]) {
		t.Fatalf("bin/nous-install.sh NOUS_PIN_SHA default %q is not a full 40-hex commit", m[1])
	}
	if !strings.Contains(sh, `git fetch --depth 1 origin "$NOUS_PIN_SHA"`) || !strings.Contains(sh, `git checkout --detach FETCH_HEAD`) {
		t.Fatal("bin/nous-install.sh must fetch and detach-checkout the pinned SHA")
	}
	if !strings.Contains(sh, `refusing to install Nous`) {
		t.Fatal("bin/nous-install.sh must refuse to pip-install when the checkout does not match the pin")
	}
}

func TestGuardRunnerFetchesOnlyThroughSetup(t *testing.T) {
	sh := readRepoFile(t, repoRelRunner)
	if m := unpinnedFetchRe.FindString(sh); m != "" {
		t.Fatalf("bin/nous-runner.sh performs its own fetch (%q); declare it as a pinned campaign dependency instead", strings.TrimSpace(m))
	}
	setupAt := strings.Index(sh, `"$HIVE_BIN" `+Subcommand)
	runAt := strings.Index(sh, "run_campaign.py")
	if setupAt < 0 {
		t.Fatalf("bin/nous-runner.sh does not run `hive %s`", Subcommand)
	}
	if runAt < 0 || setupAt > runAt {
		t.Fatal("bin/nous-runner.sh must run `hive nous-setup` BEFORE run_campaign.py so the campaign never starts on unverified inputs")
	}
	if !strings.Contains(sh, EnvFile) {
		t.Fatalf("bin/nous-runner.sh does not source %s; fetched paths are invisible to the campaign", EnvFile)
	}
}
