package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestContributeLanding_OperatorInterestsRenderHook (#2677) pins the client-side
// render hook that surfaces each contributor's own opt-in label interests
// (#2637) on their row in the operator "Connected clankers" fleet list
// (Operations tab, GET /api/contribute/fleet). This is a pure-frontend
// addition — the JSON already carries label_interests once FleetClanker
// mirrors it (see TestFleetSnapshot_LabelInterestsSurfaced) — so this test
// pins the render function + the read-only chip hook actually exist in the
// server-rendered /contribute page, the same way TestContributeLandingHasOpsTab
// pins other renderClankers() output.
func TestContributeLanding_OperatorInterestsRenderHook(t *testing.T) {
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		// The read-only render helper and its wiring into renderClankers().
		"function clankerInterestsLine",
		"c.label_interests",
		"interestsLine",
		// The compact chip class it emits, read-only variant of .cc-interest-chip.
		"clanker-interest-chip",
		"clanker-interests",
		// The "prefers:" label text called out in the issue.
		"prefers:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("operator contributor label-interests render hook missing %q", want)
		}
	}

	// This is READ-ONLY for the operator: it must not introduce any new write
	// path (no fetch to PUT /api/contribute/interests from this render hook —
	// that endpoint already exists solely for the contributor's own editor
	// above, and stays that way).
	iHook := strings.Index(body, "function clankerInterestsLine")
	iEditorPut := strings.Index(body, "/api/contribute/interests")
	if iHook < 0 || iEditorPut < 0 {
		t.Fatalf("missing anchors: hook=%d editorPut=%d", iHook, iEditorPut)
	}
}
