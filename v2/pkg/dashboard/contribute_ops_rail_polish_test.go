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

// TestOpsRailRendersHelloReplay proves the rail's SSE hello handler (ccHydrate)
// still feeds hello.replay into the rail log — now routed through the SHARED,
// deduped activity store (ccIngestActivity) and rendered via the same narration
// path (ccNarrate) + ccRenderLog. The rail's resilience (seed + poll from the
// reliable /api/contribute/activity endpoint) is asserted separately in
// TestOpsRailResilientFromActivityPoll; here we pin that the SSE hello path is
// still wired and consistent with the shared store. Runtime behavior of the inline
// script is out of reach for a Go test; node --check on the extracted script covers
// syntax/runtime correctness.
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
		"payload.replay",      // reads the hello frame's ring-buffer field
		"ccIngestActivity(e)", // routes each replay entry through the shared, deduped store
	} {
		if !strings.Contains(hydrateBody, want) {
			t.Errorf("ccHydrate does not replay hello.replay into the shared store: missing %q", want)
		}
	}
	// The shared rebuild path must narrate via ccNarrate and re-render via ccRenderLog
	// (format parity with live events), so the rail log is populated immediately.
	rebuildStart := strings.Index(body, "function ccRebuildLogFromActivity(){")
	if rebuildStart < 0 {
		t.Fatal("ccRebuildLogFromActivity is missing — the shared rail rebuild path was removed")
	}
	rebuildBody := body[rebuildStart : rebuildStart+strings.Index(body[rebuildStart:], "\n}")]
	for _, want := range []string{"ccNarrate", "ccRenderLog()"} {
		if !strings.Contains(rebuildBody, want) {
			t.Errorf("ccRebuildLogFromActivity does not narrate/render the rail log: missing %q", want)
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

// TestOpsRailResilientFromActivityPoll proves the fix for the perpetually-empty
// rail: it is seeded + kept current from the RELIABLE /api/contribute/activity
// polling endpoint (the same source Onboarding's Live Activity uses), NOT solely
// from the SSE stream which returns nothing on hosted spokes. So the rail shows the
// backlog even when SSE never delivers, and it dedupes events that arrive via both.
func TestOpsRailResilientFromActivityPoll(t *testing.T) {
	body := renderContributePage(t)

	// ccStart must kick off the activity poll (the seed + resilience layer).
	if !strings.Contains(body, "ccPollActivity()") {
		t.Error("ccStart does not seed/poll the rail from /api/contribute/activity — the rail can go blank when SSE is silent")
	}
	// The poll must fetch the reliable activity endpoint.
	pollStart := strings.Index(body, "function ccPollActivity(){")
	if pollStart < 0 {
		t.Fatal("ccPollActivity is missing — the rail's polling resilience was removed")
	}
	pollBody := body[pollStart : pollStart+strings.Index(body[pollStart:], "\n}")]
	if !strings.Contains(pollBody, "/api/contribute/activity") {
		t.Error("ccPollActivity does not fetch /api/contribute/activity (the reliable source)")
	}
	// Dedupe: an event arriving via BOTH poll and SSE must be counted once. The
	// store keys events by timestamp+username+action+task.
	keyStart := strings.Index(body, "function ccActivityKey(e){")
	if keyStart < 0 {
		t.Fatal("ccActivityKey (dedupe key) is missing")
	}
	keyBody := body[keyStart : keyStart+strings.Index(body[keyStart:], "\n}")]
	for _, want := range []string{"e.timestamp", "e.username", "e.action", "e.task"} {
		if !strings.Contains(keyBody, want) {
			t.Errorf("ccActivityKey does not dedupe on %q — double-counting is possible", want)
		}
	}
	// SSE events must route through the SAME shared store (so they dedupe against the
	// poll), not into a separate rail-only path.
	if !strings.Contains(body, "ccSSEDelivered=true") || !strings.Contains(body, "if(ccIngestActivity(e)){") {
		t.Error("SSE onActivity does not route through the shared deduped store")
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
