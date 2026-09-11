package agent

import (
	"strings"
	"testing"
	"time"
)

// #6578: the token-restart cap latched correctly and was then unlatched by the
// boot pane of its own restart, so it could never hold. Three hives ran at up
// to ×1334 restarts/24h — one every ~65s, i.e. the cooldown's own period —
// while tokenRestartMaxAttempts was 3 the whole time.
//
// These tests drive the REAL decision functions with REAL pane text through the
// REAL detectors, so they fail if either the rule or the strings it depends on
// regress.

// The pane a CLI shows while it is still booting: startup chrome, no login
// prompt yet, and nothing a ready-signature check would accept. This is the
// state every token restart produces moments after it fires.
var bootingPane = []string{
	"Loading:  ⣾",
}

// The pane after the CLI finishes booting and paints its device-flow prompt.
// Verbatim shape of the GitHub device-flow screen (loginPromptPatterns).
var loginPane = []string{
	"To authenticate, visit:",
	"  https://github.com/login/device",
	"Enter one-time code: ABCD-1234",
}

// The pane of an agent that genuinely came back: a real CLI prompt, no login
// chrome. "❯" is a cliPaneMarkers entry.
var readyPane = []string{
	"❯ ",
	"/ commands   ? help",
}

// paneFacts runs the production detectors over a pane, so the tests below
// cannot drift from what the poller actually computes.
func paneFacts(t *testing.T, pane []string) (showsLogin, hasCLI bool) {
	t.Helper()
	return paneShowsLoginPrompt(pane), paneHasCLIMarker(strings.Join(pane, "\n"))
}

// TestPaneFixturesMatchTheProductionDetectors pins the premises the storm
// simulation rests on. Without this, a change to loginPromptPatterns or
// cliPaneMarkers could turn the simulation into a test of nothing.
func TestPaneFixturesMatchTheProductionDetectors(t *testing.T) {
	if login, cli := paneFacts(t, bootingPane); login || cli {
		t.Fatalf("booting pane: showsLogin=%v hasCLI=%v, want false/false — "+
			"the whole bug is that this pane looks like neither", login, cli)
	}
	if login, _ := paneFacts(t, loginPane); !login {
		t.Fatalf("login pane not detected as a login prompt: %q", loginPane)
	}
	if login, cli := paneFacts(t, readyPane); login || !cli {
		t.Fatalf("ready pane: showsLogin=%v hasCLI=%v, want false/true", login, cli)
	}
}

// capState mirrors the poller's per-tick handling of the cap, using the real
// functions. Returns whether a restart fired on this tick.
func (a *AgentProcess) simulatePollTick(t *testing.T, pane []string, startedAt *time.Time, now time.Time, loginStreak *int) (fired bool) {
	t.Helper()
	showsLogin, hasCLI := paneFacts(t, pane)
	if showsLogin {
		*loginStreak++
	} else {
		*loginStreak = 0
		if a.shouldResetTokenRestartCap(hasCLI, startedAt, now) {
			a.tokenRestartAttempts = 0
			a.tokenRestartGaveUp = false
		}
	}
	if !showsLogin || *loginStreak < loginStreakRestartMin {
		return false
	}
	switch a.decideTokenRestart(now) {
	case tokenRestartFire:
		return true
	case tokenRestartGiveUp:
		a.tokenRestartGaveUp = true
	}
	return false
}

// TestTokenRestartCapSurvivesItsOwnBootPane is the regression the issue asks
// for: an agent whose pane alternates blank-during-boot → login-prompt must
// reach tokenRestartGiveUp and STAY there.
//
// The simulated day is the assertion that matters. Before the fix this loop
// fired once per cooldown forever (86400/65 ≈ 1330 — the observed rate); the
// cap says it may fire at most tokenRestartMaxAttempts times, ever.
func TestTokenRestartCapSurvivesItsOwnBootPane(t *testing.T) {
	const (
		pollInterval = 3 * time.Second
		simulatedDay = 24 * time.Hour
		// How long after a restart the CLI spends booting before it paints
		// the login prompt again. Deliberately shorter than the boot grace:
		// that is the case the old code got wrong.
		bootPaintDelay = 12 * time.Second
	)

	a := &AgentProcess{Name: "quality"}
	now := time.Now()
	started := now
	startedAt := &started
	loginStreak := 0
	fires := 0

	for elapsed := time.Duration(0); elapsed < simulatedDay; elapsed += pollInterval {
		pane := loginPane
		if now.Sub(*startedAt) < bootPaintDelay {
			// Still booting after the most recent (re)launch.
			pane = bootingPane
		}
		if a.simulatePollTick(t, pane, startedAt, now, &loginStreak) {
			fires++
			// A restart relaunches the CLI: StartedAt is refreshed and the
			// pane goes back to booting. This is the step that used to clear
			// the latch.
			relaunched := now
			startedAt = &relaunched
			loginStreak = 0
		}
		now = now.Add(pollInterval)
	}

	if fires > tokenRestartMaxAttempts {
		t.Fatalf("fired %d token restarts across a simulated 24h; the cap is %d. "+
			"The give-up latch is being cleared by the boot pane of its own restart (#6578)",
			fires, tokenRestartMaxAttempts)
	}
	if fires == 0 {
		t.Fatal("never fired at all: the restart is now suppressed outright, which is a different bug")
	}
	if !a.tokenRestartGaveUp {
		t.Fatal("ended the simulated day without latching tokenRestartGaveUp, " +
			"so diagnoseStuckLogin never persists and the operator is never told to log in")
	}
	if a.tokenRestartAttempts != tokenRestartMaxAttempts {
		t.Fatalf("tokenRestartAttempts = %d at end of day, want %d — the counter was re-armed",
			a.tokenRestartAttempts, tokenRestartMaxAttempts)
	}
}

// TestTokenRestartCapHoldsWhenBootOutlastsTheGrace covers the hole a
// time-only guard would leave: a CLI slower to paint than cliBootGraceSeconds.
// Past the grace the pane is still bare, so there is still no evidence the
// login cleared, and the cap must still hold.
func TestTokenRestartCapHoldsWhenBootOutlastsTheGrace(t *testing.T) {
	const pollInterval = 3 * time.Second

	a := &AgentProcess{Name: "guide"}
	now := time.Now()
	started := now
	startedAt := &started
	loginStreak := 0
	fires := 0

	// A boot that takes twice the grace to paint anything.
	bootPaintDelay := 2 * time.Duration(cliBootGraceSeconds) * time.Second

	for elapsed := time.Duration(0); elapsed < 6*time.Hour; elapsed += pollInterval {
		pane := loginPane
		if now.Sub(*startedAt) < bootPaintDelay {
			pane = bootingPane
		}
		if a.simulatePollTick(t, pane, startedAt, now, &loginStreak) {
			fires++
			relaunched := now
			startedAt = &relaunched
			loginStreak = 0
		}
		now = now.Add(pollInterval)
	}

	if fires > tokenRestartMaxAttempts {
		t.Fatalf("fired %d restarts with a slow-booting CLI; cap is %d — "+
			"a bare pane past the boot grace must not be read as a cleared login",
			fires, tokenRestartMaxAttempts)
	}
	if !a.tokenRestartGaveUp {
		t.Fatal("latch did not hold for a slow-booting CLI")
	}
}

// TestTokenRestartCapReArmsAfterGenuineRecovery is the other half of the
// contract, and guards against over-correcting into "the cap never resets".
// An agent that really does come back — past the boot grace, with a CLI
// prompt on screen — must be able to earn a fresh nudge later.
func TestTokenRestartCapReArmsAfterGenuineRecovery(t *testing.T) {
	a := &AgentProcess{Name: "supervisor"}
	now := time.Now()

	// Drive to the cap.
	cooldown := time.Duration(tokenRestartCooldownSec) * time.Second
	const maxProbes = 25
	for i := 0; i < maxProbes && a.decideTokenRestart(now) != tokenRestartGiveUp; i++ {
		now = now.Add(cooldown)
	}
	a.tokenRestartGaveUp = true
	if got := a.decideTokenRestart(now); got != tokenRestartGiveUp {
		t.Fatalf("expected giveUp before recovery, got %v", got)
	}

	// The operator logs in. The agent has been up well past the boot grace and
	// its pane now shows a real CLI prompt.
	started := now.Add(-10 * time.Minute)
	loginStreak := 0
	a.simulatePollTick(t, readyPane, &started, now, &loginStreak)

	if a.tokenRestartGaveUp {
		t.Fatal("latch survived a genuine recovery: a real login would never re-enable the nudge")
	}
	if a.tokenRestartAttempts != 0 {
		t.Fatalf("attempts = %d after recovery, want 0", a.tokenRestartAttempts)
	}
	if got := a.decideTokenRestart(now); got != tokenRestartFire {
		t.Fatalf("after recovery: got %v, want fire", got)
	}
}

// TestShouldResetTokenRestartCapConditions pins each branch of the rule,
// including the two that used to be conflated.
func TestShouldResetTokenRestartCapConditions(t *testing.T) {
	now := time.Now()
	grace := time.Duration(cliBootGraceSeconds) * time.Second
	booting := now.Add(-grace / 2)
	old := now.Add(-2 * grace)
	atBoundary := now.Add(-grace)

	cases := []struct {
		name      string
		paneReady bool
		startedAt *time.Time
		want      bool
		why       string
	}{
		{"booting, nothing painted", false, &booting, false,
			"the boot pane of a token restart — the exact state that used to clear the latch"},
		{"booting, chrome already painted", true, &booting, false,
			"a banner during boot is not proof the login cleared; only time plus a marker is"},
		{"past grace, pane still bare", false, &old, false,
			"a slow or dead CLI leaves a bare pane; that is not a cleared login either"},
		{"past grace, CLI prompt on screen", true, &old, true,
			"the only combination that is positive evidence"},
		{"exactly at the grace boundary", true, &atBoundary, true,
			"the grace is inclusive at its edge, matching watchdog's `< BootGrace` booting test"},
		{"undatable agent, pane ready", true, nil, true,
			"no StartedAt means no grace rather than an unbounded one (watchdog.Classify's rule)"},
		{"undatable agent, pane bare", false, nil, false,
			"the marker condition still gates an undatable agent"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &AgentProcess{Name: "quality"}
			if got := a.shouldResetTokenRestartCap(tc.paneReady, tc.startedAt, now); got != tc.want {
				t.Errorf("shouldResetTokenRestartCap = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestTokenRestartCapBootGraceMatchesWatchdog pins the constant relationship
// the comment claims. pkg/agent cannot import pkg/watchdog's settings for the
// same reason watchdog cannot import this constant (the dependency runs one
// way), so the equality is asserted rather than shared.
func TestTokenRestartCapBootGraceMatchesWatchdog(t *testing.T) {
	if cliBootGraceSeconds <= 0 {
		t.Fatalf("cliBootGraceSeconds = %d, want a positive grace", cliBootGraceSeconds)
	}
	// A grace longer than the restart cooldown would let a restart fire before
	// the previous boot was ever evaluated, which would re-open the storm from
	// the other side.
	if cliBootGraceSeconds > tokenRestartCooldownSec {
		t.Fatalf("cliBootGraceSeconds (%d) exceeds tokenRestartCooldownSec (%d): "+
			"a restart could fire before its own boot pane was ever judged",
			cliBootGraceSeconds, tokenRestartCooldownSec)
	}
}
