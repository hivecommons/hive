package dashboard

// #7330 asked for the hub's per-contributor decisions "on the Operations tab".
// #7332 shipped the API — GET /api/contribute/decisions — and nothing that
// renders it, so the issue's actual ask was half done: an operator still had to
// curl the endpoint, which is precisely the position #7317 was filed to end.
//
// These tests hold the rendered half. They assert against the REAL page string
// (renderContributePage), not against a fixture, so they fail if the card is
// deleted or the fetch is rewired.

import (
	"strings"
	"testing"
)

// The card must exist, be inside the Operations tab, and sit with the run
// history — the two are the two halves of one conversation and an operator
// reading one without the other draws the wrong conclusion.
func TestOpsDecisionsCardIsRendered(t *testing.T) {
	body := renderContributePage(t)

	for _, want := range []string{
		`id="decisions-card"`,
		`id="decisions-list"`,
		`id="decisions-count"`,
		`<h3>Hub decisions</h3>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page is missing %s — the #7330 card is not on the page", want)
		}
	}

	runsAt := strings.Index(body, `id="runs-card"`)
	decAt := strings.Index(body, `id="decisions-card"`)
	if runsAt < 0 || decAt < 0 {
		t.Fatalf("could not locate both cards (runs=%d decisions=%d)", runsAt, decAt)
	}
	if decAt < runsAt {
		t.Error("the decisions card renders BEFORE the run history; it is filled by the run " +
			"history's lookup form and reads as orphaned above it")
	}

	// Both cards must be in the ops tab, not stranded in another panel.
	tabAt := strings.Index(body, `id="tab-ops"`)
	if tabAt < 0 || tabAt > decAt {
		t.Errorf("the decisions card is not inside the Operations tab (tab at %d, card at %d)", tabAt, decAt)
	}
}

// The endpoint is the one #7332 registered. A typo here is a card that is
// permanently empty with no error, which is the exact failure mode #7317
// describes: indistinguishable from a contributor nothing happened to.
func TestOpsDecisionsFetchesTheRegisteredEndpoint(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, `fetch('/api/contribute/decisions?username='+encodeURIComponent(user)`) {
		t.Error("the decisions card does not fetch /api/contribute/decisions with an encoded username")
	}
	// The route the server actually registers, asserted from the same page
	// string so the two cannot drift apart silently.
	if !strings.Contains(body, "/api/contribute/decisions") {
		t.Fatal("the decisions endpoint path is absent from the page")
	}

	// One lookup must fill both cards. If ccLookupRuns stops calling it, the
	// card still renders but never populates — a silent regression.
	runsFn := sliceBetween(t, body, "function ccLookupRuns(", "function ccRenderDecisions(")
	if !strings.Contains(runsFn, "ccLookupDecisions(user)") {
		t.Error("ccLookupRuns no longer triggers the decisions lookup; the card would never populate")
	}
}

// A 403 is an EXPECTED answer here, not a failure: the endpoint is gated to
// owner/read-write because the entries carry generation numbers, lease identity
// and configured rate limits. This card sits directly beneath one a read-only
// viewer CAN use, so rendering "could not load" would make a correctly-working
// hive look broken to exactly the viewer least able to tell the difference.
func TestOpsDecisions403RendersAsAGateNotAnError(t *testing.T) {
	body := renderContributePage(t)

	// Scoped to ccLookupDecisions on purpose. An unrelated card ("your
	// contribution") also 403s, so a page-wide Contains here passes even with
	// this function's 403 branch deleted — verified: it did.
	fn := sliceBetween(t, body, "function ccLookupDecisions(", "\n}\n")
	if !strings.Contains(fn, "if(r.status===403)") {
		t.Fatal("the decisions fetch does not special-case 403; a read-only viewer sees an error")
	}
	if !strings.Contains(fn, "err.gated") {
		t.Error("the 403 branch does not reach a distinct gated render path")
	}
	if !strings.Contains(fn, "owner and read-write users") {
		t.Error("the gate notice does not say WHO can see this, so a read-only viewer " +
			"cannot tell whether to ask for access or file a bug")
	}
	// The gated copy must explain the refusal rather than just stating it —
	// hubDecisionViewer's own comment is the reason, and it belongs on screen.
	if !strings.Contains(fn, "generation numbers") {
		t.Error("the gate notice does not say why the endpoint refuses rather than serving a stripped list")
	}
}

// in_memory_only is the whole reason the response carries "since". An empty
// list after a hub restart is NOT evidence that nothing happened, and a card
// that renders it as "no decisions" full stop invites exactly that conclusion.
func TestOpsDecisionsSaysTheRingIsInMemoryOnly(t *testing.T) {
	body := renderContributePage(t)

	fn := sliceBetween(t, body, "function ccRenderDecisions(", "function ccLookupDecisions(")
	if !strings.Contains(fn, "data.since") {
		t.Error("the decisions card ignores the response's since field, so an empty list " +
			"after a restart reads as 'nothing happened'")
	}
	if !strings.Contains(fn, "In memory only") {
		t.Error("the card does not tell the operator the ring is in memory only")
	}
	if !strings.Contains(fn, "A restart empties this") {
		t.Error("the card does not warn that a hub restart empties the ring")
	}
}

// Every decision field that reaches the DOM is hub-authored, but detail is a
// flattened slog line and repo/task_id are strings the hub took from a client
// payload. They go through esc() like everything else on this page.
func TestOpsDecisionsEscapesEveryRenderedField(t *testing.T) {
	body := renderContributePage(t)
	fn := sliceBetween(t, body, "function ccRenderDecisions(", "function ccLookupDecisions(")

	for _, raw := range []string{"+d.detail+", "+d.repo+", "+d.task_id+", "+d.event+", "+d.ts+"} {
		if strings.Contains(fn, raw) {
			t.Errorf("decision field interpolated without esc(): %s", raw)
		}
	}
	for _, escaped := range []string{"esc(d.detail)", "esc(d.repo)", "esc(d.task_id", "esc(rel(d.ts))"} {
		if !strings.Contains(fn, escaped) {
			t.Errorf("expected %s in the decision renderer", escaped)
		}
	}
}

// The six event strings are a closed vocabulary shared with hub_decisions.go.
// The card gives each one a plain-English gloss, because "stale_gen_rejected"
// is the hub's word and the operator reading this page is trying to work out
// whether their contributor is broken or being blocked.
func TestOpsDecisionsGlossesEveryEventKind(t *testing.T) {
	body := renderContributePage(t)
	fn := sliceBetween(t, body, "function ccDecisionLabel(", "function ccRenderDecisions(")

	for _, ev := range []string{
		decisionStaleGenRejected,
		decisionUnassignedIgnored,
		decisionAbandoned,
		decisionResumeRejected,
		decisionLeaseExpired,
		decisionRefused,
	} {
		if !strings.Contains(fn, ev+":") {
			t.Errorf("event %q has no plain-English gloss in the Operations tab; an operator "+
				"sees the hub's internal word and has to guess", ev)
		}
	}
}

// sliceBetween returns the page text between two markers, failing the test if
// either is absent — so a renamed function fails loudly instead of silently
// turning every assertion in the caller into a vacuous pass over "".
func sliceBetween(t *testing.T, body, start, end string) string {
	t.Helper()
	i := strings.Index(body, start)
	if i < 0 {
		t.Fatalf("marker %q not found in the rendered page", start)
	}
	rest := body[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("end marker %q not found after %q", end, start)
	}
	return rest[:j]
}
