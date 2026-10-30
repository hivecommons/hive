package outcomebench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestCommittedReportAssets regenerates the benchmark from its JSON.
// The hand-written root README has separate snapshot and asset checks.
func TestCommittedReportAssets(t *testing.T) {
	docDir := filepath.Join("..", "..", "docs", "benchmarks")
	reports, err := filepath.Glob(filepath.Join(docDir, "outcomes-*.json"))
	if err != nil || len(reports) == 0 {
		t.Skipf("no committed report under %s (%v)", docDir, err)
	}
	sort.Strings(reports)
	latest := reports[len(reports)-1]
	data, err := os.ReadFile(latest)
	if err != nil {
		t.Fatal(err)
	}
	var rep Report
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("%s: %v", latest, err)
	}
	base := strings.TrimSuffix(latest, ".json")
	check := func(path, want string) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Errorf("%s drifted from %s; regenerate with src/scripts/outcome-benchmark.sh", path, latest)
		}
	}
	check(base+".md", rep.Markdown())
	check(filepath.Join(docDir, "assets", rep.AssetBase()+"-bars.svg"), rep.BarChartSVG())
	check(filepath.Join(docDir, "assets", rep.AssetBase()+"-trend.svg"), rep.TrendSVG())
	for file, svg := range rep.Cards() {
		check(filepath.Join(docDir, "assets", file), svg)
		pngPath := filepath.Join(docDir, "assets", strings.TrimSuffix(file, ".svg")+".png")
		b, err := os.ReadFile(pngPath)
		if err != nil {
			t.Errorf("%s: %v (regenerate with src/scripts/outcome-benchmark.sh)", pngPath, err)
			continue
		}
		if got, want := PNGText(b, PNGSourceKey), SourceHash(svg); got != want {
			t.Errorf("%s was rendered from a different SVG (source %.12s, want %.12s); regenerate with src/scripts/outcome-benchmark.sh", pngPath, got, want)
		}
		w, h, err := PNGSize(b)
		if err != nil {
			t.Errorf("%s: %v", pngPath, err)
		} else if wantW, wantH := svgSize(svg); w != wantW || h != wantH {
			t.Errorf("%s is %dx%d, SVG declares %dx%d", pngPath, w, h, wantW, wantH)
		}
	}
	// A card the generator no longer emits must not linger as a stale asset.
	stale, _ := filepath.Glob(filepath.Join(docDir, "assets", rep.AssetBase()+"-card-*.*"))
	for _, p := range stale {
		svgName := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)) + ".svg"
		if _, ok := rep.Cards()[svgName]; !ok {
			t.Errorf("%s is not emitted by the generator any more; delete it", p)
		}
	}
}

// svgSize reads the declared width/height of a generated card SVG.
func svgSize(svg string) (w, h int) {
	fmt.Sscanf(svg[strings.Index(svg, ` width="`):], ` width="%d" height="%d"`, &w, &h)
	return w, h
}
