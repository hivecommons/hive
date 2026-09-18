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

// The image installs claude-code with --ignore-scripts (deliberately — no
// arbitrary postinstall runs during the build), but that also skips the
// package's install.cjs, which links the platform-native binary into bin/.
// Without an explicit postinstall + verification, `claude` in the container
// dies with "claude native binary not installed" on every task while local
// mode works fine. src/Dockerfile Layer 7 fixed this for the spoke image;
// this pins the contributor image's copy of the same fix.
func TestContributorDockerfileLinksClaudeNativeBinary(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile.contributor"))
	if err != nil {
		t.Fatalf("read Dockerfile.contributor: %v", err)
	}
	dockerfile := string(body)
	for _, want := range []string{
		`node "$(npm root -g)/@anthropic-ai/claude-code/install.cjs"`,
		"claude --version",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile.contributor missing %q — claude's native binary is not linked (or not verified) at build time, so container-mode claude fails at runtime with 'claude native binary not installed'", want)
		}
	}
}

// #7661: `just contribute-hive omp` (container mode, the default) died at
// startup with `omp CLI not found` because the contributor image never
// installed OMP — setup had probed the host's copy, which the container does
// not see. This pins that the image ships omp, and ships it the way the file
// requires of every other CLI: a pinned release verified with sha256sum
// before install, never the mutable `curl https://omp.sh/install | sh`.
func TestContributorDockerfileInstallsOmpPinnedAndChecksummed(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile.contributor"))
	if err != nil {
		t.Fatalf("read Dockerfile.contributor: %v", err)
	}
	dockerfile := string(body)
	for _, forbidden := range []string{"omp.sh/install", "oh-my-pi/main/scripts/install.sh"} {
		if strings.Contains(dockerfile, forbidden) {
			t.Fatalf("Dockerfile.contributor must not execute the mutable omp installer (%q)", forbidden)
		}
	}
	for _, want := range []string{
		"ARG OMP_VERSION=",
		"ARG OMP_SHA256_AMD64=",
		"ARG OMP_SHA256_ARM64=",
		"https://github.com/can1357/oh-my-pi/releases/download/v${OMP_VERSION}/${OMP_ASSET}",
		`echo "${OMP_SHA256}  /tmp/omp" | sha256sum -c -`,
		"install -m 0755 /tmp/omp /usr/local/bin/omp",
		"omp --version",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile.contributor missing %q — the contributor image does not ship omp (or not pinned/checksummed), so container-mode omp fails at startup with 'omp CLI not found' (#7661)", want)
		}
	}
	// The pin must be a concrete version and the digests real 64-hex SHA-256s
	// — a placeholder would pass the substring checks above while verifying
	// nothing.
	for _, arg := range []string{"OMP_VERSION", "OMP_SHA256_AMD64", "OMP_SHA256_ARM64"} {
		marker := "ARG " + arg + "="
		i := strings.Index(dockerfile, marker)
		value := dockerfile[i+len(marker):]
		if nl := strings.IndexByte(value, '\n'); nl >= 0 {
			value = value[:nl]
		}
		value = strings.TrimSpace(value)
		switch {
		case value == "" || strings.EqualFold(value, "latest"):
			t.Fatalf("%s must be pinned to a concrete value, got %q", arg, value)
		case strings.HasPrefix(arg, "OMP_SHA256") && (len(value) != 64 || strings.Trim(value, "0123456789abcdef") != ""):
			t.Fatalf("%s must be a lowercase 64-hex SHA-256, got %q", arg, value)
		}
	}
	// The install must be bounded by the checksum: verify-then-install, in
	// that order, inside the same RUN.
	verify := strings.Index(dockerfile, `echo "${OMP_SHA256}  /tmp/omp" | sha256sum -c -`)
	install := strings.Index(dockerfile, "install -m 0755 /tmp/omp /usr/local/bin/omp")
	if verify > install {
		t.Fatal("Dockerfile.contributor installs omp before verifying its checksum")
	}
}
