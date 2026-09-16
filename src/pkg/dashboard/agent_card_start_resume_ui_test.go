package dashboard

import (
	"strings"
	"testing"
)

// Combined start & resume (#5594 follow-up). Resume-leads (#5599) made the
// pause impossible to hide and its tooltip names the blockers resuming will
// not clear — but a paused agent whose session is also down still took two
// operator actions (resume, then restart) to get moving. These pins hold the
// final step: one '▶ start & resume' button that clears both flags in a
// single click, chaining the two EXISTING endpoints client-side.

// TestPausedAgentCombinedStartResumePinned pins the invariants:
//
//  1. The paused action is built by the shared pausedAgentActionHtml helper
//     at every render site (card grid, detail panel, detail fast path, and
//     toggleAgent's optimistic patch) — the single place that guarantees a
//     paused agent never shows a bare Start.
//  2. Session up keeps plain '▶ resume' with agentToggleTitle's
//     blocker-naming tooltip; session down renders the combined
//     '▶ start & resume' wired to startResumeAgent.
//  3. startResumeAgent chains resume BEFORE restart, so the freshly spawned
//     session is never born paused.
func TestPausedAgentCombinedStartResumePinned(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		// The shared helper: session-up branch delegates to the resume-leads
		// tooltip, session-down branch renders the combined action.
		"function pausedAgentActionHtml(a) {",
		"agentToggleTitle(a, true, false)",
		`data-action="startResumeAgent"`,
		"start &amp; resume</button>",
		// Dispatcher case for the combined action.
		"case 'startResumeAgent': startResumeAgent(agent); break;",
		// Client-side chaining of the two existing endpoints.
		"function startResumeAgent(agent)",
		"`/api/resume/${agent}`",
		"`/api/restart/${agent}`",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("combined start & resume action missing snippet %q", snippet)
		}
	}

	// Ordering pin inside startResumeAgent: resume must precede restart.
	start := strings.Index(html, "function startResumeAgent(agent)")
	if start < 0 {
		t.Fatal("startResumeAgent definition missing")
	}
	fn := html[start:]
	if end := strings.Index(fn, "async function restartAgent"); end > 0 {
		fn = fn[:end]
	}
	resumeIdx := strings.Index(fn, "/api/resume/")
	restartIdx := strings.Index(fn, "/api/restart/")
	if resumeIdx < 0 || restartIdx < 0 || resumeIdx > restartIdx {
		t.Fatalf("startResumeAgent must call /api/resume before /api/restart (resume@%d restart@%d)", resumeIdx, restartIdx)
	}

	// Every paused-action render site delegates to the helper: definition +
	// card grid + detail full render + detail fast path + toggleAgent's
	// optimistic just-paused patch.
	if got := strings.Count(html, "pausedAgentActionHtml(a"); got < 5 {
		t.Fatalf("pausedAgentActionHtml referenced %d times, want >= 5 (definition + grid + detail + fast path + toggleAgent patch)", got)
	}
}

// TestCombinedActionPreservesResumeLeads pins that the follow-up sits ON TOP
// of resume-leads rather than replacing it: the grid's paused branch still
// wins the slot before the cadence gear, and the detail panel checks paused
// before off — so the combined button can never be displaced by, nor
// degrade into, a bare Start.
func TestCombinedActionPreservesResumeLeads(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		// Grid: paused leads (same structural pin as TestAgentCardResumeLeads),
		// now delegating to the shared helper.
		"const toggleBtn = (canToggle && !agentIsDisabled(a)) ? (isPaused && canPauseToggle\n          ? pausedAgentActionHtml(a)",
		// Detail panel: paused checked before the off/start branch.
		": isPaused\n            ? pausedAgentActionHtml(a)",
		// Detail fast path: paused rebuilds via the helper so data-action
		// tracks the session state, not just the label.
		"fastToggle.outerHTML = pausedAgentActionHtml(a);",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("resume-leads integration missing snippet %q", snippet)
		}
	}
	// The combined label must carry the HTML-escaped '&amp;' — a raw '&'
	// inside button markup is invalid HTML and a regression magnet.
	if strings.Contains(html, "start & resume</button>") {
		t.Error("combined label must use the HTML-escaped '&amp;' form, not a raw '&'")
	}
}
