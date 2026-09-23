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
		"ARG PI_CODING_AGENT_VERSION=0.87.1",
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

// #7925: the contributor image could run the relay and the agent CLIs but not
// the checked-out repository's own checks — no pip/pytest/pyyaml/jsonschema/
// requests, no make, no just, no shellcheck, no yq. The failure mode that
// makes this a correctness bug rather than a convenience one is substitution:
// with no pytest, pytest-style tests were run under `python3 -m unittest`,
// reported "Ran 0 tests ... OK", and were nearly cited as a green run.
//
// This asserts the baseline is in the image, not that some tool is merely
// mentioned in a comment: each entry below is checked against the apt install
// list itself.
func TestContributorDockerfileShipsRepoCheckBaseline(t *testing.T) {
	dockerfile := readContributorDockerfile(t)
	for _, pkg := range []string{
		"make",
		"shellcheck",
		"python3-pip",
		"python3-venv",
		"python3-pytest",
		"python3-yaml",
		"python3-jsonschema",
		"python3-requests",
	} {
		if !aptInstallsPackage(dockerfile, pkg) {
			t.Fatalf("Dockerfile.contributor no longer apt-installs %q — a container-mode agent cannot run the repository's own checks without it (#7925)", pkg)
		}
	}
}

// Debian marks the system Python externally-managed (PEP 668) and this
// container runs as non-root `dev`, so shipping python3-pip alone leaves
// `pip install` failing twice over — the headline "no pip" complaint would
// survive the package list. The venv is the part that actually makes pip
// usable, so it is pinned separately: created with --system-site-packages (so
// the apt-installed libraries above stay visible through it), owned by uid
// 1000, and first on PATH.
func TestContributorDockerfileMakesPipUsableByTheAgentUser(t *testing.T) {
	dockerfile := readContributorDockerfile(t)
	for _, want := range []string{
		"python3 -m venv --system-site-packages /opt/hive/pyenv",
		"chown -R 1000:1000 /opt/hive/pyenv",
		"ENV VIRTUAL_ENV=/opt/hive/pyenv",
		"ENV PATH=/opt/hive/pyenv/bin:$PATH",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile.contributor missing %q — `pip install` in the contributor container fails as externally-managed or unwritable (#7925)", want)
		}
	}
	// The venv must be usable by the unprivileged agent user, which means the
	// chown has to come after the venv is populated, not before it.
	if strings.Index(dockerfile, "python3 -m venv --system-site-packages /opt/hive/pyenv") >
		strings.Index(dockerfile, "chown -R 1000:1000 /opt/hive/pyenv") {
		t.Fatal("Dockerfile.contributor chowns /opt/hive/pyenv before creating it")
	}
}

// just and yq are not packaged in Debian 12, so they arrive as release
// binaries. They must arrive the way everything else in this file does: a
// pinned version verified with sha256sum BEFORE install, never a
// curl-pipe-to-shell installer that resolves the release at run time.
func TestContributorDockerfileInstallsJustAndYqPinnedAndChecksummed(t *testing.T) {
	dockerfile := readContributorDockerfile(t)
	for _, forbidden := range []string{
		"just.systems/install.sh",
		"raw.githubusercontent.com/casey/just",
	} {
		if strings.Contains(dockerfile, forbidden) {
			t.Fatalf("Dockerfile.contributor must not execute a mutable installer (%q)", forbidden)
		}
	}
	for _, want := range []string{
		"https://github.com/casey/just/releases/download/${JUST_VERSION}/just-${JUST_VERSION}-${JUST_TARGET}.tar.gz",
		`echo "${JUST_SHA256}  /tmp/just.tar.gz" | sha256sum -c -`,
		"install -m 0755 /tmp/just /usr/local/bin/just",
		"just --version",
		"https://github.com/mikefarah/yq/releases/download/v${YQ_VERSION}/${YQ_ASSET}",
		`echo "${YQ_SHA256}  /tmp/yq" | sha256sum -c -`,
		"install -m 0755 /tmp/yq /usr/local/bin/yq",
		"yq --version",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile.contributor missing %q — just/yq are part of the #7925 baseline and must be pinned, checksummed and verified at build time", want)
		}
	}
	for _, arg := range []string{
		"JUST_VERSION", "JUST_SHA256_AMD64", "JUST_SHA256_ARM64",
		"YQ_VERSION", "YQ_SHA256_AMD64", "YQ_SHA256_ARM64",
	} {
		value := dockerfileArg(t, dockerfile, arg)
		switch {
		case value == "" || strings.EqualFold(value, "latest"):
			t.Fatalf("%s must be pinned to a concrete value, got %q", arg, value)
		case strings.Contains(arg, "SHA256") && (len(value) != 64 || strings.Trim(value, "0123456789abcdef") != ""):
			t.Fatalf("%s must be a lowercase 64-hex SHA-256, got %q", arg, value)
		}
	}
	// Verify-then-install, in that order, for both binaries.
	for _, pair := range [][2]string{
		{`echo "${JUST_SHA256}  /tmp/just.tar.gz" | sha256sum -c -`, "install -m 0755 /tmp/just /usr/local/bin/just"},
		{`echo "${YQ_SHA256}  /tmp/yq" | sha256sum -c -`, "install -m 0755 /tmp/yq /usr/local/bin/yq"},
	} {
		if strings.Index(dockerfile, pair[0]) > strings.Index(dockerfile, pair[1]) {
			t.Fatalf("Dockerfile.contributor installs %q before verifying its checksum", pair[1])
		}
	}
}

func readContributorDockerfile(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile.contributor"))
	if err != nil {
		t.Fatalf("read Dockerfile.contributor: %v", err)
	}
	return string(body)
}

// dockerfileArg returns the default value of `ARG <name>=...`, or "" when the
// ARG is absent — an absent pin must fail the caller, not pass vacuously.
func dockerfileArg(t *testing.T, dockerfile, name string) string {
	t.Helper()
	marker := "ARG " + name + "="
	i := strings.Index(dockerfile, marker)
	if i < 0 {
		t.Fatalf("Dockerfile.contributor has no ARG %s", name)
	}
	value := dockerfile[i+len(marker):]
	if nl := strings.IndexByte(value, '\n'); nl >= 0 {
		value = value[:nl]
	}
	return strings.TrimSpace(value)
}

// aptInstallsPackage reports whether pkg appears as a package name in an
// `apt-get install` list, rather than anywhere in the file. A comment naming
// shellcheck, or a longer package that merely has pkg as a prefix
// (python3-pytest vs python3-pytest-cov), must not satisfy the check.
func aptInstallsPackage(dockerfile, pkg string) bool {
	for _, run := range strings.Split(dockerfile, "apt-get install")[1:] {
		// The package list runs to the first `&&` (the cleanup step) or, for a
		// list that ends the instruction, to the first newline not held open
		// by a backslash. Stopping at `&&` is what keeps a verification step
		// later in the same RUN — `make --version` — from standing in for the
		// package it is meant to verify.
		list := run
		if i := strings.Index(list, "&&"); i >= 0 {
			list = list[:i]
		}
		for i := 0; i < len(list); i++ {
			if list[i] == '\n' && (i == 0 || list[i-1] != '\\') {
				list = list[:i]
				break
			}
		}
		for _, field := range strings.Fields(list) {
			if field == pkg {
				return true
			}
		}
	}
	return false
}
