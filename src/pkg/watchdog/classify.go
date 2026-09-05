package watchdog

import (
	"strings"
	"time"
)

// PaneClass is the liveness state machine's verdict for one agent pane.
// Every value is an explicit classification — there is no path on which a
// pane the machine cannot understand is reported healthy (unknown ≠ ready).
type PaneClass string

const (
	// ClassReady — the CLI is at its ready signature.
	ClassReady PaneClass = "ready"
	// ClassShellPrompt — the CLI died and the pane sits at a bare shell.
	ClassShellPrompt PaneClass = "shell-prompt"
	// ClassAuthRequired — the pane shows a login/credential screen. Restart
	// does not help; this is an Authenticated=false condition, not a kick.
	ClassAuthRequired PaneClass = "auth-required"
	// ClassStuckOverlay — a modal (onboarding picker, update prompt) has sat
	// unanswered past the threshold.
	ClassStuckOverlay PaneClass = "stuck-overlay"
	// ClassNoOutput — the pane has been empty/silent past the threshold.
	ClassNoOutput PaneClass = "no-output"
	// ClassNoSession — the tmux session itself is gone.
	ClassNoSession PaneClass = "no-session"
	// ClassUnknown — the pane matched no signature and is inside its grace
	// window; the honest "cannot tell yet" verdict. It never triggers a
	// restart and never reports ready.
	ClassUnknown PaneClass = "unknown"
)

// Dead reports whether this class warrants a restart. auth-required is NOT
// dead: restarting into a dead credential is exactly the 1042-restart loop
// the RFC documents (failure mode 1); it raises Authenticated=false instead.
func (c PaneClass) Dead() bool {
	switch c {
	case ClassShellPrompt, ClassStuckOverlay, ClassNoOutput, ClassNoSession:
		return true
	}
	return false
}

// Observation is one agent's observed truth, gathered by the Fleet (the
// agent manager) using its existing pane-capture and marker machinery.
type Observation struct {
	// Backend is the agent's effective CLI backend (claude, agy, codex, …).
	Backend string
	// SessionExists is whether the agent's tmux session exists at all.
	SessionExists bool
	// Pane is the visible pane content (no scrollback — stale markers in
	// scroll history must not vouch for a dead CLI).
	Pane string
	// HasCLIMarker is the fleet's per-backend ready-signature verdict over
	// Pane (the existing cliPaneMarkers tables in the agent manager).
	HasCLIMarker bool
	// ShowsLoginPrompt is the fleet's login-chrome verdict over Pane (the
	// existing paneShowsLoginPrompt + governor.sensing login patterns).
	ShowsLoginPrompt bool
	// LastChange is when the pane content last changed, per the pane poller.
	LastChange time.Time
	// StartedAt is when this agent's current launch began; zero when the
	// manager has no launch timestamp. A CLI legitimately has no tmux session
	// and no pane for the first seconds of its launch, so every dead verdict
	// is suppressed inside BootGrace of this — restarting a still-booting
	// agent spawns a second concurrent CLI, the race cliBootGraceSeconds was
	// added for on the manager side.
	StartedAt time.Time
	// AuthAvailable/AuthKnown carry the manager's owner-aware per-agent
	// credential-file verdict (AgentAuthState, #4619/#4641): AuthKnown=false
	// means the probe could not determine an answer and the reconciler must
	// report Unknown, never guess.
	AuthAvailable bool
	AuthKnown     bool
	// CredentialProven is the fleet's POSITIVE-EVIDENCE-ONLY credential probe
	// (AgentHasValidCredential, #5291): true means this agent's backend is
	// demonstrably able to authenticate without a human. False means "no
	// proof" — never "logged out" — so it can only ever soften a verdict, not
	// invent one.
	//
	// It is a separate field from AuthAvailable rather than a refinement of it
	// because AuthAvailable comes from AgentAuthState, whose precedence is
	// built for the dashboard badge: a pane showing login chrome outranks the
	// credential file there. That is the exact evidence this reconciler must
	// not trust on its own when deciding whether to page a human.
	CredentialProven bool
	// ProviderErrorClass/Line carry an active inference-provider block from the
	// agent manager. It is a producing/readiness fault, not a dead pane.
	ProviderErrorClass string
	ProviderErrorLine  string
}

// authScreenPatterns match credential screens the marker tables miss: the
// RFC's observed login-expired chrome and first-run auth pickers. Matched
// case-sensitively against the CLI's own UI chrome, never ordinary English —
// the sensing-pattern lesson (#3959): the pane also contains the issue text
// the agent is READING.
var authScreenPatterns = []string{
	"Login expired",
	"Please run /login",
	"Run /login",
	"/login to authenticate",
	"Sign in to continue",
}

// overlayPatterns match modal first-run/update overlays that block every
// launch until dismissed (RFC failure mode 2: agy's accent/terms picker held
// four agents for hours while the dashboard said running).
var overlayPatterns = []string{
	"[Next]",
	"↑/↓ Navigate",
	"↑/↓ to navigate",
	"Press Enter to continue",
	"accept the terms",
	"Choose an accent",
	"update available",
	"Update available",
}

// shellPromptSuffixes are bare-shell prompt tails. Checked only on the last
// non-empty pane line and only when no CLI marker matched, so a CLI that
// happens to print "$" mid-output is never misread as a dead shell.
var shellPromptSuffixes = []string{"$", "#", "%"}

func paneMatchesAny(pane string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(pane, p) {
			return true
		}
	}
	return false
}

// looksLikeShellPrompt reports whether the last non-empty line of the pane is
// a bare shell prompt (short line ending in a prompt character).
func looksLikeShellPrompt(pane string) bool {
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		// A real CLI's output lines are long; a bash prompt is short
		// ("user@host:~$", "bash-5.2$", "#"). The length bound keeps prose
		// that merely ends in "$" from reading as a prompt.
		const maxShellPromptLen = 64
		if len(line) > maxShellPromptLen {
			return false
		}
		for _, suffix := range shellPromptSuffixes {
			if strings.HasSuffix(line, suffix) {
				return true
			}
		}
		return false
	}
	return false
}

// paneIsBlank reports whether the pane has no visible content.
func paneIsBlank(pane string) bool {
	return strings.TrimSpace(pane) == ""
}

// Classification is the liveness verdict plus the evidence behind it.
type Classification struct {
	Class  PaneClass
	Reason string
}

// Classify runs the per-pane liveness state machine of RFC #4665. Precedence,
// most-specific first:
//
//  1. no tmux session            → no-session (dead)
//  2. login/credential chrome    → auth-required (credential failure, NOT dead)
//  3. CLI marker + stale overlay → stuck-overlay (dead) / else ready
//  4. blank pane past grace      → no-output (dead)
//  5. bare shell past grace      → shell-prompt (dead)
//  6. anything else              → unknown (grace window / unclassifiable)
//
// Auth outranks overlay deliberately: a login picker is both, and a restart
// cannot mint a credential.
func Classify(obs Observation, now time.Time, s Settings) Classification {
	// Boot grace outranks every dead verdict. A launching agent legitimately
	// has no tmux session, no pane, and no CLI marker for the first seconds of
	// its life; without this, the watchdog would restart it underneath itself
	// and spawn a second concurrent CLI. An agent with no StartedAt cannot be
	// dated, so it gets no grace rather than an unbounded one.
	booting := !obs.StartedAt.IsZero() && now.Sub(obs.StartedAt) < s.BootGrace

	if !obs.SessionExists {
		if booting {
			return Classification{ClassUnknown, "tmux session missing inside boot grace"}
		}
		return Classification{ClassNoSession, "tmux session missing"}
	}
	if obs.ShowsLoginPrompt || paneMatchesAny(obs.Pane, authScreenPatterns) {
		return Classification{ClassAuthRequired, "pane shows a login/credential screen"}
	}
	stale := func(after time.Duration) bool {
		// A still-booting agent is never stale: its pane has not had time to
		// paint, so an unchanged pane is expected rather than evidence of death.
		if booting {
			return false
		}
		// A zero LastChange means the pane poller has never seen a change;
		// treat as "no evidence of freshness" only once the grace elapses
		// from... nothing — we cannot date it, so it is NOT stale. Honest
		// unknown beats a fabricated timestamp.
		if obs.LastChange.IsZero() {
			return false
		}
		return now.Sub(obs.LastChange) >= after
	}
	if obs.HasCLIMarker {
		if paneMatchesAny(obs.Pane, overlayPatterns) && stale(s.StuckOverlayAfter) {
			return Classification{ClassStuckOverlay, "modal overlay unanswered past " + s.StuckOverlayAfter.String()}
		}
		return Classification{ClassReady, "CLI ready signature present"}
	}
	// No CLI marker: an overlay can also hide every marker (agy's first-run
	// picker paints the whole pane).
	if paneMatchesAny(obs.Pane, overlayPatterns) {
		if stale(s.StuckOverlayAfter) {
			return Classification{ClassStuckOverlay, "modal overlay unanswered past " + s.StuckOverlayAfter.String()}
		}
		return Classification{ClassUnknown, "modal overlay inside grace window"}
	}
	if paneIsBlank(obs.Pane) {
		if stale(s.ShellPromptAfter) {
			return Classification{ClassNoOutput, "pane empty past " + s.ShellPromptAfter.String()}
		}
		return Classification{ClassUnknown, "pane empty inside grace window"}
	}
	if looksLikeShellPrompt(obs.Pane) {
		if stale(s.ShellPromptAfter) {
			return Classification{ClassShellPrompt, "bare shell prompt past " + s.ShellPromptAfter.String()}
		}
		return Classification{ClassUnknown, "shell prompt inside grace window"}
	}
	return Classification{ClassUnknown, "pane matched no known signature"}
}
