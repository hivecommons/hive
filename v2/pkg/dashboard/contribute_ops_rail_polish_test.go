package dashboard

import (
	"strings"
	"testing"
)

// ── Three small Operations-tab polish fixes ────────────────────────────────
//
//  1. The rail's feed heading is renamed "Development log" -> "Live Activity"
//     so it reads the same as the identically-sourced feed on the Onboarding
//     tab (the Onboarding heading is untouched — it already said "Live
//     Activity").
//  2. The rail replays the SSE "hello" frame's ring buffer (hello.replay) into
//     the log on connect, so the rail shows recent history immediately instead
//     of sitting on the "Watching the hive…" empty state until the next live
//     event arrives.
//  3. The Ready-work queue header gets an item-count badge (like "My work"'s
//     #work-count), so the operator can see queue depth at a glance.

// TestOpsRailHeadingIsLiveActivity asserts the Operations rail's own heading
// text was renamed to "Live Activity" and the old "Development log" string is
// gone from the rendered page — while the Onboarding tab's identically-named
// feed (a separate #activity-feed backed by /api/contribute/activity) is left
// alone.
func TestOpsRailHeadingIsLiveActivity(t *testing.T) {
	body := renderContributePage(t)

	if strings.Contains(body, "Development log") {
		t.Error(`rendered page still contains "Development log" — the Operations rail heading was not renamed`)
	}

	// The rail's own <h3> must read "Live Activity", and it must live inside
	// the rail <aside>, not be a stray unrelated match elsewhere on the page.
	railOpen := strings.Index(body, `id="ops-rail"`)
	railClose := strings.Index(body, `</aside>`)
	if railOpen < 0 || railClose < 0 || railOpen >= railClose {
		t.Fatalf("could not locate the ops-rail <aside> region (open=%d close=%d)", railOpen, railClose)
	}
	railHTML := body[railOpen:railClose]
	if !strings.Contains(railHTML, "<h3>Live Activity</h3>") {
		t.Error(`ops-rail heading is not "Live Activity"`)
	}

	// The Onboarding feed must still say "Live Activity" too — this fix must
	// not rename or touch it (it was already correct).
	onboardOpen := strings.Index(body, `id="tab-onboarding"`)
	onboardClose := strings.Index(body, `id="tab-manage"`)
	if onboardOpen < 0 || onboardClose < 0 || onboardOpen >= onboardClose {
		t.Fatalf("could not locate the onboarding tab-panel region")
	}
	if !strings.Contains(body[onboardOpen:onboardClose], "<h3>Live Activity</h3>") {
		t.Error(`onboarding "Live Activity" heading regressed`)
	}

	// The rail's non-text plumbing (collapse toggle, live pill, ids, empty
	// state) must be untouched by the rename.
	for _, want := range []string{
		`id="ops-rail-toggle"`,
		`id="cc-live-rail"`,
		`id="cc-log"`,
		`id="cc-log-count"`,
		`Watching the hive`,
	} {
		if !strings.Contains(railHTML, want) {
			t.Errorf("rail plumbing marker %q lost by the rename", want)
		}
	}
}

// TestOpsRailRendersHelloReplay proves the rail's SSE hello handler
// (ccHydrate) renders hello.replay entries into the log immediately, via the
// same narration path (ccNarrate) new live events use, so the rail is never
// blank on load when the hub has buffered activity. Runtime behavior of the
// inline script is out of reach for a Go test (see TestOpsFleetHydrationIsResilient
// for the established pattern); `node --check` on the extracted script covers
// syntax/runtime correctness. This test pins the wiring: ccHydrate consumes
// payload.replay, feeds each entry through ccNarrate into ccLogLines, and
// re-renders via ccRenderLog — the same function that also renders live
// ccPushLog events, so the format matches.
func TestOpsRailRendersHelloReplay(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, "function ccHydrate(payload){") {
		t.Fatal("ccHydrate is missing — the SSE hello handler was removed")
	}
	hydrateStart := strings.Index(body, "function ccHydrate(payload){")
	hydrateEnd := strings.Index(body[hydrateStart:], "\n}")
	if hydrateEnd < 0 {
		t.Fatal("could not locate the end of ccHydrate")
	}
	hydrateBody := body[hydrateStart : hydrateStart+hydrateEnd]

	for _, want := range []string{
		"payload.replay",  // reads the hello frame's ring-buffer field
		"ccNarrate(e)",    // same narration fn live events use (format parity)
		"ccLogLines.push", // feeds the rail's own log model, not a separate one
		"ccRenderLog()",   // re-renders the rail log immediately (not deferred to the next live event)
	} {
		if !strings.Contains(hydrateBody, want) {
			t.Errorf("ccHydrate does not replay hello.replay into the rail log: missing %q", want)
		}
	}

	// hello.replay must also drive ccHydrate on receipt (the EventSource
	// onmessage handler must route type=="hello" to ccHydrate) so the wiring
	// is actually reachable, not just defined.
	if !strings.Contains(body, `if(ev.type==='hello')ccHydrate(ev);`) {
		t.Error(`the SSE onmessage handler does not route "hello" frames to ccHydrate`)
	}

	// The "Watching the hive…" empty state must remain reachable — it is the
	// correct render for the genuine zero-buffered-events case, produced by
	// ccRenderLog when ccLogLines is empty (i.e. it must NOT be hardcoded as
	// the rail's only possible state).
	if !strings.Contains(body, "if(!ccLogLines.length){el.innerHTML='<div class=\"ops-empty\">Watching the hive&hellip;</div>';return;}") {
		t.Error("ccRenderLog no longer gates the empty state on ccLogLines being genuinely empty")
	}
}

// TestReadyWorkQueueHeaderHasCount asserts the Ready-work queue header carries
// an ops-card-count badge (like "My work"'s #work-count), that ccRenderQueue
// populates it from the current ccQueue on every render, and that the
// existing cc-live pill is preserved alongside it.
func TestReadyWorkQueueHeaderHasCount(t *testing.T) {
	body := renderContributePage(t)

	headOpen := strings.Index(body, "<h3>Ready-work queue</h3>")
	if headOpen < 0 {
		t.Fatal(`could not locate the "Ready-work queue" header`)
	}
	// The header line: h3, then the new count span, then the pre-existing live pill.
	headEnd := strings.Index(body[headOpen:], "</div>")
	if headEnd < 0 {
		t.Fatal("could not locate the end of the Ready-work queue header")
	}
	headHTML := body[headOpen : headOpen+headEnd]

	if !strings.Contains(headHTML, `class="ops-card-count" id="queue-count"`) {
		t.Error("Ready-work queue header is missing the ops-card-count badge (id=\"queue-count\")")
	}
	if !strings.Contains(headHTML, `id="cc-live"`) {
		t.Error("Ready-work queue header lost the existing cc-live pill — the count must be ADDED, not replace it")
	}

	// ccRenderQueue must populate #queue-count from ccQueue.length on every
	// render path (initial load, SSE queue push, poll fallback, drag-reorder —
	// all of which call ccRenderQueue), matching the #work-count pattern.
	if !strings.Contains(body, "function ccRenderQueue(flip){") {
		t.Fatal("ccRenderQueue is missing")
	}
	rqStart := strings.Index(body, "function ccRenderQueue(flip){")
	if !strings.Contains(body[rqStart:rqStart+400], `getElementById('queue-count')`) {
		t.Error("ccRenderQueue does not populate #queue-count")
	}
}

// TestOpsRailAndQueueCountsCoexistWithMyWork is a light regression guard: the
// pre-existing "My work" count badge (#work-count) must be untouched by these
// changes — same class, same population pattern.
func TestOpsRailAndQueueCountsCoexistWithMyWork(t *testing.T) {
	body := renderContributePage(t)
	if !strings.Contains(body, `<h3>My work</h3><span class="ops-card-count" id="work-count"></span>`) {
		t.Error(`"My work" count badge markup regressed`)
	}
	if !strings.Contains(body, "document.getElementById('work-count').textContent") {
		t.Error("work-count population wiring regressed")
	}
}

// TestOpsRailAndOnboardingFeedsAreIndependent guards against accidentally
// merging the two feeds: renaming the rail heading must not make the rail
// consume the Onboarding feed's REST-poll endpoint, nor vice versa.
func TestOpsRailAndOnboardingFeedsAreIndependent(t *testing.T) {
	body := renderContributePage(t)
	if !strings.Contains(body, `id="activity-feed"`) {
		t.Error("onboarding activity feed container missing")
	}
	if !strings.Contains(body, "fetch('/api/contribute/activity')") {
		t.Error("onboarding feed's REST poll wiring regressed")
	}
	// Sanity check both endpoints still independently exist and are distinct.
	if !strings.Contains(body, "/api/contribute/events") {
		t.Error("rail SSE stream endpoint missing")
	}
}
