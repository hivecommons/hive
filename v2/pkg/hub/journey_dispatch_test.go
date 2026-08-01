package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// snoozeTestDuration is a legitimate, well under-the-cap snooze window used by
// the dispatch tests ("a vendor pilot lasting a day").
const snoozeTestDuration = 24 * time.Hour

// dispatchTestServer builds a HubServer wired only with the in-memory pieces
// the journey dispatch handlers touch: a registry, a journey store, and a
// logger. Journey state is redirected to a temp file so save() never writes to
// the real /data PVC.
func dispatchTestServer(t *testing.T, hives ...RegistryEntry) *HubServer {
	t.Helper()
	withTempStatePath(t)
	s := &HubServer{
		logger:  slog.Default(),
		journey: newJourneyStore(),
	}
	s.registry.Hives = hives
	return s
}

// postSnoozeJSON issues a POST with a JSON body to the given handler and returns the
// recorder.
func postSnoozeJSON(t *testing.T, h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/saas/admin/journey-snooze", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// ── writeJSONError ──────────────────────────────────────────────────────────

// TestWriteJSONError pins the error envelope shape the dashboard parses: a JSON
// object with an "error" key, the requested status code, and a JSON content
// type. A plain-text body here would break every client error path.
func TestWriteJSONError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSONError(rec, http.StatusTeapot, "kettle offline")

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v (body=%q)", err, rec.Body.String())
	}
	if got["error"] != "kettle offline" {
		t.Errorf(`body["error"] = %q, want "kettle offline"`, got["error"])
	}
}

// ── attachJourneyStatus ─────────────────────────────────────────────────────

// TestAttachJourneyStatusFillsEveryHive checks the derived-data contract: every
// entry in the slice comes back with a non-nil Journey, and the status reflects
// that hive's own state rather than a shared or last-wins value.
func TestAttachJourneyStatusFillsEveryHive(t *testing.T) {
	// One hive stalled at stage 1 (no GitHub App), one fully satisfied.
	stalled := *hive(30 * 24 * time.Hour)
	stalled.ID = "stalled"
	stalled.PendingGitHubAppInstall = true

	healthy := *hive(30 * 24 * time.Hour)
	healthy.ID = "healthy"
	healthy.PendingGitHubAppInstall = false

	s := dispatchTestServer(t)
	hives := []RegistryEntry{stalled, healthy}
	s.attachJourneyStatus(hives, baseTime)

	for i := range hives {
		if hives[i].Journey == nil {
			t.Fatalf("hive %q has nil Journey, want a computed status", hives[i].ID)
		}
	}
	if hives[0].Journey.Stage != StageGitHubApp.String() {
		t.Errorf("stalled hive stage = %q, want %q", hives[0].Journey.Stage, StageGitHubApp.String())
	}
	if hives[0].Journey.Stage == hives[1].Journey.Stage {
		t.Errorf("both hives reported stage %q; status is not per-hive", hives[0].Journey.Stage)
	}
}

// TestAttachJourneyStatusNilStoreIsNoOp covers the nil-store guard: a HubServer
// without a journey store must leave the slice untouched, not panic.
func TestAttachJourneyStatusNilStoreIsNoOp(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	hives := []RegistryEntry{*hive(30 * 24 * time.Hour)}

	s.attachJourneyStatus(hives, baseTime)

	if hives[0].Journey != nil {
		t.Errorf("Journey = %+v, want nil when the server has no journey store", hives[0].Journey)
	}
}

// ── handleJourneyStatus ─────────────────────────────────────────────────────

// TestHandleJourneyStatusReturnsEveryHive checks the admin status endpoint
// returns one row per registered hive, carrying identity plus a computed
// status.
func TestHandleJourneyStatusReturnsEveryHive(t *testing.T) {
	h1 := *hive(30 * 24 * time.Hour)
	h1.ID = "hive-a"
	h1.Name = "acme/alpha"
	h1.Owner = "alice"
	h1.PendingGitHubAppInstall = true

	h2 := *hive(30 * 24 * time.Hour)
	h2.ID = "hive-b"
	h2.Name = "acme/beta"
	h2.Owner = "bob"

	s := dispatchTestServer(t, h1, h2)

	req := httptest.NewRequest(http.MethodGet, "/api/saas/admin/journey-status", nil)
	rec := httptest.NewRecorder()
	s.handleJourneyStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp struct {
		Hives []struct {
			HiveID string         `json:"hive_id"`
			Name   string         `json:"name"`
			Owner  string         `json:"owner"`
			Status *JourneyStatus `json:"status"`
		} `json:"hives"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	if len(resp.Hives) != 2 {
		t.Fatalf("got %d rows, want 2", len(resp.Hives))
	}
	byID := map[string]string{}
	for _, r := range resp.Hives {
		if r.Status == nil {
			t.Errorf("hive %q has a null status", r.HiveID)
			continue
		}
		byID[r.HiveID] = r.Owner
	}
	if byID["hive-a"] != "alice" || byID["hive-b"] != "bob" {
		t.Errorf("owners = %v, want hive-a=alice hive-b=bob", byID)
	}
}

// TestHandleJourneyStatusEmptyRegistry checks an empty registry serialises as
// an empty array, not JSON null — the UI iterates this field directly.
func TestHandleJourneyStatusEmptyRegistry(t *testing.T) {
	s := dispatchTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/saas/admin/journey-status", nil)
	rec := httptest.NewRecorder()
	s.handleJourneyStatus(rec, req)

	if !strings.Contains(rec.Body.String(), `"hives":[]`) {
		t.Errorf("body = %q, want an empty hives array", rec.Body.String())
	}
}

// ── handleJourneySnooze ─────────────────────────────────────────────────────

// TestHandleJourneySnoozeSetsExemption is the primary contract: a valid request
// records a snooze that actually suppresses journey nudges for that hive.
func TestHandleJourneySnoozeSetsExemption(t *testing.T) {
	s := dispatchTestServer(t)

	rec := postSnoozeJSON(t, s.handleJourneySnooze, `{"hive_id":"hive-1","duration":"24h","reason":"vendor pilot"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Ok           bool   `json:"ok"`
		HiveID       string `json:"hive_id"`
		Snoozed      bool   `json:"snoozed"`
		SnoozedUntil string `json:"snoozed_until"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Ok || resp.HiveID != "hive-1" || !resp.Snoozed {
		t.Errorf("resp = %+v, want ok+snoozed for hive-1", resp)
	}
	if resp.SnoozedUntil == "" {
		t.Error("snoozed_until is empty, want an RFC3339 expiry")
	}

	// The stored state must actually suppress nudges.
	st := s.journey.get("hive-1")
	if st == nil {
		t.Fatal("no journey state recorded for hive-1")
	}
	if st.SnoozeReason != "vendor pilot" {
		t.Errorf("SnoozeReason = %q, want %q", st.SnoozeReason, "vendor pilot")
	}
	if !st.snoozeActive(time.Now()) {
		t.Error("snoozeActive = false immediately after setting a 24h snooze")
	}
}

// TestHandleJourneySnoozeClearsOnEmptyDuration covers the documented "empty or
// zero duration clears the snooze" rule, including the round trip: set, then
// clear, then confirm the exemption is gone.
func TestHandleJourneySnoozeClearsOnEmptyDuration(t *testing.T) {
	for _, dur := range []string{"", "0"} {
		t.Run("duration="+dur, func(t *testing.T) {
			s := dispatchTestServer(t)
			s.journey.setSnooze("hive-1", time.Now().Add(snoozeTestDuration), "admin", "pilot")
			if !s.journey.get("hive-1").snoozeActive(time.Now()) {
				t.Fatal("precondition: snooze was not active before clearing")
			}

			rec := postSnoozeJSON(t, s.handleJourneySnooze, `{"hive_id":"hive-1","duration":"`+dur+`"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
			}
			var resp struct {
				Snoozed      bool   `json:"snoozed"`
				SnoozedUntil string `json:"snoozed_until"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Snoozed {
				t.Error(`resp["snoozed"] = true, want false after clearing`)
			}
			if resp.SnoozedUntil != "" {
				t.Errorf("snoozed_until = %q, want it omitted when cleared", resp.SnoozedUntil)
			}
			if s.journey.get("hive-1").snoozeActive(time.Now()) {
				t.Error("snooze is still active after a clear request")
			}
		})
	}
}

// TestHandleJourneySnoozeRejectsBadInput covers the three rejection paths:
// unparseable body, missing hive_id, and a duration past the cap. Each must be
// a 400, and none may record any state.
func TestHandleJourneySnoozeRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed json", body: `{not json`},
		{name: "missing hive_id", body: `{"duration":"24h"}`},
		{name: "blank hive_id", body: `{"hive_id":"   ","duration":"24h"}`},
		{name: "unparseable duration", body: `{"hive_id":"hive-1","duration":"forever"}`},
		{name: "duration beyond the cap", body: `{"hive_id":"hive-1","duration":"9000h"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := dispatchTestServer(t)
			rec := postSnoozeJSON(t, s.handleJourneySnooze, tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
			}
			if st := s.journey.get("hive-1"); st != nil && st.snoozeActive(time.Now()) {
				t.Error("a rejected request still recorded an active snooze")
			}
		})
	}
}

// TestHandleJourneySnoozeTruncatesLongReason checks the PVC-bound guard: an
// over-long operator note is truncated to maxSnoozeReasonLen runes rather than
// stored whole or rejected.
func TestHandleJourneySnoozeTruncatesLongReason(t *testing.T) {
	// Mirrors the maxSnoozeReasonLen constant inside handleJourneySnooze.
	const wantMaxReasonLen = 200
	// Multi-byte runes ensure the truncation is rune-based, not byte-based:
	// a byte-slice truncation would both cut at the wrong place and risk
	// splitting a rune into invalid UTF-8.
	longReason := strings.Repeat("é", wantMaxReasonLen+50)

	s := dispatchTestServer(t)
	body, err := json.Marshal(map[string]string{
		"hive_id":  "hive-1",
		"duration": "24h",
		"reason":   longReason,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := postSnoozeJSON(t, s.handleJourneySnooze, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	st := s.journey.get("hive-1")
	if st == nil {
		t.Fatal("no journey state recorded")
	}
	if got := len([]rune(st.SnoozeReason)); got != wantMaxReasonLen {
		t.Errorf("stored reason length = %d runes, want %d", got, wantMaxReasonLen)
	}
	if !strings.HasPrefix(longReason, st.SnoozeReason) {
		t.Error("stored reason is not a prefix of the submitted reason")
	}
}

// TestHandleJourneySnoozeSuppressesNudge is the end-to-end reason the endpoint
// exists: after an admin snoozes a hive, journeyBannerFor must return nothing
// for it, even though the same hive is nudged without the snooze.
func TestHandleJourneySnoozeSuppressesNudge(t *testing.T) {
	// A long-registered hive with no GitHub App is squarely due a stage-1 nudge.
	stalled := *hive(60 * 24 * time.Hour)
	stalled.ID = "hive-1"
	stalled.PendingGitHubAppInstall = true

	now := time.Now()
	s := dispatchTestServer(t, stalled)

	if banner := s.journeyBannerFor("hive-1", now); banner == nil {
		t.Fatal("precondition: expected a nudge for an un-snoozed stalled hive")
	}

	// Fresh server so the just-recorded "sent" state does not confound the
	// second half of the check.
	s = dispatchTestServer(t, stalled)
	rec := postSnoozeJSON(t, s.handleJourneySnooze, `{"hive_id":"hive-1","duration":"24h","reason":"vendor pilot"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("snooze status = %d, want 200", rec.Code)
	}
	if banner := s.journeyBannerFor("hive-1", now); banner != nil {
		t.Errorf("journeyBannerFor returned %+v for a snoozed hive, want nil", banner)
	}
}
