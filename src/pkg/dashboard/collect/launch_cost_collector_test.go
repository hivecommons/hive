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
