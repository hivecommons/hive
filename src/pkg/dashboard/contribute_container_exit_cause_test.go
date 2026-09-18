package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #7661: `just contribute-hive omp` pulled the contributor image and died at
// startup because the image did not ship omp. The container's own log said
// `ERROR: omp CLI not found.` — and the Justfile then printed a "Common
// causes" list (expired GH_TOKEN, unreadable mounts, missing registration
// token) that was wrong on every line, so the failure read as an auth
// problem. The block must name the cause its own captured log proves, and
// point at the one command that runs the backend from the host, instead.
//
// These tests run the real startup-failure block from the Justfile against a
// fake container runtime, the way contribute_local_mode_backend_matrix_test.go
// runs backends.conf functions: the assertions are on what an operator sees.

// contributeHiveContainerExitBlock returns contribute-hive's "container died
// during the grace period" report, from its state check to the terminal
// attach that follows it.
func contributeHiveContainerExitBlock(t *testing.T) string {
	t.Helper()
	src := justfileSource(t)
	start := strings.Index(src, `if [[ "$CONTAINER_STATE" != "true" ]]; then`)
	if start < 0 {
		t.Fatal("contribute-hive container-exit report not found in the Justfile")
	}
	end := strings.Index(src[start:], "# Open the CLI session in a new terminal window")
	if end < 0 {
		t.Fatal("end of the container-exit report not found in the Justfile")
	}
	return src[start : start+end]
}

// runContainerExitReport executes the block with a fake `docker` whose `logs`
// prints containerLogs, and returns everything the operator would see.
func runContainerExitReport(t *testing.T, backend, containerLogs string) string {
	t.Helper()
	block := contributeHiveContainerExitBlock(t)
	// Undo just's interpolation syntax the way `just` itself would. The
	// `{{ "{{" }}` escapes become the Go-template braces docker inspect
	// reads, so check for stray interpolations before, not after.
	block = strings.ReplaceAll(block, "{{hive_image}}", "ghcr.io/example/hive-contributor:test")
	block = strings.ReplaceAll(block, "{{backend}}", backend)
	if leftover := strings.ReplaceAll(strings.ReplaceAll(block, `{{ "{{" }}`, ""), `{{ "}}" }}`, ""); strings.Contains(leftover, "{{") {
		t.Fatalf("unhandled just interpolation in the container-exit block: %s", block)
	}
	block = strings.ReplaceAll(block, `{{ "{{" }}`, "{{")
	block = strings.ReplaceAll(block, `{{ "}}" }}`, "}}")

	binDir := t.TempDir()
	logFile := filepath.Join(binDir, "container.log")
	if err := os.WriteFile(logFile, []byte(containerLogs), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  inspect) echo 1 ;;\n" +
		"  logs) cat '" + logFile + "' ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	script := "set -uo pipefail\n" +
		"RUNTIME=docker\nCONTAINER_NAME=hive-contributor-test\nCONTAINER_STATE=false\nBACKEND=" + backend + "\n" +
		"report_container_termination() { :; }\n" +
		block
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = filepath.Join("..", "..", "..") // repo root: the block sources config/backends.conf from $(pwd)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("container-exit report should exit 1, got err=%v output:\n%s", err, out)
	}
	return string(out)
}

func TestContainerExitReportNamesMissingImageCLI(t *testing.T) {
	// What the container actually logged in #7661, verbatim.
	out := runContainerExitReport(t, "omp", "ERROR: omp CLI not found.\nInstall it and try again.\n")

	for _, want := range []string{
		"ERROR: omp CLI not found.", // the captured log still shows
		"does not ship the omp CLI",
		"just contribute-setup omp",
		"HIVE_OMP_DANGEROUSLY_RUN_UNCONFINED=1 just contribute-hive omp local",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("container-exit report for a missing CLI does not say %q:\n%s", want, out)
		}
	}
	for _, wrong := range []string{
		"GH_TOKEN empty/expired",
		"config mounts unreadable",
		"missing HIVE_REGISTRATION_TOKEN",
	} {
		if strings.Contains(out, wrong) {
			t.Errorf("container-exit report blames %q for a CLI the image does not ship:\n%s", wrong, out)
		}
	}
}

// A backend with no unconfined opt-in (its local mode is confined) gets the
// plain local-mode command, not an env var that means nothing for it.
func TestContainerExitReportLocalCommandWithoutOptIn(t *testing.T) {
	out := runContainerExitReport(t, "codex", "ERROR: codex CLI not found.\nInstall it and try again.\n")
	if !strings.Contains(out, "just contribute-hive codex local") {
		t.Errorf("container-exit report does not print the local-mode command:\n%s", out)
	}
	if strings.Contains(out, "DANGEROUSLY_RUN_UNCONFINED") {
		t.Errorf("container-exit report invents an unconfined opt-in for codex:\n%s", out)
	}
}

// Any other startup death keeps the auth/mount list: those causes are real
// for a container that got past the CLI check.
func TestContainerExitReportKeepsCommonCausesOtherwise(t *testing.T) {
	out := runContainerExitReport(t, "omp", "ERROR: relay could not authenticate: 401\n")
	for _, want := range []string{
		"Common causes:",
		"GH_TOKEN empty/expired",
		"config mounts unreadable",
		"missing HIVE_REGISTRATION_TOKEN",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("container-exit report dropped %q for a non-CLI failure:\n%s", want, out)
		}
	}
	if strings.Contains(out, "does not ship the omp CLI") {
		t.Errorf("container-exit report blames the image for a failure its log does not show:\n%s", out)
	}
}

// The setup preflight probes the HOST's CLI. That is what local mode runs,
// but container mode — the default — runs the copy inside the image, which
// is why setup said omp was ready and the container said it was not. The
// preflight has to say which copy it is looking at.
func TestBackendPreflightSaysItProbesTheHostCLI(t *testing.T) {
	src := justfileSource(t)
	start := strings.Index(src, `echo "── Preflight: {{backend}} CLI ──"`)
	if start < 0 {
		t.Fatal("contribute-check-backend preflight header not found in the Justfile")
	}
	end := strings.Index(src[start:], `case "{{backend}}" in`)
	if end < 0 {
		t.Fatal("contribute-check-backend case dispatch not found in the Justfile")
	}
	head := src[start : start+end]
	for _, want := range []string{"host CLI", "contribute-hive {{backend}} local", "container mode runs the copy inside the image"} {
		if !strings.Contains(head, want) {
			t.Errorf("preflight header does not say %q — a passing host probe reads as 'container mode will work':\n%s", want, head)
		}
	}
}
