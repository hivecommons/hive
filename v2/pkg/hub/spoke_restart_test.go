package hub

import (
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Two instances alternating as one hive is the production signature this
// package exists to catch (the WVA flip: ok at :22, key-invalid at :58,
// forever). A single reporter change is a rollout and must stay silent.
func TestNoteReporterFlagsAlternation(t *testing.T) {
	s := &HubServer{logger: slog.Default()}

	if got := s.noteReporter("h1", "pod-a"); got != "" {
		t.Fatalf("first beat flagged: %q", got)
	}
	if got := s.noteReporter("h1", "pod-b"); got != "" {
		t.Fatalf("single handover flagged (a rollout would false-positive): %q", got)
	}
	if got := s.noteReporter("h1", "pod-a"); got != "" {
		t.Fatalf("first return flagged too early: %q", got)
	}
	got := s.noteReporter("h1", "pod-b")
	if got == "" {
		t.Fatal("sustained alternation was not flagged")
	}
	if !strings.Contains(got, "pod-a") || !strings.Contains(got, "pod-b") {
		t.Errorf("conflict does not name both reporters: %q", got)
	}
	// The conflict must persist while the alternation continues.
	if got := s.noteReporter("h1", "pod-a"); got == "" {
		t.Error("ongoing alternation lost the conflict")
	}
}

func TestNoteReporterSilentOnRolloutAndUnknown(t *testing.T) {
	s := &HubServer{logger: slog.Default()}

	// Rollout: a → b, then b keeps reporting. Never a conflict.
	s.noteReporter("h2", "pod-a")
	s.noteReporter("h2", "pod-b")
	for i := 0; i < 5; i++ {
		if got := s.noteReporter("h2", "pod-b"); got != "" {
			t.Fatalf("steady reporter after rollout flagged: %q", got)
		}
	}

	// A spoke too old to report one sends "": UNKNOWN, never data, and it
	// must not disturb the state of what HAS been reported.
	s3 := &HubServer{logger: slog.Default()}
	s3.noteReporter("h3", "pod-a")
	s3.noteReporter("h3", "pod-b")
	s3.noteReporter("h3", "")
	s3.noteReporter("h3", "pod-a")
	if got := s3.noteReporter("h3", "pod-b"); got == "" {
		t.Error("an interleaved empty reporter destroyed alternation tracking")
	}
}

// An armed restart rides every beat inside the window (all instances must
// hear it) and the record clears itself on the first beat after it.
func TestRestartSpokeDeliveryWindow(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{logger: slog.Default()}

	// Not armed → nothing.
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice"})
	if s.restartSpokeForHeartbeat("h1") {
		t.Fatal("unarmed hive got a restart")
	}

	// Freshly armed → delivered, and STILL armed for the next beat.
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice",
		RequestedRestartAt: time.Now().UTC().Format(time.RFC3339)})
	if !s.restartSpokeForHeartbeat("h1") {
		t.Fatal("armed restart not delivered")
	}
	if !s.restartSpokeForHeartbeat("h1") {
		t.Fatal("second beat inside the window missed the restart — a second instance would never hear it")
	}

	// Expired → not delivered, and the record is cleared durably.
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice",
		RequestedRestartAt: time.Now().Add(-spokeRestartDeliveryWindow - time.Minute).UTC().Format(time.RFC3339)})
	if s.restartSpokeForHeartbeat("h1") {
		t.Fatal("expired restart still delivered")
	}
	if h := loadSaaSHive("h1"); h.RequestedRestartAt != "" {
		t.Errorf("expired restart not cleared from the record: %q", h.RequestedRestartAt)
	}

	// Unparseable → cleared, never delivered.
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", RequestedRestartAt: "not-a-time"})
	if s.restartSpokeForHeartbeat("h1") {
		t.Fatal("unparseable timestamp delivered a restart")
	}
	if h := loadSaaSHive("h1"); h.RequestedRestartAt != "" {
		t.Errorf("unparseable timestamp not cleared: %q", h.RequestedRestartAt)
	}
}

func TestHandleRestartSpokeArmsDurably(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := &HubServer{hubSecret: testHubSecret, logger: slog.Default()}
	mkUser(t, hubAdminUsername)
	mkUser(t, "mallory")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "someone"})

	// Non-admin: forbidden, nothing armed.
	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser("POST", "/rs", "", "mallory"), "id", "h1")
	s.handleRestartSpoke(rec, req)
	if rec.Code != 403 {
		t.Fatalf("non-admin restart status = %d, want 403", rec.Code)
	}
	if h := loadSaaSHive("h1"); h.RequestedRestartAt != "" {
		t.Fatal("non-admin request armed a restart")
	}

	// Admin: armed with a parseable fresh timestamp.
	rec = httptest.NewRecorder()
	req = setPathValue(reqWithUser("POST", "/rs", "", hubAdminUsername), "id", "h1")
	s.handleRestartSpoke(rec, req)
	if rec.Code != 200 {
		t.Fatalf("admin restart status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	h := loadSaaSHive("h1")
	armedAt, err := time.Parse(time.RFC3339, h.RequestedRestartAt)
	if err != nil {
		t.Fatalf("armed timestamp unparseable: %q (%v)", h.RequestedRestartAt, err)
	}
	if time.Since(armedAt) > time.Minute {
		t.Errorf("armed timestamp not fresh: %v", armedAt)
	}
	// And the armed record is exactly what delivery reads.
	if !s.restartSpokeForHeartbeat("h1") {
		t.Error("endpoint-armed restart not delivered on the next beat")
	}
}
