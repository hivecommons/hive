package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// Configured-but-not-scheduled (#7474). A reviewer whose only cadence is in
// surge is kicked while the backlog holds the fleet in surge; the moment its
// own work drives the backlog below the threshold the fleet is in busy, the
// reviewer has no cadence there (and none in idle to inherit), and it goes
// silent — while the card shows it green and idle, because it is enabled,
// running, has a cadence, and is not on-demand. These tests pin the per-agent
// flag the card reads, and the card copy that names the gap and the modes
// that DO schedule the agent.

func modeUnscheduledCfg() *config.Config {
	return &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner":    {Backend: "claude", Enabled: true},
			"reviewer":   {Backend: "claude", Enabled: true},
			"reviewer-2": {Backend: "claude", Enabled: true, ReplicaOf: "reviewer", ReplicaIndex: 2, ReplicaCount: 2},
			"janitor":    {Backend: "claude", Enabled: true},
			"parked":     {Backend: "claude", Enabled: true},
			"orphan":     {Backend: "claude", Enabled: true},
			"helper":     {Backend: "claude", Enabled: true, OnDemand: true},
			"mothballed": {Backend: "claude"},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle":  {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "1h", "janitor": "2h"}},
			"quiet": {Threshold: 28, Cadences: map[string]config.Cadence{"scanner": "30m"}},
			"busy":  {Threshold: 150, Cadences: map[string]config.Cadence{"scanner": "15m", "parked": "pause"}},
			"surge": {Threshold: 550, Cadences: map[string]config.Cadence{"scanner": "5m", "reviewer": "30m", "parked": "30m", "helper": "30m", "mothballed": "30m"}},
		}},
	}
}

func TestBuildAgents_UnscheduledInMode(t *testing.T) {
	cfg := modeUnscheduledCfg()
	statuses := map[string]*agent.AgentProcess{}
	for name, ac := range cfg.Agents {
		statuses[name] = newBlockerProc(name, ac, nil)
	}

	type want struct {
		flag  bool
		modes []string
	}
	cases := map[governor.Mode]map[string]want{
		// In surge every named agent is scheduled: nothing to flag.
		governor.ModeSurge: {
			"reviewer": {false, nil}, "reviewer-2": {false, nil}, "scanner": {false, nil}, "janitor": {false, nil},
			"parked": {false, nil}, "orphan": {false, nil}, "helper": {false, nil}, "mothballed": {false, nil},
		},
		// In busy the surge-only reviewer (and its replica, which inherits
		// the base's cadence) is configured but unscheduled. parked has an
		// explicit pause entry (offByCadence's signal), janitor inherits
		// idle, orphan has no cadence anywhere (noCadence's class),
		// helper is on-demand, mothballed is disabled.
		governor.ModeBusy: {
			"reviewer": {true, []string{"surge"}}, "reviewer-2": {true, []string{"surge"}},
			"scanner": {false, nil}, "janitor": {false, nil}, "parked": {false, nil},
			"orphan": {false, nil}, "helper": {false, nil}, "mothballed": {false, nil},
		},
		// In quiet parked is unscheduled too: its entries are busy and surge.
		governor.ModeQuiet: {
			"reviewer": {true, []string{"surge"}}, "reviewer-2": {true, []string{"surge"}},
			"parked":  {true, []string{"busy", "surge"}},
			"scanner": {false, nil}, "janitor": {false, nil}, "orphan": {false, nil},
			"helper": {false, nil}, "mothballed": {false, nil},
		},
	}
	for mode, wants := range cases {
		byName := map[string]FrontendAgent{}
		for _, a := range buildAgents(statuses, cfg, governor.State{Mode: mode}) {
			byName[a.Name] = a
		}
		for name, w := range wants {
			a, ok := byName[name]
			if !ok {
				t.Fatalf("%s: agent %q missing from buildAgents output", mode, name)
			}
			if a.UnscheduledInMode != w.flag {
				t.Errorf("%s: %s UnscheduledInMode = %v, want %v", mode, name, a.UnscheduledInMode, w.flag)
			}
			if !reflect.DeepEqual(a.CadenceModes, w.modes) {
				t.Errorf("%s: %s CadenceModes = %v, want %v", mode, name, a.CadenceModes, w.modes)
			}
			// The three cadence flags name disjoint classes.
			if a.UnscheduledInMode && (a.NoCadence || a.OffByCadence) {
				t.Errorf("%s: %s is unscheduledInMode AND noCadence=%v/offByCadence=%v; the classes must be disjoint",
					mode, name, a.NoCadence, a.OffByCadence)
			}
		}
	}
	// orphan is the never-scheduled class, reported by the other flag.
	byName := map[string]FrontendAgent{}
	for _, a := range buildAgents(statuses, cfg, governor.State{Mode: governor.ModeBusy}) {
		byName[a.Name] = a
	}
	if !byName["orphan"].NoCadence {
		t.Errorf("orphan has no cadence anywhere and must carry NoCadence, not be dropped")
	}
	if !byName["parked"].OffByCadence {
		t.Errorf("parked has an explicit pause in busy and must carry OffByCadence")
	}
}

// An agent that is configured but has no runtime process yet goes through
// buildMissingRuntimeAgent; it must carry the same flag, or a reviewer whose
// session is down reads as merely down.
func TestBuildMissingRuntimeAgent_UnscheduledInMode(t *testing.T) {
	cfg := modeUnscheduledCfg()
	a := buildMissingRuntimeAgent("reviewer", cfg.Agents["reviewer"], cfg, "busy", map[string]bool{})
	if !a.UnscheduledInMode || !reflect.DeepEqual(a.CadenceModes, []string{"surge"}) {
		t.Fatalf("missing-runtime reviewer in busy: UnscheduledInMode=%v CadenceModes=%v, want true [surge]", a.UnscheduledInMode, a.CadenceModes)
	}
	a = buildMissingRuntimeAgent("reviewer", cfg.Agents["reviewer"], cfg, "surge", map[string]bool{})
	if a.UnscheduledInMode {
		t.Fatalf("missing-runtime reviewer in surge flagged: %+v", a)
	}
}

// A nil config must not accuse every agent of being unscheduled.
func TestAgentUnscheduledInMode_NilConfigIsNotAnAccusation(t *testing.T) {
	if flag, _ := agentUnscheduledInMode(nil, "reviewer", "busy", true, false, true); flag {
		t.Fatal("nil config reported an agent as unscheduled")
	}
}

// The card structure: every site that decides the "off" display state reads
// the shared helper, so the surge-only reviewer renders hollow-green "no
// cadence in busy" with the ⚙ Cadences action rather than solid-green idle;
// and the blockers line names the modes that DO schedule it.
func TestAgentCardModeUnscheduledStructure(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"function agentHeldByMode(a) {",
		"return a.offByCadence === true || a.unscheduledInMode === true;",
		"function agentOffTitle(a) {",
		"function agentCadenceModesLabel(a) {",
		"if (a.unscheduledInMode === true) {",
		"(only in ${agentCadenceModesLabel(a)})",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	if strings.Contains(html, "const isOff = a.offByCadence === true && !isPaused && !isOnDemand;") {
		t.Error("an isOff site still reads offByCadence alone; it must go through agentHeldByMode(a) or the unscheduled reviewer renders green-idle there")
	}
	if n := strings.Count(html, "const isOff = agentHeldByMode(a) && !isPaused && !isOnDemand;"); n != 3 {
		t.Errorf("expected the card grid, the ops-center nav and the detail panel (3 sites) to derive isOff from agentHeldByMode, found %d", n)
	}
	if strings.Contains(html, "title=\"${esc(OFF_HEALTHY_TITLE)}\"") {
		t.Error("an off-healthy dot still uses the paused-only OFF_HEALTHY_TITLE; it must use agentOffTitle(a), which says WHICH gap holds the agent")
	}
}

// The card copy, executed under node: the blockers line and the dot title
// name the current mode and the modes that schedule the agent, and the
// unscheduled flag alone (no explicit pause) lands the agent in the off
// display state.
func TestAgentCardModeUnscheduledCopy(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the mode-unscheduled card copy was NOT executed by this run; TestAgentCardModeUnscheduledStructure still ran")
	}
	html := indexHTML(t)
	script := "const window = { _lastStatus: { governor: { mode: 'BUSY' } } };\n" +
		"const OFF_HEALTHY_TITLE = " + strings.TrimSpace(strings.TrimPrefix(extractLine(t, html, "const OFF_HEALTHY_TITLE = "), "const OFF_HEALTHY_TITLE = ")) + "\n" +
		jsFunc(t, html, "agentHeldByMode") + "\n" +
		jsFunc(t, html, "agentCadenceModesLabel") + "\n" +
		jsFunc(t, html, "agentOffTitle") + "\n" +
		jsFunc(t, html, "agentSchedulingBlockers") + "\n" + modeUnscheduledCardAssertions

	path := filepath.Join(t.TempDir(), "card.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("mode-unscheduled card copy check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

// extractLine returns the first line of html that starts with prefix.
func extractLine(t *testing.T, html, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(html, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("index.html has no line starting with %q", prefix)
	return ""
}

const modeUnscheduledCardAssertions = `
let fails = 0;
function check(name, cond) {
  if (!cond) { fails++; console.log('FAIL ' + name); }
}

const reviewer = { name: 'reviewer', unscheduledInMode: true, cadenceModes: ['surge'] };
const parked = { name: 'parked', offByCadence: true };
const both = { name: 'both', offByCadence: true, unscheduledInMode: true, cadenceModes: ['surge'] };
const scanner = { name: 'scanner' };

check('unscheduled alone is held by mode', agentHeldByMode(reviewer) === true);
check('explicit pause is held by mode', agentHeldByMode(parked) === true);
check('scheduled agent is not held', agentHeldByMode(scanner) === false);

const blockers = agentSchedulingBlockers(reviewer);
check('blockers name the current mode', blockers.some(b => b.includes('no cadence in busy')));
check('blockers name the modes that DO schedule it', blockers.some(b => b.includes('only in surge')));
check('a scheduled agent has no blockers', agentSchedulingBlockers(scanner).length === 0);
check('two scheduling modes read naturally',
  agentSchedulingBlockers({ unscheduledInMode: true, cadenceModes: ['busy', 'surge'] }).some(b => b.includes('only in busy and surge')));
check('missing cadenceModes does not render undefined',
  !agentSchedulingBlockers({ unscheduledInMode: true }).join(' ').includes('undefined'));

const title = agentOffTitle(reviewer);
check('dot title says which modes schedule it', title.includes('only in surge'));
check('dot title names the current mode', title.includes('busy'));
check('dot title points at the fix', title.includes('Cadences'));
check('dot title does not call it paused', !title.includes('paused'));
check('explicit pause keeps the paused title', agentOffTitle(parked) === OFF_HEALTHY_TITLE);
check('pause wins the title when both flags are set', agentOffTitle(both) === OFF_HEALTHY_TITLE);

if (fails) { console.log(fails + ' check(s) failed'); process.exit(1); }
`
