package main

// Login-required scanning: detecting that a coding backend has dropped to an
// interactive login prompt, debouncing that signal across sightings so a
// single noisy line cannot pause an agent, and acting on the verdict.

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	// automaxprocs sets GOMAXPROCS to match the container's CPU quota (Linux
	// CFS) at init. Without it the Go runtime sizes its P count to the whole
	// NODE's core count, so on a many-core IKS worker a pod limited to a few
	// CPUs spawns far more runnable Ps than its CFS quota can service; when the
	// quota is exhausted mid-period EVERY goroutine — including the netpoller
	// that answers the :3002 liveness probe and the heartbeat loop — is
	// throttled until the next CFS period, which stacks on top of the NFS
	// stalls to push probe latency past the kubelet timeout. Matching GOMAXPROCS
	// to the quota removes that self-inflicted throttling.
	//
	// This is called explicitly rather than via the package's blank import
	// because that import's init writes a line to the default logger (stderr)
	// unconditionally. `hive` re-execs itself as a Git transport shim, and the
	// setup path captures a child's stdout and stderr into a single buffer to
	// parse (e.g. `symbolic-ref --short origin/HEAD`), so an init-time banner
	// is indistinguishable from Git's answer and corrupts the parsed branch
	// name. Setting it with a no-op logger keeps the GOMAXPROCS behaviour and
	// drops the banner.

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/notify"
)

// loginCommandForBackend returns the login instruction for a given CLI backend.
func loginCommandForBackend(backend string) string {
	switch backend {
	case "claude":
		return "Run: claude login"
	case "copilot":
		return "Run: copilot auth login"
	case "gemini":
		return "Run: gemini auth login"
	case "goose":
		return "Run: goose auth login"
	default:
		return "Run the login command for " + backend
	}
}

// loginScanAction is what the detector should do about one agent this cycle.
type loginScanAction int

const (
	// loginScanIgnore: nothing that looks like a login problem, or a startup
	// modal is on screen. Any sighting streak is cleared.
	loginScanIgnore loginScanAction = iota
	// loginScanDeferAuthenticated: the pane matched, but the backend credential
	// is demonstrably valid, so this is residue or a stuck CLI — the manager's
	// token-restart heal's case, not an operator's (kubestellar/hive#5291).
	loginScanDeferAuthenticated
	// loginScanDeferStreak: the pane matched and the credential is not provably
	// good, but this is the first consecutive cycle to see it.
	loginScanDeferStreak
	// loginScanPause: pause the agent and page the operator.
	loginScanPause
)

// loginPauseMinSightings is how many CONSECUTIVE governor cycles must see a
// login pattern before the detector pauses (kubestellar/hive#5291).
//
// The manager's own pane poller learned this at its ~3s cadence, where a single
// sighting restarted healthy agents; it now requires loginStreakRestartMin = 3.
// The detector had no equivalent, and a pause is far more expensive than a
// restart — it is sticky, it needs a human to undo, and it cancels the agent
// context that hosts the heal. Two is deliberate rather than three: a governor
// cycle is minutes, not seconds, so each extra cycle is real delay for a
// genuine logout, and the credential gate above already covers the case this
// backstops. It matters most for backends with no credential file this process
// can check, where it is the only new protection.
const loginPauseMinSightings = 2

// loginSightingTracker counts CONSECUTIVE cycles in which each agent's pane
// matched a login pattern. A clean cycle resets the count to zero, so a match
// has to persist to accumulate — a single flicker never reaches the threshold.
type loginSightingTracker struct {
	mu     sync.Mutex
	streak map[string]int
}

func newLoginSightingTracker() *loginSightingTracker {
	return &loginSightingTracker{streak: map[string]int{}}
}

// loginSightings is the detector's process-scoped state. The governor cycle is
// a function rather than an object, so the consecutive-sighting counts have to
// outlive a single call; tests build their own tracker and pass it explicitly.
var loginSightings = newLoginSightingTracker()

// observe records this cycle's reading for one agent and returns the resulting
// consecutive-sighting count (1 on the first sighting).
func (t *loginSightingTracker) observe(agent string, matched bool) int {
	if t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
	}
	if t == nil {
		// No tracker wired: behave as if every sighting is its own streak, which
		// is exactly the pre-#5291 single-observation behaviour.
		if matched {
			return loginPauseMinSightings
		}
		return 0
	}
	if !matched {
		delete(t.streak, agent)
		return 0
	}
	t.streak[agent]++
	return t.streak[agent]
}

// forget drops an agent's streak — on pause (it stops being scanned) and for
// agents that are no longer present, so the map cannot grow without bound
// across a long-lived process.
func (t *loginSightingTracker) forget(agent string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.streak, agent)
}

// retain drops every agent not in the given set.
func (t *loginSightingTracker) retain(present map[string]bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for name := range t.streak {
		if !present[name] {
			delete(t.streak, name)
		}
	}
}

// loginScanDecision is the detector's whole judgement about one agent, as a
// pure function of what was observed. It exists apart from scanForLoginRequired
// so the decision can be tested against real pane text without a tmux session,
// a manager, or a governor cycle.
//
// sightings is the consecutive-cycle count INCLUDING this one.
//
// The credential gate is the fix for kubestellar/hive#5291: the detector used
// to pause on pane text alone, and the pane during and just after an
// interactive /login necessarily contains login-screen chrome — so it fired on
// the evidence the operator's own fix had just produced, seven minutes after
// the credential was already valid. Worse, Pause() cancels the agent context
// and tears down the poller that hosts the token-restart heal (#4606), which is
// the mechanism built for exactly "login prompt on screen, credential valid".
// Pausing first therefore disabled the machinery that would have fixed the pane
// it misread.
//
// Text matching cannot be narrowed out of this: two earlier fixes tried
// (tail-only matching, then a tighter copilot pattern) and this incident is the
// third false positive. The pane legitimately contains login text at the moment
// the credential is freshest, so the credential has to be consulted.
func loginScanDecision(
	backend, paneText string,
	compiled []*regexp.Regexp,
	credentialValid bool,
	sightings int,
) (loginScanAction, *regexp.Regexp) {
	matched := loginScanMatch(backend, paneText, compiled)
	return loginScanVerdict(matched != nil, credentialValid, sightings), matched
}

// loginScanMatch reports which login pattern this pane trips, or nil for none.
// Separate from the verdict so the scan loop can match ONCE and use the answer
// both to advance the sighting streak and to decide.
func loginScanMatch(backend, paneText string, compiled []*regexp.Regexp) *regexp.Regexp {
	// Stand down while a startup-blocking modal (folder trust, codex update, …)
	// is on screen: that is not a login problem, and pausing the agent for it
	// cancels the trust-prompt watcher that would answer it — the deadlock that
	// kept copilot agents "sitting at login prompt" through every operator
	// re-login (hivecommons/hive, 2026-08-22). The watcher answers the modal
	// within seconds; if a REAL login prompt follows, the next detector tick
	// sees it on a clean pane.
	if agent.PaneShowsBlockingPrompt(backend, paneText) {
		return nil
	}
	for _, re := range compiled {
		if re.MatchString(paneText) {
			return re
		}
	}
	return nil
}

// loginScanVerdict turns "what the pane showed" into "what to do". It returns
// loginScanIgnore whenever matched is false, which is what lets the scan loop
// rely on a non-Ignore verdict implying a non-nil pattern to log.
func loginScanVerdict(matched, credentialValid bool, sightings int) loginScanAction {
	if !matched {
		return loginScanIgnore
	}
	if credentialValid {
		return loginScanDeferAuthenticated
	}
	if sightings < loginPauseMinSightings {
		return loginScanDeferStreak
	}
	return loginScanPause
}

type loginScanAgentManager interface {
	AllStatuses() map[string]*agent.AgentProcess
	GetOutput(name string, lines int) ([]string, error)
	AgentHasValidCredential(agentName string) bool
	RefreshAgentTokenFor(ctx context.Context, name string) error
	Pause(name, trigger, reason string) error
}

type loginScanNotifier interface {
	Send(title, message string, priority notify.Priority)
}

type loginScanAuditor interface {
	AuditLog(user, action, detail, agent string)
}

// scanForLoginRequired checks each running agent's tmux pane output for login-required
// patterns. When a match is found, the agent is paused and a notification is sent.
func scanForLoginRequired(
	ctx context.Context,
	cfg *config.Config,
	agentMgr loginScanAgentManager,
	notifier loginScanNotifier,
	dashSrv loginScanAuditor,
	logger *slog.Logger,
	sightings *loginSightingTracker,
) {
	patterns := cfg.Governor.Sensing.LoginPatterns
	if len(patterns) == 0 {
		return
	}

	// Compile regex patterns, skipping empty and invalid ones
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if strings.TrimSpace(p) == "" {
			continue
		}
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			logger.Warn("invalid login pattern regex", "pattern", p, "error", err)
			continue
		}
		compiled = append(compiled, re)
	}
	if len(compiled) == 0 {
		return
	}

	// Scan the pane TAIL only. A login prompt the CLI is genuinely stuck at
	// sits at the BOTTOM of the pane; the 50-line window this used to read
	// reached deep into scrollback, where agent WORK OUTPUT that merely
	// mentions a pattern phrase lives — quality's scan findings quoting
	// "gh auth login" from auth documentation got the agent paused mid-kick
	// (hivecommons/hive, 2026-08-22 08:27, on a fully-authenticated CLI).
	// Same discipline as the poller's tail-only match (#4577).
	const paneLines = 12
	statuses := agentMgr.AllStatuses()
	scanned := make(map[string]bool, len(statuses))
	for name, proc := range statuses {
		if proc.State != "running" {
			continue
		}
		scanned[name] = true

		output, err := agentMgr.GetOutput(name, paneLines)
		if err != nil || len(output) == 0 {
			continue
		}

		joined := strings.Join(output, "\n")
		backend := cfg.Agents[name].Backend

		// #5291: ask the CREDENTIAL, not just the pane. A valid credential plus
		// a login prompt is the token-restart heal's case; only an invalid one
		// needs a human.
		credentialValid := agentMgr.AgentHasValidCredential(name)

		// Match once. The streak has to reflect what the pane SHOWED, including
		// on the cycles where a gate below declines to act on it, so the
		// sighting is recorded before the verdict is taken.
		re := loginScanMatch(backend, joined, compiled)
		streak := sightings.observe(name, re != nil)

		switch loginScanVerdict(re != nil, credentialValid, streak) {
		case loginScanIgnore:
			continue
		case loginScanDeferAuthenticated:
			// Logged at Info, not Warn: this is the detector working correctly,
			// and it is the line that explains an agent staying up with login
			// text on its pane.
			logger.Info("login pattern matched but the backend credential is valid — leaving it to the token-restart heal",
				"agent", name, "backend", backend, "pattern", re.String())
			continue
		case loginScanDeferStreak:
			logger.Info("login pattern matched but not yet on enough consecutive cycles — deferring",
				"agent", name, "backend", backend, "pattern", re.String(),
				"sightings", streak, "required", loginPauseMinSightings)
			continue
		case loginScanPause:
			logger.Warn("login required detected",
				"agent", name,
				"pattern", re.String(),
				"sightings", streak,
			)
			sightings.forget(name)

			// Attempt a per-agent token re-cache BEFORE pausing. On an
			// App-authenticated hive the likeliest cause of a "gh auth
			// login" prompt is an expired scoped-token cache (#4072);
			// re-minting it now means the operator's Resume immediately
			// works instead of 401ing straight back into this pause.
			// Best-effort: hives without App auth (or agents without a
			// dedicated UID) simply skip it.
			if refreshErr := agentMgr.RefreshAgentTokenFor(ctx, name); refreshErr == nil {
				logger.Info("re-cached per-agent scoped token before login-detector pause", "agent", name)
			}

			// Pause the agent instead of restarting
			if pauseErr := agentMgr.Pause(name, "login-detector", "login required detected"); pauseErr != nil {
				logger.Warn("failed to pause agent after login detection",
					"agent", name, "error", pauseErr)
			} else {
				dashSrv.AuditLog("system", "pause", "trigger=login-detector", name)
			}

			// Determine the login instruction based on the agent's backend
			loginCmd := loginCommandForBackend(backend)

			notifier.Send(
				fmt.Sprintf("\U0001F511 Login required: %s", name),
				fmt.Sprintf(
					"Agent '%s' needs authentication. Open the agent's terminal "+
						"(tmux attach -t hive-%s) and run the login command for the CLI (%s). %s",
					name, name, backend, loginCmd,
				),
				notify.PriorityHigh,
			)
		}
	}
	// Agents that vanished (removed from config, stopped) must not keep a
	// streak alive in the map for the life of the process.
	sightings.retain(scanned)
}
