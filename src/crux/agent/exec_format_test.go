package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Pane signatures from the OKE 6.12 outage (hivecommons/hive#17729): the npm
// shim's sh fell back to running the ELF as a script, so the pane had no
// copilot output at all — only sh "not found" lines naming the PT_INTERP.
func TestPaneShowsExecFormatError(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"kernel strerror", "sh: 1: /usr/local/lib/node_modules/@github/copilot/node_modules/@github/copilot-linux-arm64/copilot: Exec format error", true},
		{"bash wording", "bash: ./copilot: cannot execute binary file: Exec format error", true},
		{"sh fallback interp line", "copilot: 1: : not found\ncopilot: 2: /lib/ld-linux-aarch64.so.1GNU: not found", true},
		{"sh fallback x86 interp", "/lib64/ld-linux-x86-64.so.2GNU: not found", true},
		{"ordinary not found is not enoexec", "sh: 1: foo: not found", false},
		{"auth failure is not enoexec", "Bad credentials\n❯", false},
		{"ready prompt", "GitHub Copilot CLI 1.0.88\n❯", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneShowsExecFormatError(strings.Split(tc.pane, "\n")); got != tc.want {
				t.Fatalf("paneShowsExecFormatError(%q) = %v, want %v", tc.pane, got, tc.want)
			}
		})
	}
}

func TestExecFormatLastErrorNamesBinaryAndRemedy(t *testing.T) {
	got := execFormatLastError("/usr/local/bin/copilot")
	for _, want := range []string{"/usr/local/bin/copilot", "ENOEXEC", "ld-linux-aarch64.so.1", "#17729"} {
		if !strings.Contains(got, want) {
			t.Fatalf("LastError %q lacks %q", got, want)
		}
	}
}

// A real execve of a non-ELF, non-shebang file is the kernel's ENOEXEC; a
// binary that merely exits non-zero is not a finding.
func TestPreflightOneBinaryClassifiesENOEXEC(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("needs a POSIX execve")
	}
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage-cli")
	// 0x7f 'E' 'L' 'F' with an impossible class byte: the kernel refuses it
	// before any loader runs, and there is no shebang for sh to fall back to.
	if err := os.WriteFile(garbage, append([]byte{0x7f, 'E', 'L', 'F', 0x09}, make([]byte, 64)...), 0o755); err != nil {
		t.Fatal(err)
	}
	exits := filepath.Join(dir, "exits-cli")
	if err := os.WriteFile(exits, []byte("#!/bin/sh\necho usage >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := binaryPreflightTimeout
	binaryPreflightTimeout = 10 * time.Second
	t.Cleanup(func() { binaryPreflightTimeout = old })

	f, bad := preflightOneBinary(context.Background(), "copilot", "copilot", garbage)
	if !bad || !f.ExecFormat {
		t.Fatalf("garbage ELF: want ENOEXEC finding, got bad=%v finding=%+v", bad, f)
	}
	if !strings.Contains(f.String(), "ENOEXEC") {
		t.Fatalf("finding string lacks remedy: %q", f.String())
	}
	if _, bad := preflightOneBinary(context.Background(), "codex", "codex", exits); bad {
		t.Fatalf("a CLI that runs and exits non-zero must not be a finding")
	}
}

func TestPreflightBackendBinariesSkipsMissingCLIs(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := PreflightBackendBinaries(context.Background(), []string{"copilot", "codex", "not-a-backend"}); len(got) != 0 {
		t.Fatalf("nothing on PATH must yield no findings, got %+v", got)
	}
}
