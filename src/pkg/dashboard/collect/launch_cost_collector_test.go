package collect

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

type fakeLaunchReader struct {
	summary  *tokens.AggregateSummary
	launches []tokens.LaunchRecord
}

func (f *fakeLaunchReader) Summary() *tokens.AggregateSummary { return f.summary }
func (f *fakeLaunchReader) Launches() []tokens.LaunchRecord   { return f.launches }

func TestLaunchCostCollectorLifecycle(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-time.Hour)
	reader := &fakeLaunchReader{
		summary: &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
			rcSession("s1", "scanner", rcUsage(rcTime(base, 15), 200, 20)),
		}},
		launches: []tokens.LaunchRecord{lcLaunch("scanner", "governor-eval", rcTime(base, 10), lcIssue("o/r", 1))},
	}

	// Nil collector and inert collector are safe.
	var nilLC *LaunchCostCollector
	nilLC.EnablePersistence("/nonexistent")
	nilLC.Start(context.Background())
	if _, ready := nilLC.Snapshot(); ready {
		t.Fatal("nil collector must not be ready")
	}
	inert := NewLaunchCostCollector(nil, nil)
	inert.Start(context.Background()) // returns immediately: no reader
	if _, ready := inert.Snapshot(); ready {
		t.Fatal("inert collector must not be ready")
	}

	persist := filepath.Join(t.TempDir(), "launch-cost.json")
	lc := NewLaunchCostCollector(reader, nil)
	lc.nowFn = func() time.Time { return now }
	lc.EnablePersistence(persist) // missing file: clean first boot
	if _, ready := lc.Snapshot(); ready {
		t.Fatal("must not be ready before first collect")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lc.Start(ctx) // one collect, then exits on the cancelled ctx

	snap, ready := lc.Snapshot()
	if !ready || len(snap.ByLaunch) != 1 || snap.AttributedTokens != 220 || !snap.CollectedAt.Equal(now) {
		t.Fatalf("snapshot ready=%v %+v", ready, snap)
	}
	if _, err := os.Stat(persist); err != nil {
		t.Fatalf("collect must persist: %v", err)
	}

	// Restore from disk.
	restored := NewLaunchCostCollector(&fakeLaunchReader{}, nil)
	restored.EnablePersistence(persist)
	if rs, ok := restored.Snapshot(); !ok || rs.AttributedTokens != 220 {
		t.Fatalf("restored ready=%v tokens=%d", ok, rs.AttributedTokens)
	}

	// Corrupt file: logged, ignored, not ready.
	corrupt := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2 := NewLaunchCostCollector(&fakeLaunchReader{}, nil)
	c2.EnablePersistence(corrupt)
	if _, ok := c2.Snapshot(); ok {
		t.Fatal("corrupt store must not produce a ready snapshot")
	}

	// Zero CollectedAt on disk is treated as never collected.
	zero := filepath.Join(t.TempDir(), "zero.json")
	data, _ := json.Marshal(persistedLaunchCost{Snapshot: LaunchCostResponse{Ready: true}})
	if err := os.WriteFile(zero, data, 0o600); err != nil {
		t.Fatal(err)
	}
	c3 := NewLaunchCostCollector(&fakeLaunchReader{}, nil)
	c3.EnablePersistence(zero)
	if _, ok := c3.Snapshot(); ok {
		t.Fatal("zero collected_at must not be ready")
	}

	// Unwritable persist path is logged, not fatal.
	c4 := NewLaunchCostCollector(reader, nil)
	c4.EnablePersistence(filepath.Join(t.TempDir(), "missing-dir", "x.json"))
	c4.collect()
	if _, ok := c4.Snapshot(); !ok {
		t.Fatal("collect must still serve a snapshot when persistence fails")
	}
}

func TestLaunchCostEmptyRefsAndNilSlices(t *testing.T) {
	now := time.Now().UTC()
	base := now.Add(-time.Hour)
	launches := []tokens.LaunchRecord{
		{Agent: "a", Trigger: "governor-eval", LaunchedAtMs: rcTime(base, 10).UnixMilli()},                                      // no refs
		{Agent: "a", Trigger: "governor-eval", LaunchedAtMs: rcTime(base, 20).UnixMilli(), Refs: []tokens.LaunchRef{{Ref: ""}}}, // blank ref
		{Agent: "", LaunchedAtMs: 1},  // dropped: no agent
		{Agent: "b", LaunchedAtMs: 0}, // dropped: no timestamp
		{Agent: "c", LaunchedAtMs: now.Add(-RepoCostWindow - time.Hour).UnixMilli(), Refs: []tokens.LaunchRef{lcIssue("o/r", 5)}}, // outside window, no usage: not counted
	}
	summary := &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		rcSession("s1", "a", rcUsage(rcTime(base, 15), 10, 1), rcUsage(rcTime(base, 25), 20, 2)),
	}}
	resp := ComputeLaunchCost(summary, launches, now)
	if len(resp.ByLaunch) != 2 || len(resp.ByRef) != 0 {
		t.Fatalf("by_launch=%d by_ref=%d", len(resp.ByLaunch), len(resp.ByRef))
	}
	for _, l := range resp.ByLaunch {
		if l.Refs == nil {
			t.Fatal("refs must marshal as [] not null")
		}
	}
	if resp.ByLaunch[0].LaunchedAt < resp.ByLaunch[1].LaunchedAt {
		t.Fatal("by_launch must be newest first")
	}
	if msToRFC3339(0) != "" {
		t.Fatal("zero ms must render empty")
	}
}

func TestLaunchCostDailyRollupSurvivesWindow(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	base := now.Add(-time.Hour)
	reader := &fakeLaunchReader{
		summary: &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
			rcSession("s1", "scanner", rcUsage(rcTime(base, 15), 200, 20)),
		}},
		launches: []tokens.LaunchRecord{lcLaunch("scanner", "review", rcTime(base, 10), lcIssue("o/r", 7))},
	}
	persist := filepath.Join(t.TempDir(), "launch-cost.json")
	lc := NewLaunchCostCollector(reader, nil)
	lc.nowFn = func() time.Time { return now }
	lc.EnablePersistence(persist)
	lc.collect()

	days := lc.DailyRollup()
	if len(days) != 1 || days[0].Day != "2026-10-27" || len(days[0].ByRef) != 1 {
		t.Fatalf("rollup = %+v", days)
	}
	ref := days[0].ByRef[0]
	if ref.Ref != "o/r#7" || ref.Launches != 1 || ref.ShareUSD <= 0 {
		t.Fatalf("ref = %+v", ref)
	}

	// The launch ages out of the window; the stored day must remain.
	reader.launches = nil
	reader.summary = &tokens.AggregateSummary{}
	lc.nowFn = func() time.Time { return now.Add(3 * 24 * time.Hour) }
	lc.collect()
	if got := lc.DailyRollup(); len(got) != 1 || got[0].ByRef[0].Launches != 1 {
		t.Fatalf("rollup lost after window: %+v", got)
	}

	// And it survives a restart.
	lc2 := NewLaunchCostCollector(reader, nil)
	lc2.EnablePersistence(persist)
	if got := lc2.DailyRollup(); len(got) != 1 || got[0].ByRef[0].Ref != "o/r#7" {
		t.Fatalf("rollup not restored: %+v", got)
	}
}

// A day's total must add up every launch that ever named a ref on it, even
// when the launch ring turns over between ticks so no single snapshot holds
// them all; and a launch still in view must follow its growing spend without
// being counted twice.
func TestLaunchCostDailyRollupSumsAcrossRingTurnover(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	base := now.Add(-2 * time.Hour)
	first := lcLaunch("scanner", "review", rcTime(base, 0), lcIssue("o/r", 7))
	second := lcLaunch("quality", "review", rcTime(base, 30), lcIssue("o/r", 7))
	reader := &fakeLaunchReader{
		summary: &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
			rcSession("s1", "scanner", rcUsage(rcTime(base, 5), 200, 20)),
		}},
		launches: []tokens.LaunchRecord{first, second},
	}
	lc := NewLaunchCostCollector(reader, nil)
	lc.nowFn = func() time.Time { return now }
	lc.collect()
	days := lc.DailyRollup()
	if len(days) != 1 || len(days[0].ByRef) != 1 {
		t.Fatalf("rollup = %+v", days)
	}
	ref := days[0].ByRef[0]
	// The no-usage launch counts, as /api/launch-cost by_ref[].launches does.
	if ref.Launches != 2 || ref.ShareUSD <= 0 {
		t.Fatalf("ref = %+v", ref)
	}
	firstShare := ref.ShareUSD

	// More usage lands on the first launch: the live share follows it, the
	// launch is still counted once.
	reader.summary = &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		rcSession("s1", "scanner", rcUsage(rcTime(base, 5), 200, 20), rcUsage(rcTime(base, 10), 400, 40)),
	}}
	lc.nowFn = func() time.Time { return now.Add(5 * time.Minute) }
	lc.collect()
	ref = lc.DailyRollup()[0].ByRef[0]
	if ref.Launches != 2 || ref.ShareUSD <= firstShare {
		t.Fatalf("live share must grow in place: before=%v after=%+v", firstShare, ref)
	}
	grown := ref.ShareUSD

	// The ring turns over: the first launch disappears and a third one for the
	// same ref/day arrives in the same tick. The day must hold all three.
	third := lcLaunch("scanner", "review", rcTime(base, 60), lcIssue("o/r", 7))
	reader.launches = []tokens.LaunchRecord{second, third}
	reader.summary = &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		rcSession("s2", "scanner", rcUsage(rcTime(base, 65), 100, 10)),
	}}
	lc.nowFn = func() time.Time { return now.Add(10 * time.Minute) }
	lc.collect()
	ref = lc.DailyRollup()[0].ByRef[0]
	if ref.Launches != 3 || ref.ShareUSD <= grown {
		t.Fatalf("ring turnover must accumulate: %+v (grown=%v)", ref, grown)
	}
	total := ref.ShareUSD

	// Everything ages out; the settled day keeps the full total and persists
	// it the same way a restart would see it.
	reader.launches = nil
	reader.summary = &tokens.AggregateSummary{}
	lc.nowFn = func() time.Time { return now.Add(3 * 24 * time.Hour) }
	lc.collect()
	ref = lc.DailyRollup()[0].ByRef[0]
	if ref.Launches != 3 || ref.ShareUSD != total {
		t.Fatalf("settled day = %+v want launches=3 share=%v", ref, total)
	}
	if len(lc.rollupLive) != 0 {
		t.Fatalf("live launches must drain once out of view: %+v", lc.rollupLive)
	}
}

// Live launches round-trip through the store so a restart neither drops a
// launch the join can no longer see nor counts a still-visible one twice.
func TestLaunchCostDailyRollupLiveLaunchesSurviveRestart(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	base := now.Add(-time.Hour)
	launch := lcLaunch("scanner", "review", rcTime(base, 10), lcIssue("o/r", 7))
	reader := &fakeLaunchReader{
		summary:  &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{rcSession("s1", "scanner", rcUsage(rcTime(base, 15), 200, 20))}},
		launches: []tokens.LaunchRecord{launch},
	}
	persist := filepath.Join(t.TempDir(), "launch-cost.json")
	lc := NewLaunchCostCollector(reader, nil)
	lc.nowFn = func() time.Time { return now }
	lc.EnablePersistence(persist)
	lc.collect()
	share := lc.DailyRollup()[0].ByRef[0].ShareUSD

	var stored persistedLaunchCost
	data, err := os.ReadFile(persist)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.DailyRollup) != 0 || len(stored.LiveLaunches) != 1 {
		t.Fatalf("store must hold the visible launch as live, not settled: %+v", stored)
	}

	// Restart while the launch is still in the ring: counted once.
	lc2 := NewLaunchCostCollector(reader, nil)
	lc2.nowFn = func() time.Time { return now.Add(5 * time.Minute) }
	lc2.EnablePersistence(persist)
	lc2.collect()
	if got := lc2.DailyRollup(); len(got) != 1 || got[0].ByRef[0].Launches != 1 || got[0].ByRef[0].ShareUSD != share {
		t.Fatalf("restart must not double count: %+v", got)
	}

	// Restart after the ring was lost: the live launch settles with its last
	// known share instead of vanishing.
	lc3 := NewLaunchCostCollector(&fakeLaunchReader{summary: &tokens.AggregateSummary{}}, nil)
	lc3.nowFn = func() time.Time { return now.Add(10 * time.Minute) }
	lc3.EnablePersistence(persist)
	lc3.collect()
	if got := lc3.DailyRollup(); len(got) != 1 || got[0].ByRef[0].Launches != 1 || got[0].ByRef[0].ShareUSD != share {
		t.Fatalf("lost ring must settle live launch: %+v", got)
	}
	if len(lc3.rollup["2026-10-27"]) != 1 || len(lc3.rollupLive) != 0 {
		t.Fatalf("expected settled day, got rollup=%+v live=%+v", lc3.rollup, lc3.rollupLive)
	}
}

// Untouched days are served from the per-day cache; a tick that changes one
// day re-sorts only that day.
func TestLaunchCostDailyRollupDayCache(t *testing.T) {
	lc := NewLaunchCostCollector(&fakeLaunchReader{}, nil)
	lc.seedRollupLocked([]LaunchCostDailyRollup{
		{Day: "2026-10-01", ByRef: []LaunchCostRollupRef{{Ref: "o/r#1", Kind: "issue", Launches: 2}}},
		{Day: "not-a-day", ByRef: []LaunchCostRollupRef{{Ref: "o/r#9"}}},
	}, map[string]launchCostLiveLaunch{
		"a@2026-10-02T00:00:00Z": {Day: "2026-10-02", Refs: []tokens.LaunchRef{lcIssue("o/r", 2)}, ShareUSD: 1},
		"bad":                    {Day: "nope", Refs: []tokens.LaunchRef{lcIssue("o/r", 3)}},
		"norefs":                 {Day: "2026-10-03"},
	})
	days := lc.DailyRollup()
	if len(days) != 2 || days[0].Day != "2026-10-01" || days[1].Day != "2026-10-02" || days[1].ByRef[0].ShareUSD != 1 {
		t.Fatalf("days = %+v", days)
	}
	if _, ok := lc.rollupDayCache["2026-10-01"]; !ok {
		t.Fatal("day must be cached after listing")
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	resp := ComputeLaunchCost(&tokens.AggregateSummary{}, nil, now)
	lc.mergeLaunchCostRollupLocked(resp, nil, now) // live launch for 10-02 ages out
	if _, ok := lc.rollupDayCache["2026-10-01"]; !ok {
		t.Fatal("untouched day must stay cached")
	}
	if _, ok := lc.rollupDayCache["2026-10-02"]; ok {
		t.Fatal("changed day must be invalidated")
	}
	if got := lc.DailyRollup(); got[1].ByRef[0].Launches != 1 || got[1].ByRef[0].ShareUSD != 1 {
		t.Fatalf("settled from live: %+v", got)
	}
	// Retention horizon drops old settled days and stale live launches.
	far := now.AddDate(3, 0, 0)
	lc.rollupLive["old"] = launchCostLiveLaunch{Day: "2026-10-02", Refs: []tokens.LaunchRef{lcIssue("o/r", 4)}}
	lc.mergeLaunchCostRollupLocked(ComputeLaunchCost(&tokens.AggregateSummary{}, nil, far), nil, far)
	if len(lc.DailyRollup()) != 0 || len(lc.rollupLive) != 0 {
		t.Fatalf("retention must drop everything: %+v %+v", lc.rollup, lc.rollupLive)
	}
	// Not-ready snapshots leave the store alone.
	lc.rollupLive["x"] = launchCostLiveLaunch{Day: "2026-10-02", Refs: []tokens.LaunchRef{lcIssue("o/r", 4)}}
	lc.mergeLaunchCostRollupLocked(LaunchCostResponse{}, nil, far)
	if len(lc.rollupLive) != 1 {
		t.Fatal("not-ready snapshot must not merge")
	}
}

// The store's settled list is built once per day and reused until that day
// changes, instead of being re-listed and re-sorted every tick.
func TestLaunchCostSettledRollupListCached(t *testing.T) {
	lc := NewLaunchCostCollector(&fakeLaunchReader{}, nil)
	lc.seedRollupLocked([]LaunchCostDailyRollup{
		{Day: "2026-10-01", ByRef: []LaunchCostRollupRef{{Ref: "o/r#1", Launches: 1}, {Ref: "o/r#2", Launches: 1}}},
		{Day: "2026-10-02", ByRef: []LaunchCostRollupRef{{Ref: "o/r#3", Launches: 1}}},
	}, nil)
	first := lc.settledRollupListLocked()
	second := lc.settledRollupListLocked()
	if len(first) != 2 || &first[0].ByRef[0] != &second[0].ByRef[0] || &first[1].ByRef[0] != &second[1].ByRef[0] {
		t.Fatal("unchanged days must be served from the cache")
	}
	lc.invalidateRollupDayLocked("2026-10-02")
	third := lc.settledRollupListLocked()
	if &third[0].ByRef[0] != &first[0].ByRef[0] {
		t.Fatal("untouched day must stay cached")
	}
	if &third[1].ByRef[0] == &first[1].ByRef[0] {
		t.Fatal("invalidated day must be rebuilt")
	}
}
