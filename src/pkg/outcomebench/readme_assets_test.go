package outcomebench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type readmeChart struct {
	ID, Kind, Note         string
	Numerator, Denominator int
	Series                 []struct {
		Label string
		Value int
	}
	Sources []struct{ Label, URL string }
}

func readmeFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	p := filepath.Join(append([]string{"..", "..", ".."}, parts...)...)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadmeSnapshotCharts(t *testing.T) {
	var report Report
	if err := json.Unmarshal(readmeFile(t, "src", "docs", "benchmarks", "outcomes-2026-10.json"), &report); err != nil {
		t.Fatal(err)
	}
	var charts []readmeChart
	if err := json.Unmarshal(readmeFile(t, "docs", "readme", "chart-data.json"), &charts); err != nil {
		t.Fatal(err)
	}
	if len(charts) != 3 {
		t.Fatalf("want three sourced charts, got %d", len(charts))
	}
	readme := string(readmeFile(t, "README.md"))
	seen := make(map[string]bool)
	for _, chart := range charts {
		if seen[chart.ID] {
			t.Fatalf("duplicate chart %q", chart.ID)
		}
		seen[chart.ID] = true
		switch chart.ID {
		case "trust-followup":
			rate := report.Followup.FixedAndApproved
			if chart.Kind != "ring" || !rate.Reported || chart.Numerator != rate.K || chart.Denominator != rate.N {
				t.Errorf("review chart differs from saved follow-up data: %+v", chart)
			}
		case "productivity-authors":
			if chart.Kind != "bars" || len(chart.Series) != 3 {
				t.Fatal("productivity chart must have three count bars")
			}
			kinds := make(map[string]bool)
			total := 0
			for _, series := range chart.Series {
				kind := strings.TrimSuffix(strings.ToLower(series.Label), "-authored")
				want, ok := report.Mediated.ByAuthorKind[kind]
				if !ok || kinds[kind] || series.Value != want {
					t.Errorf("unexpected author bar %+v", series)
				}
				kinds[kind] = true
				total += series.Value
			}
			if total != report.Mediated.Merged {
				t.Errorf("author bars total %d, want %d", total, report.Mediated.Merged)
			}
		case "governance-input-attacks":
			if report.RedTeam == nil {
				t.Fatal("missing input-screening test data")
			}
			if chart.Kind != "ring" || chart.Numerator != report.RedTeam.Caught || chart.Denominator != report.RedTeam.Attacks {
				t.Errorf("input chart differs from saved test data: %+v", chart)
			}
			disclosure := fmt.Sprintf("**%d of %d harmless test cases**", report.RedTeam.FalseAlarms, report.RedTeam.Benign)
			if !strings.Contains(readme, disclosure) {
				t.Error("README must disclose the harmless-input mistakes")
			}
		default:
			t.Fatalf("unknown chart %q", chart.ID)
		}
		if !strings.Contains(readme, "(docs/readme/"+chart.ID+".svg)") || chart.Note == "" || !strings.Contains(readme, chart.Note) {
			t.Errorf("%s must be embedded with its readable note", chart.ID)
		}
		if len(chart.Sources) == 0 {
			t.Errorf("%s has no sources", chart.ID)
		}
		for _, source := range chart.Sources {
			if !strings.HasPrefix(source.URL, "https://github.com/hivecommons/hive/blob/") || !strings.Contains(readme, "]("+source.URL+")") {
				t.Errorf("%s source is missing from README: %s", chart.ID, source.URL)
			}
		}
		if chart.Kind == "ring" {
			if chart.Denominator <= 0 || chart.Numerator < 0 || chart.Numerator > chart.Denominator {
				t.Fatalf("invalid ring counts: %+v", chart)
			}
			label := fmt.Sprintf("%.1f%% (%d / %d)", float64(chart.Numerator)/float64(chart.Denominator)*100, chart.Numerator, chart.Denominator)
			if !strings.Contains(readme, label) {
				t.Errorf("%s rate differs from its counts", chart.ID)
			}
		}
	}
}

func TestReadmeGraphicsManifest(t *testing.T) {
	var manifest struct {
		Source string
		Assets map[string]string
	}
	if err := json.Unmarshal(readmeFile(t, "docs", "readme", "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	hash.Write(readmeFile(t, "docs", "readme", "render-charts.mjs"))
	hash.Write(readmeFile(t, "docs", "readme", "chart-data.json"))
	if got := hex.EncodeToString(hash.Sum(nil)); got != manifest.Source {
		t.Error("README graphic inputs changed; run node docs/readme/render-charts.mjs")
	}
	expected := []string{"trust-followup.svg", "productivity-authors.svg", "governance-input-attacks.svg", "workflow.svg"}
	if len(manifest.Assets) != len(expected) {
		t.Fatalf("want %d graphics, got %d", len(expected), len(manifest.Assets))
	}
	for _, file := range expected {
		svg := readmeFile(t, "docs", "readme", file)
		sum := sha256.Sum256(svg)
		if hex.EncodeToString(sum[:]) != manifest.Assets[file] {
			t.Errorf("%s differs from generated output; run node docs/readme/render-charts.mjs", file)
		}
		for _, token := range []string{`role="img"`, "<title", "<desc", "fill:#f0f6fc", "fill:#a4adb8", `fill="#161b22"`} {
			if !strings.Contains(string(svg), token) {
				t.Errorf("%s lacks accessible text or an explicit contrasting palette: %s", file, token)
			}
		}
	}
}

func TestReadmeManualLayout(t *testing.T) {
	readme := string(readmeFile(t, "README.md"))
	for _, section := range []string{"Trust", "Productivity", "Efficiency", "Governance", "Auditability", "What Hive adds", "Install", "Community"} {
		if !strings.Contains(readme, "\n## "+section+"\n") {
			t.Errorf("missing README section %q", section)
		}
	}
	for _, text := range []string{"GitHub Enterprise", "GitHub Projects", "Linear", "Jira", "(CODE_OF_CONDUCT.md)", "(LICENSE)", "(docs/readme/workflow.svg)"} {
		if !strings.Contains(readme, text) {
			t.Errorf("missing README entry %q", text)
		}
	}
	if _, err := UpdateReadme(readme, "legacy layout"); err != ErrNoMarkers {
		t.Error("the legacy benchmark generator must not overwrite the hand-written README")
	}
}
