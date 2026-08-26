package dashboard

import (
	"strings"
	"testing"
)

// Regression guard for #4348: clicking ✕ on an agent's restart count zeroed
// the display, then it flickered BACK to the stale value before settling at 0.
// The cached /api/status snapshot is rebuilt asynchronously after a mutation,
// so an in-flight poll/SSE frame carrying the pre-reset snapshot repainted the
// old count over the optimistic patch. The fix is a stale-snapshot guard in
// render(): out-of-order statusSeq payloads are dropped, and after a confirmed
// mutation everything below the server-supplied minStatusSeq floor is
// discarded. These checks pin the wiring so the guard (or one of its callees)
// can't silently vanish — the renderAll()/hiveToast() bug class where an
// undefined callee turns a success path into a runtime error.
func TestStatusSeqGuardWiring(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	// Every function the guard path calls must be defined in the page.
	for _, def := range []string{
		"function noteStatusMutation(",
		"function render(",
		"function refreshStatus()",
		"function showToast(",
		"function dismissToast(",
	} {
		if !strings.Contains(html, def) {
			t.Errorf("index.html is missing definition %q", def)
		}
	}

	// Guard state and the render() drop logic.
	for _, snippet := range []string{
		"let _lastStatusSeq = 0;",
		"let _statusSeqFloor = 0;",
		"let _statusInstance = '';",
		"if (data.statusSeq < _statusSeqFloor || data.statusSeq < _lastStatusSeq) return;",
		"_lastStatusSeq = data.statusSeq;",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing stale-snapshot guard snippet %q", snippet)
		}
	}

	// A spoke restart resets statusSeq to 1; the guard must reset with it or
	// the page freezes on pre-restart state forever.
	for _, snippet := range []string{
		"data.statusInstance !== _statusInstance",
		"_statusInstance = data.statusInstance;",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing instance-change reset snippet %q", snippet)
		}
	}

	// Both reset flows must raise the floor from the mutation response and the
	// restart-count flow must refetch so the real snapshot repaints.
	for _, snippet := range []string{
		"noteStatusMutation(data.minStatusSeq);",
		"noteStatusMutation(ok.minStatusSeq);",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing mutation-floor call %q", snippet)
		}
	}

	// resetRestarts must refetch after the optimistic patch — the guard drops
	// stale frames, so without a refetch the page would wait a full SSE cycle.
	resetFn := html[strings.Index(html, "async function resetRestarts("):]
	if end := strings.Index(resetFn, "\n    }"); end > 0 {
		resetFn = resetFn[:end]
	}
	for _, snippet := range []string{"noteStatusMutation(data.minStatusSeq);", "refreshStatus();"} {
		if !strings.Contains(resetFn, snippet) {
			t.Errorf("resetRestarts() is missing %q", snippet)
		}
	}

	// Stale callees from the historic bug class must stay dead.
	for _, stale := range []string{"renderAll(", "hiveToast("} {
		if strings.Contains(html, stale) {
			t.Errorf("index.html calls %q, which is never defined", stale)
		}
	}
}
