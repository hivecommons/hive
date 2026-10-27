package outcomebench

import (
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
	for _, want := range []string{"<title>", `<text`, "ACCURACY · 15 PULL REQUESTS SENT BACK · (oct &#39;26)", "The review agent sent 15 pull requests", "Three in four", ">3 in 4<", ">20 min<", "15 sent back (oct &#39;26); 4 merged as-is", `filter="url(#glow)"`, `width="1760"`, `viewBox="0 0 880 `} {
		if !strings.Contains(svg, want) {
			t.Errorf("accuracy card missing %q:\n%s", want, svg)
		}
	}
	if light, _ := rep.Card(CardAccuracy, cardThemes[1]); strings.Contains(light, `filter="url(#glow)"`) {
		t.Error("light theme must not glow")
	}
	if len(rep.Cards()) != 2*5 { // accuracy, checks-green, cost, also, how-it-works × dark/light
		t.Errorf("Cards() = %d files: %v", len(rep.Cards()), rep.Cards())
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
	if !ok || !strings.Contains(svg, ">97%<") || !strings.Contains(svg, ">1 in 12<") || !strings.Contains(svg, "We hid 63 texts that try to redirect an") || !strings.Contains(svg, "TEST SET · (oct &#39;26)") || !strings.Contains(svg, "75 texts (oct &#39;26)") || !strings.Contains(svg, "not production traffic") {
		t.Fatalf("prompt-injection card:\n%s", svg)
	}
}
