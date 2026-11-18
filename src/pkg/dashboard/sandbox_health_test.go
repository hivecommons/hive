package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func TestSandboxRunHealth(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	ok := &crux.SandboxRun{Runtime: "job", StartedAt: now.Add(-40 * time.Minute), EndedAt: now.Add(-11 * time.Minute), Succeeded: true, PRURL: "https://github.com/o/r/pull/1402", Usage: crux.SandboxRunUsage{Model: "m", Input: 10, Output: 5}}
	failed := &crux.SandboxRun{Runtime: "job", StartedAt: now.Add(-40 * time.Minute), EndedAt: now.Add(-39 * time.Minute), Error: "exit 1"}
	kick := func(ago time.Duration) *time.Time { t := now.Add(-ago); return &t }
	cases := []struct {
		name     string
		proc     *crux.Process
		cadence  config.Cadence
		onDemand bool
		want     string
	}{
		{"nil proc", nil, "30m", false, SandboxHealthOK},
		{"succeeded, next cron in the future", &crux.Process{State: crux.StateIdle, LastKick: kick(40 * time.Minute), SandboxLastRun: ok}, "0 */6 * * *", false, SandboxHealthOK},
		{"idle between interval kicks", &crux.Process{State: crux.StateIdle, LastKick: kick(40 * time.Minute), SandboxLastRun: ok}, "1h", false, SandboxHealthOK},
		{"kick in flight", &crux.Process{State: crux.StateRunning, LastKick: kick(time.Minute), SandboxLastRun: ok}, "1h", false, SandboxHealthRunning},
		{"last run failed", &crux.Process{State: crux.StateIdle, LastKick: kick(40 * time.Minute), SandboxLastRun: failed}, "1h", false, SandboxHealthFailed},
		{"failed state without a record", &crux.Process{State: crux.StateFailed, LastKick: kick(40 * time.Minute)}, "1h", false, SandboxHealthOK},
		{"missed interval past grace", &crux.Process{State: crux.StateIdle, LastKick: kick(3 * time.Hour), SandboxLastRun: ok}, "1h", false, SandboxHealthOverdue},
		{"missed interval within grace", &crux.Process{State: crux.StateIdle, LastKick: kick(75 * time.Minute), SandboxLastRun: ok}, "1h", false, SandboxHealthOK},
		{"on-demand never overdue", &crux.Process{State: crux.StateIdle, LastKick: kick(30 * 24 * time.Hour), SandboxLastRun: ok}, "1h", true, SandboxHealthOK},
		{"paused cadence never overdue", &crux.Process{State: crux.StateIdle, LastKick: kick(30 * 24 * time.Hour), SandboxLastRun: ok}, "pause", false, SandboxHealthOK},
		{"paused process never overdue", &crux.Process{State: crux.StatePaused, Paused: true, LastKick: kick(30 * 24 * time.Hour), SandboxLastRun: ok}, "1h", false, SandboxHealthOK},
		{"never kicked, fresh start", &crux.Process{State: crux.StateIdle, StartedAt: kick(10 * time.Minute)}, "1h", false, SandboxHealthOK},
		{"never kicked, first period long gone", &crux.Process{State: crux.StateIdle, StartedAt: kick(5 * time.Hour)}, "1h", false, SandboxHealthOverdue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sandboxRunHealth(tc.proc, tc.cadence, tc.onDemand, now)
			if got.Health != tc.want {
				t.Fatalf("health = %q, want %q (%+v)", got.Health, tc.want, got)
			}
			if tc.want == SandboxHealthOverdue && got.DueAt == "" {
				t.Errorf("overdue verdict must name the missed occurrence")
			}
			if tc.want != SandboxHealthOverdue && got.DueAt != "" {
				t.Errorf("dueAt must be empty unless overdue, got %q", got.DueAt)
			}
		})
	}

	got := sandboxRunHealth(&crux.Process{State: crux.StateIdle, LastKick: kick(40 * time.Minute), SandboxLastRun: ok}, "1h", false, now)
	if got.Runtime != "job" || !got.Succeeded || got.DurationS != 29*60 || got.PRURL != ok.PRURL || got.Tokens != 15 || got.Model != "m" {
		t.Errorf("run fields not carried over: %+v", got)
	}

	withheld := &crux.SandboxRun{Runtime: "job", StartedAt: now.Add(-40 * time.Minute), EndedAt: now.Add(-11 * time.Minute),
		Error: "sandbox run reported failure", PRWithheld: "agent report status=failed: rpm install 401 token ghp_abcdefghijklmnopqrstuvwxyz0123456789", LogID: "20261027-195251.000-sandbox.log"}
	got = sandboxRunHealth(&crux.Process{State: crux.StateIdle, LastKick: kick(40 * time.Minute), SandboxLastRun: withheld}, "1h", false, now)
	if got.Health != SandboxHealthFailed || got.LogID != withheld.LogID {
		t.Errorf("withheld run must be failed and name its archive: %+v", got)
	}
	if !strings.HasPrefix(got.PRWithheld, "agent report status=failed: rpm install 401") || strings.Contains(got.PRWithheld, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("prWithheld must be carried over with tokens redacted, got %q", got.PRWithheld)
	}
}

func TestSandboxOverdueGraceClamped(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for cadence, want := range map[config.Cadence]time.Duration{
		"10m":        sandboxOverdueGraceMin,
		"1h":         30 * time.Minute,
		"0 0 * * *":  sandboxOverdueGraceMax,
		"continuous": sandboxOverdueGraceMax,
	} {
		if got := sandboxOverdueGrace(cadence, now); got != want {
			t.Errorf("grace(%q) = %v, want %v", cadence, got, want)
		}
	}
}

func TestBuildAgents_SandboxedAgentCarriesLastRun(t *testing.T) {
	yes := true
	agentCfg := config.AgentConfig{Backend: "claude", Enabled: true, Sandbox: &config.AgentSandboxOverride{Enabled: &yes}}
	last := time.Now().Add(-30 * time.Minute)
	run := &crux.SandboxRun{Runtime: "job", StartedAt: last, EndedAt: last.Add(29 * time.Minute), Succeeded: true, Summary: "opened spyre-inference#1402"}
	statuses := map[string]*crux.Process{
		"deps":  {Name: "deps", Config: agentCfg, State: crux.StateIdle, LastKick: &last, SandboxLastRun: run, OutputBuffer: cruxtest.NewRingBuffer(10)},
		"plain": {Name: "plain", Config: config.AgentConfig{Backend: "claude", Enabled: true}, State: crux.StateIdle, LastKick: &last, OutputBuffer: cruxtest.NewRingBuffer(10)},
	}
	cfg := &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents:       map[string]config.AgentConfig{"deps": agentCfg, "plain": statuses["plain"].Config},
			AgentSandbox: config.AgentSandboxConfig{Enabled: true},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle": {Cadences: map[string]config.Cadence{"deps": "0 */6 * * *", "plain": "1h"}},
		}},
	}
	byName := map[string]FrontendAgent{}
	for _, a := range buildAgents(statuses, cfg, governor.State{Mode: governor.ModeIdle}) {
		byName[a.Name] = a
	}
	deps := byName["deps"]
	if !deps.Sandboxed || deps.SandboxRun == nil {
		t.Fatalf("sandboxed agent must carry sandboxRun: %+v", deps)
	}
	if deps.SandboxRun.Health != SandboxHealthOK || deps.SandboxRun.Runtime != "job" || !deps.SandboxRun.Succeeded {
		t.Errorf("sandboxRun = %+v", deps.SandboxRun)
	}
	if !strings.Contains(deps.DetailSummary, "spyre-inference#1402") {
		t.Errorf("detailSummary must fall back to the run summary, got %q", deps.DetailSummary)
	}
	if byName["plain"].SandboxRun != nil {
		t.Errorf("non-sandboxed agent must not carry sandboxRun")
	}
}

// A Job whose pod cannot be scheduled and a node whose preflight failed are
// their own verdicts, distinct from running and from a failed agent (#18811).
func TestSandboxRunHealthInfrastructureStates(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	phase := &crux.SandboxPhase{Phase: "waiting_for_node", Detail: "0/38 nodes are available: 38 node(s) didn't match Pod's node affinity/selector.", Since: now}
	waiting := sandboxRunHealth(&crux.Process{State: crux.StateRunning, SandboxPhase: phase}, "1h", false, now)
	if waiting.Health != SandboxHealthWaitingForNode || !strings.Contains(waiting.WaitDetail, "0/38 nodes") {
		t.Errorf("waiting: %+v", waiting)
	}
	if running := sandboxRunHealth(&crux.Process{State: crux.StateRunning}, "1h", false, now); running.Health != SandboxHealthRunning || running.WaitDetail != "" {
		t.Errorf("a running kick with no phase must stay running: %+v", running)
	}
	bad := &crux.SandboxRun{Runtime: "job", StartedAt: now.Add(-time.Minute), EndedAt: now.Add(-50 * time.Second), Error: "sandbox: node_unhealthy on node p1-worker-93: smoke failed", FailReason: "node_unhealthy", Node: "p1-worker-93"}
	got := sandboxRunHealth(&crux.Process{State: crux.StateIdle, SandboxLastRun: bad}, "1h", false, now)
	if got.Health != SandboxHealthNodeUnhealthy || got.FailReason != "node_unhealthy" || got.Node != "p1-worker-93" || got.Succeeded {
		t.Errorf("node unhealthy: %+v", got)
	}
	noNode := &crux.SandboxRun{Runtime: "job", StartedAt: now.Add(-time.Hour), EndedAt: now.Add(-50 * time.Minute), Error: "sandbox: waiting_for_node: ...", FailReason: "waiting_for_node"}
	if got := sandboxRunHealth(&crux.Process{State: crux.StateIdle, SandboxLastRun: noNode}, "1h", false, now); got.Health != SandboxHealthFailed || got.FailReason != "waiting_for_node" {
		t.Errorf("a kick that never got a node reads as a failed run carrying its reason: %+v", got)
	}
	a := FrontendAgent{Sandboxed: true, State: "idle", SandboxRun: &FrontendSandboxRun{Health: SandboxHealthNodeUnhealthy}}
	if got := hiveAgentClassOf(a); got != hiveAgentDown {
		t.Errorf("a node_unhealthy agent must read as down in the agent roster, got %q", got)
	}
}
