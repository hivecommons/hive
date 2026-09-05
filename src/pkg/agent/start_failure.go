package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Start-failure classification and relaunch backoff (#5958, from incident
// #5921).
//
// The incident: a hive spent 4,025 log lines and a tmux session every ~3
// minutes relaunching agents that could not possibly start. The underlying
// causes were ordinary and user-fixable — Copilot was not logged in, a bob API
// key was rejected — but neither reached the operator. Every surface said the
// same unactionable thing ("restart needed", "hung"), and the relaunch loop had
// only a fixed cooldown, so it retried an unfixable condition forever while the
// fleet page showed the agents green.
//
// Two things were missing, and this file supplies both:
//
//  1. A REASON. A start failure is recorded with a stable class and a sentence
//     an operator can act on ("copilot: not logged in"), which then rides state
//     out to the dashboard card, the heartbeat and the fleet verdict.
//  2. A LIMIT. Consecutive failures with the SAME reason escalate a backoff and,
//     past startFailureBlockThreshold(), mark the agent blocked. A blocked
//     agent is not relaunched on the tick, and does not count toward the
//     fleet's "able" total.
//
// The shape deliberately mirrors the inference-provider backoff already in
// manager.go (markProviderErrorLocked and friends): same "same-signal repeat
// does not re-arm" rule, same escalating ladder, same "cleared by success"
// lifecycle. An operator who has learned one has learned the other, and the two
// cannot drift into contradicting each other about what a stuck agent means.

// StartFailureClass is the stable, machine-comparable kind of a start failure.
// The class — not the human sentence — is what consecutive-failure counting
// compares, so re-phrasing a message can never reset the ladder, and two
// genuinely different faults can never be counted as one recurrence.
type StartFailureClass string

const (
	// StartFailureLoginRequired: the CLI came up but has no usable interactive
	// session — Copilot's "/login to sign in", a device-code prompt. A human
	// must authenticate; no number of relaunches will do it.
	StartFailureLoginRequired StartFailureClass = "login-required"
	// StartFailureCredentialRejected: a credential was presented and the server
	// refused it — bob's "Invalid or expired API Key", a 401. Also terminal
	// until a human replaces the credential.
	StartFailureCredentialRejected StartFailureClass = "credential-rejected"
	// StartFailureCredentialMissing: no credential is configured at all.
	StartFailureCredentialMissing StartFailureClass = "credential-missing"
	// StartFailureBackendMismatch: the agent is launched as one CLI and
	// health-checked as another, because backend and launch_cmd disagree
	// (#5921's root cause 1). Relaunching cannot converge — the readiness probe
	// is watching for a prompt the launched binary never prints.
	StartFailureBackendMismatch StartFailureClass = "backend-mismatch"
	// StartFailureBinaryMissing: the backend's CLI is not on PATH.
	StartFailureBinaryMissing StartFailureClass = "binary-missing"
	// StartFailureNoOutput: the CLI produced no readiness marker and the
	// diagnostic could not say why. The honest residual class — it means "we
	// know it did not start and we do not know why", which is different from
	// every named class above and must not borrow their wording.
	StartFailureNoOutput StartFailureClass = "no-output"
)

// startFailureBlockThresholdDefault is how many CONSECUTIVE failures of the
// same class mark an agent blocked, unless StartFailureBlockThresholdEnv
// overrides it.
//
// Three, not one: a single failed start is routinely transient — a cold pod
// whose CLI lost a race with its own config write, a token refreshed a second
// late — and blocking on the first would turn recoverable noise into an agent
// an operator has to un-wedge by hand. Three identical outcomes in a row is no
// longer bad luck. Backoff is applied from the FIRST failure regardless, so the
// two retries before the block are already paced, not a free burst.
const startFailureBlockThresholdDefault = 3

// StartFailureBlockThresholdEnv overrides startFailureBlockThresholdDefault.
const StartFailureBlockThresholdEnv = "HIVE_START_FAILURE_BLOCK_THRESHOLD"

// StartFailureBackoffLadderEnv overrides startFailureBackoffLadderDefault with
// a comma-separated list of positive Go durations, for example "30s,2m,10m".
const StartFailureBackoffLadderEnv = "HIVE_START_FAILURE_BACKOFF_LADDER"

// startFailureBackoffLadder is the delay before the next relaunch attempt,
// indexed by consecutive-failure count. It is capped rather than unbounded: a
// blocked agent must still re-probe occasionally, because the conditions here
// are exactly the ones a human fixes out of band (a login completed, a key
// pasted into Settings) with nothing to notify the hive that they did.
//
// The cap is 30 minutes for that reason and not longer. An operator who has
// just fixed a credential should not have to wait an hour to see it take, and
// the explicit relaunch paths (RelaunchBobAgentsAwaitingKey on a key save, the
// dashboard restart button) clear the backoff outright — the ladder governs the
// AUTOMATIC loop, never a human's deliberate retry.
var startFailureBackoffLadderDefault = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
}

func startFailureBlockThreshold() int {
	if v := strings.TrimSpace(os.Getenv(StartFailureBlockThresholdEnv)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return startFailureBlockThresholdDefault
}

func startFailureBackoffLadder() []time.Duration {
	if v := strings.TrimSpace(os.Getenv(StartFailureBackoffLadderEnv)); v != "" {
		parts := strings.Split(v, ",")
		ladder := make([]time.Duration, 0, len(parts))
		for _, part := range parts {
			d, err := time.ParseDuration(strings.TrimSpace(part))
			if err != nil || d <= 0 {
				return startFailureBackoffLadderDefault
			}
			ladder = append(ladder, d)
		}
		if len(ladder) > 0 {
			return ladder
		}
	}
	return startFailureBackoffLadderDefault
}

// startFailureBackoffDelay returns the ladder delay for the n-th consecutive
// failure (1-based), saturating at the last rung.
func startFailureBackoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	ladder := startFailureBackoffLadder()
	if attempt > len(ladder) {
		attempt = len(ladder)
	}
	return ladder[attempt-1]
}

// startFailureReason renders the operator-facing sentence for a class, scoped
// to the backend it happened to, in the shape #5921 asked for:
// "copilot: not logged in", "bob: API key rejected (401)".
//
// detail carries the specific evidence when there is any (the rejected status,
// the missing binary's error). It is appended rather than substituted so the
// class wording stays stable and greppable across hives.
func startFailureReason(backend string, class StartFailureClass, detail string) string {
	backend = strings.TrimSpace(backend)
	if backend == "" {
		backend = "agent"
	}
	detail = strings.TrimSpace(detail)

	var base string
	switch class {
	case StartFailureLoginRequired:
		base = backend + ": not logged in"
	case StartFailureCredentialRejected:
		if backend == bobBackend {
			base = "bob API key invalid or expired — refresh " + config.DefaultBobAPIKeyEnv
		} else {
			base = backend + ": API key rejected"
		}
	case StartFailureCredentialMissing:
		base = backend + ": no API key configured"
	case StartFailureBackendMismatch:
		// Deliberately NOT backend-scoped: the whole point of this class is that
		// the configured backend is not the thing that launched, so naming one
		// of the two would assert the confusion the message exists to report.
		base = "backend/launch_cmd mismatch"
	case StartFailureBinaryMissing:
		base = backend + ": CLI binary not found"
	case StartFailureNoOutput:
		base = backend + ": no CLI prompt after launch"
	default:
		base = backend + ": failed to start"
	}
	if detail == "" {
		return base
	}
	return base + " (" + detail + ")"
}

// launchCmdBackend reports which known CLI a launch_cmd actually runs, or ""
// when the command names none we recognise.
//
// Only a POSITIVE identification counts. An unrecognised wrapper, an empty
// command, or a script we cannot see through all return "" — read by callers as
// "no opinion", never as a mismatch. Guessing wrong here would manufacture a
// backend-mismatch block on a working agent, which is strictly worse than the
// missing diagnosis this exists to provide.
//
// Two forms are understood, matching how launch_cmd is written in practice
// (src/deploy/*.yaml, src/docs/agent-configuration.md):
//
//	"/usr/bin/copilot --allow-all --model ..."      → the binary itself
//	"agent-launch.sh --backend copilot --model ..." → the wrapper's flag
//
// NOTE: PR #5944 adds config.LaunchCmdDeclaredBackend for the load/save-time
// half of this same question. When that lands, this should delegate to it
// rather than keep a second table — the runtime and the validator disagreeing
// about what a launch_cmd launches is the #5921 bug in a new place.
func launchCmdBackend(launchCmd string) string {
	fields := strings.Fields(launchCmd)
	if len(fields) == 0 {
		return ""
	}

	// Wrapper form: read the --backend flag it was given.
	for i, f := range fields {
		if f == "--backend" && i+1 < len(fields) {
			if known := knownCLIBackend(fields[i+1]); known != "" {
				return known
			}
			return ""
		}
		if rest, ok := strings.CutPrefix(f, "--backend="); ok {
			return knownCLIBackend(rest)
		}
	}

	// Direct form: the leading token is the binary. Take its base name so an
	// absolute path ("/usr/bin/copilot") reads the same as a bare one.
	binary := fields[0]
	if i := strings.LastIndex(binary, "/"); i >= 0 {
		binary = binary[i+1:]
	}
	return knownCLIBackend(binary)
}

// knownCLIBackend canonicalises a token to a CLI backend name, or "" when it
// names none. Inference backends are excluded on purpose: they all exec the
// claude binary, so a claude command line proves nothing about which of them is
// configured and could never establish a mismatch.
func knownCLIBackend(token string) string {
	token = strings.ToLower(strings.TrimSpace(token))
	if token == "" {
		return ""
	}
	for _, b := range config.CLIBackends {
		if token == b {
			return b
		}
	}
	return ""
}

// backendMismatch reports the launch_cmd-declared backend when it CONTRADICTS
// the agent's configured backend, or "" when the two agree or the launch_cmd
// says nothing recognisable.
//
// Inference backends are exempt: they legitimately launch the claude binary, so
// "backend: vllm, launch_cmd: claude ..." is correct configuration rather than
// a contradiction — the same carve-out #5944 makes at validation time.
func backendMismatch(configured, launchCmd string) string {
	declared := launchCmdBackend(launchCmd)
	if declared == "" {
		return ""
	}
	configured = strings.ToLower(strings.TrimSpace(configured))
	if configured == "" || declared == configured {
		return ""
	}
	if IsInferenceBackend(configured) && declared == "claude" {
		return ""
	}
	return declared
}

// markStartFailureLocked records one failed start and returns the delay before
// the next automatic relaunch attempt, plus whether the agent is now blocked.
//
// A repeat of the SAME class while a backoff is already running does not re-arm
// it: the poller and the crash loop both observe the same condition on their
// own cadences, and letting each observation push the deadline out would make
// the ladder a function of tick rate rather than of failures. A DIFFERENT class
// is a different fault and starts its own ladder from the first rung.
//
// Caller holds m.mu.
func (m *Manager) markStartFailureLocked(agent *AgentProcess, class StartFailureClass, reason string, now time.Time) (time.Duration, bool) {
	if agent == nil {
		return 0, false
	}
	if agent.StartFailureClass == string(class) && !agent.StartBackoffUntil.IsZero() && now.Before(agent.StartBackoffUntil) {
		return agent.StartBackoffUntil.Sub(now), agent.StartBlocked
	}
	if agent.StartFailureClass != string(class) {
		agent.StartFailureCount = 0
	}
	agent.StartFailureClass = string(class)
	agent.StartFailureReason = reason
	agent.StartFailureCount++
	agent.StartFailureLastAt = now
	agent.LastError = reason
	agent.StartBlocked = agent.StartFailureCount >= startFailureBlockThreshold()
	delay := startFailureBackoffDelay(agent.StartFailureCount)
	agent.StartBackoffUntil = now.Add(delay)
	return delay, agent.StartBlocked
}

// recordStartFailureLocked is the call site helper: it renders the reason for
// (backend, class, detail) and records the failure. Every production hook goes
// through here so the wording of a class is decided in exactly one place and a
// hive cannot report the same fault two different ways.
//
// Caller holds m.mu.
func (m *Manager) recordStartFailureLocked(agent *AgentProcess, backend string, class StartFailureClass, detail string) (time.Duration, bool) {
	return m.markStartFailureLocked(agent, class, startFailureReason(backend, class, detail), time.Now())
}

// clearStartFailureLocked retires the whole start-failure record. Called when a
// launch demonstrably succeeds, and by the explicit operator paths (restart
// button, key-save relaunch) whose whole purpose is to retry now.
//
// Caller holds m.mu.
func (m *Manager) clearStartFailureLocked(agent *AgentProcess) {
	if agent == nil {
		return
	}
	if agent.StartFailureClass == "" && agent.StartFailureCount == 0 && agent.StartBackoffUntil.IsZero() {
		return
	}
	if agent.LastError == agent.StartFailureReason {
		agent.LastError = ""
	}
	agent.StartFailureClass = ""
	agent.StartFailureReason = ""
	agent.StartFailureCount = 0
	agent.StartFailureLastAt = time.Time{}
	agent.StartFailureExitCode = nil
	agent.StartFailureSignal = ""
	agent.StartBlocked = false
	agent.StartBackoffUntil = time.Time{}
}

// startFailureBackoffRemainingLocked reports how long the automatic relaunch
// loop must still hold off. Zero means "attempt allowed".
//
// Caller holds m.mu (read side is enough).
func (m *Manager) startFailureBackoffRemainingLocked(agent *AgentProcess, now time.Time) time.Duration {
	if agent == nil || agent.StartBackoffUntil.IsZero() || !now.Before(agent.StartBackoffUntil) {
		return 0
	}
	return agent.StartBackoffUntil.Sub(now)
}

// StartFailureState reports an agent's current start-failure record for callers
// outside the package (the dashboard status builder, the heartbeat). blocked is
// the fleet-visible bit: it means the agent has failed the same way
// startFailureBlockThreshold times and is no longer being relaunched on the
// tick.
type StartFailureSnapshot struct {
	Reason       string
	Count        int
	LastAt       time.Time
	LastExitCode *int
	LastSignal   string
	Blocked      bool
}

func (m *Manager) StartFailureState(name string) (StartFailureSnapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent, found := m.agents[name]
	if !found || agent == nil {
		return StartFailureSnapshot{}, false
	}
	return StartFailureSnapshot{
		Reason:       agent.StartFailureReason,
		Count:        agent.StartFailureCount,
		LastAt:       agent.StartFailureLastAt,
		LastExitCode: agent.StartFailureExitCode,
		LastSignal:   agent.StartFailureSignal,
		Blocked:      agent.StartBlocked,
	}, true
}

// startBlockedDescription renders the blocked state for an operator-facing
// message: the reason plus how many identical failures produced it, so the
// sentence carries its own justification instead of asking the reader to trust
// a bare verdict.
func startBlockedDescription(agent *AgentProcess) string {
	if agent == nil || !agent.StartBlocked {
		return ""
	}
	reason := strings.TrimSpace(agent.StartFailureReason)
	if reason == "" {
		reason = "start failed repeatedly"
	}
	return fmt.Sprintf("%s (%d consecutive failed starts)", reason, agent.StartFailureCount)
}
