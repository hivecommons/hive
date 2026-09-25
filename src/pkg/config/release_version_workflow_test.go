package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", ".."}, parts...)...)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func TestReleaseDockerWorkflowStampsReleaseVersionInput(t *testing.T) {
	docker := readRepoFile(t, ".github", "workflows", "docker.yml")
	for _, want := range []string{
		"release_version:",
		"release_version: ${{ steps.decide.outputs.release_version }}",
		"RELEASE_VERSION: ${{ inputs.release_version || '' }}",
		"echo \"release_version=$RELEASE_VERSION\" >> \"$GITHUB_OUTPUT\"",
		"VERSION=${{ needs.gate.outputs.release_version }}",
		"fetch-depth: 0",
		"fetch-tags: true",
	} {
		if !strings.Contains(docker, want) {
			t.Errorf("docker.yml missing %q", want)
		}
	}

	tagged := readRepoFile(t, ".github", "workflows", "tagged-release.yml")
	if !strings.Contains(tagged, `-f "release_version=v${VERSION}"`) {
		t.Fatal("tagged-release.yml must pass the exact release tag into docker.yml")
	}
}

func TestReleaseDockerfilesDoNotStampDirtyWorktrees(t *testing.T) {
	for _, name := range []string{"Dockerfile", "Dockerfile.hub"} {
		body := readRepoFile(t, "src", name)
		if strings.Contains(body, "git -C /repo describe --tags --always --dirty") {
			t.Fatalf("src/%s still asks git describe for --dirty inside the partial Docker build context", name)
		}
		if !strings.Contains(body, "ARG VERSION") || !strings.Contains(body, "-X main.version=${RESOLVED_VERSION}") {
			t.Fatalf("src/%s must keep accepting VERSION and stamping main.version", name)
		}
	}
}
