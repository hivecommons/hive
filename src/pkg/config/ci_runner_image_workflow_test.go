package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hivecommons/hive#7289.
//
// #7206 wrote the Dockerfile that bakes the CI toolchain into the runner
// image; a day later the image still did not exist on GHCR and two more race
// shards had died at "Prepare cgo toolchain", because publishing was left to
// an operator with docker and GHCR write. .github/workflows/ci-runner-image.yml
// moves the build and push into CI. These tests pin the properties that keep
// that workflow from reintroducing the failure it exists to remove.

// ciRunnerImageWorkflow returns the workflow with comment lines removed: the
// header explains, at length, exactly the things these tests forbid
// (HIVE_RUNNER_LABELS, type=gha, a push trigger), so matching the prose would
// make every guard here fire on its own rationale.
func ciRunnerImageWorkflow(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", ".github", "workflows", "ci-runner-image.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var kept []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// The build must run on a GitHub-hosted runner. The image exists to remove
// CI's dependency on the self-hosted cluster's egress; building it on that
// cluster would put the Dockerfile's own apt-get behind the exact route #6648
// is about. Every other trusted lane routes through HIVE_RUNNER_LABELS — this
// one must not.
func TestCIRunnerImageWorkflowBuildsOffCluster(t *testing.T) {
	wf := ciRunnerImageWorkflow(t)
	if strings.Contains(wf, "HIVE_RUNNER_LABELS") {
		t.Fatal("ci-runner-image.yml must not route through HIVE_RUNNER_LABELS: the self-hosted cluster's egress is the problem the image removes")
	}
	if !regexp.MustCompile(`(?m)^\s*runs-on:\s*ubuntu-latest\s*$`).MatchString(wf) {
		t.Fatal("ci-runner-image.yml must run on a GitHub-hosted runner (runs-on: ubuntu-latest)")
	}
}

// It builds THE Dockerfile, with the base image as a build arg, and pushes only
// from a manual dispatch — a PR must build (the gate) but never publish.
func TestCIRunnerImageWorkflowBuildsTheRunnerDockerfile(t *testing.T) {
	wf := ciRunnerImageWorkflow(t)
	for _, want := range []string{
		"file: src/deploy/ci-runners/Dockerfile",
		"context: src/deploy/ci-runners",
		"RUNNER_BASE_IMAGE=",
		"push: ${{ github.event_name == 'workflow_dispatch' }}",
		"packages: write",
		"ghcr.io/${{ github.repository_owner }}/hive-ci-runner",
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("ci-runner-image.yml missing %q", want)
		}
	}
	if !strings.Contains(wf, "workflow_dispatch:") || !strings.Contains(wf, "pull_request:") {
		t.Error("ci-runner-image.yml must be dispatchable and run as a PR gate")
	}
	// A push trigger would roll a new tag on every merged Dockerfile change,
	// which is an operator's decision to make (it restarts every runner). It
	// would also give the workflow a branches: filter the release-line guard
	// then has to classify.
	if regexp.MustCompile(`(?m)^\s*push:\s*$`).MatchString(wf) || strings.Contains(wf, "branches:") {
		t.Error("ci-runner-image.yml must not trigger on push or carry a branches: filter")
	}
}

// The repo's Actions cache is the resource #7206 measured as full (9.99 GB of
// 10 GB, 96.6% buildkit blobs) — the reason the #7009 offline .deb cache never
// survived. This workflow must not add to that competition.
func TestCIRunnerImageWorkflowDoesNotUseActionsCache(t *testing.T) {
	wf := ciRunnerImageWorkflow(t)
	for _, banned := range []string{"type=gha", "actions/cache"} {
		if strings.Contains(wf, banned) {
			t.Errorf("ci-runner-image.yml uses %q; the Actions cache is the resource #7206 found full", banned)
		}
	}
}

// Attestations turn a pushed tag into an OCI index (#3760); ARC pulls a plain
// image. Same contract check-no-image-attestations.sh applies to docker.yml.
func TestCIRunnerImageWorkflowKeepsAttestationsOff(t *testing.T) {
	wf := ciRunnerImageWorkflow(t)
	for _, want := range []string{"provenance: false", "sbom: false"} {
		if !strings.Contains(wf, want) {
			t.Errorf("ci-runner-image.yml build-push-action must set %s", want)
		}
	}
}

// Every third-party action is SHA-pinned (the check-action-pins.sh contract).
func TestCIRunnerImageWorkflowPinsActions(t *testing.T) {
	wf := ciRunnerImageWorkflow(t)
	uses := regexp.MustCompile(`(?m)^\s*uses:\s*(\S+)`).FindAllStringSubmatch(wf, -1)
	if len(uses) == 0 {
		t.Fatal("ci-runner-image.yml has no uses: steps")
	}
	pinned := regexp.MustCompile(`@[0-9a-f]{40}$`)
	for _, m := range uses {
		if !pinned.MatchString(m[1]) {
			t.Errorf("action %q is not pinned to a 40-hex commit SHA", m[1])
		}
	}
}

// The README is what an operator follows; it has to point at the workflow
// rather than only at a laptop docker build, and it has to warn about the one
// thing a first push gets wrong by default (a private package ARC cannot pull).
func TestCIRunnerREADMEDocumentsTheWorkflow(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "ci-runners", "README.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	readme := string(body)
	for _, want := range []string{
		"ci-runner-image.yml",
		"gh workflow run ci-runner-image.yml",
		"imagePullSecrets",
		"public",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("ci-runners/README.md missing %q", want)
		}
	}
}
