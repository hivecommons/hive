package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	standbypkg "github.com/hivecommons/hive/pkg/standby"
)

func standbyAutoDispatchConfig(auto bool, floor string, cap int) *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Org: "alice", Repos: []string{"repo"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{"quality": {
				Mode: "ISSUES_AND_PRS",
				Standby: &config.StandbyConfig{
					Enabled:                true,
					AutoDispatch:           auto,
					MinModelCapability:     floor,
					DailyCapPerContributor: cap,
				},
			}},
		},
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
	return &StatusPayload{Governor: FrontendGovernor{SuppressedLanes: []string{"quality"}}, Repos: []FrontendRepo{{
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
	s.deps.GHClient = standbyRepoVisibilityClient(t, false, false)
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
	h := s.contributeHub
	h.completedMu.Lock()
	outcomes := len(h.standbyOutcomes)
	h.completedMu.Unlock()
	if outcomes != 0 {
		t.Fatalf("dispatch created %d PR outcome rows for an issue", outcomes)
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

func TestStandbyDispatchRefusesDisabledLane(t *testing.T) {
	cfg := standbyAutoDispatchConfig(true, "T2", 1)
	cfg.Agents["quality"].Standby.Enabled = false
	s, _ := standbyAutoDispatchHarness(t, cfg)

	_, err := s.contributeHub.DispatchStandby("quality", "")
	if err == nil || !strings.Contains(err.Error(), string(standbypkg.ReasonStandbyDisabled)) {
		t.Fatalf("DispatchStandby disabled lane error = %v, want %s", err, standbypkg.ReasonStandbyDisabled)
	}
}

func TestStandbyDispatchPrivateRepoGate(t *testing.T) {
	t.Run("default off", func(t *testing.T) {
		cfg := standbyAutoDispatchConfig(true, "T2", 1)
		s, _ := standbyAutoDispatchHarness(t, cfg)
		s.deps.GHClient = standbyRepoVisibilityClient(t, true, false)

		_, err := s.contributeHub.DispatchStandby("quality", "")
		if err == nil || !strings.Contains(err.Error(), taskUnavailablePrivateRepo) {
			t.Fatalf("DispatchStandby private repo error = %v, want %s", err, taskUnavailablePrivateRepo)
		}
	})
	t.Run("allowed by config", func(t *testing.T) {
		cfg := standbyAutoDispatchConfig(true, "T2", 1)
		cfg.Hub.StandbyAllowPrivateRepos = true
		s, _ := standbyAutoDispatchHarness(t, cfg)

		msg, err := s.contributeHub.DispatchStandby("quality", "")
		if err != nil {
			t.Fatalf("DispatchStandby allowed private repo: %v", err)
		}
		if msg == nil || msg.Type != "task_assign" {
			t.Fatalf("DispatchStandby msg = %#v, want task_assign", msg)
		}
	})
	t.Run("visibility error", func(t *testing.T) {
		cfg := standbyAutoDispatchConfig(true, "T2", 1)
		s, _ := standbyAutoDispatchHarness(t, cfg)
		s.deps.GHClient = standbyRepoVisibilityClient(t, false, true)

		_, err := s.contributeHub.DispatchStandby("quality", "")
		if err == nil || !strings.Contains(err.Error(), taskUnavailablePrivateRepo) {
			t.Fatalf("DispatchStandby visibility error = %v, want %s", err, taskUnavailablePrivateRepo)
		}
	})
	t.Run("public repo", func(t *testing.T) {
		cfg := standbyAutoDispatchConfig(true, "T2", 1)
		s, _ := standbyAutoDispatchHarness(t, cfg)
		s.deps.GHClient = standbyRepoVisibilityClient(t, false, false)

		msg, err := s.contributeHub.DispatchStandby("quality", "")
		if err != nil {
			t.Fatalf("DispatchStandby public repo: %v", err)
		}
		if msg == nil || msg.Repo != "alice/repo" || msg.Number != 1 {
			t.Fatalf("DispatchStandby msg = %#v, want alice/repo#1", msg)
		}
	})
}

func standbyRepoVisibilityClient(t *testing.T, private bool, fail bool) *ghpkg.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/alice/repo" {
			http.NotFound(w, r)
			return
		}
		if fail {
			http.Error(w, "try later", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, `{"full_name":"alice/repo","private":%t}`, private)
	}))
	t.Cleanup(srv.Close)
	return ghpkg.NewClientForTest(srv.URL, "alice", []string{"repo"}, nil)
}

func testNow() time.Time { return time.Now() }

func testReadDeadline() time.Time { return time.Now().Add(100 * time.Millisecond) }
