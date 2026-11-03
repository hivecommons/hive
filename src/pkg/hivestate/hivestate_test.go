package hivestate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func writeRaw(t *testing.T, dir string, at time.Time, s Snapshot) {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", at.UnixMilli())), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func fixedStore(dir string, now *time.Time) *Store {
	s := NewStore(dir)
	s.now = func() time.Time { return *now }
	return s
}

func TestFromSnapshot(t *testing.T) {
	b := FromSnapshot(t0, Snapshot{
		QueueIssues: 3, QueuePRs: 2, QueueHold: 1, SLAViolations: 4,
		AgentsKicked: []string{"a", "b"},
		AgentStates:  map[string]string{"a": "idle", "b": "running", "c": "stopped", "d": "paused"},
		TotalTokens:  100,
	})
	if b.Samples != 1 || b.SLAViolatedSamples != 1 || b.Kicks != 2 || b.AgentSamples != 2 || b.IdleAgentSamples != 1 {
		t.Fatalf("bucket = %+v", b)
	}
	if b.FirstTokens != 100 || b.LastTokens != 100 || b.Tokens != 0 || !b.Start.Equal(t0) || !b.Last.Equal(t0) {
		t.Fatalf("bucket = %+v", b)
	}
	if b := FromSnapshot(t0, Snapshot{}); b.SLAViolatedSamples != 0 {
		t.Fatalf("no-violation sample counted: %+v", b)
	}
}

func TestModeSamples(t *testing.T) {
	if b := FromSnapshot(t0, Snapshot{}); b.ModeSamples != nil {
		t.Fatalf("mode counted without a mode: %+v", b)
	}
	first := FromSnapshot(t0, Snapshot{Mode: "surge"})
	var agg Bucket
	agg.merge(first)
	agg.merge(FromSnapshot(t0.Add(time.Minute), Snapshot{Mode: "surge"}))
	agg.merge(FromSnapshot(t0.Add(2*time.Minute), Snapshot{Mode: "idle"}))
	agg.merge(FromSnapshot(t0.Add(3*time.Minute), Snapshot{}))
	if agg.ModeSamples["surge"] != 2 || agg.ModeSamples["idle"] != 1 || agg.Samples != 4 {
		t.Fatalf("agg = %+v", agg)
	}
	if first.ModeSamples["surge"] != 1 {
		t.Fatalf("merge aliased the source map: %+v", first)
	}

	// Buckets written before mode_samples existed still decode and merge.
	var old Bucket
	if err := json.Unmarshal([]byte(`{"start":"2026-10-01T12:00:00Z","last":"2026-10-01T12:30:00Z","samples":2}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.ModeSamples != nil {
		t.Fatalf("old bucket = %+v", old)
	}
	var mixed Bucket
	mixed.merge(old)
	mixed.merge(first)
	old.merge(first)
	if mixed.Samples != 3 || mixed.ModeSamples["surge"] != 1 || old.ModeSamples["surge"] != 1 {
		t.Fatalf("mixed = %+v old = %+v", mixed, old)
	}
}

func TestMergeTokensIgnoresDrops(t *testing.T) {
	var agg Bucket
	agg.merge(Bucket{})
	if agg.Samples != 0 {
		t.Fatal("empty merge added samples")
	}
	for i, tok := range []int64{100, 150, 120, 200} {
		agg.merge(FromSnapshot(t0.Add(time.Duration(i)*time.Minute), Snapshot{TotalTokens: tok}))
	}
	// +50, drop ignored, +80
	if agg.Tokens != 130 || agg.FirstTokens != 100 || agg.LastTokens != 200 || agg.Samples != 4 {
		t.Fatalf("agg = %+v", agg)
	}
	if !agg.Last.Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("last = %v", agg.Last)
	}
}

func TestQuantiles(t *testing.T) {
	if q := quantiles(nil); q != (Quantiles{}) {
		t.Fatalf("empty = %+v", q)
	}
	if q := quantiles([]int{7}); q != (Quantiles{Median: 7, P90: 7}) {
		t.Fatalf("single = %+v", q)
	}
	in := []int{10, 1, 9, 2, 8, 3, 7, 4, 6, 5}
	if q := quantiles(in); q != (Quantiles{Median: 5, P90: 9}) {
		t.Fatalf("1..10 = %+v", q)
	}
	if in[0] != 10 {
		t.Fatal("quantiles sorted its input in place")
	}
	if got := nearestRank([]int{4}, 0); got != 4 {
		t.Fatalf("q=0 -> %d", got)
	}
}

func TestIdleRatioObservationDenominator(t *testing.T) {
	for _, tc := range []struct {
		state   string
		samples int
	}{{"running", 1}, {"idle", 1}, {"paused", 0}} {
		b := FromSnapshot(t0, Snapshot{AgentStates: map[string]string{"a": tc.state}})
		w := summarizeWindow([]Bucket{b}, t0.Add(2*time.Hour), 7*24*time.Hour)
		if w.AgentSamples != tc.samples {
			t.Fatalf("%s: %+v", tc.state, w)
		}
		if tc.state == "running" && w.AgentIdleRatio != 0 {
			t.Fatal("all-running agents must report measured zero idle")
		}
	}
}

func TestSummarizeWindows(t *testing.T) {
	now := t0
	var buckets []Bucket
	// 7d window: one sample per hour for 48h, queue_issues = hour index.
	for h := 47; h >= 0; h-- {
		at := now.Add(-time.Duration(h)*time.Hour - time.Minute)
		s := Snapshot{
			QueueIssues: h, QueuePRs: 1, QueueHold: 0,
			AgentStates: map[string]string{"a": "idle", "b": "running"},
			TotalTokens: int64(1000 - h*10),
		}
		if h < 24 {
			s.AgentsKicked = []string{"a"}
		}
		if h%4 == 0 {
			s.SLAViolations = 1
		}
		buckets = append(buckets, FromSnapshot(at, s))
	}
	// Outside both windows and in the future: ignored.
	buckets = append(buckets, FromSnapshot(now.Add(-8*24*time.Hour), Snapshot{QueueIssues: 999}))
	buckets = append(buckets, FromSnapshot(now.Add(time.Hour), Snapshot{QueueIssues: 999}))

	tr := Summarize(buckets, now)
	if tr == nil {
		t.Fatal("nil trends")
	}
	day, week := tr.Last24h, tr.Last7d
	if day.Samples != 24 || week.Samples != 48 {
		t.Fatalf("samples day=%d week=%d", day.Samples, week.Samples)
	}
	if day.QueueIssues != (Quantiles{Median: 11, P90: 21}) {
		t.Fatalf("day queue_issues = %+v", day.QueueIssues)
	}
	if week.QueueIssues != (Quantiles{Median: 23, P90: 43}) {
		t.Fatalf("week queue_issues = %+v", week.QueueIssues)
	}
	if week.QueuePRs != (Quantiles{Median: 1, P90: 1}) || week.QueueHold != (Quantiles{}) {
		t.Fatalf("week prs/hold = %+v %+v", week.QueuePRs, week.QueueHold)
	}
	if day.SLAViolationRate != 0.25 || week.SLAViolationRate != 0.25 {
		t.Fatalf("sla rate day=%v week=%v", day.SLAViolationRate, week.SLAViolationRate)
	}
	if week.AgentIdleRatio != 0.5 {
		t.Fatalf("idle ratio = %v", week.AgentIdleRatio)
	}
	if day.CoverageHours != 23.02 || week.CoverageHours != 47.02 {
		t.Fatalf("coverage day=%v week=%v", day.CoverageHours, week.CoverageHours)
	}
	// 24 kicks over 23.02h; 24 kicks over 47.02h.
	if day.KicksPerDay != 25.0 || week.KicksPerDay != 12.3 {
		t.Fatalf("kicks/day day=%v week=%v", day.KicksPerDay, week.KicksPerDay)
	}
	// tokens rise 10 per sample: 23*10 over 23.02h, 47*10 over 47.02h.
	if day.TokensPerDay != 239.8 || week.TokensPerDay != 239.9 {
		t.Fatalf("tokens/day day=%v week=%v", day.TokensPerDay, week.TokensPerDay)
	}
}

func TestSummarizeEdges(t *testing.T) {
	if Summarize(nil, t0) != nil {
		t.Fatal("no samples should be nil")
	}
	old := []Bucket{FromSnapshot(t0.Add(-8*24*time.Hour), Snapshot{})}
	if Summarize(old, t0) != nil {
		t.Fatal("only stale samples should be nil")
	}
	// Under an hour of coverage: no per-day rates, no agents: zero ratio.
	recent := []Bucket{
		FromSnapshot(t0.Add(-10*time.Minute), Snapshot{AgentsKicked: []string{"a"}, TotalTokens: 1}),
		FromSnapshot(t0.Add(-5*time.Minute), Snapshot{TotalTokens: 50}),
	}
	tr := Summarize(recent, t0)
	if tr == nil || tr.Last24h.Samples != 2 {
		t.Fatalf("trends = %+v", tr)
	}
	if tr.Last24h.KicksPerDay != 0 || tr.Last24h.TokensPerDay != 0 || tr.Last24h.AgentIdleRatio != 0 {
		t.Fatalf("short window rates = %+v", tr.Last24h)
	}
	// 24h window empty while 7d has data.
	tr = Summarize([]Bucket{FromSnapshot(t0.Add(-48*time.Hour), Snapshot{QueueIssues: 2})}, t0)
	if tr == nil || tr.Last24h != (Window{}) || tr.Last7d.Samples != 1 {
		t.Fatalf("trends = %+v", tr)
	}
}

func TestFileKind(t *testing.T) {
	cases := []struct {
		name   string
		bucket bool
		ok     bool
	}{
		{"1759320000000.json", false, true},
		{"hour-1759320000000.json", true, true},
		{"hour-1759320000000.json.tmp", false, false},
		{"notes.txt", false, false},
		{"abc.json", false, false},
		{"-5.json", false, false},
		{"hour-x.json", false, false},
	}
	for _, c := range cases {
		at, bucket, ok := fileKind(c.name)
		if ok != c.ok || bucket != c.bucket {
			t.Errorf("%s: bucket=%v ok=%v", c.name, bucket, ok)
		}
		if ok && at.UnixMilli() != 1759320000000 {
			t.Errorf("%s: at=%v", c.name, at)
		}
	}
	if got := bucketName(time.UnixMilli(1759320000000)); got != "hour-1759320000000.json" {
		t.Fatalf("bucketName = %s", got)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, dir, t0.Add(-time.Hour), Snapshot{QueueIssues: 1})
	writeRaw(t, dir, t0.Add(-10*24*time.Hour), Snapshot{QueueIssues: 2})
	if err := writeFileAtomic(filepath.Join(dir, bucketName(t0.Add(-3*time.Hour))), Bucket{Start: t0.Add(-3 * time.Hour), Samples: 2, QueueIssues: []int{5, 6}}); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, bucketName(t0.Add(-4*time.Hour))), Bucket{}); err != nil {
		t.Fatal(err)
	}
	garbageAt := t0.Add(-2 * time.Hour).UnixMilli()
	for _, name := range []string{fmt.Sprintf("%d.json", garbageAt), fmt.Sprintf("hour-%d.json", garbageAt)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "789.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir, t0.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Samples != 2 || got[1].QueueIssues[0] != 1 {
		t.Fatalf("loaded = %+v", got)
	}
	if _, err := Load(filepath.Join(dir, "missing"), t0); err == nil {
		t.Fatal("missing dir should error")
	}
	if _, err := readSnapshot(filepath.Join(dir, "missing.json"), t0); err == nil {
		t.Fatal("missing snapshot should error")
	}
	if _, err := readBucket(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing bucket should error")
	}
}

func TestCompactRollsHoursAndPrunes(t *testing.T) {
	dir := t.TempDir()
	now := t0
	hour := now.Add(-30 * time.Hour).Truncate(time.Hour)
	for i := 0; i < 3; i++ {
		writeRaw(t, dir, hour.Add(time.Duration(i)*10*time.Minute), Snapshot{QueueIssues: i + 1, TotalTokens: int64(100 * (i + 1)), AgentsKicked: []string{"a"}})
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", hour.Add(40*time.Minute).UnixMilli())), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, dir, now.Add(-time.Hour), Snapshot{QueueIssues: 9})
	writeRaw(t, dir, now.Add(-31*24*time.Hour), Snapshot{})
	if err := writeFileAtomic(filepath.Join(dir, bucketName(now.Add(-40*24*time.Hour).Truncate(time.Hour))), Bucket{Samples: 1}); err != nil {
		t.Fatal(err)
	}

	s := NewStore(dir)
	if err := s.Compact(now); err != nil {
		t.Fatal(err)
	}
	want := []string{fmt.Sprintf("%d.json", now.Add(-time.Hour).UnixMilli()), bucketName(hour)}
	sort.Strings(want)
	if got := dirNames(t, dir); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("dir = %v, want %v", got, want)
	}
	b, err := readBucket(filepath.Join(dir, bucketName(hour)))
	if err != nil {
		t.Fatal(err)
	}
	if b.Samples != 3 || !b.Start.Equal(hour) || b.Kicks != 3 || b.Tokens != 200 || fmt.Sprint(b.QueueIssues) != "[1 2 3]" {
		t.Fatalf("bucket = %+v", b)
	}

	// A later raw in the same hour merges into the existing bucket; a raw the
	// bucket already holds (crash before delete) is not double counted.
	writeRaw(t, dir, hour.Add(10*time.Minute), Snapshot{QueueIssues: 100})
	writeRaw(t, dir, hour.Add(50*time.Minute), Snapshot{QueueIssues: 4, TotalTokens: 450})
	if err := s.Compact(now); err != nil {
		t.Fatal(err)
	}
	b, err = readBucket(filepath.Join(dir, bucketName(hour)))
	if err != nil {
		t.Fatal(err)
	}
	if b.Samples != 4 || fmt.Sprint(b.QueueIssues) != "[1 2 3 4]" || b.Tokens != 350 {
		t.Fatalf("re-compacted bucket = %+v", b)
	}
	if got := dirNames(t, dir); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("dir after re-compact = %v", got)
	}

	// Compacted history still summarizes.
	tr := Summarize(mustLoad(t, dir, now.Add(-7*24*time.Hour)), now)
	if tr == nil || tr.Last7d.Samples != 5 || tr.Last24h.Samples != 1 {
		t.Fatalf("trends = %+v", tr)
	}
}

func mustLoad(t *testing.T, dir string, since time.Time) []Bucket {
	t.Helper()
	b, err := Load(dir, since)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCompactErrors(t *testing.T) {
	var nilStore *Store
	if err := nilStore.Compact(t0); err != nil {
		t.Fatal(err)
	}
	if err := NewStore("").Compact(t0); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(filepath.Join(t.TempDir(), "missing")).Compact(t0); err == nil {
		t.Fatal("missing dir should error")
	}
	if err := writeFileAtomic(filepath.Join(t.TempDir(), "missing", "x.json"), Bucket{}); err == nil {
		t.Fatal("unwritable path should error")
	}
	if err := writeFileAtomic(filepath.Join(t.TempDir(), "x.json"), func() {}); err == nil {
		t.Fatal("unmarshalable value should error")
	}
	// Rename onto a non-empty directory fails and the temp file is removed.
	dir := t.TempDir()
	target := filepath.Join(dir, "x.json")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, Bucket{}); err == nil {
		t.Fatal("rename onto directory should error")
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestCompactBucketWriteFailureKeepsRaws(t *testing.T) {
	dir := t.TempDir()
	hour := t0.Add(-30 * time.Hour).Truncate(time.Hour)
	writeRaw(t, dir, hour.Add(time.Minute), Snapshot{QueueIssues: 1})
	// A directory where the bucket file should go makes the rename fail.
	if err := os.MkdirAll(filepath.Join(dir, bucketName(hour), "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(dir).Compact(t0); err == nil {
		t.Fatal("expected bucket write error")
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d.json", hour.Add(time.Minute).UnixMilli()))); err != nil {
		t.Fatalf("raw snapshot removed despite failed bucket write: %v", err)
	}
}

func TestStoreRecord(t *testing.T) {
	var nilStore *Store
	if err := nilStore.Record(Snapshot{}); err != nil {
		t.Fatal(err)
	}
	if nilStore.Trends() != nil {
		t.Fatal("nil store trends")
	}

	dir := t.TempDir()
	now := t0
	writeRaw(t, dir, now.Add(-30*time.Hour), Snapshot{QueueIssues: 7})
	s := fixedStore(dir, &now)
	if err := s.Record(Snapshot{QueueIssues: 2}); err != nil {
		t.Fatal(err)
	}
	// The first Record compacts; the old raw became a bucket.
	names := dirNames(t, dir)
	if len(names) != 2 || names[0] != fmt.Sprintf("%d.json", now.UnixMilli()) || names[1] != bucketName(now.Add(-30*time.Hour).Truncate(time.Hour)) {
		t.Fatalf("dir = %v", names)
	}
	data, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Timestamp != now.Format(time.RFC3339) || snap.QueueIssues != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}

	// Within the hour no compaction runs.
	writeRaw(t, dir, now.Add(-29*time.Hour), Snapshot{})
	now = now.Add(time.Minute)
	if err := s.Record(Snapshot{Timestamp: "kept"}); err != nil {
		t.Fatal(err)
	}
	if got := len(dirNames(t, dir)); got != 4 {
		t.Fatalf("compacted too early: %v", dirNames(t, dir))
	}
	now = now.Add(time.Hour)
	if err := s.Record(Snapshot{}); err != nil {
		t.Fatal(err)
	}
	if got := len(dirNames(t, dir)); got != 5 {
		t.Fatalf("after hourly compaction: %v", dirNames(t, dir))
	}

	if err := NewStore(filepath.Join(dir, "missing")).Record(Snapshot{}); err == nil {
		t.Fatal("write into missing dir should error")
	}
	if err := NewStore("").Record(Snapshot{}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRecordCompactionError(t *testing.T) {
	dir := t.TempDir()
	hour := t0.Add(-30 * time.Hour).Truncate(time.Hour)
	writeRaw(t, dir, hour.Add(time.Minute), Snapshot{})
	if err := os.MkdirAll(filepath.Join(dir, bucketName(hour), "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := t0
	if err := fixedStore(dir, &now).Record(Snapshot{}); err == nil {
		t.Fatal("expected compaction error")
	}
}

func TestStoreTrendsCached(t *testing.T) {
	dir := t.TempDir()
	now := t0
	s := fixedStore(dir, &now)
	if s.Trends() != nil {
		t.Fatal("empty dir should have nil trends")
	}
	writeRaw(t, dir, now.Add(-time.Minute), Snapshot{QueueIssues: 3})
	// Still cached as nil within the TTL.
	if s.Trends() != nil {
		t.Fatal("trends recomputed inside TTL")
	}
	now = now.Add(trendsTTL)
	tr := s.Trends()
	if tr == nil || tr.Last24h.Samples != 1 || tr.Last24h.QueueIssues.Median != 3 {
		t.Fatalf("trends = %+v", tr)
	}
	tr.Last24h.Samples = 99
	if s.Trends().Last24h.Samples != 1 {
		t.Fatal("Trends returned the cached value, not a copy")
	}
	if NewStore(filepath.Join(dir, "missing")).Trends() != nil {
		t.Fatal("missing dir should have nil trends")
	}
	if NewStore("").Trends() != nil {
		t.Fatal("empty dir path should have nil trends")
	}
}

func TestTrendsJSONShape(t *testing.T) {
	data, err := json.Marshal(Trends{Last24h: Window{Samples: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"24h", "7d"} {
		w, ok := m[k]
		if !ok {
			t.Fatalf("missing %q in %s", k, data)
		}
		for _, f := range []string{"samples", "coverage_hours", "queue_issues", "queue_prs", "queue_hold", "sla_violation_rate", "agent_idle_ratio", "kicks_per_day", "tokens_per_day"} {
			if _, ok := w[f]; !ok {
				t.Fatalf("missing %s.%s in %s", k, f, data)
			}
		}
	}
}

func TestCountScopesFilesToWindow(t *testing.T) {
	dir := t.TempDir()
	if n, first := Count(filepath.Join(dir, "missing"), time.Time{}); n != 0 || !first.IsZero() {
		t.Fatalf("missing dir: n=%d first=%v", n, first)
	}
	if n, first := Count(dir, time.Time{}); n != 0 || !first.IsZero() {
		t.Fatalf("empty dir: n=%d first=%v", n, first)
	}
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		writeRaw(t, dir, t0.Add(time.Duration(i)*time.Hour), Snapshot{QueueIssues: i})
	}
	// One hourly bucket before the raws and an unrelated file.
	if err := os.WriteFile(filepath.Join(dir, bucketName(t0.Add(-2*time.Hour))), []byte(`{"samples":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	n, first := Count(dir, time.Time{})
	if n != 5 || !first.Equal(t0.Add(-2*time.Hour)) {
		t.Fatalf("all: n=%d first=%v", n, first)
	}
	n, first = Count(dir, t0.Add(90*time.Minute))
	if n != 2 || !first.Equal(t0.Add(-2*time.Hour)) {
		t.Fatalf("windowed: n=%d first=%v", n, first)
	}
}
