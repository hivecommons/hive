package hub

// Tests for hub receipt of spoke component-reach reports (#3993, phase 2a of
// #3973). The heartbeat body is spoke INPUT, so the load-bearing assertions
// are the receive-path bounds: an oversized report is clipped to
// tracing.MaxReachComponents (a hostile spoke must not grow hub memory),
// strings are sanitized, garbage counts/timestamps are neutralized — and the
// stored shape keeps the exact JSON keys 2b reads.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tracing"
)

// reachBody builds a heartbeat JSON body for hive h1 carrying n reach entries.
func reachBody(t *testing.T, n int) string {
	t.Helper()
	entries := make([]tracing.ReachEntry, n)
	for i := range entries {
		entries[i] = tracing.ReachEntry{
			Component:  fmt.Sprintf("comp%04d", i),
			Commit:     "abc1234",
			SpansTotal: int64(i + 1),
			SpansError: 1,
			FirstSeen:  "2026-08-17T10:00:00Z",
			LastSeen:   "2026-08-17T11:00:00Z",
		}
	}
	report := tracing.ReachReport{Entries: entries}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal reach report: %v", err)
	}
	return fmt.Sprintf(`{"hive_id":"h1","org":"kubestellar","component_reach":%s}`, data)
}

// storedReach fetches the stored report for hive id from the registry.
func storedReach(t *testing.T, s *HubServer, id string) *tracing.ReachReport {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == id {
			return h.ComponentReach
		}
	}
	t.Fatalf("hive %q not in registry", id)
	return nil
}

// TestHeartbeatComponentReach_Stored verifies the storage path end to end
// through handleHeartbeat: a well-formed report lands on the registry entry
// with its values intact.
func TestHeartbeatComponentReach_Stored(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	rec := postHeartbeat(t, s, reachBody(t, 3))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	stored := storedReach(t, s, "h1")
	if stored == nil {
		t.Fatal("component_reach not stored")
	}
	if len(stored.Entries) != 3 {
		t.Fatalf("stored entries = %d, want 3", len(stored.Entries))
	}
	e := stored.Entries[0]
	if e.Component != "comp0000" || e.Commit != "abc1234" || e.SpansTotal != 1 || e.SpansError != 1 {
		t.Errorf("stored entry mangled: %+v", e)
	}
	if e.FirstSeen != "2026-08-17T10:00:00Z" || e.LastSeen != "2026-08-17T11:00:00Z" {
		t.Errorf("stored timestamps mangled: %+v", e)
	}
}

// TestHeartbeatComponentReach_ClipsOversizedInput is the hostile-spoke bound:
// a report with far more than tracing.MaxReachComponents entries must be
// CLIPPED on receive — stored entries never exceed the cap, and the refused
// count is surfaced in overflow_components rather than silently dropped.
// (Removing the clip in sanitizeComponentReach makes this test fail.)
func TestHeartbeatComponentReach_ClipsOversizedInput(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	const excess = 36
	rec := postHeartbeat(t, s, reachBody(t, tracing.MaxReachComponents+excess))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	stored := storedReach(t, s, "h1")
	if stored == nil {
		t.Fatal("component_reach not stored")
	}
	if len(stored.Entries) != tracing.MaxReachComponents {
		t.Errorf("stored entries = %d, want clipped to %d", len(stored.Entries), tracing.MaxReachComponents)
	}
	if stored.OverflowComponents != excess {
		t.Errorf("overflow_components = %d, want %d (the clip must be visible, not silent)",
			stored.OverflowComponents, excess)
	}
}

// TestHeartbeatComponentReach_SanitizesGarbage verifies string sanitization,
// count clamping, and timestamp validation on the receive path.
func TestHeartbeatComponentReach_SanitizesGarbage(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	body := `{"hive_id":"h1","component_reach":{"entries":[
		{"component":"gov<script>ernor","commit":"abc1234!!","spans_total":-5,"spans_error":9223372036854775807,"first_seen":"not-a-time","last_seen":"2026-08-17T11:00:00Z"},
		{"component":"<>","commit":"x","spans_total":1,"spans_error":0,"first_seen":"","last_seen":""}
	],"overflow_spans":-3}}`
	rec := postHeartbeat(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d (body=%s)", rec.Code, rec.Body.String())
	}

	stored := storedReach(t, s, "h1")
	if stored == nil {
		t.Fatal("component_reach not stored")
	}
	// The all-symbols component sanitizes to "" and is dropped entirely.
	if len(stored.Entries) != 1 {
		t.Fatalf("stored entries = %d, want 1 (empty-name entry dropped): %+v", len(stored.Entries), stored.Entries)
	}
	e := stored.Entries[0]
	if e.Component != "govscripternor" {
		t.Errorf("component = %q, want angle brackets stripped", e.Component)
	}
	if e.Commit != "abc1234" {
		t.Errorf("commit = %q, want %q", e.Commit, "abc1234")
	}
	if e.SpansTotal != 0 {
		t.Errorf("negative spans_total stored as %d, want clamped to 0", e.SpansTotal)
	}
	if e.SpansError != maxReachSpanCount {
		t.Errorf("absurd spans_error stored as %d, want clamped to %d", e.SpansError, int64(maxReachSpanCount))
	}
	if e.FirstSeen != "" {
		t.Errorf("unparseable first_seen stored as %q, want dropped", e.FirstSeen)
	}
	if e.LastSeen != "2026-08-17T11:00:00Z" {
		t.Errorf("valid last_seen mangled: %q", e.LastSeen)
	}
	if stored.OverflowSpans != 0 {
		t.Errorf("negative overflow_spans stored as %d, want 0", stored.OverflowSpans)
	}
}

// TestHeartbeatComponentReach_CarriedForwardWhenOmitted verifies a beat
// WITHOUT component_reach preserves the previous stored report — a spoke
// restart must not blank the hive's last real reach data.
func TestHeartbeatComponentReach_CarriedForwardWhenOmitted(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	if rec := postHeartbeat(t, s, reachBody(t, 2)); rec.Code != http.StatusOK {
		t.Fatalf("first heartbeat status = %d", rec.Code)
	}
	if rec := postHeartbeat(t, s, `{"hive_id":"h1","org":"kubestellar"}`); rec.Code != http.StatusOK {
		t.Fatalf("second heartbeat status = %d", rec.Code)
	}

	stored := storedReach(t, s, "h1")
	if stored == nil || len(stored.Entries) != 2 {
		t.Fatalf("reach report not carried forward across an omitting beat: %+v", stored)
	}
}

// TestHeartbeatPayload_ComponentReachWireShape pins the wire contract 2b
// reads: the field is named component_reach, entries carry EXACTLY the epic's
// keys, and a payload without reach omits the field entirely.
func TestHeartbeatPayload_ComponentReachWireShape(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	p := HeartbeatPayload{
		HiveID: "h1",
		ComponentReach: &tracing.ReachReport{
			Entries: []tracing.ReachEntry{{
				Component:  "governor",
				Commit:     "abc1234",
				SpansTotal: 10,
				SpansError: 2,
				FirstSeen:  now,
				LastSeen:   now,
			}},
		},
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	body := string(data)
	for _, key := range []string{
		`"component_reach"`, `"entries"`,
		`"component":"governor"`, `"commit":"abc1234"`,
		`"spans_total":10`, `"spans_error":2`,
		`"first_seen"`, `"last_seen"`,
	} {
		if !strings.Contains(body, key) {
			t.Errorf("payload JSON missing %s: %s", key, body)
		}
	}

	empty, err := json.Marshal(HeartbeatPayload{HiveID: "h1"})
	if err != nil {
		t.Fatalf("marshal empty payload: %v", err)
	}
	if strings.Contains(string(empty), "component_reach") {
		t.Errorf("nil report must omit component_reach: %s", string(empty))
	}
}

// TestSanitizeComponentReach_NilAndEmpty pins nil-in → nil-out and
// empty-report → nil (so the carry-forward path treats both as "no data").
func TestSanitizeComponentReach_NilAndEmpty(t *testing.T) {
	if got := sanitizeComponentReach(nil); got != nil {
		t.Errorf("sanitizeComponentReach(nil) = %+v, want nil", got)
	}
	if got := sanitizeComponentReach(&tracing.ReachReport{}); got != nil {
		t.Errorf("sanitizeComponentReach(empty) = %+v, want nil", got)
	}
}
