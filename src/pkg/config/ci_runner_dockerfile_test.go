package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hivecommons/hive#7206.
//
// The apt-egress failure class (#6648 → #6870 → #6935 → #7124 → #7201) was
// closed four times and recurred every time, because the stock ARC runner
// image has no C toolchain and every -race shard installed one over the
// network. src/deploy/ci-runners/Dockerfile removes that dependency.
//
// These tests pin the properties that make it work. They are cheap guards on a
// file nothing else in CI builds — the image is built and pushed by an
// operator — so a regression here would otherwise only surface as the whole
// failure class coming back for a fifth time.

func ciRunnerDockerfile(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "ci-runners", "Dockerfile")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// The entire point of the image. ci-install-tool.sh no-ops when the tool is
// already present, so these three packages are what make every call site in
// .github/workflows/* take the zero-network path.
//
// Checked against the apt-get install list specifically, not against the file
// as a whole: every one of these names also appears in prose and in the
// verification commands, so a substring match over the whole Dockerfile still
// passes after the package is dropped from the install.
func TestCIRunnerDockerfileBakesCIToolchain(t *testing.T) {
	dockerfile := ciRunnerDockerfile(t)
	if !strings.Contains(dockerfile, "apt-get install") {
		t.Fatal("ci-runners/Dockerfile must install the toolchain with apt-get")
	}

	// Package names are one per line in the install list, each carrying only
	// line-continuation punctuation. Reduce every line to its bare token so a
	// name mentioned in a comment or a `tmux -V` check cannot satisfy this.
	installed := map[string]bool{}
	for _, line := range strings.Split(dockerfile, "\n") {
		token := strings.TrimSpace(line)
		token = strings.TrimSuffix(token, "\\")
		token = strings.TrimSpace(token)
		token = strings.TrimSuffix(token, ";")
		token = strings.TrimSpace(token)
		if token != "" {
			installed[token] = true
		}
	}

	for _, pkg := range []string{"gcc", "libc6-dev", "tmux"} {
		if !installed[pkg] {
			t.Fatalf("ci-runners/Dockerfile must install %q as its own entry in the apt-get install list — without it that tool is still fetched from the Ubuntu mirrors at job time (#7206)", pkg)
		}
	}
}

// A build that installs the packages but never exercises them can still ship an
// image that sends CI straight back to the mirrors. Failing inside the build is
// the only place this is cheap to catch.
func TestCIRunnerDockerfileVerifiesToolchainDuringBuild(t *testing.T) {
	dockerfile := ciRunnerDockerfile(t)
	for _, want := range []string{"gcc --version", "tmux -V"} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("ci-runners/Dockerfile must verify the toolchain during the build (missing %q) so a stale base image or mirror fails the build instead of shipping (#7206)", want)
		}
	}
}

// The base image runs as uid 1001. Installing packages needs root, but shipping
// an image that STAYS root would change the security posture of every CI job.
func TestCIRunnerDockerfileDropsBackToRunnerUser(t *testing.T) {
	dockerfile := ciRunnerDockerfile(t)
	if !strings.Contains(dockerfile, "USER root") {
		t.Fatal("ci-runners/Dockerfile needs USER root to write /etc/apt and install packages")
	}
	idxRoot := strings.LastIndex(dockerfile, "USER root")
	idxRunner := strings.LastIndex(dockerfile, "USER runner")
	if idxRunner == -1 {
		t.Fatal("ci-runners/Dockerfile must return to USER runner — the ARC entrypoint and runner binary assume uid 1001 (#7206)")
	}
	if idxRunner < idxRoot {
		t.Fatal("ci-runners/Dockerfile must drop back to USER runner AFTER the root install steps, or the image ships as root")
	}
}

// The base image must be overridable (the deployment moves) but must not be a
// mutable floating tag, or "which runner is this?" stops being answerable.
func TestCIRunnerDockerfilePinsBaseImage(t *testing.T) {
	dockerfile := ciRunnerDockerfile(t)
	if !strings.Contains(dockerfile, "ARG RUNNER_BASE_IMAGE=") {
		t.Fatal("ci-runners/Dockerfile must expose RUNNER_BASE_IMAGE as a build ARG so the operator can track the deployed runner version (#7206)")
	}
	if strings.Contains(dockerfile, "FROM summerwind/actions-runner:latest") ||
		strings.Contains(dockerfile, "ARG RUNNER_BASE_IMAGE=summerwind/actions-runner:latest") {
		t.Fatal("ci-runners/Dockerfile must not default to a floating :latest runner image")
	}
}

// The #6648 apt repair exists in two places: the ConfigMap mounted into running
// pods, and this Dockerfile (which needs it at BUILD time, when no ConfigMap is
// mounted). Two copies drift. This pins them together — in particular the
// mirror host and the Ubuntu suite, which the README's #6648 caveat calls out
// as the thing that breaks on a base-image release bump.
func TestCIRunnerDockerfileAptConfigMatchesConfigMap(t *testing.T) {
	dockerfile := ciRunnerDockerfile(t)
	cmPath := filepath.Join("..", "..", "deploy", "ci-runners", "apt-mirror-configmap.yaml")
	body, err := os.ReadFile(cmPath)
	if err != nil {
		t.Fatalf("read %s: %v", cmPath, err)
	}
	configMap := string(body)

	const mirror = "http://azure.archive.ubuntu.com/ubuntu/"
	if !strings.Contains(configMap, mirror) {
		t.Fatalf("apt-mirror-configmap.yaml no longer uses %s — update the Dockerfile in the same change (#6648/#7206)", mirror)
	}
	if !strings.Contains(dockerfile, mirror) {
		t.Fatalf("ci-runners/Dockerfile must build against %s: archive.ubuntu.com is unroutable from this cluster, so a build run inside it would hit the exact dead route #6648 is about", mirror)
	}

	// Suite names must agree, or `apt-get update` fails with a confusing error
	// that looks like a recurrence of #6648 but is not.
	for _, suite := range []string{"noble noble-updates noble-backports", "noble-security"} {
		if strings.Contains(configMap, suite) && !strings.Contains(dockerfile, suite) {
			t.Fatalf("ci-runners/Dockerfile is missing suite %q that apt-mirror-configmap.yaml pins — a release-name drift breaks apt-get update (#6648 caveat)", suite)
		}
	}

	// Without ForceIPv4 apt spends its whole timeout budget on AAAA records
	// that this cluster can never route. That is the original #6648 symptom.
	const forceIPv4 = `Acquire::ForceIPv4 "true";`
	if !strings.Contains(dockerfile, forceIPv4) {
		t.Fatalf("ci-runners/Dockerfile must set %s — the cluster has no IPv6 egress (#6648)", forceIPv4)
	}
}
