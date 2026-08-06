package hub

import (
	"strings"
	"testing"
)

// TestDashboardHeartbeatHeartFlashesOnReceipt pins the event-driven red-heart
// heartbeat indicator: the heart is NOT visible by default, appears only when
// a hive's lastHeartbeat ADVANCES across renders, flashes exactly three times
// (finite CSS animation), then disappears until the next beat. String-anchored
// on the raw dashboardHTML, same as the other JS structure tests (see
// TestDashboardPlaceholderRowsRenderGreen).
func TestDashboardHeartbeatHeartFlashesOnReceipt(t *testing.T) {
	if !strings.Contains(dashboardHTML, "function heartbeatHeart(h) {") {
		t.Fatal("heartbeatHeart(h) is missing from dashboardHTML")
	}
	if !strings.Contains(dashboardHTML, "link(orgName, true) + heartbeatHeart(h) + nameEditAffordance(h)") {
		t.Error("heartbeatHeart(h) is not wired into the row's identity line (line1), next to the name link and status dot")
	}
	// Reuses h.lastHeartbeat rather than introducing a second heartbeat
	// source of truth.
	if !strings.Contains(dashboardHTML, "if (!h || !h.id || !h.lastHeartbeat) return '';") {
		t.Error("heartbeatHeart no longer gates on h.lastHeartbeat — it must reuse the same field healthBadge uses, not track its own")
	}
	// Event-driven trigger: the flash fires only when the tracked timestamp
	// ADVANCES. A first sighting (prev === undefined) seeds silently so the
	// whole table does not flash on initial page load.
	if !strings.Contains(dashboardHTML, "if (prev !== undefined && beatMs > prev) {") {
		t.Error("heartbeatHeart lost its advance-detection guard — the flash must fire only when lastHeartbeat is NEWER than the seeded value, never on first sighting")
	}
	// Per-hive state lives in JS maps (like _upgradingHives), not on the
	// element, so it survives the periodic full re-render.
	for _, m := range []string{"var _heartSeenBeats = {};", "var _heartFlashUntil = {};"} {
		if !strings.Contains(dashboardHTML, m) {
			t.Errorf("heartbeatHeart is missing its %q row-state map", m)
		}
	}
	// Outside the flash window the function emits NO markup: the heart must
	// not exist between heartbeats.
	if !strings.Contains(dashboardHTML, "if (!until || Date.now() >= until) return '';") {
		t.Error("heartbeatHeart lost its flash-window gate — the heart must render nothing once the 3-pulse window has passed")
	}
	// Named constants sizing the window; the CSS duration/iteration-count
	// below must agree with them.
	for _, c := range []string{"var HEART_PULSE_MS = 600;", "var HEART_PULSE_COUNT = 3;", "var HEART_FLASH_TOTAL_MS = HEART_PULSE_MS * HEART_PULSE_COUNT;"} {
		if !strings.Contains(dashboardHTML, c) {
			t.Errorf("heartbeatHeart lost its %q flash-window constant", c)
		}
	}
	// Tooltip still reports how fresh the beat that triggered the flash is.
	if !strings.Contains(dashboardHTML, "'Last heartbeat: ' + ageStr") {
		t.Error("heartbeatHeart lost its 'Last heartbeat: Xs/Xm ago' tooltip")
	}
	// CSS: invisible by default, and a FINITE 3-iteration animation whose
	// forwards fill-mode leaves the heart invisible after the third pulse.
	if !strings.Contains(dashboardHTML, ".heartbeat-heart { color: #e5534b; vertical-align: middle; margin-left: 4px; opacity: 0; }") {
		t.Error("heartbeat-heart base rule must keep the heart invisible (opacity: 0) by default — no persistent heart between heartbeats")
	}
	if !strings.Contains(dashboardHTML, ".heartbeat-heart-flash { animation: heartbeatPulse 0.6s ease-in-out 3 forwards; }") {
		t.Error("heartbeat-heart-flash lost its finite 3-iteration, forwards-filling pulse animation")
	}
	if !strings.Contains(dashboardHTML, "@keyframes heartbeatPulse") {
		t.Error("heartbeatPulse keyframe animation is missing")
	}
	// The spent element is also removed from hit-testing once the animation
	// finishes, via a single delegated listener that survives re-renders.
	if !strings.Contains(dashboardHTML, "t.classList.contains('heartbeat-heart-flash')) t.style.display = 'none';") {
		t.Error("heartbeat-heart-flash lost its animationend cleanup — a spent heart must not linger as an invisible hover target")
	}
	// Reduced-motion guard matching the other animated elements in this file
	// (see .online-dot.upgrading): no animation, and the opacity-0 base rule
	// keeps the heart hidden.
	if !strings.Contains(dashboardHTML, "@media (prefers-reduced-motion: reduce) { .heartbeat-heart-flash { animation: none; } }") {
		t.Error("heartbeat-heart-flash is missing its prefers-reduced-motion guard")
	}
	// The old always-on freshness bands are gone: no steady heart, no aging
	// dimmed heart — the heart exists ONLY during the 3-pulse flash.
	for _, gone := range []string{"heartbeat-heart-fresh", "heartbeat-heart-aging", "HEART_FRESH_MS", "HEART_INTERVAL_MS", "HEART_STALE_MS"} {
		if strings.Contains(dashboardHTML, gone) {
			t.Errorf("stale always-on heart artifact %q is still present — the heart must only exist during the flash window", gone)
		}
	}
}
