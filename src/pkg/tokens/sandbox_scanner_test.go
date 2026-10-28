package tokens

import (
	"os"
	"path/filepath"
	"testing"
)

const sandboxRunsFixture = `{"agent":"deps","backend":"claude","model":"opus","run":{"runtime":"job","startedAt":"2026-10-21T10:00:00Z","endedAt":"2026-10-21T10:29:00Z","succeeded":true,"usage":{"model":"claude-opus-4","input":100,"output":20,"cacheRead":300,"cacheCreate":5}}}
{"agent":"quality","backend":"copilot","run":{"runtime":"job","startedAt":"2026-10-21T11:00:00Z","endedAt":"2026-10-21T11:05:00Z","succeeded":true}}
not json at all
{"agent":"deps","backend":"claude","run":{"runtime":"job","startedAt":"2026-10-21T16:00:00Z","endedAt":"2026-10-21T16:20:00Z","succeeded":false,"usage":{"input":10,"output":1}}}
`

func TestScanSandboxRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(path, []byte(sandboxRunsFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	agg, err := ScanSandboxRuns(path)
	if err != nil {
		t.Fatal(err)
	}
	// quality reported no usage and the garbage line is skipped: two sessions.
	if agg.SessionCount != 2 || len(agg.Sessions) != 2 {
		t.Fatalf("sessions = %d (%+v)", agg.SessionCount, agg.Sessions)
	}
	if agg.TotalTokens != 436 || agg.ByAgent["deps"] != 436 || agg.ByAgent["quality"] != 0 {
		t.Errorf("totals = %d, byAgent = %v", agg.TotalTokens, agg.ByAgent)
	}
	if agg.ByModel["claude-opus-4"] != 425 || agg.ByModel["unknown"] != 11 {
		t.Errorf("byModel = %v", agg.ByModel)
	}
	for _, s := range agg.Sessions {
		if s.Backend != BackendSandbox || s.Agent != "deps" || s.SessionID == "" || s.TotalTokens == 0 {
			t.Errorf("session = %+v", s)
		}
		if len(s.Usage) != 1 || s.Usage[0].Input+s.Usage[0].Output+s.Usage[0].CacheRead+s.Usage[0].CacheCreate != s.TotalTokens {
			t.Errorf("one timeline event per run reconciling to the total, got %+v", s.Usage)
		}
	}
	if agg.Sessions[0].SessionID == agg.Sessions[1].SessionID {
		t.Errorf("runs of the same agent must not collide: %q", agg.Sessions[0].SessionID)
	}
}

func TestScanSandboxRuns_Missing(t *testing.T) {
	agg, err := ScanSandboxRuns(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || agg == nil || agg.SessionCount != 0 {
		t.Fatalf("missing history must be empty and quiet, got %+v, %v", agg, err)
	}
	if agg, err := ScanSandboxRuns(""); err != nil || agg.SessionCount != 0 {
		t.Fatalf("unset path must be empty and quiet, got %+v, %v", agg, err)
	}
}

func TestCollector_MergesSandboxRuns(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "session1.jsonl"), []byte(`{"type":"usage","model":"sonnet","input_tokens":100,"output_tokens":50}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(dir, "runs.jsonl")
	if err := os.WriteFile(runs, []byte(sandboxRunsFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCollector(dir, testLogger())
	c.SetSandboxRunHistoryPath(runs)
	c.scan()
	sum := c.Summary()
	if sum == nil || sum.ByAgent["deps"] != 436 {
		t.Fatalf("sandbox runs must be merged into the aggregate: %+v", sum)
	}
	if sum.TotalTokens != 150+436 {
		t.Errorf("total = %d", sum.TotalTokens)
	}
	if d := c.Diagnostics(); d.LastSandboxScanError != "" {
		t.Errorf("unexpected scan error %q", d.LastSandboxScanError)
	}
}
