package agent

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/agent/panesignal"
	"github.com/hivecommons/hive/crux/config"
)

// toolSummaryRe matches Claude Code's collapsed tool-activity summary lines,
// rendered only when tools actually executed (verified against Claude Code
// v2.1.204): "Running 1 shell command…" while a Bash call is in flight, and
// "Ran 1 shell command" / "Read 1 file, ran 2 shell commands" once done.
// Edit/write variants are included for completeness across versions.
var toolSummaryRe = regexp.MustCompile(`(?i)\b(?:ran|running) \d+ shell command|\bread \d+ file|\bedited \d+ file|\bwrote \d+ file|\bupdated \d+ file`)

func paneShowsBusyFooter(pane string) bool {
	return panesignal.ShowsBusyFooter(pane)
}

// expandedToolCallMarkers are literal fragments of Claude Code's expanded
// per-tool rendering. "⎿" is the result elbow drawn under a tool call
// (verified live on v2.1.204: "⎿  $ sleep 15 && echo probe3"); the
// "⏺ Name(" forms are the expanded tool-call headers older CLI versions
// render. A bare "⏺" is NOT a tool marker — v2.1.204 uses it as the bullet
// for every assistant response block, including pure prose.
var expandedToolCallMarkers = []string{
	"⎿",
	"⏺ Bash(",
	"⏺ Read(",
	"⏺ Write(",
	"⏺ Edit(",
	"⏺ Update(",
	"⏺ Search(",
	"⏺ Fetch(",
	"⏺ Task(",
}

// countToolMarkers counts tool-execution markers in captured pane content.
// The no-action watchdog compares the count after a kick against the count
// recorded at kick delivery: scrollback keeps markers from work done before
// the kick, so only an increase proves the model executed tools since.
//
// EXPLAIN lines are stripped first (#3887). toolSummaryRe matches ENGLISH
// PHRASES — "read 3 files", "running 2 shell commands" — because that is how
// the CLI renders its own collapsed tool summaries. An agent in explain mode is
// asked to state, in plain English, what it is about to do, so it writes
// exactly those phrases as narration:
//
//	EXPLAIN: reading 3 files under pkg/agent to find the kick handler.
//
// That counts as a tool marker, the watchdog concludes "real tool activity
// since the kick", and the prose-only action nudge never fires — on precisely
// the agents an operator turned explanation ON to debug, which is the worst
// possible time to silently lose the check. Narration about a tool is not tool
// execution, so an explanation line contributes nothing to the count.
//
// The whole line is dropped rather than just the prefix: a "⎿" or "⏺ Bash("
// quoted INSIDE an explanation is the agent describing a tool call, not making
// one.
func countToolMarkers(pane string) int {
	pane = stripExplainLines(pane)
	n := len(toolSummaryRe.FindAllStringIndex(pane, -1))
	for _, marker := range expandedToolCallMarkers {
		n += strings.Count(pane, marker)
	}
	return n
}

// stripExplainLines removes the agent's explanation lines from captured pane
// content, so pane analysis sees only what the agent actually did.
//
// Matching is on the TRIMMED line, mirroring the read-time filter in
// pkg/dashboard (filterExplainLines): the CLI indents assistant output, so
// anchoring at column 0 would miss every real line. Returns the input unchanged
// when nothing matches, which is every pane on a hive with explain mode off.
func stripExplainLines(pane string) string {
	if !strings.Contains(pane, config.ExplainLinePrefix) {
		return pane
	}
	lines := strings.Split(pane, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), config.ExplainLinePrefix) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// paneShowsActiveWork reports whether the CLI is mid-response: either a busy
// footer action (esc interrupt / esc to interrupt / esc stop agents / esc to
// cancel), the live spinner counter, or an OMP work label is visible. The idle
// input prompt "❯" alone proves nothing on v2.1.204 — the input box stays
// rendered while a response streams. Callers must pass the VISIBLE pane only:
// a completed turn's busy footer can linger in scrollback.
//
// OMP renders neither marker (verified live, omp 18.1.16/18.1.17): a tool
// call shows "⏺ Running… (esc to cancel)" and plain generation shows a
// spinner glyph plus "Working…", so an in-flight OMP turn read as idle
// without these two additions.
func paneShowsActiveWork(pane string) bool {
	return paneShowsBusyFooter(pane) ||
		strings.Contains(pane, cliWorkingMarker) ||
		strings.Contains(pane, cliActiveCounterMarker) ||
		strings.Contains(pane, "Working…") ||
		strings.Contains(pane, "Running…")
}

// paneShowsEmptyInputPrompt reports whether the CLI is sitting at an idle,
// empty input prompt.
//
// Trailing chrome is skipped before the test so that both CLI shapes are
// handled: Claude Code ends at "❯", while the Copilot CLI draws a rule and a
// hint footer underneath it. A prompt with typed text ("❯ do the thing") still
// fails the equality check, which is the point — that is not an idle prompt.
func paneShowsEmptyInputPrompt(pane string) bool {
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if lineIsCLIChrome(line) {
			continue
		}
		return line == cliInputPromptMarker
	}
	return false
}

// paneContentHash returns a short stable hash of pane content, used to detect
// whether a pane has changed since a kick was delivered.
func paneContentHash(pane string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(pane))
	return fmt.Sprintf("%016x", h.Sum64())
}

func paneAfterKickBaseline(pane, baseline string) string {
	if baseline == "" {
		return pane
	}
	if idx := strings.LastIndex(pane, baseline); idx >= 0 {
		return pane[idx+len(baseline):]
	}
	paneLines := strings.Split(pane, "\n")
	baseLines := strings.Split(baseline, "\n")
	common := 0
	for common < len(paneLines) && common < len(baseLines) && paneLines[common] == baseLines[common] {
		common++
	}
	if common > 0 {
		return strings.Join(paneLines[common:], "\n")
	}
	return pane
}

// scrollbackAnchorLines is how many non-blank lines identify the kick-time
// position in the scrollback capture.
const scrollbackAnchorLines = 5

// scrollbackAnchor returns the block of capture lines just above the visible
// screen that spans its last few non-blank lines. The screen is redrawn
// (input box, footer, spinner), but lines already in scrollback never change,
// so they mark where post-kick output begins even after the bounded capture
// window slides (#20214). The block is a contiguous slice of the capture,
// blank lines between output blocks included, so it is found verbatim by
// toolMarkersAfterAnchor. Returns "" when the capture holds no more than one
// screen or the scrollback is all blank.
func scrollbackAnchor(capture, visible string) string {
	lines := strings.Split(strings.TrimRight(capture, "\n"), "\n")
	screen := len(strings.Split(strings.TrimRight(visible, "\n"), "\n"))
	if visible == "" || len(lines) <= screen {
		return ""
	}
	lines = lines[:len(lines)-screen]
	end, start, found := -1, -1, 0
	for i := len(lines) - 1; i >= 0 && found < scrollbackAnchorLines; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if end < 0 {
			end = i
		}
		start = i
		found++
	}
	if end < 0 {
		return ""
	}
	return strings.Join(lines[start:end+1], "\n")
}

// toolMarkersAfterAnchor counts tool markers in capture after the first
// occurrence of anchor (the whole capture when anchor is ""). ok is false
// when the anchor is no longer in the capture — it scrolled out of the
// window or the pane was cleared — so no count is comparable. The first
// occurrence is used so a repeated block can only widen the counted region.
func toolMarkersAfterAnchor(capture, anchor string) (marks int, ok bool) {
	if anchor == "" {
		return countToolMarkers(capture), true
	}
	idx := strings.Index(capture, anchor)
	if idx < 0 {
		return 0, false
	}
	return countToolMarkers(capture[idx+len(anchor):]), true
}

// kickTurnLength is how long the kicked turn was seen busy: from kick
// delivery to the last governor pass that found the CLI mid-response. It is a
// lower bound that, unlike the time since the kick, does not grow while an
// already-finished turn waits for the next tick. Caller must hold m.mu.
func kickTurnLength(agent *AgentProcess) time.Duration {
	if agent.lastInferKickBusyAt.IsZero() {
		return 0
	}
	return agent.lastInferKickBusyAt.Sub(agent.lastInferKickAt)
}
