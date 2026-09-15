package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// findCadence returns the matrix entry for the named agent.
func findCadence(matrix []FrontendCadence, name string) (FrontendCadence, bool) {
	for _, e := range matrix {
		if e.Agent == name {
			return e, true
		}
	}
	return FrontendCadence{}, false
}

// findAgent returns the card for the named agent.
func findAgent(agents []FrontendAgent, name string) (FrontendAgent, bool) {
	for _, a := range agents {
		if a.Name == name {
			return a, true
		}
	}
	return FrontendAgent{}, false
}

// TestCadenceTooltipAndCardAgreeOnNextKick pins the actual user-visible bug in
// #7109: for an interval cadence with a known LastKick, the Governor-matrix
// tooltip's "next:" must equal the agent card's NEXT KICK. Both surfaces are
// built from the same cfg/statuses/govState here, exactly as the dashboard does,
// so a divergence between anchors (now vs LastKick) is caught end to end.
func TestCadenceTooltipAndCardAgreeOnNextKick(t *testing.T) {
	// LastKick deliberately in the past by less than the interval, so that
	// "now + interval" and "lastKick + interval" fall in different minutes and
	// the wrong anchor is detectable.
	lastKick := time.Now().Add(-40 * time.Minute)

	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude"},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle":  {Cadences: map[string]config.Cadence{"scanner": "6h"}},
				"quiet": {Cadences: map[string]config.Cadence{"scanner": "3h"}},
				"busy":  {Cadences: map[string]config.Cadence{"scanner": "2h"}},
				"surge": {Cadences: map[string]config.Cadence{"scanner": "1h"}},
			},
		},
	}
	statuses := map[string]*agent.AgentProcess{
		"scanner": {
			Name:     "scanner",
			Config:   config.AgentConfig{Backend: "claude"},
			State:    agent.StateRunning,
			LastKick: &lastKick,
		},
	}

	govState := governor.State{Mode: governor.ModeSurge}

	agents := buildAgents(statuses, cfg, govState)
	card, ok := findAgent(agents, "scanner")
	if !ok {
		t.Fatal("scanner card not found")
	}
	if card.NextKick == "" {
		t.Fatal("card NextKick empty; test cannot verify agreement")
	}

	// Pin the anchor absolutely: the card's next kick must be lastKick+interval,
	// not now+interval. This is what a helper that ignores LastKick would get
	// wrong even though both surfaces would still agree with each other.
	wantNext := formatHumanTime(lastKick.Add(time.Hour))
	if card.NextKick != wantNext {
		t.Fatalf("card NEXT KICK anchored wrong: got %q, want lastKick+1h = %q", card.NextKick, wantNext)
	}

	matrix := buildCadenceMatrix(cfg, statuses, "surge")
	entry, ok := findCadence(matrix, "scanner")
	if !ok {
		t.Fatal("scanner matrix entry not found")
	}

	// The surge cell is the active mode; its tooltip must carry the same "next".
	wantSuffix := "next: " + card.NextKick
	if !strings.Contains(entry.SurgeTitle, wantSuffix) {
		t.Fatalf("tooltip and card disagree on next kick:\n  card NEXT KICK = %q\n  tooltip        = %q\n  want tooltip to end with %q",
			card.NextKick, entry.SurgeTitle, wantSuffix)
	}
}

// TestCadenceTooltipCronAnchorsOnNow pins that cron cadences are unaffected:
// both card and tooltip pass time.Now() into schedule.Next, so they agree, and
// LastKick must NOT rebase a cron "next".
func TestCadenceTooltipCronAnchorsOnNow(t *testing.T) {
	// A last kick far in the past. For an interval cadence this would move the
	// next time; for cron it must be ignored.
	lastKick := time.Now().Add(-72 * time.Hour)
	cron := config.Cadence(`tod:{"cron":"0 * * * *","tz":"UTC"}`) // top of every hour

	if cron.Mode() != config.CadenceModeCron {
		t.Fatalf("test fixture not a cron cadence: mode = %q", cron.Mode())
	}

	withKick := cadenceTooltip(cron, &lastKick, true)
	withoutKick := cadenceTooltip(cron, nil, true)
	if withKick != withoutKick {
		t.Fatalf("cron tooltip must ignore LastKick:\n  with kick    = %q\n  without kick = %q", withKick, withoutKick)
	}

	// And it must match the card computation for the same cron/lastKick.
	cardNext := computeNextKickFromCadence(&lastKick, cron)
	if cardNext == "" {
		t.Fatal("card cron next empty; cannot verify")
	}
	if !strings.Contains(withKick, "next: "+cardNext) {
		t.Fatalf("cron tooltip and card disagree:\n  card    = %q\n  tooltip = %q", cardNext, withKick)
	}
}

// TestCadenceTooltipNonActiveModesOmitNext pins the deliberate decision for the
// non-active cells: only the active mode's cell shows "next:", because
// lastKick+thatInterval for an inactive mode is a kick that will never happen
// (#7109). Non-active interval cells show the human summary only.
func TestCadenceTooltipNonActiveModesOmitNext(t *testing.T) {
	lastKick := time.Now().Add(-40 * time.Minute)

	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude"},
		},
		Governor: config.GovernorConfig{
			Modes: map[string]config.ModeConfig{
				"idle":  {Cadences: map[string]config.Cadence{"scanner": "6h"}},
				"quiet": {Cadences: map[string]config.Cadence{"scanner": "3h"}},
				"busy":  {Cadences: map[string]config.Cadence{"scanner": "2h"}},
				"surge": {Cadences: map[string]config.Cadence{"scanner": "1h"}},
			},
		},
	}
	statuses := map[string]*agent.AgentProcess{
		"scanner": {
			Name:     "scanner",
			Config:   config.AgentConfig{Backend: "claude"},
			State:    agent.StateRunning,
			LastKick: &lastKick,
		},
	}

	matrix := buildCadenceMatrix(cfg, statuses, "surge")
	entry, ok := findCadence(matrix, "scanner")
	if !ok {
		t.Fatal("scanner matrix entry not found")
	}

	// Active mode (surge) shows next.
	if !strings.Contains(entry.SurgeTitle, "next: ") {
		t.Fatalf("active surge cell must show next, got %q", entry.SurgeTitle)
	}
	// Non-active modes must NOT show a next time.
	for label, title := range map[string]string{
		"idle":  entry.IdleTitle,
		"quiet": entry.QuietTitle,
		"busy":  entry.BusyTitle,
	} {
		if strings.Contains(title, "next: ") {
			t.Fatalf("non-active %s cell must not show a hypothetical next, got %q", label, title)
		}
		if title == "" {
			t.Fatalf("non-active %s cell should still carry the human summary, got empty", label)
		}
	}
}
