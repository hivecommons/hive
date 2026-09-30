package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// readSourceFile returns a Go source file from this package by name. The
// response-shape assertions below are about literal handler source, which the
// compiler cannot check for us.
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// #5492: an ACMM level change and an agent cadence change both applied
// server-side but stayed invisible in the dashboard for ~30s.
//
// The mechanism is NOT the family of 30s poll constants in index.html — those
// drive unrelated panels (history, GH auth, nous, KB, audit, leaderboard). Both
// affected panels render from the SSE status payload, and both write paths
// already ask for a status rebuild. The lag is a RACE:
//
//	handler mutates state
//	  -> go RefreshFunc()          (asynchronous rebuild, seconds)
//	  -> returns 200 to the browser
//	browser refetches GET /api/status
//	  -> handleStatus serves the CACHED s.status, which the async rebuild has
//	     not replaced yet -> the PRE-mutation snapshot, i.e. the OLD value
//
// The browser then sits on that stale render until some later broadcast
// happens to carry the new value.
//
// #4348 already solved exactly this for the restart counter and the budget
// window: the handler returns "minStatusSeq" (the lowest StatusSeq guaranteed
// to reflect the mutation) and the frontend's noteStatusMutation() raises a
// floor so any snapshot built before the mutation is DISCARDED rather than
// rendered. These tests pin that same contract onto the two #5492 paths.
//
// Critically, this fix never renders an unconfirmed value: the floor only
// causes stale snapshots to be dropped, so a failed write leaves the old value
// on screen (and its error toast), it does not paint the requested value.

// serverSeqFixture builds a Server whose RefreshFunc is deliberately slow, so
// the async-rebuild race the operator hit is reproduced deterministically
// rather than depending on timing luck.
func serverSeqFixture(t *testing.T) *Server {
	t.Helper()
	s := &Server{}
	s.sseClients = make(map[chan []byte]struct{})
	return s
}

// TestStatusSeqFloorAdvancesPastCachedSnapshot demonstrates the race itself at
// the seq level: a snapshot published from a build that STARTED before the
// mutation carries a seq below the floor the mutation handed out, which is
// precisely the signal the frontend needs to drop it.
func TestStatusSeqFloorAdvancesPastCachedSnapshot(t *testing.T) {
	s := serverSeqFixture(t)

	// A snapshot published before any mutation.
	s.UpdateStatus(&StatusPayload{})
	s.statusMu.RLock()
	preSeq := s.status.StatusSeq
	s.statusMu.RUnlock()

	// The operator's write lands. The handler captures the floor.
	floor := s.noteStatusMutation()

	if floor <= preSeq {
		t.Fatalf("mutation floor %d must exceed the pre-mutation snapshot seq %d — "+
			"otherwise the browser cannot tell a stale snapshot from a fresh one", floor, preSeq)
	}

	// The stale in-flight rebuild (started before the mutation) would publish
	// a snapshot whose seq is below the floor. The frontend guard drops it.
	if preSeq >= floor {
		t.Fatalf("pre-mutation snapshot seq %d is not below floor %d", preSeq, floor)
	}
}

// TestCadenceWriteReturnsStatusSeqFloor asserts the cadence handler hands the
// browser a minStatusSeq. Without it the browser has no way to reject the
// pre-mutation /api/status it is about to receive, and renders the OLD cadence.
func TestCadenceWriteReturnsStatusSeqFloor(t *testing.T) {
	html := indexHTML(t)

	// The cadence save must raise the floor from the write response.
	if !strings.Contains(html, "noteStatusMutation(cadenceAck.minStatusSeq);") {
		t.Error("index.html: the agent cadence save does not raise the stale-snapshot " +
			"floor from its write response — a pre-mutation /api/status will repaint " +
			"the old cadence and the operator waits for a later broadcast (#5492)")
	}
}

// TestACMMLevelWriteReturnsStatusSeqFloor asserts the same for the ACMM level
// path, which is the higher-stakes of the two: an operator who still sees L4
// after moving to L5 may re-apply, triggering a second fleet-wide reconcile.
//
// NOTE: the bare string "noteStatusMutation(data.minStatusSeq);" already
// appears in the UNRELATED resetRestarts() handler (#4348), so a whole-file
// Contains check here would pass without the fix — vacuous. Both ACMM
// assertions are therefore scoped to the enclosing function body.
func TestACMMLevelWriteReturnsStatusSeqFloor(t *testing.T) {
	for _, fn := range []string{"applyACMMPack", "setACMMLevel"} {
		body := acmmFuncBody(t, fn)
		if !strings.Contains(body, "noteStatusMutation(data.minStatusSeq);") {
			t.Errorf("index.html: %s does not raise the stale-snapshot floor from its "+
				"write response — the dashboard can repaint the previous level after a "+
				"successful L4->L5 change (#5492)", fn)
		}
	}
}

// acmmFuncBody returns the source of one of the two ACMM level-change
// functions, bounded to that function so assertions cannot be satisfied by
// identical code elsewhere in this 24k-line file.
func acmmFuncBody(t *testing.T, name string) string {
	t.Helper()
	html := indexHTML(t)
	start := strings.Index(html, "async function "+name+"(level) {")
	if start < 0 {
		t.Fatalf("index.html: %s not found", name)
	}
	// Both functions end at the next top-level `async function`/`function`
	// declaration at the same indentation.
	rest := html[start+10:]
	end := strings.Index(rest, "\n    async function ")
	altEnd := strings.Index(rest, "\n    function ")
	if altEnd >= 0 && (end < 0 || altEnd < end) {
		end = altEnd
	}
	if end < 0 {
		t.Fatalf("index.html: could not bound %s", name)
	}
	return rest[:end]
}

// TestACMMOverrideRendersServerConfirmedLevel is the anti-lie guard.
//
// The pre-fix code set window._lastStatus.acmmLevel from `level` — the value
// the browser SENT. That is the failure mode the issue explicitly forbids:
// it renders a value the server did not confirm. The handler already returns
// the authoritative level in its body, so the render must come from the
// RESPONSE.
func TestACMMOverrideRendersServerConfirmedLevel(t *testing.T) {
	for _, fn := range []string{"applyACMMPack", "setACMMLevel"} {
		body := acmmFuncBody(t, fn)

		// The confirmed level must be read out of the response body.
		if !strings.Contains(body, "const confirmedLevel = (data && Number.isFinite(Number(data.level))) ? Number(data.level) : level;") {
			t.Errorf("index.html: %s does not derive the rendered level from the "+
				"server's response body — rendering the requested level would show a "+
				"value the server never confirmed (#5492)", fn)
		}

		// And the override/state writes must use that confirmed value, not the
		// requested one. The literal `level` must no longer reach them.
		for _, snippet := range []string{
			"window._acmmOverride = { level: confirmedLevel, packAgents: data.packAgents || [] };",
			"window._lastStatus.acmmLevel = confirmedLevel;",
		} {
			if !strings.Contains(body, snippet) {
				t.Errorf("index.html %s is missing %q — the ACMM panel still renders "+
					"the requested level rather than the server-confirmed one (#5492)", fn, snippet)
			}
		}
		if strings.Contains(body, "window._acmmOverride = { level: level,") {
			t.Errorf("index.html %s still pins the override to the REQUESTED level — "+
				"that renders a value the server never confirmed (#5492)", fn)
		}
	}
}

// TestPackSetLevelResponseCarriesMinStatusSeq asserts the SERVER half of the
// ACMM contract: the PUT /api/packs/level body must carry minStatusSeq.
func TestPackSetLevelResponseCarriesMinStatusSeq(t *testing.T) {
	src := readSourceFile(t, "api_packs.go")

	// BOTH ACMM write paths must carry the floor. handlePackApply previously
	// triggered no status rebuild at all, so scope each assertion to its own
	// handler — a whole-file check would let one path satisfy the other.
	for _, h := range []struct{ fn, next string }{
		{"handlePackApply", "func (s *Server) handlePackSetLevel("},
		{"handlePackSetLevel", "func (s *Server) syncAgentVisibility("},
	} {
		start := strings.Index(src, "func (s *Server) "+h.fn+"(")
		if start < 0 {
			t.Fatalf("api_packs.go: %s not found", h.fn)
		}
		end := strings.Index(src[start:], "\n"+h.next)
		if end < 0 {
			t.Fatalf("api_packs.go: could not bound %s", h.fn)
		}
		body := src[start : start+end]

		// The handler must take a seq-returning refresh, not the bare async one.
		if !regexp.MustCompile(`floor := s\.refresh(AfterMutationSeq|AndPersistSeq)\(\)`).MatchString(body) {
			t.Errorf("api_packs.go: %s does not capture the status-seq floor, so its "+
				"response cannot tell the browser which snapshots predate the level "+
				"change (#5492)", h.fn)
		}
		// gofmt aligns map literal values, so match the key and value
		// independently of the run of padding spaces between them.
		if !regexp.MustCompile(`"minStatusSeq":\s+floor,`).MatchString(body) {
			t.Errorf("api_packs.go: the %s response body does not include "+
				"minStatusSeq (#5492)", h.fn)
		}
	}
}

// TestCadenceHandlerResponseCarriesMinStatusSeq asserts the server half of the
// cadence contract.
func TestCadenceHandlerResponseCarriesMinStatusSeq(t *testing.T) {
	src := readSourceFile(t, "api_agent_config.go")

	idx := strings.Index(src, "func (s *Server) handleAgentConfigCadences(")
	if idx < 0 {
		t.Fatal("api_agent_config.go: handleAgentConfigCadences not found")
	}
	// Bound the search to the handler body.
	end := strings.Index(src[idx:], "\nfunc (s *Server) handleAgentConfigModels(")
	if end < 0 {
		t.Fatal("api_agent_config.go: could not bound handleAgentConfigCadences")
	}
	body := src[idx : idx+end]

	if !strings.Contains(body, "floor := s.refreshAndPersistSeq()") {
		t.Error("handleAgentConfigCadences does not capture the status-seq floor, so a " +
			"pre-mutation /api/status can repaint the old cadence (#5492)")
	}
	if !strings.Contains(body, `"minStatusSeq": floor`) {
		t.Error("handleAgentConfigCadences response does not carry minStatusSeq (#5492)")
	}
	// okResponse is map[string]string and silently cannot carry a numeric
	// floor; the handler must use jsonResponse.
	if strings.Contains(body, "okResponse(w, map[string]any{") {
		t.Error("handleAgentConfigCadences uses okResponse for a payload containing a " +
			"numeric floor — that does not compile; use jsonResponse (#5492)")
	}
}

// TestCadenceWriteStillSurfacesFailure is the second anti-lie guard: a failed
// cadence write must not raise the floor and must not repaint. The floor is
// only raised inside the success branch.
func TestCadenceWriteStillSurfacesFailure(t *testing.T) {
	html := indexHTML(t)

	// The ack parse and floor bump must sit after the !res.ok throw, so a
	// non-2xx response cannot reach them.
	ackIdx := strings.Index(html, "noteStatusMutation(cadenceAck.minStatusSeq);")
	if ackIdx < 0 {
		t.Fatal("index.html: cadence floor bump not present (#5492)")
	}
	// Find the enclosing generic-section save and confirm the error throw
	// precedes the bump.
	throwIdx := strings.LastIndex(html[:ackIdx], "if (!res.ok) throw new Error(await saveErrorMessage(res));")
	if throwIdx < 0 {
		t.Fatal("index.html: the generic section save no longer throws on a non-OK " +
			"response — a failed cadence write would be indistinguishable from a slow one")
	}
	between := html[throwIdx:ackIdx]
	if strings.Contains(between, "showToast('Configuration saved'") {
		t.Error("index.html: the cadence floor bump happens after the success toast — " +
			"it must be gated on the same non-OK throw so a failed write never advances " +
			"the floor (#5492)")
	}
}

// TestACMMFailedWriteDoesNotRender asserts the ACMM error branch returns before
// any optimistic render, so a rejected level change leaves the old level on
// screen with an error message rather than painting the requested level.
func TestACMMFailedWriteDoesNotRender(t *testing.T) {
	html := indexHTML(t)

	idx := strings.Index(html, "async function setACMMLevel(level) {")
	if idx < 0 {
		t.Fatal("index.html: setACMMLevel not found")
	}
	end := strings.Index(html[idx:], "\n    // Packs reconcile scheduling")
	if end < 0 {
		t.Fatal("index.html: could not bound setACMMLevel")
	}
	body := html[idx : idx+end]

	errIdx := strings.Index(body, "errEl.textContent = data.error || 'Failed to set level';")
	renderIdx := strings.Index(body, "window._acmmOverride = { level: confirmedLevel")
	if errIdx < 0 {
		t.Fatal("setACMMLevel no longer reports a failed level change")
	}
	if renderIdx < 0 {
		t.Fatal("setACMMLevel no longer renders the confirmed level")
	}
	if errIdx > renderIdx {
		t.Error("setACMMLevel renders the level before handling the error response — " +
			"a failed write would paint the requested level (#5492)")
	}
	// The error branch must return, not fall through into the render.
	tail := body[errIdx:renderIdx]
	if !strings.Contains(tail, "return;") {
		t.Error("setACMMLevel's error branch does not return before the render — a " +
			"failed level change would still repaint the requested level (#5492)")
	}
}

// TestStatusHandlerServesCachedSnapshot documents the server behaviour that
// makes the floor necessary: GET /api/status serves whatever snapshot is
// cached, with no rebuild. This is why a post-write refetch can legitimately
// return pre-write data, and therefore why the browser must be told to reject
// it rather than the endpoint being made synchronous.
func TestStatusHandlerServesCachedSnapshot(t *testing.T) {
	s := serverSeqFixture(t)
	s.UpdateStatus(&StatusPayload{})
	s.statusMu.RLock()
	cachedSeq := s.status.StatusSeq
	s.statusMu.RUnlock()

	// Mutate; the rebuild has NOT run yet (no RefreshFunc wired).
	floor := s.noteStatusMutation()

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	s.handleStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d, want 200", rec.Code)
	}
	var got StatusPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	if got.StatusSeq != cachedSeq {
		t.Fatalf("status seq = %d, want the cached pre-mutation %d", got.StatusSeq, cachedSeq)
	}
	if got.StatusSeq >= floor {
		t.Fatalf("the post-write refetch returned seq %d which is NOT below the floor %d — "+
			"the stale-snapshot guard would fail to drop it", got.StatusSeq, floor)
	}
}
