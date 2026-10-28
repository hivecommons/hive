package commands

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	for _, f := range []string{"outcomes-2026-10.md", "outcomes-2026-10.json", "assets/outcomes-2026-10-bars.svg", "assets/outcomes-2026-10-trend.svg"} {
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

func TestEvidenceBenchmarkFromJSON(t *testing.T) {
	server := httptest.NewServer(nil)
	defer server.Close()
	root := t.TempDir()
	source := filepath.Join(root, "saved.json")
	rep := outcomebench.Report{GeneratedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Source: "saved sample", PRs: 123}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, data, 0o644); err != nil {
		t.Fatal(err)
	}
	readme := filepath.Join(root, "README.md")
	old := rep.HowItWorksPicture(benchmarkReadmeDocDir)
	old = strings.ReplaceAll(old, ".png", ".svg")
	if err := os.WriteFile(readme, []byte(old+"\n"+outcomebench.ReadmeStart+"\nold\n"+outcomebench.ReadmeEnd), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	stdout, _, err := execute(t, server, "", "evidence", "benchmark", "--from-json", source, "--out-dir", out, "--readme", readme)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "123 PRs") {
		t.Fatalf("lost saved counts: %s", stdout)
	}
	saved, err := os.ReadFile(filepath.Join(out, "outcomes-2026-10.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got outcomebench.Report
	if err := json.Unmarshal(saved, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, rep) {
		t.Fatalf("rerender changed report: %+v", got)
	}
	svg := rep.Cards()[rep.CardFile(outcomebench.CardHowItWorks, "dark")]
	png, err := os.ReadFile(filepath.Join(out, "assets", "outcomes-2026-10-card-how-it-works-dark.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := outcomebench.ValidateCardPNG(png, svg); err != nil {
		t.Fatal(err)
	}
	updated, _ := os.ReadFile(readme)
	if !strings.Contains(string(updated), rep.HowItWorksPicture(benchmarkReadmeDocDir)) || strings.Contains(string(updated), ".svg") {
		t.Fatal("ladder embed not updated")
	}
	for _, extra := range [][]string{{"--root", root}, {"--month", "2026-10"}, {"--readme-min-base", "1"}} {
		args := append([]string{"evidence", "benchmark", "--from-json", source, "--out-dir", out}, extra...)
		if _, _, err := execute(t, server, "", args...); err == nil {
			t.Errorf("accepted conflicting flags %v", extra)
		}
	}
	for _, body := range []string{"{", "null"} {
		if err := os.WriteFile(source, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := execute(t, server, "", "evidence", "benchmark", "--from-json", source, "--out-dir", out); err == nil {
			t.Errorf("accepted invalid report %s", body)
		}
	}
	if _, _, err := execute(t, server, "", "evidence", "benchmark", "--from-json", filepath.Join(root, "missing"), "--out-dir", out); err == nil {
		t.Error("accepted missing report")
	}
}
