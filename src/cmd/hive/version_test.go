package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizeVersion pins the pure formatter that turns a build-stamped
// value (a git tag or `git describe` output) into the digit-leading semver
// Hive reports. It is exercised against the four shapes a build can produce:
// a clean release tag, a `git describe` suffix, a dirty build, and the
// empty/fallback case — the exact set called out in #7092.
func TestNormalizeVersion(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"clean tag", "v4.2.1", "4.2.1"},
		{"describe suffix", "v4.2.1-15-gd51e290", "4.2.1-15-gd51e290"},
		{"dirty describe", "v4.2.1-15-gd51e290-dirty", "4.2.1-15-gd51e290-dirty"},
		{"already stripped", "4.2.1", "4.2.1"},
		{"dev fallback passes through", "0.0.0-dev", "0.0.0-dev"},
		{"empty stays empty", "", ""},
		{"whitespace trimmed", "  v4.2.1\n", "4.2.1"},
		// A bare commit sha from `git describe --always` on a tagless build has
		// no leading "v<digit>", so it is left untouched rather than corrupted.
		{"bare sha untouched", "d51e290", "d51e290"},
		// A lone "v" or "version"-like word must not have its leading letter
		// eaten: only "v" immediately followed by a digit is a version prefix.
		{"non-version v-word untouched", "vintage", "vintage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeVersion(c.in); got != c.want {
				t.Fatalf("normalizeVersion(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestReportedVersionFallback asserts the safety net: with no ldflag supplied
// (the state of a plain `go test`/`go build`), the reported version is exactly
// "0.0.0-dev" — never empty. This fails if the fallback default in main.go is
// ever weakened to an empty string.
func TestReportedVersionFallback(t *testing.T) {
	if got := reportedVersion(); got != "0.0.0-dev" {
		t.Fatalf("reportedVersion() with no ldflag = %q, want %q (the 0.0.0-dev safety net)", got, "0.0.0-dev")
	}
}

// TestReportedVersionNormalizesInjectedValue asserts the shared reporter — the
// single function --version, hub heartbeats and dashboard registration all go
// through — normalizes an injected build value.
func TestReportedVersionNormalizesInjectedValue(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })

	version = "v4.34.0-7-g43a51078"
	if got := reportedVersion(); got != "4.34.0-7-g43a51078" {
		t.Fatalf("reportedVersion() = %q, want %q", got, "4.34.0-7-g43a51078")
	}
}

// TestAllVersionConsumersUseReportedVersion is a source assertion guarding the
// wiring: the three consumers #7092 names (the `--version` banner, the hub
// heartbeat payload, and the dashboard registration payload) plus the restart
// audit line must all report via reportedVersion(), not the raw `version` var.
// Passing the raw var would leak a "v"-prefixed git tag to those surfaces and
// re-open the bug. Checked at the source level so the guard holds even where
// the payloads are assembled inline in main() and are awkward to unit-test.
func TestAllVersionConsumersUseReportedVersion(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	s := string(src)

	// The raw var must never be handed straight to a reported surface.
	for _, bad := range []string{"Version:     version,", "Version:                 version,"} {
		if strings.Contains(s, bad) {
			t.Fatalf("main.go passes the raw `version` var to a payload (%q); it must use reportedVersion() so the reported string is normalized semver", bad)
		}
	}

	// --version banner, audit line, and the two payloads = four call sites.
	if n := strings.Count(s, "reportedVersion()"); n < 4 {
		t.Fatalf("expected at least 4 reportedVersion() call sites (banner, audit, heartbeat, registration), found %d", n)
	}
}

// TestInjectedVersionReportedByBinary is the end-to-end half: it builds the
// real hive binary with `-ldflags -X main.version=...` exactly as
// src/Dockerfile does, runs `hive --version`, and asserts the injected value
// (with its leading "v" stripped) is what the binary reports. This fails if
// the ldflag is ever ignored so the 0.0.0-dev fallback always wins.
func TestInjectedVersionReportedByBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "hive-version-test")
	build := exec.Command("go", "build",
		"-ldflags", "-X main.version=v9.9.0-3-gdeadbee",
		"-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building hive with injected version: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("hive --version failed: %v\noutput: %s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "hive 9.9.0-3-gdeadbee (") {
		t.Fatalf("expected --version to report the injected, v-stripped version 'hive 9.9.0-3-gdeadbee (...)', got: %s", got)
	}
	if strings.Contains(got, "0.0.0-dev") {
		t.Fatalf("--version reported the 0.0.0-dev fallback despite an injected ldflag; output: %s", got)
	}
}
