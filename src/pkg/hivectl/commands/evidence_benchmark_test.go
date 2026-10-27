package commands

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvidenceBenchmarkStdout(t *testing.T) {
	root := t.TempDir()
	writeExportFixtures(t, root)
	server := httptest.NewServer(nil)
	defer server.Close()

	stdout, stderr, err := execute(t, server, "", "evidence", "benchmark", "--root", root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "skipped 2 unreadable or non-v1 bundle(s)") {
		t.Fatalf("stderr = %q", stderr)
	}
	for _, want := range []string{
		"# Outcome benchmark: Hive-mediated vs unmediated pull requests",
		"- **PRs in window:** 3 (1 mediated, 2 unmediated;",
		"- **Files skipped as unreadable or not schema v1:** 2",
		"| Revert rate (merged PRs) | not reported (n=1) | not reported (n=0) |",
		"| Cost per merged PR | not available (no fleet spend supplied) |",
		"## Methodology",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, stdout)
		}
	}
	dash, _, err := execute(t, server, "", "evidence", "benchmark", "--root", root, "--out", "-", "--repo", "o/r")
	if err != nil || !strings.Contains(dash, "- **Repository:** o/r") || !strings.Contains(dash, "- **PRs in window:** 2 ") {
		t.Fatalf("--out - / --repo: err %v\n%s", err, dash)
	}
}

func TestEvidenceBenchmarkOutReproducible(t *testing.T) {
	root := t.TempDir()
	writeExportFixtures(t, root)
	server := httptest.NewServer(nil)
	defer server.Close()
	dir := t.TempDir()
	args := func(out string) []string {
		return []string{"evidence", "benchmark", "--root", root,
			"--since", "2026-10-01T00:00:00Z", "--until", "2026-11-01T00:00:00Z",
			"--fleet-spend-usd", "42.5", "--out", out}
	}
	a, b := filepath.Join(dir, "a.md"), filepath.Join(dir, "b.md")
	_, stderr, err := execute(t, server, "", args(a)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "wrote benchmark over 3 PR(s) to "+a) {
		t.Fatalf("stderr = %q", stderr)
	}
	if _, _, err := execute(t, server, "", args(b)...); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("re-running on the same evidence root must reproduce the same report")
	}
	md := string(first)
	for _, want := range []string{
		"- **Window:** PRs opened from 2026-10-01T00:00:00Z to 2026-11-01T00:00:00Z (exclusive)",
		"| Cost per merged PR | not reported (n=1) |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report missing %q\n%s", want, md)
		}
	}
}

func TestEvidenceBenchmarkMissingRoot(t *testing.T) {
	server := httptest.NewServer(nil)
	defer server.Close()
	stdout, _, err := execute(t, server, "", "evidence", "benchmark", "--root", filepath.Join(t.TempDir(), "absent"))
	if err != nil || !strings.Contains(stdout, "- **PRs in window:** 0 ") {
		t.Fatalf("err %v\n%s", err, stdout)
	}
}

func TestEvidenceBenchmarkUsageErrors(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(nil)
	defer server.Close()
	for _, tc := range []struct {
		name string
		args []string
		exit int
		want string
	}{
		{"bad since", []string{"--since", "yesterday"}, ExitUsage, "invalid --since"},
		{"bad until", []string{"--until", "tomorrow"}, ExitUsage, "invalid --until"},
		{"until before since", []string{"--since", "2026-10-02T00:00:00Z", "--until", "2026-10-01T00:00:00Z"}, ExitUsage, "--until must be after --since"},
		{"bad repo", []string{"--repo", "hive"}, ExitUsage, "invalid --repo"},
		{"negative spend", []string{"--fleet-spend-usd", "-1"}, ExitUsage, "invalid --fleet-spend-usd"},
		{"nan spend", []string{"--fleet-spend-usd", "NaN"}, ExitUsage, "invalid --fleet-spend-usd"},
		{"positional", []string{"extra"}, ExitUsage, ""},
		{"unwritable out", []string{"--out", filepath.Join(root, "missing", "x.md")}, ExitFailure, "writing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := execute(t, server, "", append([]string{"evidence", "benchmark", "--root", root}, tc.args...)...)
			if ExitCode(err) != tc.exit {
				t.Fatalf("exit %d (err %v), want %d", ExitCode(err), err, tc.exit)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
