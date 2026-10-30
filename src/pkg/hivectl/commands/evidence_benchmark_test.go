package commands

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/outcomebench"
)

func TestEvidenceBenchmarkWritesReportAndReadme(t *testing.T) {
	root := t.TempDir()
	writeExportFixtures(t, root)
	out := filepath.Join(t.TempDir(), "bench")
	readme := filepath.Join(t.TempDir(), "README.md")
	if err := os.WriteFile(readme, []byte("# x\n\n"+outcomebench.ReadmeStart+"\nold\n"+outcomebench.ReadmeEnd+"\n\nrest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spend := filepath.Join(t.TempDir(), "cost.json")
	if err := os.WriteFile(spend, []byte(`{"estimated":{"total_usd":30},"merged_prs":15}`), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(hub, []byte(`{"spokes":3,"reporting":2,"window":{"from":"a","to":"b"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	evidenceBenchmarkNow = func() time.Time { return time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { evidenceBenchmarkNow = func() time.Time { return time.Now().UTC() } })
	server := httptest.NewServer(nil)
	defer server.Close()

	stdout, stderr, err := execute(t, server, "", "evidence", "benchmark", "--root", root, "--month", "2026-10",
		"--spend", spend, "--hub", hub, "--out-dir", out, "--readme", readme, "--source", "fixtures")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "skipped 2 unreadable") || !strings.Contains(stderr, "updated outcomes block") {
		t.Fatalf("stderr = %q", stderr)
	}
	if !strings.HasPrefix(stdout, "outcomes 2026-10:") || !strings.Contains(stdout, "mediated 1 merged, unmediated 0 merged") {
		t.Fatalf("stdout = %q", stdout)
	}
	for _, f := range []string{"outcomes-2026-10.md", "outcomes-2026-10.json", "assets/outcomes-2026-10-bars.svg", "assets/outcomes-2026-10-trend.svg",
		"assets/outcomes-2026-10-card-how-it-works-dark.svg", "assets/outcomes-2026-10-card-how-it-works-dark.png", "assets/outcomes-2026-10-card-how-it-works-light.png"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	md, _ := os.ReadFile(filepath.Join(out, "outcomes-2026-10.md"))
	if !strings.Contains(string(md), "from fixtures") || !strings.Contains(string(md), "3 spokes registered") || !strings.Contains(string(md), "≈$2.00") {
		t.Fatalf("report:\n%s", md)
	}
	got, _ := os.ReadFile(readme)
	if strings.Contains(string(got), "\nold\n") || !strings.HasPrefix(string(got), "# x\n\n"+outcomebench.ReadmeStart) || !strings.HasSuffix(string(got), "\n\nrest\n") {
		t.Fatalf("readme:\n%s", got)
	}

	// --from-json re-renders the same outputs from the saved report, so the
	// committed assets can be redrawn without the evidence tree.
	out2 := t.TempDir()
	readme2 := filepath.Join(out2, "README.md")
	if err := os.WriteFile(readme2, []byte("# y\n\n"+outcomebench.ReadmeStart+"\nold\n"+outcomebench.ReadmeEnd+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execute(t, server, "", "evidence", "benchmark", "--from-json", filepath.Join(out, "outcomes-2026-10.json"), "--out-dir", out2, "--readme", readme2); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"outcomes-2026-10.md", "assets/outcomes-2026-10-card-how-it-works-dark.png"} {
		a, _ := os.ReadFile(filepath.Join(out, f))
		b, err := os.ReadFile(filepath.Join(out2, f))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("--from-json re-render of %s differs from the original (%v)", f, err)
		}
	}
	if got2, _ := os.ReadFile(readme2); !strings.HasPrefix(string(got2), "# y\n\n"+outcomebench.ReadmeStart) || strings.Contains(string(got2), "\nold\n") {
		t.Fatalf("readme after --from-json:\n%s", got2)
	}
	if _, _, err := execute(t, server, "", "evidence", "benchmark", "--from-json", filepath.Join(out, "outcomes-2026-10.json"), "--spend", spend); err == nil {
		t.Error("--from-json with evidence inputs should be a usage error")
	}
}

func TestEvidenceBenchmarkRejectsBadInput(t *testing.T) {
	server := httptest.NewServer(nil)
	defer server.Close()
	root := t.TempDir()
	cases := [][]string{
		{"--month", "October"},
		{"--now", "yesterday"},
		{"--spend", filepath.Join(root, "missing.json")},
		{"--hub", filepath.Join(root, "missing.json")},
		{"--readme", filepath.Join(root, "missing.md")},
	}
	for _, extra := range cases {
		args := append([]string{"evidence", "benchmark", "--root", root, "--out-dir", filepath.Join(root, "out")}, extra...)
		if _, _, err := execute(t, server, "", args...); err == nil {
			t.Errorf("%v: expected error", extra)
		}
	}
	noMarkers := filepath.Join(root, "plain.md")
	if err := os.WriteFile(noMarkers, []byte("# no block\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execute(t, server, "", "evidence", "benchmark", "--root", root, "--out-dir", filepath.Join(root, "out"), "--readme", noMarkers); err == nil || !strings.Contains(err.Error(), "outcomes:start") {
		t.Fatalf("expected marker error, got %v", err)
	}
	if _, err := writeBenchmark(outcomebench.Report{}, filepath.Join(noMarkers, "x")); err == nil {
		t.Fatal("unwritable out-dir must fail")
	}
}
