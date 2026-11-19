package governor

import "testing"

// RecordKickWithReason (#18494) counts the kick like RecordKick and keeps the
// reason on the kick-history entry; a plain RecordKick leaves it empty.
func TestRecordKickWithReason_KeepsReasonOnHistory(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	g.RecordKick("scanner")
	g.RecordKickWithReason("scanner", "new release: requests 2.32.0 -> 2.33.0")

	hist := g.KickHistory()
	if len(hist) != 2 {
		t.Fatalf("history = %d entries, want 2", len(hist))
	}
	if hist[0].Reason != "" {
		t.Fatalf("cadence kick reason = %q, want empty", hist[0].Reason)
	}
	if hist[1].Agent != "scanner" || hist[1].Reason != "new release: requests 2.32.0 -> 2.33.0" {
		t.Fatalf("release kick entry = %+v", hist[1])
	}
	if g.GetState().LastKick["scanner"].IsZero() {
		t.Fatal("RecordKickWithReason must stamp LastKick like RecordKick")
	}
}
