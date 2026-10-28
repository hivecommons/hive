package outcomebench

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCardsRespectMinimumBase(t *testing.T) {
	since, until, _ := ParseWindow("2026-10")
	opts := Options{Since: since, Until: until, Now: now, Spend: &Spend{USD: 26, MergedPRs: 13}}
	// The fixture has 13 reviewed merges: below the default README base, so
	// no production-count card is drawn, but the ladder always is.
	rep := Compute(fixture(), 0, opts)
	if rep.MinBase() != DefaultReadmeMinBase {
		t.Fatalf("MinBase = %d", rep.MinBase())
	}
	for _, c := range []string{CardAccuracy, CardChecksGreen, CardCost, CardAlso, CardStories, CardPromptInjection} {
		if rep.HasCard(c) {
			t.Errorf("%s drawn with base below %d", c, DefaultReadmeMinBase)
		}
	}
	if !rep.HasCard(CardHowItWorks) {
		t.Error("how-it-works card is data-free and must always render")
	}
	// Lowering the threshold draws the count cards; cost is never below base
	// because MergedPRs is its own base.
	opts.ReadmeMinBase = 10
	rep = Compute(fixture(), 0, opts)
	for _, c := range []string{CardAccuracy, CardCost, CardAlso} {
		if !rep.HasCard(c) {
			t.Errorf("%s not drawn at min base 10", c)
		}
	}
	// The fixture records a check for every reviewed merge, so checks-green
	// is drawn; dropping coverage below the floor must suppress it.
	if !rep.HasCard(CardChecksGreen) {
		t.Error("checks-green not drawn with full CI coverage")
	}
	low := rep
	low.Mediated.CICoverage.Rate = minCICardCoverage - 0.01
	if low.HasCard(CardChecksGreen) {
		t.Error("checks-green drawn with CI coverage below the floor")
	}
	svg, _ := rep.Card(CardAccuracy, cardThemes[0])
	for _, want := range []string{"<title>", `<text`, "ACCURACY · 15 PULL REQUESTS SENT BACK · (oct &#39;26)", "The review agent sent 15 pull requests", "Three out of four", ">3 in 4<", "usually within 20 minutes", "fine on a second look", "15 pull requests sent back (oct &#39;26)", `width="1760"`, `viewBox="0 0 880 500"`, `font-weight="700"`} {
		if !strings.Contains(svg, want) {
			t.Errorf("accuracy card missing %q:\n%s", want, svg)
		}
	}
	// Flat colour only: no gradient, glow or filter, which render
	// differently per browser and are not reproduced in the PNG.
	for _, bad := range []string{"<filter", "<linearGradient", "url(#", "<defs>"} {
		if strings.Contains(svg, bad) {
			t.Errorf("accuracy card contains %q; cards are flat", bad)
		}
	}
	if len(rep.Cards()) != 2*5 { // accuracy, checks-green, cost, also, how-it-works × dark/light
		t.Errorf("Cards() = %d files: %v", len(rep.Cards()), rep.Cards())
	}
	// Every stat card is the same height so the README reads as a set.
	for _, name := range []string{CardAccuracy, CardChecksGreen, CardCost} {
		if c, _ := rep.card(name, cardThemes[0]); c.h != statCardH {
			t.Errorf("%s card is %d tall, want %d", name, c.h, statCardH)
		}
	}
}

// TestCardPNGs renders every card to PNG with the embedded face and checks
// each one is the SVG's size at cardScale, carries its source hash, and
// has opaque card pixels inside a transparent rounded corner.
func TestCardPNGs(t *testing.T) {
	since, until, _ := ParseWindow("2026-10")
	opts := Options{Since: since, Until: until, Now: now, Spend: &Spend{USD: 26, MergedPRs: 13}, ReadmeMinBase: 10}
	rep := Compute(fixture(), 0, opts)
	pngs, err := rep.CardPNGs()
	if err != nil {
		t.Fatal(err)
	}
	svgs := rep.Cards()
	if len(pngs) != len(svgs) {
		t.Fatalf("%d PNGs for %d SVGs", len(pngs), len(svgs))
	}
	for name, svg := range svgs {
		pngName := strings.TrimSuffix(name, ".svg") + ".png"
		b, ok := pngs[pngName]
		if !ok {
			t.Errorf("no PNG for %s", name)
			continue
		}
		if got, want := PNGText(b, PNGSourceKey), SourceHash(svg); got != want {
			t.Errorf("%s source hash %q, want %q", pngName, got, want)
		}
		w, h, err := PNGSize(b)
		if err != nil {
			t.Fatal(err)
		}
		var sw, sh int
		fmt.Sscanf(svg[strings.Index(svg, ` width="`):], ` width="%d" height="%d"`, &sw, &sh)
		if w != sw || h != sh || w != cardW*cardScale {
			t.Errorf("%s is %dx%d, SVG declares %dx%d", pngName, w, h, sw, sh)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
			t.Errorf("%s corner pixel is not transparent", pngName)
		}
		if _, _, _, a := img.At(w/2, h/2).RGBA(); a == 0 {
			t.Errorf("%s centre pixel is transparent", pngName)
		}
	}
	if len(pngs["x"]) != 0 {
		t.Error("unexpected key")
	}
}

func TestPNGTextRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	out, err := addPNGText(buf.Bytes(), "k", "v")
	if err != nil {
		t.Fatal(err)
	}
	if PNGText(out, "k") != "v" || PNGText(out, "other") != "" {
		t.Error("tEXt chunk round trip failed")
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("PNG with tEXt chunk does not decode: %v", err)
	}
	if _, err := addPNGText([]byte("nope"), "k", "v"); err == nil {
		t.Error("expected error for non-PNG input")
	}
}

func TestCardHelpers(t *testing.T) {
	for in, want := range map[int]string{7: "7", 1455: "1,455", 11571: "11,571", 1234567: "1,234,567"} {
		if got := commas(in); got != want {
			t.Errorf("commas(%d) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[float64]string{0.3368: "1 in 3", 0.5: "1 in 2", 0.1: "1 in 10", 0.024: "1 in 42"} {
		if got := oneIn(in); got != want {
			t.Errorf("oneIn(%v) = %q, want %q", in, got, want)
		}
	}
	if minutes(0.4418) != 27 || minutes(0.1917) != 12 {
		t.Error("minutes rounding")
	}
	lines := wrap("the quick brown fox jumps over the lazy dog", 15)
	for _, l := range lines {
		if len(l) > 15 && !strings.Contains(l, " ") == false {
			t.Errorf("line too long: %q", l)
		}
	}
	if strings.Join(lines, " ") != "the quick brown fox jumps over the lazy dog" {
		t.Errorf("wrap lost words: %v", lines)
	}
}

func TestLoadStoriesAndRedTeam(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("stories.json", `[{"repo":"hivecommons/hive","number":1,"caught":"x","opened_at":"2026-10-01T00:00:00Z","caught_at":"2026-10-01T00:05:00Z","fix_approved_at":"2026-10-01T00:09:00Z","merged_at":"2026-10-01T00:16:00Z"}]`)
	stories, err := LoadStories(good)
	if err != nil || len(stories) != 1 || stories[0].FixMinutes() != 4 || stories[0].MergeMinutes() != 16 {
		t.Fatalf("LoadStories: %v %+v", err, stories)
	}
	if _, err := LoadStories(write("bad-order.json", `[{"repo":"a/b","number":1,"caught":"x","opened_at":"2026-10-01T00:10:00Z","caught_at":"2026-10-01T00:05:00Z","fix_approved_at":"2026-10-01T00:09:00Z","merged_at":"2026-10-01T00:16:00Z"}]`)); err == nil {
		t.Error("out-of-order timeline accepted")
	}
	if _, err := LoadStories(write("bad-fields.json", `[{"repo":"a/b"}]`)); err == nil {
		t.Error("story without number/caught accepted")
	}
	rt, err := LoadRedTeam(write("rt.json", `{"attacks":63,"caught":61,"benign":12,"false_alarms":1,"run_at":"2026-10-23T00:00:00Z"}`))
	if err != nil || rt.Attacks != 63 || rt.RunAt.IsZero() {
		t.Fatalf("LoadRedTeam: %v %+v", err, rt)
	}
	if _, err := LoadRedTeam(write("rt-bad.json", `{"attacks":10,"caught":11,"benign":12,"false_alarms":1}`)); err == nil {
		t.Error("caught > attacks accepted")
	}
	rep := Report{RedTeam: rt, GeneratedAt: now}
	svg, ok := rep.Card(CardPromptInjection, cardThemes[1])
	if !ok || !strings.Contains(svg, ">97%<") || !strings.Contains(svg, "harmless text in 12") || !strings.Contains(svg, "We planted 63 texts that try to redirect an") || !strings.Contains(svg, "TEST SET · (oct &#39;26)") || !strings.Contains(svg, "75 texts (oct &#39;26)") || !strings.Contains(svg, "not production traffic") {
		t.Fatalf("prompt-injection card:\n%s", svg)
	}
}
