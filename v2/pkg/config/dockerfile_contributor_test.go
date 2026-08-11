package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContributorDockerfileInstallsPiWithoutCurlPipeShell(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile.contributor"))
	if err != nil {
		t.Fatalf("read Dockerfile.contributor: %v", err)
	}
	dockerfile := string(body)
	if strings.Contains(dockerfile, "https://pi.dev/install.sh | sh") {
		t.Fatal("Dockerfile.contributor must not execute the mutable pi.dev installer with curl|sh")
	}
	for _, want := range []string{
		"ARG PI_CODING_AGENT_VERSION=0.84.1",
		"@earendil-works/pi-coding-agent@${PI_CODING_AGENT_VERSION}",
		"npm install -g --ignore-scripts",
		"which pi",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile.contributor missing %q", want)
		}
	}
}
