package hub

import (
	"strings"
	"testing"
)

// TestDashboardHeartbeatHeartRendersInRow pins the subtle red-heart
// heartbeat indicator: the heartbeatHeart(h) function must exist, must be
// wired into the row's identity line (line1) right after the name link, and
// must carry its freshness bands and reduced-motion guard. String-anchored
// on the raw dashboardHTML, same as the other JS structure tests (see
// TestDashboardPlaceholderRowsRenderGreen).
func TestDashboardHeartbeatHeartRendersInRow(t *testing.T) {
	if !strings.Contains(dashboardHTML, "function heartbeatHeart(h) {") {
		t.Fatal("heartbeatHeart(h) is missing from dashboardHTML")
	}
	if !strings.Contains(dashboardHTML, "link(orgName, true) + heartbeatHeart(h) + nameEditAffordance(h)") {
		t.Error("heartbeatHeart(h) is not wired into the row's identity line (line1), next to the name link and status dot")
	}
	// Reuses h.lastHeartbeat rather than introducing a second heartbeat
	// source of truth.
	if !strings.Contains(dashboardHTML, "if (!h.lastHeartbeat) return '';") {
		t.Error("heartbeatHeart no longer gates on h.lastHeartbeat — it must reuse the same field healthBadge uses, not track its own")
	}
	// Freshness thresholds: a just-landed beat pulses once, a beat older than
	// the normal cadence dims, and a stale/offline reading hides the heart.
	if !strings.Contains(dashboardHTML, "var HEART_FRESH_MS = 5000;") {
		t.Error("heartbeatHeart lost its HEART_FRESH_MS threshold for the one-shot pulse")
	}
	if !strings.Contains(dashboardHTML, "var HEART_INTERVAL_MS = 2 * 60000;") {
		t.Error("heartbeatHeart lost its HEART_INTERVAL_MS threshold for the steady-vs-aging cutover")
	}
	if !strings.Contains(dashboardHTML, "if (!h.online && ageMs > HEART_STALE_MS) return '';") {
		t.Error("heartbeatHeart no longer hides the heart for a stale/offline hive — a dead hive must not show a beating heart")
	}
	// Tooltip mirrors the freshness the heart itself is showing.
	if !strings.Contains(dashboardHTML, "'Last heartbeat: ' + ageStr") {
		t.Error("heartbeatHeart lost its 'Last heartbeat: Xs/Xm ago' tooltip")
	}
	// Class hooks the CSS keys its pulse/dim states off of.
	for _, cls := range []string{"heartbeat-heart-fresh", "heartbeat-heart-aging"} {
		if !strings.Contains(dashboardHTML, cls) {
			t.Errorf("heartbeatHeart is missing the %q class hook", cls)
		}
	}
	// CSS: subtle red, themeable via currentColor's host, one-shot pulse
	// keyframe, and a reduced-motion guard matching the other animated
	// elements in this file (see .online-dot.upgrading).
	if !strings.Contains(dashboardHTML, ".heartbeat-heart { color: #e5534b;") {
		t.Error("heartbeat-heart lost its muted red color rule")
	}
	if !strings.Contains(dashboardHTML, "@keyframes heartbeatPulse") {
		t.Error("heartbeatPulse keyframe animation is missing")
	}
	if !strings.Contains(dashboardHTML, "@media (prefers-reduced-motion: reduce) { .heartbeat-heart-fresh { animation: none; } }") {
		t.Error("heartbeat-heart-fresh is missing its prefers-reduced-motion guard")
	}
}
