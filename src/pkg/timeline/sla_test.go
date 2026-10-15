package timeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Delivery SLA derivation: spans from issue journeys, percentile math,
// daily buckets with persistence across restarts and eviction, and the
// stale started-without-PR detector. Everything runs on a fixed clock.

var slaNow = time.Date(2026, 10, 14, 18, 0, 0, 0, time.UTC)

func ms(t time.Time) int64 { return t.UnixMilli() }

// slaJourney builds an issue journey with the given stage instants (zero =
// stage absent). created is the GitHub created_at carried on enumerated.
func slaJourney(ref string, created, classified, started, prOpened, merged time.Time) Journey {
	j := Journey{Ref: ref, Stages: map[Kind]*Stage{}}
	if !created.IsZero() {
		j.Stages[KindEnumerated] = &Stage{FirstAt: ms(created.Add(time.Minute)), LastAt: ms(created.Add(time.Minute)), Count: 1,
			Attrs: map[string]string{AttrIssueCreatedAt: created.Format(time.RFC3339)}}
	}
	if !classified.IsZero() {
		j.Stages[KindClassified] = &Stage{FirstAt: ms(classified), LastAt: ms(classified), Count: 1}
	}
	if !started.IsZero() {
		j.Stages[KindStarted] = &Stage{FirstAt: ms(started), LastAt: ms(started), Count: 1, Agent: "scanner"}
	}
	if !prOpened.IsZero() {
		j.Stages[KindPROpened] = &Stage{FirstAt: ms(prOpened), LastAt: ms(prOpened), Count: 1, Attrs: map[string]string{AttrPRRef: "hive#900"}}
	}
	if !merged.IsZero() {
		j.Stages[KindMerged] = &Stage{FirstAt: ms(merged), LastAt: ms(merged), Count: 1}
	}
	j.Current = deriveCurrent(&j)
	return j
}

func TestDeriveSLASamples_AllSpansFromOneJourney(t *testing.T) {
	created := slaNow.Add(-10 * time.Hour)
	j := slaJourney("hive#1", created, created.Add(time.Hour), created.Add(2*time.Hour), created.Add(4*time.Hour), created.Add(9*time.Hour))
	samples := DeriveSLASamples([]Journey{j})
	got := map[SLASpan]float64{}
	for _, s := range samples {
		if s.Repo != "hive" || s.Ref != "hive#1" {
			t.Fatalf("sample repo/ref = %q/%q", s.Repo, s.Ref)
		}
		got[s.Span] = s.Hours
	}
	want := map[SLASpan]float64{SpanOpenToTriage: 1, SpanTriageToStart: 1, SpanStartToPR: 2, SpanPRToMerge: 5, SpanOpenToMerge: 9}
	if len(got) != len(want) {
		t.Fatalf("spans = %v, want %v", got, want)
	}
	for span, h := range want {
		if got[span] != h {
			t.Errorf("%s = %v h, want %v", span, got[span], h)
		}
	}
}

func TestDeriveSLASamples_SkipsPRKeyedJourneysAndOpenEnds(t *testing.T) {
	// PR-keyed journey (audit bridge): pr_ref == own ref, no issue stages.
	pr := Journey{Ref: "hive#900", Stages: map[Kind]*Stage{
		KindPROpened: {FirstAt: ms(slaNow.Add(-3 * time.Hour)), LastAt: ms(slaNow.Add(-3 * time.Hour)), Attrs: map[string]string{AttrPRRef: "hive#900"}},
		KindMerged:   {FirstAt: ms(slaNow.Add(-time.Hour)), LastAt: ms(slaNow.Add(-time.Hour))},
	}}
	// Issue started but no PR yet: no closed span.
	open := slaJourney("hive#2", time.Time{}, time.Time{}, slaNow.Add(-2*time.Hour), time.Time{}, time.Time{})
	// Out-of-order instants (merge recorded before PR open) are not samples.
	bad := slaJourney("hive#3", time.Time{}, time.Time{}, time.Time{}, slaNow.Add(-time.Hour), slaNow.Add(-2*time.Hour))
	// Issue journey whose only stage is pr_opened naming another PR counts.
	linked := Journey{Ref: "hive#4", Stages: map[Kind]*Stage{
		KindPROpened: {FirstAt: ms(slaNow.Add(-3 * time.Hour)), Attrs: map[string]string{AttrPRRef: "hive#901"}},
		KindMerged:   {FirstAt: ms(slaNow.Add(-time.Hour))},
	}}
	samples := DeriveSLASamples([]Journey{pr, open, bad, linked})
	if len(samples) != 1 || samples[0].Ref != "hive#4" || samples[0].Span != SpanPRToMerge || samples[0].Hours != 2 {
		t.Fatalf("samples = %+v, want one pr_to_merge of 2h on hive#4", samples)
	}
}

func TestSlaIssueJourney_PRWithStartedIsNotIssue(t *testing.T) {
	pr := Journey{Ref: "hive#900", Stages: map[Kind]*Stage{
		KindPROpened: {FirstAt: ms(slaNow.Add(-3 * time.Hour)), Attrs: map[string]string{AttrPRRef: "hive#900"}},
		KindStarted:  {FirstAt: ms(slaNow.Add(-2 * time.Hour))},
		KindMerged:   {FirstAt: ms(slaNow.Add(-time.Hour))},
	}}
	if slaIssueJourney(&pr) {
		t.Fatal("PR journey with a stray started stage must not be an issue journey")
	}
	if samples := DeriveSLASamples([]Journey{pr}); len(samples) != 0 {
		t.Fatalf("samples = %+v, want none", samples)
	}
}

func TestPercentile_NearestRank(t *testing.T) {
	vals := []float64{5, 1, 4, 2, 3}
	cases := []struct {
		p, want float64
	}{{50, 3}, {95, 5}, {100, 5}, {0, 1}, {20, 1}, {40, 2}}
	for _, c := range cases {
		if got := Percentile(vals, c.p); got != c.want {
			t.Errorf("p%v = %v, want %v", c.p, got, c.want)
		}
	}
	if Percentile(nil, 50) != 0 {
		t.Error("empty input must yield 0")
	}
	if Percentile([]float64{7}, 95) != 7 {
		t.Error("single value is every percentile")
	}
}

func TestAggregateSLAWindow_RespectsWindowAndRounds(t *testing.T) {
	samples := []SLASample{
		{Repo: "hive", Span: SpanStartToPR, Hours: 1.234, At: ms(slaNow.Add(-time.Hour))},
		{Repo: "hive", Span: SpanStartToPR, Hours: 3, At: ms(slaNow.Add(-5 * 24 * time.Hour))},
		{Repo: "hive", Span: SpanStartToPR, Hours: 100, At: ms(slaNow.Add(-20 * 24 * time.Hour))},
		{Repo: "hive", Span: SpanStartToPR, Hours: 1000, At: ms(slaNow.Add(time.Hour))}, // future: ignored
	}
	w7 := AggregateSLAWindow(samples, SLAWindows["7d"], slaNow)
	if w7.StartToPR.N != 2 || w7.StartToPR.P50 != 1.23 || w7.StartToPR.P95 != 3 {
		t.Fatalf("7d start_to_pr = %+v", w7.StartToPR)
	}
	w30 := AggregateSLAWindow(samples, SLAWindows["30d"], slaNow)
	if w30.StartToPR.N != 3 || w30.StartToPR.P50 != 3 || w30.StartToPR.P95 != 100 {
		t.Fatalf("30d start_to_pr = %+v", w30.StartToPR)
	}
	if w30.PRToMerge.N != 0 || w30.PRToMerge.P50 != 0 {
		t.Fatalf("span without samples must be zero: %+v", w30.PRToMerge)
	}
}

func TestDeriveSLADays_BucketsByClosingDayPerRepoAndFleet(t *testing.T) {
	d0 := time.Date(2026, 10, 13, 1, 0, 0, 0, time.UTC)
	a := slaJourney("hive#1", time.Time{}, time.Time{}, d0, d0.Add(2*time.Hour), d0.Add(26*time.Hour)) // PR on 13th, merge on 14th
	b := slaJourney("hub#7", time.Time{}, time.Time{}, d0, d0.Add(4*time.Hour), time.Time{})
	journeys := []Journey{a, b}
	days := DeriveSLADays(journeys, DeriveSLASamples(journeys))

	hive13 := days["hive"]["2026-10-13"]
	if hive13 == nil || hive13.NStarted != 1 || hive13.NPR != 1 || hive13.NMerged != 0 || hive13.StartToPRP50 != 2 {
		t.Fatalf("hive 13th = %+v", hive13)
	}
	hive14 := days["hive"]["2026-10-14"]
	if hive14 == nil || hive14.NMerged != 1 || hive14.PRToMergeP50 != 24 || hive14.NPR != 0 {
		t.Fatalf("hive 14th = %+v", hive14)
	}
	fleet13 := days[""]["2026-10-13"]
	if fleet13 == nil || fleet13.NStarted != 2 || fleet13.NPR != 2 || fleet13.StartToPRP50 != 2 {
		t.Fatalf("fleet 13th = %+v (p50 of {2,4} nearest-rank is 2)", fleet13)
	}
	if days["hub"]["2026-10-14"] != nil {
		t.Fatal("hub had nothing on the 14th")
	}
}

func TestSLAHistory_RolloverPersistenceAndEvictionGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sla-history.json")
	h := NewSLAHistory()
	if err := h.EnablePersistence(path, nil); err != nil {
		t.Fatal(err)
	}
	day := func(d string, nPR int, p50 float64) map[string]map[string]*SLADay {
		return map[string]map[string]*SLADay{"hive": {d: {Day: d, NPR: nPR, StartToPRP50: p50}}}
	}
	h.Update(day("2026-10-13", 3, 2.5), slaNow)
	if err := h.Flush(); err != nil {
		t.Fatal(err)
	}
	// A later derivation backed by FEWER events (journeys evicted) must not
	// shrink the stored day; one backed by more replaces it.
	h.Update(day("2026-10-13", 1, 9), slaNow)
	if got := h.Series("hive", slaNow); got[len(got)-2].NPR != 3 || got[len(got)-2].StartToPRP50 != 2.5 {
		t.Fatalf("evicted-journey derivation overwrote the day: %+v", got[len(got)-2])
	}
	h.Update(day("2026-10-13", 4, 3), slaNow)
	if got := h.Series("hive", slaNow); got[len(got)-2].NPR != 4 {
		t.Fatalf("fuller derivation did not replace the day: %+v", got[len(got)-2])
	}
	// Throttle: a write inside slaPersistMinInterval is deferred, Flush forces it.
	if err := h.Flush(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedSLAHistory
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Version != slaHistoryFormatVersion || len(persisted.Repos["hive"]) != 1 || persisted.Repos["hive"][0].NPR != 4 {
		t.Fatalf("persisted = %+v", persisted)
	}

	// Restart: a fresh history loads the file.
	h2 := NewSLAHistory()
	if err := h2.EnablePersistence(path, nil); err != nil {
		t.Fatal(err)
	}
	series := h2.Series("hive", slaNow)
	if len(series) != SLAHistoryDays || series[0].Day != "2026-09-15" || series[SLAHistoryDays-1].Day != "2026-10-14" {
		t.Fatalf("series bounds = %s..%s (%d)", series[0].Day, series[len(series)-1].Day, len(series))
	}
	if series[SLAHistoryDays-2].NPR != 4 || series[SLAHistoryDays-1].NPR != 0 {
		t.Fatalf("loaded series wrong: %+v", series[SLAHistoryDays-2:])
	}

	// Rollover: 31 days later the day falls off the series and the file.
	later := slaNow.AddDate(0, 0, 31)
	h2.Update(map[string]map[string]*SLADay{}, later)
	for _, d := range h2.Series("hive", later) {
		if d.NPR != 0 {
			t.Fatalf("day %s survived the 30-day prune", d.Day)
		}
	}
	if len(h2.Repos()) != 0 {
		t.Fatalf("repos after prune = %v", h2.Repos())
	}
}

func TestSLAHistory_CorruptFileMovedAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sla-history.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewSLAHistory()
	if err := h.EnablePersistence(path, nil); err != nil {
		t.Fatalf("corrupt file must not fail startup: %v", err)
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt file not moved aside: %v", err)
	}
}

func TestDeriveSLAStale_StartedWithoutPRPastTTL(t *testing.T) {
	ttl := 2 * time.Hour
	stale := slaJourney("hive#1", time.Time{}, time.Time{}, slaNow.Add(-3*time.Hour), time.Time{}, time.Time{})
	fresh := slaJourney("hive#2", time.Time{}, time.Time{}, slaNow.Add(-time.Hour), time.Time{}, time.Time{})
	withPR := slaJourney("hive#3", time.Time{}, time.Time{}, slaNow.Add(-5*time.Hour), slaNow.Add(-4*time.Hour), time.Time{})
	ancient := slaJourney("hive#4", time.Time{}, time.Time{}, slaNow.Add(-40*24*time.Hour), time.Time{}, time.Time{})
	older := slaJourney("hub#9", time.Time{}, time.Time{}, slaNow.Add(-6*time.Hour), time.Time{}, time.Time{})
	got := DeriveSLAStale([]Journey{stale, fresh, withPR, ancient, older}, ttl, slaNow)
	if len(got) != 2 || got[0].Repo != "hub" || got[0].Number != 9 || got[1].Repo != "hive" || got[1].Number != 1 {
		t.Fatalf("stale = %+v", got)
	}
	if got[1].StartedAt != "2026-10-14T15:00:00Z" || got[1].Agent != "scanner" {
		t.Fatalf("stale entry = %+v", got[1])
	}
	if len(DeriveSLAStale([]Journey{stale}, 0, slaNow)) != 0 {
		t.Fatal("zero ttl disables the check")
	}
}

func TestSLATracker_ReportFleetAndRepos(t *testing.T) {
	created := slaNow.Add(-30 * time.Hour)
	a := slaJourney("hive#1", created, created.Add(time.Hour), created.Add(2*time.Hour), created.Add(6*time.Hour), created.Add(20*time.Hour))
	b := slaJourney("hub#2", time.Time{}, time.Time{}, slaNow.Add(-5*time.Hour), time.Time{}, time.Time{})
	tr := NewSLATracker()
	rep := tr.Report([]Journey{a, b}, []string{"org/hive", "org/hub", "quiet"}, 2*time.Hour, slaNow)

	if rep.Fleet.Windows["7d"].StartToPR.N != 1 || rep.Fleet.Windows["7d"].StartToPR.P50 != 4 {
		t.Fatalf("fleet 7d start_to_pr = %+v", rep.Fleet.Windows["7d"].StartToPR)
	}
	if rep.Fleet.Windows["30d"].OpenToMerge.P50 != 20 || rep.Fleet.MergedLast7d != 1 {
		t.Fatalf("fleet 30d = %+v merged7d=%d", rep.Fleet.Windows["30d"].OpenToMerge, rep.Fleet.MergedLast7d)
	}
	if len(rep.Fleet.StaleInProgress) != 1 || rep.Fleet.StaleInProgress[0].Number != 2 {
		t.Fatalf("fleet stale = %+v", rep.Fleet.StaleInProgress)
	}
	for _, name := range []string{"hive", "hub", "quiet"} {
		if _, ok := rep.Repos[name]; !ok {
			t.Fatalf("repo %q missing from report: %v", name, rep.Repos)
		}
	}
	if rep.Repos["hive"].Windows["7d"].PRToMerge.P50 != 14 || rep.Repos["hive"].MergedLast7d != 1 {
		t.Fatalf("hive = %+v", rep.Repos["hive"].Windows["7d"])
	}
	if len(rep.Repos["hive"].StaleInProgress) != 0 || len(rep.Repos["hub"].StaleInProgress) != 1 {
		t.Fatal("stale entries must be scoped per repo")
	}
	if q := rep.Repos["quiet"]; q.Windows["7d"].StartToPR.N != 0 || len(q.History) != SLAHistoryDays || len(q.StaleInProgress) != 0 {
		t.Fatalf("quiet repo must have an empty, non-null report: %+v", q)
	}
	today := rep.Repos["hive"].History[SLAHistoryDays-1]
	if today.Day != "2026-10-14" || today.NMerged != 1 || today.PRToMergeP50 != 14 {
		t.Fatalf("today's bucket = %+v", today)
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"fleet"`, `"repos"`, `"windows"`, `"7d"`, `"30d"`, `"history"`, `"stale_in_progress"`, `"start_to_pr_p50"`, `"n_started"`, `"p50_h"`, `"p95_h"`} {
		if !containsBytes(data, key) {
			t.Errorf("payload missing %s", key)
		}
	}
}

func containsBytes(b []byte, s string) bool {
	return len(b) >= len(s) && (string(b) == s || indexOf(string(b), s) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestStore_LinkedIssueRefs(t *testing.T) {
	s := NewStore()
	s.Record(Event{IssueRef: "hive#900", Kind: KindPROpened, At: 1, Attrs: map[string]string{AttrPRRef: "hive#900"}})
	s.Record(Event{IssueRef: "hive#1", Kind: KindPROpened, At: 1, Attrs: map[string]string{AttrPRRef: "hive#900"}})
	s.Record(Event{IssueRef: "hive#2", Kind: KindPROpened, At: 1, Attrs: map[string]string{AttrPRRef: "hive#900"}})
	s.Record(Event{IssueRef: "hive#3", Kind: KindPROpened, At: 1, Attrs: map[string]string{AttrPRRef: "hive#901"}})
	got := s.LinkedIssueRefs("hive#900")
	if len(got) != 2 || got[0] != "hive#1" || got[1] != "hive#2" {
		t.Fatalf("linked = %v", got)
	}
	if s.LinkedIssueRefs("") != nil || (*Store)(nil).LinkedIssueRefs("x") != nil {
		t.Fatal("empty/nil guards")
	}
}
