package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/watchdog"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

type healthStatusRuntime struct {
	crux.Runtime
	statuses map[string]*crux.Process
}

func (r *healthStatusRuntime) AllStatuses() map[string]*crux.Process { return r.statuses }

func TestDeepHealthAgentDelivery(t *testing.T) {
	s, deps := apiServer(t)
	deps.GHClient = &github.Client{}
	deps.Config.Agents["paused"] = config.AgentConfig{Enabled: true}
	deps.Config.Agents["disabled"] = config.AgentConfig{Enabled: false}
	missing := []crux.Condition{{Type: watchdog.ConditionReady, Status: watchdog.ConditionFalse, Reason: string(watchdog.ClassNoSession)}}
	worker := &crux.Process{State: crux.StateRunning, LastKickMessage: "work", WatchdogConditions: missing}
	deps.AgentMgr = &healthStatusRuntime{Runtime: deps.AgentMgr, statuses: map[string]*crux.Process{
		"scanner":  worker,
		"paused":   {State: crux.StateRunning, Paused: true, WatchdogConditions: missing},
		"disabled": {State: crux.StateStopped},
	}}
	s.status = &StatusPayload{}
	s.MarkReady()

	check := func(want, detail, overall string) {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handleHealthDeep(rec, httptest.NewRequest(http.MethodGet, "/api/health/deep", nil))
		var body struct {
			Status string `json:"status"`
			Fails  int    `json:"fails"`
			Checks struct {
				Agents map[string]struct {
					Status string `json:"status"`
					Detail string `json:"detail"`
				} `json:"agents"`
			} `json:"checks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Status != overall || body.Checks.Agents["scanner"].Status != want || !strings.Contains(body.Checks.Agents["scanner"].Detail, detail) {
			t.Fatalf("health = %s", rec.Body.String())
		}
		if want == "fail" && body.Fails < 1 {
			t.Fatalf("failure not counted: %s", rec.Body.String())
		}
		for _, name := range []string{"paused", "disabled"} {
			if body.Checks.Agents[name].Status != "skip" {
				t.Fatalf("%s not skipped: %s", name, rec.Body.String())
			}
		}
	}

	check("fail", "tmux session missing", "degraded")
	if status, detail := agentsCheckOf(t, s); status != "fail" || !strings.Contains(detail, "scanner (tmux session missing)") {
		t.Fatalf("summary agents = %q, %q", status, detail)
	}
	worker.WatchdogConditions = nil
	worker.ConsecutiveKickFailures = 3
	worker.LastKickError = "tmux session hive-scanner not found"
	check("fail", worker.LastKickError, "degraded")
	deps.Config.Dashboard.KickFailureThreshold = 4
	check("pass", "", "ok")
	deps.Config.Dashboard.KickFailureThreshold = 0
	worker.ConsecutiveKickFailures = 0
	worker.LastKickMessage = ""
	check("warn", "no kick message recorded", "ok")
}
