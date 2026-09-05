// Pane-content classification: CLI markers, blocking/login/quota/
// transient-error/network-error prompt detection, input-prompt and
// CLI-ready predicates, and pane output filtering/deduplication.
package agent

import (
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/watchdog"
)

// cliPaneMarkers are strings that appear in a tmux pane when a CLI (claude,
// copilot, gemini, goose, aider) is running. A bare bash prompt has none of
// these. Checking pane content is more reliable than inspecting /proc/comm
// because CLIs may run as node, python, or other interpreters whose process
// name doesn't match the CLI binary.
var cliPaneMarkers = []string{
	"❯",
	"esc cancel",
	"/ commands",
	"? help",
	"Claude",
	"Copilot",
	"Gemini",
	"goose",
	// pi's marker. pi renders a TUI status bar showing model context usage
	// (e.g. "↑37k ↓20k R756k CH99.6% $0.013 5.9%/1.0M (auto)") instead of a
	// "❯"/"goose is ready" prompt, so none of the entries above match a
	// running pi: waitForCLIReadyForAgent would never see it as ready and the
	// startup kick would be dropped after cliReadyTimeout even though pi is
	// healthy. "%/" matches the fixed "%%/1.0M" context-meter suffix pi
	// renders at every context size.
	piContextMarker,
	// bob's markers. NONE of the entries above match a running bob: verified
	// against the installed bundle (bobshell 1.0.6 bundle/bob.js), which
	// contains zero "❯" characters and no "esc cancel" / "/ commands" /
	// "? help" / "goose" strings. ("Claude"/"Gemini" occur only as model-name
	// data, never as UI chrome.) Without these two entries
	// waitForCLIReadyForAgent can never see a booted bob, so its startup kick
	// would be dropped after cliReadyTimeout even though bob is healthy.
	bobInputPlaceholder,
	bobInputPlaceholderDefault,
	bobProductMarker,
	// codex's markers. Codex 0.144.1's TUI renders NONE of the entries above:
	// verified live (daviddiaz "Visual Hive", the hub-reachable cluster) — an idle codex pane
	// contained no "❯", "goose", "Claude"/"Gemini" chrome, or bob strings, only
	// the "›" (U+203A) input caret and the "OpenAI Codex" banner. Without these
	// two entries waitForCLIReadyForAgent can never see a booted codex, so its
	// kick is dropped after cliReadyTimeout even though codex is healthy.
	codexInputPromptMarker,
	codexProductMarker,
}

const (
	// bobInputPlaceholder is the placeholder bob renders inside its input box
	// when it is idle and accepting input. This is bob's equivalent of the "❯"
	// prompt for the other TUIs and is the PRIMARY readiness signal: the
	// bundle renders it on the same component whose presence is gated by
	// `isInputActive`, so seeing it means the input is live, not merely
	// painted. Copied verbatim from bobshell 1.0.6 — see TestBobPaneMarkers.
	bobInputPlaceholder = "Type your message or @path/to/file"
	// bobInputPlaceholderDefault is the OTHER placeholder bob renders in that
	// same input box. The two are alternatives chosen by editor mode, not
	// versions: the bundle picks bobInputPlaceholder only when vim-style
	// modal editing is on, and this string in every other case — which is the
	// default, so it is what a stock bob actually shows when idle and ready.
	//
	// Verified live on bobshell 1.0.6: a healthy authenticated bob pane
	// contained this string and ZERO occurrences of bobInputPlaceholder, so
	// waitForCLIReadyForAgent never saw it as ready and every governor kick
	// was dropped with "CLI did not become ready after restart" while bob sat
	// perfectly healthy at its prompt. Both strings are present in the 1.0.6
	// bundle, so match either rather than replacing one with the other.
	bobInputPlaceholderDefault = "Enter your prompt, / for commands"
	// bobProductMarker is bob's product name, which appears in its banner and
	// dialogs. It is a weaker, secondary signal than bobInputPlaceholder — it
	// also shows on trust/auth/license dialogs, which are NOT ready states —
	// so it is used only for coarse CLI-presence detection (is anything other
	// than bash in this pane?), never as the input-ready gate.
	bobProductMarker = "Bob-Shell"
	// piContextMarker is pi's context-meter suffix ("5.9%/1.0M (auto)" in
	// its TUI status bar). It is the PRIMARY readiness signal for a running
	// pi: the status bar renders only when the agent TUI is live and has a
	// model configured, and it is the only marker pi renders in common with
	// no other CLI (pi never shows "❯"/"goose is ready"). Matching on "%/"
	// rather than a context size keeps it valid at any model/context
	// configuration.
	piContextMarker = "%/"
)

// paneHasCLIMarker reports whether the given pane content contains any known
// CLI UI marker.
func paneHasCLIMarker(output string) bool {
	if output == "" {
		return false
	}
	for _, marker := range cliPaneMarkers {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

// blockingPrompt is a startup-blocking modal that must be answered with a
// SPECIFIC numbered option rather than a bare Enter or a generic
// navigate-away-from-"No" heuristic.
//
// The generic heuristic in dismissInferencePrompts steers away from options
// containing "no"/"exit" and otherwise confirms whatever is selected. That is
// wrong for menus whose DEFAULT selection is affirmative but harmful — most
// notably codex's update prompt, where the pre-selected option shells out to
// `npm install -g`. Answering those needs the exact key, so each entry names
// the one prompt it answers and nothing else is ever blind-fired at.
//
// This mirrors blockingPromptKey() in bin/contributor-relay.sh, which solved
// the same problem contributor-side. The hub had no equivalent.
type blockingPrompt struct {
	// backend this prompt belongs to. Prompts are matched only against the
	// backend that actually renders them, so a codex pattern can never fire at
	// a claude pane that happens to contain similar words.
	backend string
	// match reports whether this prompt is the one on screen. All conditions
	// must hold, so a prompt is only answered when positively identified.
	match func(pane string) bool
	key   string // the option to type before Enter
	label string // for the audit log
}

var blockingPrompts = []blockingPrompt{
	{
		backend: "copilot",
		// Copilot: "Confirm folder trust" → 1. Yes (THIS SESSION ONLY).
		//
		// Deliberately NOT "2. Yes, and remember": remembering makes the CLI
		// rewrite the SHARED ~/.copilot/config.json from its own in-memory
		// snapshot, which stomps every other agent's state in that file — traced
		// live on hivecommons/hive (2026-08-22): each agent's "remember" wiped
		// the others' trustedFolders entries, and one stale rewrite resurrected
		// a dead token over the operator's fresh login, which is why re-logins
		// never stuck. Session-only trust writes NOTHING; the watcher now runs
		// for the agent's whole lifetime, so every (re)launch gets re-answered
		// and persistence is unnecessary.
		match: func(p string) bool {
			return strings.Contains(p, "Confirm folder trust") || strings.Contains(p, "Do you trust the files")
		},
		key:   "1",
		label: "copilot folder trust",
	},
	{
		backend: "agy",
		// agy (Antigravity CLI): "Do you trust the contents of this project?"
		// An arrow-key menu whose affirmative option is ALREADY selected, so a
		// bare Enter is correct and there is no numbered option to type. It
		// blocks startup exactly like the codex/copilot trust dialogs.
		match: func(p string) bool {
			return strings.Contains(p, "Do you trust the contents of this project")
		},
		key:   "",
		label: "agy project trust",
	},
	{
		backend: "codex",
		// codex: "Do you trust the contents of this directory?" → 1. Yes, continue.
		match: func(p string) bool {
			return strings.Contains(p, "Do you trust the contents of this directory")
		},
		key:   "1",
		label: "codex directory trust",
	},
	{
		backend: "codex",
		// codex: "✨ Update available! x -> y" → 3. Skip until next version.
		//
		// Deliberately NOT "1. Update now", which is the PRE-SELECTED option:
		// it runs `npm install -g @openai/codex` as the unprivileged agent UID,
		// which fails with EACCES and takes the CLI down with it — on every
		// launch, indefinitely, until a human intervenes. Even where it could
		// succeed it is slow, needs network, can fail half-way, and drifts the
		// CLI version out from under the image.
		//
		// "Skip until next version" is chosen over a plain "Skip" because it
		// persists: a plain Skip re-prompts on the very next launch.
		match: func(p string) bool {
			return strings.Contains(p, "Update available!") && strings.Contains(p, "Skip until next version")
		},
		key:   "3",
		label: codexUpdatePromptLabel,
	},
}

// blockingPromptTailLines bounds how much of the pane a prompt may be matched
// in. captureTmuxPaneForAgent returns SCROLLBACK, not just the visible screen,
// so matching the whole capture answers prompts that have long since scrolled
// away: after a codex CLI died, its update menu stayed in history and the
// watcher typed "3" into the bash shell that replaced it, once every poll,
// forever. A live modal is always at the bottom of the pane, so only the tail
// is eligible.
const blockingPromptTailLines = 25

// paneTail returns the last n non-blank lines of a captured pane.
func paneTail(pane string, n int) string {
	lines := strings.Split(pane, "\n")
	kept := make([]string, 0, n)
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		kept = append(kept, lines[i])
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.Join(kept, "\n")
}

// blockingPromptKey returns the keystroke that dismisses whatever known
// startup-blocking modal this backend currently has on screen, and whether one
// was recognised. Only the tail of the pane is considered — see
// blockingPromptTailLines.
func blockingPromptKey(backend, pane string) (key, label string, ok bool) {
	tail := paneTail(pane, blockingPromptTailLines)
	for _, p := range blockingPrompts {
		if p.backend == backend && p.match(tail) {
			return p.key, p.label, true
		}
	}
	return "", "", false
}

// PaneShowsBlockingPrompt reports whether the pane is currently sitting on a
// known startup-blocking modal (folder trust, codex update, …) for the given
// backend. The login-detector uses it to stand down: a trust-wedged pane is
// NOT a login problem, and pausing the agent for it kills the very watcher
// that would answer the prompt — the deadlock that kept hivecommons/hive's
// copilot agents "sitting at login prompt" through every re-login (2026-08-22).
func PaneShowsBlockingPrompt(backend, pane string) bool {
	_, _, ok := blockingPromptKey(backend, pane)
	return ok
}

// paneHasBlockingPrompt reports whether ANY known blocking prompt is on the
// pane, regardless of backend. Used by the readiness gate, which does not know
// the backend; the patterns are specific enough that a false positive only
// delays readiness rather than mis-firing a keystroke.
func paneHasBlockingPrompt(pane string) bool {
	tail := paneTail(pane, blockingPromptTailLines)
	for _, p := range blockingPrompts {
		if p.match(tail) {
			return true
		}
	}
	return false
}

// backendHasBlockingPrompts reports whether any known startup-blocking prompt
// belongs to this backend, and so whether the watcher is worth running for it.
//
// Derived from the table rather than hardcoded: the watcher used to be gated on
// `backend == "copilot"`, which is why codex agents were never rescued from
// their update menu even after the menu itself was understood. Adding a prompt
// for a new backend now enables the watcher for it automatically.
func backendHasBlockingPrompts(backend string) bool {
	for _, p := range blockingPrompts {
		if p.backend == backend {
			return true
		}
	}
	return false
}

// paneShowsInputPrompt reports whether the pane content shows a CLI input
// prompt that is ready to accept a kick.
//
// The first four markers are the pre-existing set, preserved verbatim so
// claude/copilot/gemini/goose readiness is bit-for-bit unchanged. The bob
// placeholder is additive: bob's TUI renders none of the other four (verified
// against bobshell 1.0.6 — the bundle contains no "❯" at all), so without it a
// healthy bob never registers as ready and its startup kick is dropped.
//
// The codex caret (U+203A "›") is likewise additive: Codex 0.144.1's TUI
// renders none of the markers above (verified live — its idle pane contains no
// "❯" at all), so without it a healthy codex pane never registers as ready and
// its kick is dropped with "did not reach input prompt".
//
// Callers pass captured pane text; empty input is not a prompt.
func paneShowsInputPrompt(output string) bool {
	if output == "" {
		return false
	}
	return strings.Contains(output, "❯") ||
		strings.Contains(output, "goose is ready") ||
		strings.Contains(output, "> Enter to send") ||
		strings.Contains(output, "\n>\n") ||
		strings.Contains(output, bobInputPlaceholder) ||
		strings.Contains(output, bobInputPlaceholderDefault) ||
		strings.Contains(output, codexInputPromptMarker) ||
		strings.Contains(output, piContextMarker)
}

func (a *AgentProcess) snapshot() AgentProcess {
	history := make([]KickRecord, len(a.KickHistory))
	copy(history, a.KickHistory)
	a.paneMu.RLock()
	pane := make([]string, len(a.lastPaneCapture))
	copy(pane, a.lastPaneCapture)
	// NeedsLogin and LastPaneChange are written by the pane poller under paneMu.
	needsLogin := a.NeedsLogin
	quotaExhausted := a.QuotaExhausted
	lastPaneChange := a.LastPaneChange
	conds := make([]watchdog.Condition, len(a.WatchdogConditions))
	copy(conds, a.WatchdogConditions)
	a.paneMu.RUnlock()
	return AgentProcess{
		Name:                      a.Name,
		ID:                        a.ID,
		Config:                    a.Config,
		State:                     a.State,
		PID:                       a.PID,
		UID:                       a.UID,
		StartedAt:                 a.StartedAt,
		LastKick:                  a.LastKick,
		Paused:                    a.Paused,
		PausedAt:                  a.PausedAt,
		PausedReason:              a.PausedReason,
		PausedTrigger:             a.PausedTrigger,
		PausedBy:                  a.PausedBy,
		PinnedCLI:                 a.PinnedCLI,
		PinnedModel:               a.PinnedModel,
		ModelOverride:             a.ModelOverride,
		BackendOverride:           a.BackendOverride,
		RestartCount:              a.RestartCount,
		RestartEvents:             cloneRestartEvents(a.RestartEvents),
		LastRestartReason:         a.LastRestartReason,
		TurnLoss:                  cloneTurnLoss(a.TurnLoss),
		KickHistory:               history,
		LastKickMessage:           a.LastKickMessage,
		NeedsLogin:                needsLogin,
		QuotaExhausted:            quotaExhausted,
		LastPaneChange:            lastPaneChange,
		WatchdogConditions:        conds,
		StallNudges:               a.StallNudges,
		ActionNudges:              a.ActionNudges,
		TransientNudges:           a.TransientNudges,
		ProviderErrorClass:        a.ProviderErrorClass,
		ProviderErrorLine:         a.ProviderErrorLine,
		ProviderErrorBackoffUntil: a.ProviderErrorBackoffUntil,
		StartFailureClass:         a.StartFailureClass,
		StartFailureReason:        a.StartFailureReason,
		StartFailureCount:         a.StartFailureCount,
		StartBlocked:              a.StartBlocked,
		StartBackoffUntil:         a.StartBackoffUntil,
		HasLaunched:               a.HasLaunched,
		LaunchedMode:              a.LaunchedMode,
		tmuxSession:               a.tmuxSession,
		tmuxSocket:                a.tmuxSocket,
		OutputBuffer:              a.OutputBuffer,
		lastPaneCapture:           pane,
	}
}

// PaneLines returns the last n lines from the most recent tmux pane capture,
// preferring content from the current CLI session (after the last ❯ prompt).
// Falls back to showing the full tail if the current session has too few lines.
func (a *AgentProcess) PaneLines(n int) []string {
	a.paneMu.RLock()
	defer a.paneMu.RUnlock()
	if len(a.lastPaneCapture) == 0 {
		return nil
	}
	return filterPaneOutput(a.lastPaneCapture, n)
}

func isVisualNoise(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	if strings.Trim(t, "─━") == "" {
		return true
	}
	if strings.HasPrefix(t, "/data/agents/") && !strings.Contains(t, " ") {
		return true
	}
	return false
}

func isCLIChrome(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	if strings.HasPrefix(t, "/ commands") ||
		strings.HasPrefix(t, "? help") ||
		strings.HasPrefix(t, "@ files") ||
		strings.HasPrefix(t, "# issues") {
		return true
	}
	// Copilot/Claude/Gemini status bar: contains "esc cancel" or model name
	if strings.Contains(t, "esc cancel") {
		return true
	}
	// Model name in status bar (short line with model identifier)
	if (strings.Contains(t, "Claude ") && !strings.Contains(t, "Claude Code")) ||
		strings.Contains(t, "Copilot v") ||
		strings.Contains(t, "Gemini ") {
		// Only match if it looks like a status bar (has spinner or command hints)
		for _, prefix := range []string{"◎", "◉", "●", "○", "◐", "◑", "◒", "◓"} {
			if strings.Contains(t, prefix) {
				return true
			}
		}
	}
	return false
}

func isBufferNoise(s string) bool {
	if isCLIChrome(s) || isVisualNoise(s) {
		return true
	}
	t := strings.TrimSpace(s)
	if t == "❯" || t == "›" || t == ">" {
		return true
	}
	for _, banner := range []string{"╭─╮", "╰─╯", "█ ▘▝ █", "▔▔▔▔", "Copilot v", "Check for mistakes"} {
		if strings.Contains(t, banner) {
			return true
		}
	}
	if strings.HasPrefix(t, "● Tip:") || strings.HasPrefix(t, "└ ") || strings.HasPrefix(t, "↑/↓ to navigate") {
		return true
	}
	if strings.Contains(t, "copilot-instructions.md") && strings.Contains(t, "/init") {
		return true
	}
	if strings.Contains(t, "Do you trust the files in this folder") {
		return true
	}
	if strings.HasPrefix(t, "› ") && (strings.Contains(t, "Yes") || strings.Contains(t, "No (Esc)")) {
		return true
	}
	if strings.HasPrefix(t, "●") && strings.Contains(t, "Folder") && strings.Contains(t, "trusted") {
		return true
	}
	if strings.HasPrefix(t, "✗ Model") && strings.Contains(t, "not available") {
		return true
	}
	return false
}

func filterPaneOutput(lines []string, n int) []string {
	lastPrompt := -1
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "❯" || trimmed == "›" || trimmed == ">" {
			lastPrompt = i
			break
		}
	}
	if lastPrompt >= 0 && lastPrompt < len(lines)-1 {
		afterPrompt := lines[lastPrompt+1:]
		hasContent := false
		for _, l := range afterPrompt {
			if !isCLIChrome(l) && !isVisualNoise(l) {
				hasContent = true
				break
			}
		}
		if hasContent {
			lines = afterPrompt
		} else {
			lines = lines[:lastPrompt]
		}
	}
	var cleaned []string
	for _, l := range lines {
		if !isVisualNoise(l) {
			cleaned = append(cleaned, l)
		}
	}
	lines = cleaned
	lines = DeduplicateBlocks(lines)
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, len(lines))
	copy(out, lines)
	return out
}

// DeduplicateBlocks removes repeated blocks from pane output.
// It finds the longest suffix that also appears earlier and removes the earlier copy.
func DeduplicateBlocks(lines []string) []string {
	if len(lines) < 4 {
		return lines
	}
	// Try block sizes from half the total down to 2 lines.
	maxBlock := len(lines) / 2
	for blockSize := maxBlock; blockSize >= 2; blockSize-- {
		// Extract the last blockSize lines as the candidate block.
		candidate := lines[len(lines)-blockSize:]
		// Scan backwards for an earlier occurrence.
		for start := len(lines) - blockSize - 1; start >= 0; start-- {
			if start+blockSize > len(lines)-blockSize {
				continue
			}
			match := true
			for j := 0; j < blockSize; j++ {
				if normalizeLine(lines[start+j]) != normalizeLine(candidate[j]) {
					match = false
					break
				}
			}
			if match {
				// Remove the earlier duplicate block.
				result := make([]string, 0, len(lines)-blockSize)
				result = append(result, lines[:start]...)
				result = append(result, lines[start+blockSize:]...)
				return DeduplicateBlocks(result)
			}
		}
	}
	return lines
}

func (a *AgentProcess) FilteredPaneLines(n int) []string {
	a.paneMu.RLock()
	defer a.paneMu.RUnlock()
	if len(a.lastPaneCapture) == 0 {
		return nil
	}
	return filterPaneOutput(a.lastPaneCapture, n)
}

// loginPromptPatterns are substrings that indicate an agent is stuck on a
// login/authentication screen (Copilot text prompts, Claude Code OAuth flow,
// GitHub device flow). Each must be distinctive enough to never appear in
// ordinary agent output.
var loginPromptPatterns = []string{
	// A BARE "/login" is deliberately NOT here — it is handled separately by
	// lineHasLoginDirective below. "/login" alone is a substring of ordinary
	// agent output (an agent reviewing an auth route writes "POST /login"; a
	// CLI printing its slash-command list renders "/login" beside "/help"), and
	// matching it painted the 🔑 badge on agents that were authenticated and
	// mid-work.
	"sign in to use",
	"Sign in to use",
	"authenticate to use",
	"Authenticate to use",
	"log in to use",
	"Log in to use",
	// Claude Code OAuth sign-in screen
	"Use the url below to sign in",
	"Paste code here if prompted",
	"Select login method",
	"/cai/oauth/authorize",
	// GitHub device-flow screen (Copilot CLI)
	"Enter one-time code",
	"github.com/login/device",
	// Antigravity OAuth hand-off
	"accounts.google.com/o/oauth2/auth",
	"If you aren't automatically redirected, paste the authorization code below:",
}

// fatalNetworkErrorPatterns are substrings that indicate a transient TLS or
// network failure killed the agent at startup. These errors leave the Copilot
// chrome visible (❯, / commands) so paneShowsCLIReady returns true, but the
// agent is dead and will never recover without a restart.
var fatalNetworkErrorPatterns = []string{
	"invalid peer certificate",
	"BadSignature",
	"fetch failed",
}

// paneShowsFatalNetworkError returns true if any line contains a fatal
// TLS/network error pattern that requires an agent restart.
func paneShowsFatalNetworkError(lines []string) bool {
	for _, line := range lines {
		for _, pat := range fatalNetworkErrorPatterns {
			if strings.Contains(line, pat) {
				return true
			}
		}
	}
	return false
}

// transientAPIErrorPatterns are substrings of API failures that a plain retry
// fixes (#4697). They are the OPPOSITE of fatalNetworkErrorPatterns above: the
// CLI survives, drops back to its idle prompt with the response truncated, and
// stays there until the next scheduled kick — which can be hours away. The
// session is alive with full context, so the remedy is a nudge, not a restart.
//
// Membership is deliberately narrow. Every pattern here must be an error where
// REPEATING THE SAME REQUEST CAN SUCCEED:
//
//   - a dropped/timed-out connection — the request never completed;
//   - 5xx and "overloaded" — the upstream failed this attempt, not this caller.
//
// Errors a retry cannot fix must NOT be listed, because nudging them loops the
// agent against a wall: 403/model-refusal (#4400) and quota exhaustion (#4583)
// are both excluded here AND re-checked at the call site via
// lineShowsUpstreamAuthorizationError / paneShowsQuotaExhausted, since Claude
// Code renders every API failure under the same "API Error:" prefix and a
// substring match alone cannot tell them apart.
var transientAPIErrorPatterns = []string{
	// The shape reported in #4697, observed repeatedly on a claude-backend
	// agent: the response is cut off mid-stream and the CLI returns to ❯.
	"connection lost mid-response",
	"connection error",
	"request timed out",
	"overloaded_error",
}

// 500/502/503/529 are retryable upstream failures; match them as whole tokens
// only, so unrelated request IDs or token counts under the same API-error chrome
// do not trip the watchdog.
var transientAPIErrorStatusRe = regexp.MustCompile(`\b(?:500|502|503|529)\b`)

// paneShowsTransientAPIError reports whether any line carries a retryable API
// failure. Pure over its input so the decision is table-testable without tmux;
// the call site supplies the VISIBLE pane tail rather than scrollback, so an
// error the agent already recovered from does not read as current.
func paneShowsTransientAPIError(lines []string) bool {
	for _, line := range lines {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "api error:") {
			continue
		}
		for _, pat := range transientAPIErrorPatterns {
			if strings.Contains(lower, pat) {
				return true
			}
		}
		if transientAPIErrorStatusRe.MatchString(line) {
			return true
		}
	}
	return false
}

// cliReadyIndicators prove copilot finished startup.
var cliReadyIndicators = []string{
	"❯",
	"/ commands",
	"? help",
	"/login",
	"sign in",
	"Sign in",
	"Copilot v",
	"Tip: /init",
	"Loading:",
	"● Loading",
}

// paneShowsCLIReady returns true if the pane shows any indicator that
// copilot finished initializing (prompt, help text, or login request).
func paneShowsCLIReady(lines []string) bool {
	for _, line := range lines {
		for _, ind := range cliReadyIndicators {
			if strings.Contains(line, ind) {
				return true
			}
		}
	}
	return false
}

// paneShowsLoginPrompt returns true if any line in the pane output matches a
// known login/authentication prompt pattern.
// loginDirectiveVerbs are the imperative words a CLI uses when it is TELLING
// the operator to authenticate ("Please /login to continue", "Run /login",
// "Type /login to sign in"). A line containing "/login" counts as a login
// prompt only when one of these also appears, which is what separates a real
// login screen from an agent discussing an auth route ("POST /login returns
// 302") or a CLI listing its slash commands ("/help  /login  /model").
var loginDirectiveVerbs = []string{
	"please", "run", "type", "use", "enter", "try", "must", "need",
}

// lineHasLoginDirective reports whether a line both mentions "/login" AND
// carries an imperative that makes it a directive to the operator.
func lineHasLoginDirective(line string) bool {
	if !strings.Contains(line, "/login") {
		return false
	}
	lower := strings.ToLower(line)
	for _, verb := range loginDirectiveVerbs {
		if strings.Contains(lower, verb) {
			return true
		}
	}
	return false
}

// modelRefusalPatterns are upstream MODEL-ENTITLEMENT refusals: the caller is
// authenticated, and the model it asked for is not one this account may use.
// Observed verbatim from a LiteLLM gateway in #4400:
//
//	team not allowed to access model. This team can only access
//	models=['gemini-2.5-pro', ..., 'aws/claude-sonnet-4-6', ...]
//
// Kept as text as well as the status check below because not every gateway
// surfaces an HTTP status through the CLI's error line.
var modelRefusalPatterns = []string{
	"not allowed to access model",
	"team not allowed to access",
}

// lineShowsUpstreamAuthorizationError reports whether a line carries an
// upstream failure that LOGGING IN CANNOT FIX (#4400).
//
// The distinction is the HTTP status, and it is not a nicety:
//
//	401  authentication — the caller is not identified. /login is the fix.
//	403  authorization  — the caller IS identified and is not permitted.
//	                      /login changes nothing; the request itself is the
//	                      problem.
//
// Keying on the status rather than on one gateway's wording keeps this working
// for gateways that phrase the refusal differently, and keeps 401 — a genuine
// logged-out signal — detected exactly as before.
func lineShowsUpstreamAuthorizationError(line string) bool {
	if strings.Contains(line, "API Error: 403") {
		return true
	}
	lower := strings.ToLower(line)
	for _, pat := range modelRefusalPatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

var quotaExhaustionPatterns = []string{
	"exceeded your monthly quota",
	"used all your copilot free chat requests",
	"budget_exceeded",
	"budget has been exceeded",
	"provider spending limit reached",
	"refused the request on a spending limit",
	"gone over your budget allowance",
	"bobcoins",
}

var quotaExhaustionStatusPattern = regexp.MustCompile(`\b\d+(?:\.\d+)?/\d+(?:\.\d+)?\s*\(0%\)\s*\|`)

func paneShowsQuotaExhausted(lines []string) bool {
	for _, line := range lines {
		lower := strings.ToLower(line)
		for _, pat := range quotaExhaustionPatterns {
			if strings.Contains(lower, pat) {
				return true
			}
		}
		if quotaExhaustionStatusPattern.MatchString(line) {
			return true
		}
	}
	return false
}

func paneShowsBobAPIKeyRejected(lines []string) bool {
	for _, line := range lines {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "api key") || !strings.Contains(lower, "401") {
			continue
		}
		if strings.Contains(lower, "verification failed") ||
			strings.Contains(lower, "invalid or expired api key") ||
			strings.Contains(lower, `"error":"unauthorized"`) ||
			strings.Contains(lower, `"error": "unauthorized"`) {
			return true
		}
	}
	return false
}

// paneShowsLoginPrompt returns true if any line in the pane output matches a
// known login/authentication prompt pattern.
//
// #4400: a line that ALSO carries an upstream authorization failure is skipped,
// because Claude Code prefixes EVERY API error with its login hint. A hive
// whose gateway refused the configured model rendered
//
//	● Please run /login · API Error: 403 {"...":"team not allowed to access
//	  model. This team can only access models=[... 'aws/claude-sonnet-4-6' ...]"}
//
// on an agent that was fully logged in. That matched "Please run /login", so
// the agent was badged as needing login AND auto-restarted by the poller's
// `showsLogin && configHasTokens()` branch — restarting into the same 403 every
// time, which is what the reporter saw as the agent "keeps crashing". The
// operator was pointed at the one action that could not help, while the real
// cause — a model id the gateway does not entitle — was sitting in the same
// line.
//
// This is the same shape as lineHasLoginDirective's existing guard: that one
// exists so "POST /login returns 302" is not read as a login screen. Claude
// Code's error decoration is the same class of false positive.
func paneShowsLoginPrompt(lines []string) bool {
	for _, line := range lines {
		// An upstream authorization failure is not a login prompt, whatever
		// the CLI decorated it with.
		if lineShowsUpstreamAuthorizationError(line) || paneShowsQuotaExhausted([]string{line}) {
			continue
		}
		if lineHasLoginDirective(line) {
			return true
		}
		for _, pat := range loginPromptPatterns {
			if strings.Contains(line, pat) {
				return true
			}
		}
	}
	return false
}

// codexInputPromptMarker is the caret Codex renders on its input line when it
// is idle and awaiting input. It is Codex's equivalent of claude/gemini's "❯"
// and bob's placeholder — the PRIMARY readiness signal for a codex agent.
//
// It is a SINGLE-ANGLE-QUOTATION-MARK (U+203A "›"), deliberately distinct from
// the "❯" (U+276F) used by the other TUIs and by the consent-screen menu, so it
// never collides with paneShowsConsentScreen's "❯"-selected-line check.
//
// Verified live on Codex 0.144.1 (daviddiaz "Visual Hive", the hub-reachable cluster): an idle
// scanner pane sitting at its prompt rendered this caret with placeholder
// ghost-text ("› Improve documentation in @filename", "› Explain this
// codebase") and contained ZERO "❯", "goose is ready", "> Enter to send", or
// bob placeholders — so without this marker a healthy codex pane never
// registers as ready and every kick is dropped with "did not reach input
// prompt", leaving the advisory issue stale.
//
// Matching the caret alone (not any specific placeholder string) is robust to
// the ghost-text varying between Codex tips/versions, and stays tight: Codex
// shows this idle input caret ONLY while awaiting input — a running turn
// renders streaming output and a working indicator instead, not the "›" caret.
const codexInputPromptMarker = "›"

// codexProductMarker is Codex's product name, rendered in its splash banner
// ("OpenAI Codex (v0.144.1)"). Like bobProductMarker it is a coarse
// CLI-presence signal only (it also shows on the splash before the input caret
// is live), never the input-ready gate — that is codexInputPromptMarker.
const codexProductMarker = "OpenAI Codex"
