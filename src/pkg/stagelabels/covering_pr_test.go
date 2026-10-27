package stagelabels

import (
	"testing"
	"time"
)

func TestStoreSetCoveringPR(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s := NewStore("", func() time.Time { return now })
	s.SetCoveringPR("o/r#7", "o/r#8") // unknown key: no record is invented
	if _, ok := s.Get("o/r#7"); ok {
		t.Fatal("SetCoveringPR on an unknown key created a record")
	}
	s.Observe("o/r#7", CoveredByPR, "open PR covers it")
	s.SetCoveringPR("o/r#7", "o/r#8")
	if rec, _ := s.Get("o/r#7"); rec.CoveringPR != "o/r#8" {
		t.Fatalf("CoveringPR = %q, want o/r#8", rec.CoveringPR)
	}
	s.SetCoveringPR("o/r#7", "")
	if rec, _ := s.Get("o/r#7"); rec.CoveringPR != "" {
		t.Fatalf("CoveringPR = %q after clear", rec.CoveringPR)
	}
	// A plain Observe keeps the field (only the engine's note changes it).
	s.SetCoveringPR("o/r#7", "o/r#8")
	s.Observe("o/r#7", CoveredByPR, "open PR #8 covers it")
	if rec, _ := s.Get("o/r#7"); rec.CoveringPR != "o/r#8" {
		t.Fatalf("Observe dropped CoveringPR: %q", rec.CoveringPR)
	}
}

// RegressionHop names the fall that opened the current regression — the
// hop from the high-water mark — even after a second fall moved the item
// further down, and nothing once the item recovered.
func TestRecordRegressionHop(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 42, 0, 0, time.UTC)
	s := NewStore("", func() time.Time { return now })
	key := "hivecommons/hotshot#269"
	s.Observe(key, Claimed, "in-progress start mark (no PR yet)")
	now = now.Add(time.Minute)
	s.Observe(key, CoveredByPR, "open PR covers it")
	if rec, _ := s.Get(key); rec.Regressed() {
		t.Fatal("forward hops are not a regression")
	}
	if _, ok := func() (Hop, bool) { rec, _ := s.Get(key); return rec.RegressionHop() }(); ok {
		t.Fatal("RegressionHop on an unregressed record")
	}
	now = now.Add(2 * time.Minute)
	fallAt := now
	s.Observe(key, Claimed, "live issue claim")
	now = now.Add(3 * time.Minute)
	s.Observe(key, Actionable, "actionable")
	rec, _ := s.Get(key)
	hop, ok := rec.RegressionHop()
	if !ok || hop.From != CoveredByPR || hop.Stage != Claimed || !hop.At.Equal(fallAt) || hop.Reason != "live issue claim" || !hop.Regressed {
		t.Fatalf("RegressionHop = %+v ok=%v, want the 040→020 fall at %v", hop, ok, fallAt)
	}
	// 020 → 010 is a lapsed reservation, not a second fall: the record keeps
	// the 040 → 020 fall as its only regression.
	if rec.RegressedFrom != CoveredByPR || !rec.RegressedAt.Equal(fallAt) {
		t.Fatalf("record = from %v at %v, want 040 high-water mark and the 040→020 fall time %v", rec.RegressedFrom, rec.RegressedAt, fallAt)
	}
	now = now.Add(time.Minute)
	s.Observe(key, CoveredByPR, "open PR #270 covers it")
	rec, _ = s.Get(key)
	if rec.Regressed() {
		t.Fatal("back at the high-water mark is a recovery")
	}
	if _, ok := rec.RegressionHop(); ok {
		t.Fatal("RegressionHop after recovery")
	}
}
