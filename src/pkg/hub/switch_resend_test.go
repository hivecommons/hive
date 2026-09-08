package hub

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSwitchRecentlySent(t *testing.T) {
	s := newHeartbeatHub()
	s.heartbeatSwitchSent = make(map[string]switchSend)
	now := time.Now()

	if _, recent := s.switchRecentlySent("h1", "candidate", now); recent {
		t.Fatal("nothing sent yet must not read as recent")
	}
	s.heartbeatSwitchSent["h1"] = switchSend{Tag: "candidate", At: now}
	if _, recent := s.switchRecentlySent("h1", "candidate", now.Add(switchResendInterval/2)); !recent {
		t.Error("same tag inside the window must be recent")
	}
	if _, recent := s.switchRecentlySent("h1", "candidate", now.Add(switchResendInterval)); recent {
		t.Error("same tag at the window's edge must be due again")
	}
	if _, recent := s.switchRecentlySent("h1", "stable", now.Add(time.Second)); recent {
		t.Error("a DIFFERENT tag is a new switch and must go out immediately")
	}
	if _, recent := s.switchRecentlySent("h2", "candidate", now.Add(time.Second)); recent {
		t.Error("another hive's send must not count")
	}
}

// The loop as observed: the old pod heartbeats the old tag on every beat while
// the new pod initializes. The FIRST beat carries the switch; the next beats
// inside the window must not, or the spoke re-stamps its Deployment and kills
// the mid-init pod each time.
func TestHandleHeartbeatSwitchTagIsNotResentInsideTheWindow(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1"}}
	s.heartbeatSwitchTag["h1"] = "candidate"

	beat := `{"hive_id":"h1","git_branch":"v4","image_ref":"ghcr.io/hivecommons/hive:stable"}`

	rec := postHeartbeat(t, s, beat)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"switch_to_tag":"candidate"`) {
		t.Fatalf("first beat must carry the switch, got %s", rec.Body.String())
	}

	// Old pod, still on :stable, beats again 30s later.
	rec = postHeartbeat(t, s, beat)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "switch_to_tag") {
		t.Fatalf("switch re-sent inside the resend window — this is the re-patch loop: %s", rec.Body.String())
	}
	s.mu.RLock()
	_, armed := s.heartbeatSwitchTag["h1"]
	s.mu.RUnlock()
	if !armed {
		t.Fatal("withholding a re-send must keep the switch armed")
	}

	// Window elapsed (the spoke's PATCH may genuinely have failed): re-send.
	s.mu.Lock()
	s.heartbeatSwitchSent["h1"] = switchSend{Tag: "candidate", At: time.Now().Add(-switchResendInterval - time.Second)}
	s.mu.Unlock()
	rec = postHeartbeat(t, s, beat)
	if !strings.Contains(rec.Body.String(), `"switch_to_tag":"candidate"`) {
		t.Fatalf("switch must be re-sent once the window elapses, got %s", rec.Body.String())
	}

	// Spoke reports the new tag: switch complete, clock cleared.
	rec = postHeartbeat(t, s, `{"hive_id":"h1","git_branch":"v4","image_ref":"ghcr.io/hivecommons/hive:candidate"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	s.mu.RLock()
	_, armed = s.heartbeatSwitchTag["h1"]
	_, clocked := s.heartbeatSwitchSent["h1"]
	s.mu.RUnlock()
	if armed || clocked {
		t.Errorf("completed switch must clear both the tag (%v) and the send clock (%v)", armed, clocked)
	}
}

// A newly armed DIFFERENT tag is a new operator decision and goes out on the
// very next beat, window or not.
func TestHandleHeartbeatNewSwitchTagGoesOutImmediately(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1"}}
	s.heartbeatSwitchTag["h1"] = "candidate"
	s.heartbeatSwitchSent = map[string]switchSend{"h1": {Tag: "candidate", At: time.Now()}}

	s.mu.Lock()
	s.heartbeatSwitchTag["h1"] = "stable"
	s.mu.Unlock()
	rec := postHeartbeat(t, s, `{"hive_id":"h1","git_branch":"v4","image_ref":"ghcr.io/hivecommons/hive:candidate"}`)
	if !strings.Contains(rec.Body.String(), `"switch_to_tag":"stable"`) {
		t.Fatalf("a different tag must be delivered immediately, got %s", rec.Body.String())
	}
}
