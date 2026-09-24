package dashboard

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/config"
	standbypkg "github.com/hivecommons/hive/pkg/standby"
)

func standbyAutoDispatchConfig(auto bool, floor string, cap int) *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Org: "alice", Repos: []string{"repo"}},
		Agents: map[string]config.AgentConfig{"quality": {
			Mode: "ISSUES_AND_PRS",
			Standby: &config.StandbyConfig{
				Enabled:                true,
				AutoDispatch:           auto,
				MinModelCapability:     floor,
				DailyCapPerContributor: cap,
			},
		}},
		Hub: config.HubConfig{
			StandbyContributors: []string{"alice"},
			StandbyModelTiers: []config.StandbyModelTier{{
				Backend: "copilot",
				Model:   "gpt-5.4-mini",
				Tier:    "T2",
			}},
			StandbyItemTiers: []config.StandbyItemTier{{
				Repo:   "alice/repo",
				Label:  "standby-e2e-t3",
				Tier:   "T2",
				Signal: "tiny diff plus green tests",
			}},
		},
	}
}

func connectStandbyRelay(t *testing.T, s *Server, tsURL string) *websocket.Conn {
	t.Helper()
	token, _ := registerWSUser(t, s, "alice")
	conn, _, err := websocket.DefaultDialer.Dial(tsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	readMsg(t, conn) // challenge
	if err := conn.WriteJSON(WSMessage{
		Type:              "auth_response",
		RegistrationToken: token,
		CLIBackend:        "copilot",
		Model:             "gpt-5.4-mini",
		Capabilities:      &ContributorCapabilities{RelayProtocolVersion: contributorProtocolVersion},
	}); err != nil {
		t.Fatalf("auth write: %v", err)
	}
	if msg := readMsg(t, conn); msg.Type != "auth_ok" {
		t.Fatalf("auth = %s: %s", msg.Type, msg.Reason)
	}
	if err := conn.WriteJSON(WSMessage{Type: "standby_declare", Seq: 2, Standby: &WSStandby{Lanes: []string{"quality"}}}); err != nil {
		t.Fatalf("standby write: %v", err)
	}
	if msg := readMsg(t, conn); msg.Type != "standby_ack" {
		t.Fatalf("standby ack = %s: %s", msg.Type, msg.Reason)
	}
	return conn
}

func seedStandbyAutoQueue(s *Server) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.status = standbyAutoStatus(1, false)
}

func standbyAutoStatus(number int, held bool) *StatusPayload {
	return &StatusPayload{Repos: []FrontendRepo{{
		Name: "repo",
		Full: "alice/repo",
		ActionableIssues: []any{map[string]any{
			"repo":   "alice/repo",
			"number": float64(number),
			"title":  "tiny standby item",
			"lane":   "quality",
			"labels": []any{"standby-e2e-t3"},
		}},
	}}}
}

func standbyAutoDispatchHarness(t *testing.T, cfg *config.Config) (*Server, *websocket.Conn) {
	t.Helper()
	s, ts := setupWSTest(t)
	t.Cleanup(ts.Close)
	if s.deps == nil {
		s.deps = &Dependencies{}
	}
	s.deps.Config = cfg
	s.contributeHub.persistTaskLedgers = false
	seedStandbyAutoQueue(s)
	return s, connectStandbyRelay(t, s, wsURL(ts))
}

func TestStandbyAutoDispatchOffByDefault(t *testing.T) {
	cfg := standbyAutoDispatchConfig(false, "T2", 1)
	s, conn := standbyAutoDispatchHarness(t, cfg)

	s.contributeHub.AutoDispatchStandby([]string{"quality"})
	conn.SetReadDeadline(testReadDeadline())
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("auto_dispatch=false sent an assignment")
	}
}

func TestStandbyAutoDispatchAssignsThroughHoldMarkedPath(t *testing.T) {
	cfg := standbyAutoDispatchConfig(true, "T2", 1)
	s, conn := standbyAutoDispatchHarness(t, cfg)

	s.contributeHub.AutoDispatchStandby([]string{"quality"})
	msg := readMsg(t, conn)
	if msg.Type != "task_assign" {
		t.Fatalf("auto dispatch msg = %s: %s", msg.Type, msg.Reason)
	}
	if msg.StandbyLane != "quality" || msg.StandbyTier != "T2" {
		t.Fatalf("standby markers = lane %q tier %q, want quality/T2", msg.StandbyLane, msg.StandbyTier)
	}
}

func TestStandbyAutoDispatchUsesAcceptedFreshStatusQueue(t *testing.T) {
	cfg := standbyAutoDispatchConfig(true, "T2", 1)
	s, conn := standbyAutoDispatchHarness(t, cfg)

	status := standbyAutoStatus(2, false)
	status.Governor.SuppressedLanes = []string{"quality"}
	if !s.UpdateStatusIfFresh(status, s.BeginStatusSnapshot()) {
		t.Fatal("fresh status was not published")
	}
	msg := readMsg(t, conn)
	if msg.Number != 2 {
		t.Fatalf("auto dispatch used issue #%d, want fresh status issue #2", msg.Number)
	}
}

func TestStandbyAutoDispatchSkipsHeldQueueItems(t *testing.T) {
	cfg := standbyAutoDispatchConfig(true, "T2", 1)
	cfg.Hub.ContributeQueueHold = []string{"alice/repo#1"}
	s, conn := standbyAutoDispatchHarness(t, cfg)

	s.contributeHub.AutoDispatchStandby([]string{"quality"})
	conn.SetReadDeadline(testReadDeadline())
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("auto dispatch assigned an operator-held item")
	}
}

func TestStandbyAutoDispatchRespectsCapFloorAndSuspension(t *testing.T) {
	t.Run("cap exhausted", func(t *testing.T) {
		cfg := standbyAutoDispatchConfig(true, "T2", 1)
		s, conn := standbyAutoDispatchHarness(t, cfg)
		s.contributeHub.recordStandbyDispatch("alice", "quality", testNow())
		s.contributeHub.AutoDispatchStandby([]string{"quality"})
		conn.SetReadDeadline(testReadDeadline())
		if _, _, err := conn.ReadMessage(); err == nil {
			t.Fatal("cap-exhausted standby contributor was assigned")
		}
	})

	t.Run("below floor", func(t *testing.T) {
		cfg := standbyAutoDispatchConfig(true, "T1", 1)
		s, conn := standbyAutoDispatchHarness(t, cfg)
		s.contributeHub.AutoDispatchStandby([]string{"quality"})
		conn.SetReadDeadline(testReadDeadline())
		if _, _, err := conn.ReadMessage(); err == nil {
			t.Fatal("below-floor standby contributor was assigned")
		}
	})

	t.Run("suspended", func(t *testing.T) {
		cfg := standbyAutoDispatchConfig(true, "T2", 1)
		s, conn := standbyAutoDispatchHarness(t, cfg)
		key := standbyOutcomeKey("alice", standbypkg.Configuration{Backend: "copilot", Model: "gpt-5.4-mini"})
		s.contributeHub.appendStandbyOutcome(standbyOutcomeRecord{Key: key, Lane: "quality", Kind: standbypkg.OutcomeClosedUnmerged})
		s.contributeHub.appendStandbyOutcome(standbyOutcomeRecord{Key: key, Lane: "quality", Kind: standbypkg.OutcomeClosedUnmerged})
		s.contributeHub.AutoDispatchStandby([]string{"quality"})
		conn.SetReadDeadline(testReadDeadline())
		if _, _, err := conn.ReadMessage(); err == nil {
			t.Fatal("suspended standby contributor was assigned")
		}
	})
}

func TestStandbyAutoDispatchNoDispatchWhenZeroQualify(t *testing.T) {
	cfg := standbyAutoDispatchConfig(true, "T2", 1)
	cfg.Hub.StandbyModelTiers = nil
	s, conn := standbyAutoDispatchHarness(t, cfg)

	s.contributeHub.AutoDispatchStandby([]string{"quality"})
	conn.SetReadDeadline(testReadDeadline())
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("unmapped configuration with 0 qualifying contributors was assigned")
	}
}

func testNow() time.Time { return time.Now() }

func testReadDeadline() time.Time { return time.Now().Add(100 * time.Millisecond) }
