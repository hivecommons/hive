package evidence

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestExpectedOpensMergesBand(t *testing.T) {
	cases := []struct {
		mode     string
		wantMode string
		min, max float64
		label    string
	}{
		{"SURGE", CadenceSurge, 0, OpensMergesDrainMax, "≤ 1"},
		{"busy", CadenceBusy, 0, OpensMergesDrainMax, "≤ 1"},
		{" Quiet ", CadenceQuiet, OpensMergesScoutMin, OpensMergesScoutMax, "1–2"},
		{"IDLE", CadenceIdle, OpensMergesScoutMin, OpensMergesScoutMax, "1–2"},
		{"", CadenceUnknown, OpensMergesParity - OpensMergesParityTolerance, OpensMergesParity + OpensMergesParityTolerance, "0.75–1.25"},
		{"weird", CadenceUnknown, OpensMergesParity - OpensMergesParityTolerance, OpensMergesParity + OpensMergesParityTolerance, "0.75–1.25"},
	}
	for _, c := range cases {
		b := ExpectedOpensMergesBand(c.mode)
		if b.Mode != c.wantMode || b.Min != c.min || b.Max != c.max || b.Label != c.label || b.Why == "" {
			t.Errorf("ExpectedOpensMergesBand(%q) = %+v", c.mode, b)
		}
	}
}

func TestOpensMergesBandVerdict(t *testing.T) {
	cases := []struct {
		mode          string
		opens, merges int
		want          string
	}{
		{CadenceSurge, 0, 0, OpensMergesNoData},
		{CadenceSurge, 5, 0, OpensMergesAbove},
		{CadenceSurge, 0, 5, OpensMergesInBand},
		{CadenceSurge, 8, 10, OpensMergesInBand},
		{CadenceSurge, 10, 10, OpensMergesInBand}, // ≤ 1 is inclusive
		{CadenceBusy, 11, 10, OpensMergesAbove},
		{CadenceQuiet, 16, 10, OpensMergesInBand},
		{CadenceQuiet, 20, 10, OpensMergesInBand},
		{CadenceQuiet, 27, 10, OpensMergesAbove},
		{CadenceIdle, 8, 10, OpensMergesBelow},
		{CadenceUnknown, 10, 10, OpensMergesInBand},
		{CadenceUnknown, 13, 10, OpensMergesAbove},
		{CadenceUnknown, 7, 10, OpensMergesBelow},
	}
	for _, c := range cases {
		if got := ExpectedOpensMergesBand(c.mode).Verdict(c.opens, c.merges); got != c.want {
			t.Errorf("%s %d:%d = %q, want %q", c.mode, c.opens, c.merges, got, c.want)
		}
	}
}

func TestBuildOpensMerges(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	windowFrom := now.Add(-24 * time.Hour)
	inWindow := func(at time.Time) bool { return !at.Before(windowFrom) && !at.After(now) }
	h := func(hours int) time.Time { return now.Add(-time.Duration(hours) * time.Hour) }
	events := []OpensMergesEvent{
		{Repo: "O/R", At: h(1)},
		{Repo: "o/r", At: h(2)},
		{Repo: "o/r", At: h(3), Merge: true},
		{Repo: "x/y", At: h(4), Merge: true},
		{Repo: "x/y", At: h(5), Merge: true},
		// outside the 24h window but inside the rolling 30 days
		{Repo: "o/r", At: h(48)},
		{Repo: "o/r", At: h(49), Merge: true},
		// outside the rolling span and in the future: ignored by the series
		{Repo: "o/r", At: h(24 * 31)},
		{Repo: "o/r", At: now.Add(time.Hour), Merge: true},
		// no repo / no time: dropped
		{Repo: "", At: h(1)},
		{Repo: "o/r"},
	}
	got := BuildOpensMerges(OpensMergesInput{Events: events, Mode: "QUIET", InWindow: inWindow, Now: now})

	if got.Mode != CadenceQuiet || got.Expected.Label != "1–2" || got.N != 5 {
		t.Fatalf("mode/n = %+v", got)
	}
	if got.Total.Opens != 2 || got.Total.Merges != 3 || got.Total.Ratio == nil || math.Abs(*got.Total.Ratio-2.0/3) > 1e-9 {
		t.Errorf("total = %+v", got.Total)
	}
	if got.Status != OpensMergesBelow || got.Headline != "0.67 opens per merge · quiet mode expects 1–2 ⚠ (scouting below cadence)" {
		t.Errorf("status %q headline %q", got.Status, got.Headline)
	}
	if len(got.Repos) != 2 || got.Repos[0].Repo != "o/r" || got.Repos[0].Opens != 2 || got.Repos[0].Merges != 1 || *got.Repos[0].Ratio != 2 {
		t.Errorf("repos = %+v", got.Repos)
	}
	if got.Repos[1].Repo != "x/y" || got.Repos[1].Opens != 0 || got.Repos[1].Merges != 2 || *got.Repos[1].Ratio != 0 {
		t.Errorf("repos[1] = %+v", got.Repos[1])
	}
	if got.Rolling30d.Opens != 3 || got.Rolling30d.Merges != 4 || got.Rolling30dStatus != OpensMergesInBand || got.Rolling30dBand.Label != "0.75–1.25" {
		t.Errorf("rolling = %+v status %q band %+v", got.Rolling30d, got.Rolling30dStatus, got.Rolling30dBand)
	}
	if len(got.Rolling30dDays) != OpensMergesRollingDays || got.Rolling30dDays[0].Day != "2026-09-25" || got.Rolling30dDays[29].Day != "2026-10-24" {
		t.Fatalf("rolling days = %d first %q last %q", len(got.Rolling30dDays), got.Rolling30dDays[0].Day, got.Rolling30dDays[29].Day)
	}
	if d := got.Rolling30dDays[29]; d.Opens != 2 || d.Merges != 3 {
		t.Errorf("today = %+v", d)
	}
	if d := got.Rolling30dDays[27]; d.Opens != 1 || d.Merges != 1 {
		t.Errorf("two days ago = %+v", d)
	}

	// No window predicate means everything counts; no events is no data.
	all := BuildOpensMerges(OpensMergesInput{Events: events, Mode: "surge", Now: now})
	if all.Total.Opens != 4 || all.Total.Merges != 5 || all.Status != OpensMergesInBand || !strings.HasSuffix(all.Headline, "surge mode expects ≤ 1 ✓") {
		t.Errorf("all = %+v", all)
	}
	empty := BuildOpensMerges(OpensMergesInput{Now: now})
	if empty.Status != OpensMergesNoData || empty.Total.Ratio != nil || empty.Headline != "no hive issue opens or PR merges · unknown mode expects 0.75–1.25" || len(empty.Repos) != 0 {
		t.Errorf("empty = %+v", empty)
	}
	if empty.Rolling30dStatus != OpensMergesNoData || len(empty.Rolling30dDays) != OpensMergesRollingDays {
		t.Errorf("empty rolling = %q %d", empty.Rolling30dStatus, len(empty.Rolling30dDays))
	}
}

func TestOpensMergesJSONShape(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	s := BuildOpensMerges(OpensMergesInput{Events: []OpensMergesEvent{{Repo: "o/r", At: now}, {Repo: "o/r", At: now, Merge: true}}, Mode: "BUSY", Now: now})
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"mode", "expected", "status", "headline", "n", "total", "repos", "rolling_30d", "rolling_30d_expected", "rolling_30d_status", "rolling_30d_days"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing %q in %s", key, raw)
		}
	}
	var total struct {
		Repo   *string  `json:"repo"`
		Ratio  *float64 `json:"ratio"`
		Opens  int      `json:"opens"`
		Merges int      `json:"merges"`
	}
	if err := json.Unmarshal(m["total"], &total); err != nil {
		t.Fatal(err)
	}
	if total.Repo != nil || total.Ratio == nil || *total.Ratio != 1 || total.Opens != 1 || total.Merges != 1 {
		t.Errorf("total = %+v (%s)", total, m["total"])
	}
	var repos []map[string]json.RawMessage
	if err := json.Unmarshal(m["repos"], &repos); err != nil || len(repos) != 1 || string(repos[0]["repo"]) != `"o/r"` {
		t.Errorf("repos = %s (%v)", m["repos"], err)
	}
	if string(m["mode"]) != `"busy"` || string(m["status"]) != `"in_band"` {
		t.Errorf("mode %s status %s", m["mode"], m["status"])
	}
	// A row without merges serialises ratio as null, never 0 or ∞.
	none := BuildOpensMerges(OpensMergesInput{Events: []OpensMergesEvent{{Repo: "o/r", At: now}}, Now: now})
	raw, _ = json.Marshal(none.Total)
	if !strings.Contains(string(raw), `"ratio":null`) {
		t.Errorf("ratio must be null without merges: %s", raw)
	}
}

func TestGradeOpensMerges(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	build := func(mode string, opens, merges int) OpensMerges {
		var events []OpensMergesEvent
		for i := 0; i < opens; i++ {
			events = append(events, OpensMergesEvent{Repo: "o/r", At: now})
		}
		for i := 0; i < merges; i++ {
			events = append(events, OpensMergesEvent{Repo: "o/r", At: now, Merge: true})
		}
		return BuildOpensMerges(OpensMergesInput{Events: events, Mode: mode, Now: now})
	}
	cases := []struct {
		name          string
		mode          string
		opens, merges int
		letter        string
		band          string
		enough        bool
	}{
		{"too few", CadenceSurge, 5, 10, NoGradeLetter, BandNone, false},
		{"surge drain in band", CadenceSurge, 8, 12, "A+", BandA, true},
		{"surge at parity", CadenceSurge, 10, 10, "A+", BandA, true},
		{"surge 10% over", CadenceSurge, 11, 10, "A-", BandA, true},
		{"busy 20% over", CadenceBusy, 12, 10, "B-", BandB, true},
		{"busy 30% over", CadenceBusy, 13, 10, "C-", BandC, true},
		{"quiet scouting in band", CadenceQuiet, 16, 10, "A+", BandA, true},
		{"quiet at ceiling", CadenceQuiet, 20, 10, "A+", BandA, true},
		{"quiet 10% over ceiling", CadenceQuiet, 22, 10, "A-", BandA, true},
		{"quiet 35% over ceiling", CadenceQuiet, 27, 10, "C-", BandC, true},
		{"idle 10% under floor", CadenceIdle, 18, 20, "A-", BandA, true},
		{"idle 20% under floor", CadenceIdle, 16, 20, "B-", BandB, true},
		{"no merges at all", CadenceQuiet, 25, 0, "C-", BandC, true},
		{"unknown parity", "", 10, 10, "A+", BandA, true},
		{"unknown 20% over tolerance", "", 15, 10, "B-", BandB, true},
	}
	for _, c := range cases {
		s := build(c.mode, c.opens, c.merges)
		g := GradeOpensMerges(s)
		if g.Letter != c.letter || g.Band != c.band || g.EnoughData != c.enough {
			t.Errorf("%s: grade = %+v", c.name, g)
		}
		if g.Rubric != opensMergesRubric || g.Reason == "" {
			t.Errorf("%s: rubric/reason = %q / %q", c.name, g.Rubric, g.Reason)
		}
		if c.enough && g.Reason != s.Headline {
			t.Errorf("%s: reason %q should be the headline %q", c.name, g.Reason, s.Headline)
		}
		if !c.enough && !strings.Contains(g.Reason, "needs 20 hive issue opens + PR merges") {
			t.Errorf("%s: reason = %q", c.name, g.Reason)
		}
	}
	if !strings.Contains(opensMergesRubric, "surge/busy ≤ 1, quiet/idle 1–2, unknown 0.75–1.25") {
		t.Errorf("rubric must name the bands: %q", opensMergesRubric)
	}
}

func TestSummarizeOpensMerges(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		s    OpensMerges
		want string
	}{
		{"empty", BuildOpensMerges(OpensMergesInput{Mode: CadenceQuiet, Now: now}), "No hive issue opens or PR merges in this window."},
		{"in band", BuildOpensMerges(OpensMergesInput{Mode: CadenceQuiet, Now: now, Events: []OpensMergesEvent{{Repo: "o/r", At: now}, {Repo: "o/r", At: now}, {Repo: "o/r", At: now}, {Repo: "o/r", At: now, Merge: true}, {Repo: "o/r", At: now, Merge: true}}}), "3 opened for 2 merged (1.5 per merge), inside the quiet band 1–2."},
		{"above", BuildOpensMerges(OpensMergesInput{Mode: CadenceSurge, Now: now, Events: []OpensMergesEvent{{Repo: "o/r", At: now}, {Repo: "o/r", At: now}, {Repo: "o/r", At: now, Merge: true}}}), "2 opened for 1 merged (2 per merge), above the surge band ≤ 1."},
		{"no merges", BuildOpensMerges(OpensMergesInput{Mode: CadenceIdle, Now: now, Events: []OpensMergesEvent{{Repo: "o/r", At: now}}}), "1 opened for 0 merged (∞ per merge), above the idle band 1–2."},
		{"below", BuildOpensMerges(OpensMergesInput{Mode: CadenceIdle, Now: now, Events: []OpensMergesEvent{{Repo: "o/r", At: now, Merge: true}}}), "0 opened for 1 merged (0 per merge), below the idle band 1–2."},
	}
	for _, c := range cases {
		got := SummarizeOpensMerges(c.s)
		if got != c.want {
			t.Errorf("%s: got %q\nwant %q", c.name, got, c.want)
		}
		if words(got) > PlainSummaryMaxWords {
			t.Errorf("%s: summary has %d words: %q", c.name, words(got), got)
		}
	}
}

func TestOpensMergesRatioText(t *testing.T) {
	cases := map[float64]string{0: "0", 1: "1", 0.5: "0.5", 2.0 / 3: "0.67", 1.25: "1.25", 2: "2", math.Inf(1): "∞"}
	for in, want := range cases {
		if got := ratioText(in); got != want {
			t.Errorf("ratioText(%v) = %q, want %q", in, got, want)
		}
	}
}
