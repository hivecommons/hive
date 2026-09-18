package governor

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// laddersConfig is standardConfig with a SEPARATE cadence map per mode —
// standardConfig shares one map across all four, so a per-mode edit there
// lands in every mode, which is the opposite of what these tests are about.
func laddersConfig(agents ...string) (config.GovernorConfig, map[string]config.AgentConfig) {
	cfg, agentMap := standardConfig(agents...)
	for key, mode := range cfg.Modes {
		own := make(map[string]config.Cadence, len(mode.Cadences))
		for a, c := range mode.Cadences {
			own[a] = c
		}
		cfg.Modes[key] = config.ModeConfig{Threshold: mode.Threshold, Cadences: own}
	}
	return cfg, agentMap
}

// The live case behind #7474: on the projectbluefin spoke the reviewer's ONLY
// cadence is in surge. While the backlog holds the fleet in surge it is
// kicked every 30 minutes and everything looks right; the moment the queue
// it exists to shrink drops below the surge threshold, the fleet is in busy,
// the reviewer has no cadence there (and none in idle to inherit), and it
// goes silent. NoCadenceAgents cannot see it — the agent HAS a cadence.
func TestModeUnscheduledAgents_SurgeOnlyReviewerGoesSilentBelowSurge(t *testing.T) {
	cfg, agents := laddersConfig("scanner")
	agents["reviewer"] = config.AgentConfig{Enabled: true}
	cfg.Modes["surge"].Cadences["reviewer"] = "30m"
	g := New(cfg, agents, testLogger())

	g.SetMode(ModeSurge)
	if got := g.ModeUnscheduledAgents(); len(got) != 0 {
		t.Fatalf("in surge the reviewer is scheduled; flagged %v", got)
	}
	if got := g.NoCadenceAgents(); len(got) != 0 {
		t.Fatalf("NoCadenceAgents must not claim the reviewer either: %v", got)
	}

	for _, mode := range []Mode{ModeBusy, ModeQuiet, ModeIdle} {
		g.SetMode(mode)
		got := g.ModeUnscheduledAgents()
		want := []ModeUnscheduledAgent{{Agent: "reviewer", Mode: modeToConfigKey(mode), CadenceModes: []string{"surge"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("in %s: ModeUnscheduledAgents = %+v, want %+v", mode, got, want)
		}
		// Still invisible to the never-scheduled detector: the two classes
		// are disjoint by construction.
		if nc := g.NoCadenceAgents(); len(nc) != 0 {
			t.Fatalf("in %s: NoCadenceAgents = %v, want none", mode, nc)
		}
	}
}

// An idle-mode entry is every other mode's fallback (resolveCadence), so an
// agent named only in idle is scheduled everywhere and must not be flagged —
// the detector has to follow the governor's real resolution chain, not just
// "is there an entry under the current key".
func TestModeUnscheduledAgents_IdleEntryIsInheritedByEveryMode(t *testing.T) {
	cfg, agents := laddersConfig("scanner")
	agents["janitor"] = config.AgentConfig{Enabled: true}
	cfg.Modes["idle"].Cadences["janitor"] = "1h"
	g := New(cfg, agents, testLogger())

	for _, mode := range []Mode{ModeIdle, ModeQuiet, ModeBusy, ModeSurge} {
		g.SetMode(mode)
		if got := g.ModeUnscheduledAgents(); len(got) != 0 {
			t.Fatalf("in %s: idle-only agent flagged: %v", mode, got)
		}
	}
}

// Everything the operator chose deliberately stays out of the list: an
// explicit pause/off entry in the current mode is offByCadence's signal; an
// agent no mode names at all is NoCadenceAgents'; on-demand, event-driven
// and disabled agents are never timer-kicked by design.
func TestModeUnscheduledAgents_DeliberateConfigurationsAreNotFlagged(t *testing.T) {
	cfg, agents := laddersConfig("scanner")
	// parked: cadence in surge, explicit "pause" in busy — an entry the
	// governor resolves, chosen by the operator.
	agents["parked"] = config.AgentConfig{Enabled: true}
	cfg.Modes["surge"].Cadences["parked"] = "30m"
	cfg.Modes["busy"].Cadences["parked"] = "pause"
	// switched-off: explicit "off" in busy, same reasoning.
	agents["switched-off"] = config.AgentConfig{Enabled: true}
	cfg.Modes["surge"].Cadences["switched-off"] = "30m"
	cfg.Modes["busy"].Cadences["switched-off"] = "off"
	// telemetry: no cadence anywhere — NoCadenceAgents' class, not this one.
	agents["telemetry"] = config.AgentConfig{Enabled: true}
	// helper: on-demand, surge-only cadence — never on a schedule by design.
	agents["helper"] = config.AgentConfig{Enabled: true, OnDemand: true}
	cfg.Modes["surge"].Cadences["helper"] = "30m"
	// watcher: event-driven channels, surge-only cadence — the timer is not
	// its trigger.
	agents["watcher"] = config.AgentConfig{Enabled: true, Channels: []config.ChannelConfig{{Type: "advisory"}}}
	cfg.Modes["surge"].Cadences["watcher"] = "30m"
	// mothballed: disabled, surge-only cadence.
	agents["mothballed"] = config.AgentConfig{Enabled: false}
	cfg.Modes["surge"].Cadences["mothballed"] = "30m"
	g := New(cfg, agents, testLogger())

	g.SetMode(ModeBusy)
	if got := g.ModeUnscheduledAgents(); len(got) != 0 {
		t.Fatalf("deliberate configurations flagged: %+v", got)
	}
	if got, want := g.NoCadenceAgents(), []string{"telemetry"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NoCadenceAgents = %v, want %v", got, want)
	}
}

// CadenceModes names EVERY mode that schedules the agent, in threshold order,
// so the banner can say "in busy and surge" and the operator knows which mode
// is the gap. Sorted output by agent name so the banner is stable.
func TestModeUnscheduledAgents_ListsCadenceModesInThresholdOrderAndSortsAgents(t *testing.T) {
	cfg, agents := laddersConfig("scanner")
	agents["reviewer"] = config.AgentConfig{Enabled: true}
	cfg.Modes["surge"].Cadences["reviewer"] = "30m"
	cfg.Modes["busy"].Cadences["reviewer"] = "1h"
	agents["auditor"] = config.AgentConfig{Enabled: true}
	cfg.Modes["surge"].Cadences["auditor"] = "2h"
	g := New(cfg, agents, testLogger())

	g.SetMode(ModeQuiet)
	got := g.ModeUnscheduledAgents()
	want := []ModeUnscheduledAgent{
		{Agent: "auditor", Mode: "quiet", CadenceModes: []string{"surge"}},
		{Agent: "reviewer", Mode: "quiet", CadenceModes: []string{"busy", "surge"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ModeUnscheduledAgents = %+v, want %+v", got, want)
	}

	// Back in busy the reviewer is scheduled and only the auditor remains.
	g.SetMode(ModeBusy)
	got = g.ModeUnscheduledAgents()
	want = []ModeUnscheduledAgent{{Agent: "auditor", Mode: "busy", CadenceModes: []string{"surge"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("in busy: ModeUnscheduledAgents = %+v, want %+v", got, want)
	}
}

// A replica inherits its base agent's cadence (resolveCadence's fallback), so
// a replica whose BASE is scheduled in the current mode must not be flagged,
// and one whose base is scheduled only elsewhere is flagged with the base's
// modes.
func TestModeUnscheduledAgents_ReplicaFollowsBaseCadence(t *testing.T) {
	cfg, agents := laddersConfig("scanner")
	agents["scanner-2"] = config.AgentConfig{Enabled: true, ReplicaOf: "scanner", ReplicaIndex: 2, ReplicaCount: 2}
	agents["reviewer"] = config.AgentConfig{Enabled: true}
	agents["reviewer-2"] = config.AgentConfig{Enabled: true, ReplicaOf: "reviewer", ReplicaIndex: 2, ReplicaCount: 2}
	cfg.Modes["surge"].Cadences["reviewer"] = "30m"
	g := New(cfg, agents, testLogger())

	g.SetMode(ModeBusy)
	got := g.ModeUnscheduledAgents()
	want := []ModeUnscheduledAgent{
		{Agent: "reviewer", Mode: "busy", CadenceModes: []string{"surge"}},
		{Agent: "reviewer-2", Mode: "busy", CadenceModes: []string{"surge"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ModeUnscheduledAgents = %+v, want %+v", got, want)
	}
}

// The result is always a measurement: non-nil even when empty, so a JSON
// consumer sees [] rather than null ("not measured").
func TestModeUnscheduledAgents_EmptyIsNonNil(t *testing.T) {
	cfg, agents := laddersConfig("scanner")
	g := New(cfg, agents, testLogger())

	got := g.ModeUnscheduledAgents()
	if got == nil {
		t.Fatal("ModeUnscheduledAgents returned nil; must be a non-nil measurement")
	}
	if len(got) != 0 {
		t.Fatalf("scheduled agent flagged: %+v", got)
	}
}

// A repo-scoped agent follows each repo's own mode. It is scheduled if ANY
// repo's mode resolves a cadence for it, and flagged only when none does.
func TestModeUnscheduledAgents_RepoScopedAgentFollowsRepoModes(t *testing.T) {
	cfg := repoScopeTestConfig()
	// reviewer: per-repo scope, surge-only cadence.
	cfg.Modes["surge"].Cadences["reviewer"] = "30m"
	g := New(cfg, map[string]config.AgentConfig{
		"scanner":  {Enabled: true, CadenceScope: config.CadenceScopePerRepo},
		"reviewer": {Enabled: true, CadenceScope: config.CadenceScopePerRepo},
	}, testLogger())

	// One hot repo in surge keeps the reviewer scheduled even though the
	// aggregate fleet mode is lower.
	g.EvaluateWithRepoDepths(25, 0, 0, 0, map[string]RepoSnapshot{
		"org/hot":  {Issues: 25},
		"org/cold": {},
	})
	if got := g.ModeUnscheduledAgents(); len(got) != 0 {
		t.Fatalf("reviewer is scheduled on the surge repo; flagged %+v", got)
	}

	// Every repo below surge: nothing schedules the reviewer anywhere.
	g.EvaluateWithRepoDepths(6, 0, 0, 0, map[string]RepoSnapshot{
		"org/hot":  {Issues: 6},
		"org/cold": {},
	})
	got := g.ModeUnscheduledAgents()
	if len(got) != 1 || got[0].Agent != "reviewer" || !reflect.DeepEqual(got[0].CadenceModes, []string{"surge"}) {
		t.Fatalf("ModeUnscheduledAgents = %+v, want only the reviewer with cadence modes [surge]", got)
	}
}

// config.ModeSchedulesIn is the dashboard's copy of the question this
// detector asks the governor's resolveCadence. The two must agree on every
// (mode, agent) pair, or the fleet banner and the agent card name different
// agents — the drift #5594 exists to prevent.
func TestModeSchedulesInAgreesWithResolveCadence(t *testing.T) {
	cfg, agents := laddersConfig("scanner")
	agents["reviewer"] = config.AgentConfig{Enabled: true}
	cfg.Modes["surge"].Cadences["reviewer"] = "30m"
	agents["janitor"] = config.AgentConfig{Enabled: true}
	cfg.Modes["idle"].Cadences["janitor"] = "1h"
	agents["parked"] = config.AgentConfig{Enabled: true}
	cfg.Modes["busy"].Cadences["parked"] = "pause"
	agents["scanner-2"] = config.AgentConfig{Enabled: true, ReplicaOf: "scanner", ReplicaIndex: 2, ReplicaCount: 2}
	agents["reviewer-2"] = config.AgentConfig{Enabled: true, ReplicaOf: "reviewer", ReplicaIndex: 2, ReplicaCount: 2}
	agents["telemetry"] = config.AgentConfig{Enabled: true}
	g := New(cfg, agents, testLogger())

	for name, ac := range agents {
		base := name
		if ac.ReplicaOf != "" {
			base = ac.ReplicaOf
		}
		for _, mode := range []string{"idle", "quiet", "busy", "surge"} {
			_, want := g.resolveCadence(mode, name)
			if got := config.ModeSchedulesIn(cfg.Modes, mode, name, base); got != want {
				t.Errorf("ModeSchedulesIn(%s, %s) = %v, resolveCadence says %v", mode, name, got, want)
			}
		}
	}
}
