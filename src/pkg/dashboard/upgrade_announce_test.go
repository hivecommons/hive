package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// #15788: every roll — hub-driven, operator kubectl, bare SIGTERM — must
// surface through the same upgradeProgress the self-upgrade uses, and the
// SIGTERM path must publish an SSE "upgrading" frame before the stream drops.

func announceOwnerRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(ownerRoleVerifiedHeader, "true")
	return req
}

func TestUpgradeAnnounceRejectsNonOwner(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/upgrade/announce", strings.NewReader(`{"target":"abc1234"}`))
	req.Header.Set("X-Hive-Role", "read-write")
	rr := httptest.NewRecorder()
	s.handleUpgradeAnnounce(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if s.upgradeAnnounce.get() != nil {
		t.Fatal("a rejected announce must not be recorded")
	}
}

func TestUpgradeAnnounceMarksProgressAndBroadcasts(t *testing.T) {
	s := newTestServer()
	ch := make(chan []byte, 4)
	s.sseClients[ch] = struct{}{}
	s.UpdateStatus(minimalPayload())
	for len(ch) > 0 { // drop the status frame UpdateStatus just published
		<-ch
	}

	rr := httptest.NewRecorder()
	s.handleUpgradeAnnounce(rr, announceOwnerRequest(http.MethodPost, "/api/upgrade/announce",
		`{"version":"0123456789abcdef0123456789abcdef01234567","source":"hub","reason":"hub-managed upgrade"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK       bool                   `json:"ok"`
		Progress *spoke.UpgradeProgress `json:"upgradeProgress"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Progress == nil {
		t.Fatalf("resp = %s", rr.Body.String())
	}
	if resp.Progress.State != spoke.UpgradeStatePulling || resp.Progress.Phase != spoke.UpgradePhaseAnnounced {
		t.Fatalf("state/phase = %s/%s, want pulling/announced", resp.Progress.State, resp.Progress.Phase)
	}
	if resp.Progress.Target != "0123456" || resp.Progress.Source != spoke.UpgradeSourceHub {
		t.Fatalf("target/source = %q/%q", resp.Progress.Target, resp.Progress.Source)
	}

	// The same record drives /api/status's upgradeProgress — the component
	// the self-upgrade renders through.
	p := s.upgradeProgress(s.status, time.Now())
	if p == nil || p.Phase != spoke.UpgradePhaseAnnounced || p.Target != "0123456" {
		t.Fatalf("upgradeProgress = %+v", p)
	}

	select {
	case frame := <-ch:
		if !strings.HasPrefix(string(frame), "event: upgrading\n") {
			t.Fatalf("frame = %q", frame)
		}
		var ev upgradeAnnounceEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(string(frame), "event: upgrading\ndata: "))), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Phase != spoke.UpgradePhaseAnnounced || ev.Target != "0123456" {
			t.Fatalf("event = %+v", ev)
		}
	default:
		t.Fatal("announce published no SSE frame")
	}
}

func TestAnnounceRestartKeepsAnnouncedTargetAndPublishesRestarting(t *testing.T) {
	s := newTestServer()
	s.UpdateStatus(minimalPayload())
	now := time.Now()
	s.AnnounceUpgrade(spoke.UpgradeAnnounce{Target: "feedfacefeedfacefeedfacefeedfacefeedface", Source: spoke.UpgradeSourceOperator}, now)

	ch := make(chan []byte, 4)
	s.sseClients[ch] = struct{}{}
	p := s.AnnounceRestart(now.Add(time.Second))
	if p.State != spoke.UpgradeStateRolling || p.Phase != spoke.UpgradePhaseRestarting {
		t.Fatalf("state/phase = %s/%s", p.State, p.Phase)
	}
	if p.Target != "feedfac" || p.Source != spoke.UpgradeSourceOperator {
		t.Fatalf("restart lost the announce: %+v", p)
	}
	if !strings.Contains(p.Detail, "reconnecting") {
		t.Fatalf("detail = %q", p.Detail)
	}
	select {
	case frame := <-ch:
		if !strings.Contains(string(frame), `"phase":"restarting"`) || !strings.Contains(string(frame), `"processStartedAt"`) {
			t.Fatalf("frame = %q", frame)
		}
	default:
		t.Fatal("restart published no SSE frame")
	}
	// Restart wins over the drain/marker chain in buildUpgradeProgress.
	if got := s.upgradeProgress(s.status, now.Add(2*time.Second)); got == nil || got.Phase != spoke.UpgradePhaseRestarting {
		t.Fatalf("upgradeProgress = %+v", got)
	}
}

func TestAnnounceRestartFallsBackToHubTarget(t *testing.T) {
	s := newTestServer()
	s.SetHubUpgradePolicy(&spoke.HeartbeatUpgradePolicy{ArmedTarget: "cafebabecafebabecafebabecafebabecafebabe"})
	p := s.AnnounceRestart(time.Now())
	if p.Target != "cafebab" || p.Source != spoke.UpgradeSourceSignal {
		t.Fatalf("progress = %+v", p)
	}

	bare := newTestServer()
	if got := bare.AnnounceRestart(time.Now()); got.Target != "" || got.Detail != "restarting · reconnecting" {
		t.Fatalf("bare restart = %+v", got)
	}
}

func TestUpgradeAnnounceExpiresWithoutAKill(t *testing.T) {
	now := time.Now()
	st := &upgradeAnnounceState{Target: "abc", At: now}
	if liveUpgradeAnnounce(st, now.Add(upgradeAnnounceStaleAfter-time.Minute)) == nil {
		t.Fatal("fresh announce dropped")
	}
	if liveUpgradeAnnounce(st, now.Add(upgradeAnnounceStaleAfter+time.Minute)) != nil {
		t.Fatal("stale announce kept — an aborted roll would animate forever")
	}
	restart := &upgradeAnnounceState{Restarting: true, RestartAt: now}
	if liveUpgradeAnnounce(restart, now.Add(24*time.Hour)) == nil {
		t.Fatal("a restart record must never expire: the process is exiting")
	}
}

func TestInitializingStatusCarriesStartupPhase(t *testing.T) {
	s := newTestServer()
	s.status = nil
	s.SetStartupPhase("bootStores")
	if got := s.StartupPhase(); got != "bootStores" {
		t.Fatalf("StartupPhase = %q", got)
	}
	rr := httptest.NewRecorder()
	s.handleStatus(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "initializing" || body["startupPhase"] != "bootStores" {
		t.Fatalf("body = %v", body)
	}
	if label, _ := body["startupPhaseLabel"].(string); label == "" || label == "bootStores" {
		t.Fatalf("startupPhaseLabel = %q, want a human label", label)
	}
	if _, ok := body["startedAt"]; !ok {
		t.Fatal("initializing status must carry the process startedAt")
	}

	// Once the first status lands the phase is over, whatever was recorded.
	s.UpdateStatus(minimalPayload())
	if got := s.StartupPhase(); got != "" {
		t.Fatalf("StartupPhase after first status = %q, want empty", got)
	}
	rr = httptest.NewRecorder()
	s.handleVersion(rr, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if !strings.Contains(rr.Body.String(), `"startedAt"`) || strings.Contains(rr.Body.String(), `"startupPhase"`) {
		t.Fatalf("/api/version = %s", rr.Body.String())
	}
}
