package dashboard

import (
	"strings"
	"testing"
)

// ── Clickable GitHub issue/PR references on the Operations tab (#2616) ─────────
//
// Jorge's ask: the "repo#number" references on Ready-work queue / My work /
// Opportunistic work were plain monospace text — make them real, obviously
// clickable links to the actual GitHub issue/PR so the page can be used to
// manage work directly.
//
// This file pins:
//   - the shared ccIssueURL/ccIssueLinkHTML helpers exist and prefer the
//     item's own url field, falling back to constructing
//     https://github.com/<repo>/issues/<number>
//   - each of the three render functions (ccRenderQueue, renderWork,
//     ccRenderOpportunistic) wires its repo#number cell through the helper
//   - the link markup carries target=_blank + rel=noopener noreferrer and an
//     external-link icon (the "obvious affordance")
//   - the link's click/mousedown handlers stop propagation so opening the
//     issue never starts the queue's drag-reorder or any other row handler
//
// Runtime DOM behavior of the inline script is out of reach for a Go test
// (no headless browser here); node --check on the extracted script is the
// separate syntax/runtime gate. These tests assert the generated markup and
// wiring are correct by construction.

func TestIssueLinkHelpersExistAndPreferURLField(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, "function ccIssueURL(item){") {
		t.Fatal("ccIssueURL helper is missing")
	}
	urlStart := strings.Index(body, "function ccIssueURL(item){")
	urlBody := body[urlStart : urlStart+strings.Index(body[urlStart:], "\n}")]
	if !strings.Contains(urlBody, "item.url") {
		t.Error("ccIssueURL does not prefer the item's own url field")
	}
	if !strings.Contains(urlBody, "https://github.com/'+item.repo+'/issues/'+item.number") {
		t.Error("ccIssueURL does not construct the github.com/<repo>/issues/<number> fallback")
	}

	if !strings.Contains(body, "function ccIssueLinkHTML(item,label,extraClass){") {
		t.Fatal("ccIssueLinkHTML helper is missing")
	}
	linkStart := strings.Index(body, "function ccIssueLinkHTML(item,label,extraClass){")
	linkBody := body[linkStart : linkStart+strings.Index(body[linkStart:], "\n}")]
	for _, want := range []string{
		`target="_blank"`,
		`rel="noopener noreferrer"`,
		`title="Open on GitHub"`,
		"event.stopPropagation();",
		"cc-issue-link-ic", // the external-link icon
	} {
		if !strings.Contains(linkBody, want) {
			t.Errorf("ccIssueLinkHTML missing %q", want)
		}
	}
}

func TestReadyQueueRepoCellUsesIssueLink(t *testing.T) {
	body := renderContributePage(t)
	rqStart := strings.Index(body, "function ccRenderQueue(flip){")
	if rqStart < 0 {
		t.Fatal("ccRenderQueue is missing")
	}
	rqEnd := strings.Index(body[rqStart:], "\nfunction ccQueueMenuHTML")
	if rqEnd < 0 {
		t.Fatal("could not bound ccRenderQueue")
	}
	rqBody := body[rqStart : rqStart+rqEnd]
	if !strings.Contains(rqBody, `'<div class="cc-q-body"><div class="cc-q-repo">'+ccIssueLinkHTML(q,`) {
		t.Error("ccRenderQueue's .cc-q-repo cell does not route through ccIssueLinkHTML")
	}
}

func TestMyWorkRepoCellUsesIssueLink(t *testing.T) {
	body := renderContributePage(t)
	rwStart := strings.Index(body, "function renderWork(list){")
	if rwStart < 0 {
		t.Fatal("renderWork is missing")
	}
	rwEnd := strings.Index(body[rwStart:], "\nfunction renderPolicy")
	if rwEnd < 0 {
		t.Fatal("could not bound renderWork")
	}
	rwBody := body[rwStart : rwStart+rwEnd]
	if !strings.Contains(rwBody, `'<div class="work-item"><div class="work-repo">'+ccIssueLinkHTML(w,repoLabel)+'</div>'`) {
		t.Error("renderWork's .work-repo cell does not route through ccIssueLinkHTML")
	}
}

func TestOpportunisticRepoCellUsesIssueLink(t *testing.T) {
	body := renderContributePage(t)
	oStart := strings.Index(body, "function ccRenderOpportunistic(){")
	if oStart < 0 {
		t.Fatal("ccRenderOpportunistic is missing")
	}
	oEnd := strings.Index(body[oStart:], "\nfunction ccBindOppAdd")
	if oEnd < 0 {
		t.Fatal("could not bound ccRenderOpportunistic")
	}
	oBody := body[oStart : oStart+oEnd]
	if !strings.Contains(oBody, `'<div class="opp-body"><div class="opp-repo">'+ccIssueLinkHTML(o,key)+'</div>'`) {
		t.Error("ccRenderOpportunistic's .opp-repo cell does not route through ccIssueLinkHTML")
	}
}

func TestDevLogPickedUpRefUsesIssueLinkWhenParseable(t *testing.T) {
	body := renderContributePage(t)
	if !strings.Contains(body, "function ccTaskRefLink(task){") {
		t.Fatal("ccTaskRefLink helper is missing")
	}
	refStart := strings.Index(body, "function ccTaskRefLink(task){")
	refBody := body[refStart : refStart+strings.Index(body[refStart:], "\n}")]
	if !strings.Contains(refBody, "ccIssueLinkHTML({repo:m[1],number:m[2]},task,'ref')") {
		t.Error("ccTaskRefLink does not build a link via ccIssueLinkHTML from the parsed repo/number")
	}

	narrateStart := strings.Index(body, "function ccNarrate(e){")
	if narrateStart < 0 {
		t.Fatal("ccNarrate is missing")
	}
	narrateBody := body[narrateStart : narrateStart+strings.Index(body[narrateStart:], "\nfunction ccRenderLog")]
	if !strings.Contains(narrateBody, "who+' grabbed'+pickedRef") {
		t.Error(`ccNarrate's "picked up" case does not use the linked pickedRef`)
	}
}

// TestIssueLinkClickStopsPropagation guards the drag-vs-click contract directly:
// the anchor markup itself carries the stopPropagation handlers, so a click on
// the link can never bubble to a parent's dragstart/click listener (the queue
// row is draggable="true" and binds its own click/drag handlers in
// ccBindQueueDrag / ccBindQueueMenus).
func TestIssueLinkClickStopsPropagation(t *testing.T) {
	body := renderContributePage(t)
	linkStart := strings.Index(body, "function ccIssueLinkHTML(item,label,extraClass){")
	if linkStart < 0 {
		t.Fatal("ccIssueLinkHTML is missing")
	}
	linkBody := body[linkStart : linkStart+strings.Index(body[linkStart:], "\n}")]
	if !strings.Contains(linkBody, `onclick="event.stopPropagation();"`) {
		t.Error("issue link does not stop click propagation — clicking it could trigger a parent row handler")
	}
	if !strings.Contains(linkBody, `onmousedown="event.stopPropagation();"`) {
		t.Error("issue link does not stop mousedown propagation — clicking it could start a drag (dragstart begins on mousedown+move)")
	}
}
