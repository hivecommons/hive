package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hub"
)

// These tests drive the hub UpgradeCallback registered by bootHeartbeatWith
// through its marker/backoff branches (#7990 recommendation 2). They rely on
// the upgradeMarkerPath seam in selfupgrade.go and on the fact that this test
// process is always younger than the 5-minute uptime floor, so every branch
// that would otherwise reach the Deployment patch stops at the floor instead.

const upgradeMarkerTestCurrent = "0123456789abcdef"

// pinUpgradeMarker points the callback at a temp marker file and pins the
// running commit; both are restored on cleanup.
func pinUpgradeMarker(t *testing.T) string {
	t.Helper()
	oldPath, oldShort := upgradeMarkerPath, gitShort
	upgradeMarkerPath = filepath.Join(t.TempDir(), "upgrade-requested")
	gitShort = upgradeMarkerTestCurrent
	t.Cleanup(func() { upgradeMarkerPath, gitShort = oldPath, oldShort })
	return upgradeMarkerPath
}

func writeTestUpgradeMarker(t *testing.T, path string, m upgradeMarker) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestUpgradeMarker(t *testing.T, path string) upgradeMarker {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("marker unreadable: %v", err)
	}
	return parseUpgradeMarker(data)
}

func TestBootHeartbeatUpgradeCallback_GivesUpAfterMaxAttemptsAndReportsToHub(t *testing.T) {
	var reports atomic.Int32
	var reported hub.HeartbeatPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/heartbeat" || r.Method != http.MethodPost {
			t.Errorf("unexpected hub call %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &reported)
		reports.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newBootHeartbeatFake()
	cfg := bootHeartbeatConfig()
	cfg.Hub.URL = srv.URL
	newBootHeartbeatCollect(t, f, cfg)
	path := pinUpgradeMarker(t)

	writeTestUpgradeMarker(t, path, upgradeMarker{
		TargetSHA:   "feedbeef",
		CurrentSHA:  upgradeMarkerTestCurrent,
		RequestedAt: time.Now().UTC().Add(-time.Hour),
		Attempts:    selfUpgradeMaxAttempts,
		LastError:   "deployments.apps is forbidden",
	})

	heartbeatCallback[hub.UpgradeCallback](t, f)("feedbeef")

	if !strings.Contains(f.log.String(), "self-upgrade FAILED: giving up after repeated attempts") {
		t.Fatalf("terminal give-up not logged:\n%s", f.log.String())
	}
	if got := reports.Load(); got != 1 {
		t.Fatalf("hub failure reports = %d, want 1", got)
	}
	if !reported.UpgradeFailed || reported.UpgradeTargetSHA != "feedbeef" {
		t.Fatalf("hub report = %+v, want UpgradeFailed for feedbeef", reported)
	}
	if !strings.Contains(reported.UpgradeError, "deployments.apps is forbidden") {
		t.Fatalf("hub report lost the real cause: %q", reported.UpgradeError)
	}
	// Terminal means terminal: the marker must survive unchanged so the next
	// heartbeat does not restart the attempt budget.
	if m := readTestUpgradeMarker(t, path); m.Attempts != selfUpgradeMaxAttempts {
		t.Fatalf("marker attempts = %d after give-up, want %d (untouched)", m.Attempts, selfUpgradeMaxAttempts)
	}
}

func TestBootHeartbeatUpgradeCallback_DefersRetryInsideBackoffWindow(t *testing.T) {
	f := newBootHeartbeatFake()
	newBootHeartbeatCollect(t, f, bootHeartbeatConfig())
	path := pinUpgradeMarker(t)

	writeTestUpgradeMarker(t, path, upgradeMarker{
		TargetSHA:   "feedbeef",
		CurrentSHA:  upgradeMarkerTestCurrent,
		RequestedAt: time.Now().UTC(),
		Attempts:    1,
	})

	heartbeatCallback[hub.UpgradeCallback](t, f)("feedbeef")

	if !strings.Contains(f.log.String(), "self-upgrade retry deferred: backing off after a failed attempt") {
		t.Fatalf("in-window retry not deferred:\n%s", f.log.String())
	}
	if strings.Contains(f.log.String(), "self-upgrade triggered") {
		t.Fatalf("deferred retry still triggered an upgrade:\n%s", f.log.String())
	}
	if m := readTestUpgradeMarker(t, path); m.Attempts != 1 {
		t.Fatalf("deferred retry rewrote the marker: attempts = %d, want 1", m.Attempts)
	}
}

func TestBootHeartbeatUpgradeCallback_RetriesAfterBackoffThenHitsUptimeFloor(t *testing.T) {
	f := newBootHeartbeatFake()
	newBootHeartbeatCollect(t, f, bootHeartbeatConfig())
	path := pinUpgradeMarker(t)

	writeTestUpgradeMarker(t, path, upgradeMarker{
		TargetSHA:   "feedbeef",
		CurrentSHA:  upgradeMarkerTestCurrent,
		RequestedAt: time.Now().UTC().Add(-2 * selfUpgradeMaxBackoff),
		Attempts:    2,
		LastError:   "registry blip",
	})

	heartbeatCallback[hub.UpgradeCallback](t, f)("feedbeef")

	log := f.log.String()
	if !strings.Contains(log, "self-upgrade retrying after a failed attempt (image unchanged)") {
		t.Fatalf("expired backoff did not retry:\n%s", log)
	}
	// The process is far younger than the 5-minute floor, so the retry must
	// stop there rather than write a new marker or exit.
	if !strings.Contains(log, "self-upgrade deferred: minimum uptime not reached") {
		t.Fatalf("retry did not stop at the uptime floor:\n%s", log)
	}
	if strings.Contains(log, "self-upgrade triggered") {
		t.Fatalf("retry under the uptime floor still triggered an upgrade:\n%s", log)
	}
	if m := readTestUpgradeMarker(t, path); m.Attempts != 2 {
		t.Fatalf("uptime-floor deferral rewrote the marker: attempts = %d, want 2", m.Attempts)
	}
}

func TestBootHeartbeatUpgradeCallback_ClearsStaleMarkerForDifferentTarget(t *testing.T) {
	f := newBootHeartbeatFake()
	newBootHeartbeatCollect(t, f, bootHeartbeatConfig())
	path := pinUpgradeMarker(t)

	// Exhausted budget for an OLD target must not block a NEW one.
	writeTestUpgradeMarker(t, path, upgradeMarker{
		TargetSHA:   "0ldta4get",
		CurrentSHA:  upgradeMarkerTestCurrent,
		RequestedAt: time.Now().UTC(),
		Attempts:    selfUpgradeMaxAttempts,
	})

	heartbeatCallback[hub.UpgradeCallback](t, f)("feedbeef")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale marker for a different target not removed (stat err=%v)", err)
	}
	log := f.log.String()
	if strings.Contains(log, "giving up after repeated attempts") {
		t.Fatalf("stale target's exhausted budget applied to the new target:\n%s", log)
	}
	if !strings.Contains(log, "self-upgrade deferred: minimum uptime not reached") {
		t.Fatalf("new target did not proceed to the uptime floor:\n%s", log)
	}
}

func TestBootHeartbeatUpgradeCallback_ClearsStaleMarkerFromPreviousImage(t *testing.T) {
	f := newBootHeartbeatFake()
	newBootHeartbeatCollect(t, f, bootHeartbeatConfig())
	path := pinUpgradeMarker(t)

	// A marker written by a DIFFERENT running commit means the image did
	// change since — that is a landed upgrade, not a failed one.
	writeTestUpgradeMarker(t, path, upgradeMarker{
		TargetSHA:   "feedbeef",
		CurrentSHA:  "previ0us",
		RequestedAt: time.Now().UTC(),
		Attempts:    3,
	})

	heartbeatCallback[hub.UpgradeCallback](t, f)("feedbeef")

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("marker from a previous image not cleared (stat err=%v)", err)
	}
	if strings.Contains(f.log.String(), "retrying after a failed attempt") {
		t.Fatalf("previous image's attempts counted against this one:\n%s", f.log.String())
	}
}

func TestBootHeartbeatUpgradeCallback_NoMarkerStopsAtUptimeFloorWithoutWriting(t *testing.T) {
	f := newBootHeartbeatFake()
	newBootHeartbeatCollect(t, f, bootHeartbeatConfig())
	path := pinUpgradeMarker(t)

	heartbeatCallback[hub.UpgradeCallback](t, f)("feedbeef")

	if !strings.Contains(f.log.String(), "self-upgrade deferred: minimum uptime not reached") {
		t.Fatalf("fresh upgrade did not stop at the uptime floor:\n%s", f.log.String())
	}
	// The attempt is recorded only once the floor is cleared; a deferral
	// must not burn budget.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("uptime-floor deferral wrote a marker (stat err=%v)", err)
	}
}
