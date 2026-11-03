package agent

import (
	"fmt"
	"strings"
	"time"
)

// Blocked-action thrash breaker: an agent that keeps hammering a policy wall
// (e.g. a push with no per-agent token, blocked every ~3s by
// git-credential-hive, or a proxy hard-deny) burns model tokens indefinitely
// with zero possible output — observed live 2026-08-04 on a hosted L2 hive
// whose guide agent retried a blocked push every 3 seconds. (Since #4289,
// ADVISORY-mode pushes are no longer blocked by the credential helper — the
// read-only token is served and GitHub rejects the push with 403 — but the
// helper still emits "git push blocked:" for unknown-UID and missing-token
// failures, which this breaker continues to catch.) The hub, not the model,
// breaks the loop: thrashThreshold blocked-action lines within thrashWindow
// pauses the session (visible, reversible, stops governor kicks) with the
// reason spelled out.
const (
	thrashWindow    = 60 * time.Second
	thrashThreshold = 5
	thrashCooldown  = 10 * time.Minute
)

// blockedActionMarkers are the policy-wall stderr lines that can never
// succeed by retrying. Keep in sync with bin/git-credential-hive.sh and the
// proxy's hard-deny responses.
var blockedActionMarkers = []string{
	"git push blocked:",
	"blocked by hive policy",
}

// blockedActionPrefix is the glyph bin/git-credential-hive.sh writes at the
// start of every policy-wall stderr line. Requiring it at line start is what
// separates a real blocked attempt from an agent merely DISPLAYING a file that
// contains the marker text (#12015): git diff (+/-), grep -n (N:), cat of a
// shell script (echo "⛔ …") or Go source ("git push blocked: …") all put
// something other than the glyph first.
const blockedActionPrefix = "⛔ "

// paneGutterChars are the leading decorations agent TUIs draw in front of a
// tool's output (Claude's "⎿", box-drawing rails, status bullets). They are
// stripped so the helper's own stderr still matches when rendered inside a
// tool-output block.
const paneGutterChars = " \t│┃▌⎿●"

// isBlockedActionLine reports whether a pane line is the credential helper's
// (or proxy's) own policy-wall output rather than echoed file content.
func isBlockedActionLine(line string) bool {
	trimmed := strings.TrimLeft(ansiEscapeRe.ReplaceAllString(line, ""), paneGutterChars)
	rest, ok := strings.CutPrefix(trimmed, blockedActionPrefix)
	if !ok {
		return false
	}
	for _, marker := range blockedActionMarkers {
		if strings.Contains(rest, marker) {
			return true
		}
	}
	return false
}

type thrashState struct {
	times    []time.Time
	lastTrip time.Time
}

// checkBlockedThrash records a blocked-action output line for the agent and,
// past the threshold, pauses the agent asynchronously (never inline: this is
// called from the output-capture goroutine and Pause takes m.mu).
func (m *Manager) checkBlockedThrash(agent, line string) {
	if !isBlockedActionLine(line) {
		return
	}
	now := time.Now()
	m.thrashMu.Lock()
	if m.thrash == nil {
		m.thrash = map[string]*thrashState{}
	}
	st := m.thrash[agent]
	if st == nil {
		st = &thrashState{}
		m.thrash[agent] = st
	}
	trip := recordBlockedAndCheck(st, now, thrashWindow, thrashThreshold, thrashCooldown)
	m.thrashMu.Unlock()
	if !trip {
		return
	}
	reason := fmt.Sprintf("blocked-action loop: %d+ policy-blocked attempts in %s — the block is terminal in this mode; paused to stop token burn", thrashThreshold, thrashWindow)
	m.logger.Warn("thrash breaker tripped", "agent", agent, "line", truncateStr(line, 160))
	go func() {
		if err := m.Pause(agent, "thrash-breaker", reason); err != nil {
			m.logger.Warn("thrash breaker pause failed", "agent", agent, "error", err)
		}
	}()
}

// recordBlockedAndCheck is the pure sliding-window decision: append now, drop
// entries older than window, and report whether the threshold is crossed
// outside the cooldown.
func recordBlockedAndCheck(st *thrashState, now time.Time, window time.Duration, threshold int, cooldown time.Duration) bool {
	st.times = append(st.times, now)
	cutoff := now.Add(-window)
	kept := st.times[:0]
	for _, t := range st.times {
		if !t.Before(cutoff) {
			kept = append(kept, t)
		}
	}
	st.times = kept
	if len(st.times) < threshold {
		return false
	}
	if !st.lastTrip.IsZero() && now.Sub(st.lastTrip) < cooldown {
		return false
	}
	st.lastTrip = now
	return true
}
