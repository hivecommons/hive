package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestHandlePackSetLevelNotifiesACMMLevelChanged(t *testing.T) {
	srv := newFullServer(t)
	var gotPrev, gotNext int
	calls := 0
	srv.deps.OnACMMLevelChanged = func(prev, next int) {
		calls++
		gotPrev, gotNext = prev, next
	}

	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":5}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != 200 {
		t.Fatalf("PUT /api/packs/level status = %d, body=%s", w.Code, w.Body.String())
	}
	if calls != 1 || gotPrev != 2 || gotNext != 5 {
		t.Fatalf("OnACMMLevelChanged calls=%d prev=%d next=%d, want one 2 -> 5", calls, gotPrev, gotNext)
	}
}

func TestHandlePackSetLevelNilACMMLevelHookOK(t *testing.T) {
	srv := newFullServer(t)

	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":3}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != 200 {
		t.Fatalf("PUT /api/packs/level with nil hook status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestHandlePackApplyNotifiesACMMLevelChanged(t *testing.T) {
	srv := newFullServer(t)
	var gotPrev, gotNext int
	calls := 0
	srv.deps.OnACMMLevelChanged = func(prev, next int) {
		calls++
		gotPrev, gotNext = prev, next
	}

	req := httptest.NewRequest("POST", "/api/packs/5/apply", nil)
	req.SetPathValue("level", "5")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackApply(w, req)

	if w.Code != 200 {
		t.Fatalf("POST /api/packs/5/apply status = %d, body=%s", w.Code, w.Body.String())
	}
	if calls != 1 || gotPrev != 2 || gotNext != 5 {
		t.Fatalf("OnACMMLevelChanged calls=%d prev=%d next=%d, want one 2 -> 5", calls, gotPrev, gotNext)
	}
}

func TestHandlePackSetLevelDefaultLeavesLevelHolds(t *testing.T) {
	srv, calls := newLevelHoldGuardDashboardServer(t, true)
	setDashboardACMMLevel(t, srv, 5)

	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":6}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body struct {
		LevelHoldsReleased int  `json:"level_holds_released"`
		ReleaseLevelHolds  bool `json:"release_level_holds"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.LevelHoldsReleased != 0 || body.ReleaseLevelHolds {
		t.Fatalf("response = %+v, want no release by default", body)
	}
	if calls.removes != 0 || calls.releaseComments != 0 {
		t.Fatalf("default level change removed/commented level holds: removes=%d comments=%d", calls.removes, calls.releaseComments)
	}
}

func TestHandlePackSetLevelReleaseLevelHoldsTrueReleasesOnlyLevelAppliedHolds(t *testing.T) {
	srv, calls := newLevelHoldGuardDashboardServer(t, true)
	setDashboardACMMLevel(t, srv, 5)

	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":6,"release_level_holds":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body struct {
		LevelHoldsReleased int  `json:"level_holds_released"`
		ReleaseLevelHolds  bool `json:"release_level_holds"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.LevelHoldsReleased != 1 || !body.ReleaseLevelHolds {
		t.Fatalf("response = %+v, want one released and release=true", body)
	}
	if calls.removes != 1 || calls.removedNumber != 11 || calls.releaseComments != 1 {
		t.Fatalf("release calls removes=%d removed=%d comments=%d, want only PR 11 released/commented", calls.removes, calls.removedNumber, calls.releaseComments)
	}
}

func TestHandlePackSetLevelNoHoldsAndNonCrossingDoNotPrompt(t *testing.T) {
	t.Run("no holds", func(t *testing.T) {
		srv, _ := newLevelHoldGuardDashboardServer(t, false)
		setDashboardACMMLevel(t, srv, 5)
		req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":6}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		markOwnerRequest(req)
		srv.handlePackSetLevel(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
		}
	})
	t.Run("non crossing", func(t *testing.T) {
		srv := newFullServer(t)
		setDashboardACMMLevel(t, srv, 3)
		req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":4}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		markOwnerRequest(req)
		srv.handlePackSetLevel(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
		}
	})
}

func TestHandlePackApplyDoesNotReleaseLevelHolds(t *testing.T) {
	srv, calls := newLevelHoldGuardDashboardServer(t, true)
	setDashboardACMMLevel(t, srv, 5)

	req := httptest.NewRequest("POST", "/api/packs/6/apply", nil)
	req.SetPathValue("level", "6")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackApply(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if calls.removes != 0 || calls.releaseComments != 0 {
		t.Fatalf("pack apply removed/commented level holds: removes=%d comments=%d", calls.removes, calls.releaseComments)
	}
}

func TestHandlePackApplyInvalidLevelDoesNotReleaseLevelHolds(t *testing.T) {
	srv, calls := newLevelHoldGuardDashboardServer(t, true)
	setDashboardACMMLevel(t, srv, 5)

	req := httptest.NewRequest("POST", "/api/packs/999/apply", strings.NewReader(`{"release_level_holds":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("level", "999")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackApply(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	if calls.removes != 0 || calls.releaseComments != 0 {
		t.Fatalf("invalid level released/commented level holds: removes=%d comments=%d", calls.removes, calls.releaseComments)
	}
}

type levelHoldGuardCalls struct {
	removes         int
	removedNumber   int
	releaseComments int
}

func newLevelHoldGuardDashboardServer(t *testing.T, withHold bool) (*Server, *levelHoldGuardCalls) {
	t.Helper()
	srv := newFullServer(t)
	calls := &levelHoldGuardCalls{}
	for _, name := range []string{"scanner", "architect", "strategist", "reviewer"} {
		if ac, ok := srv.deps.Config.Agents[name]; ok {
			ac.Enabled = true
			srv.deps.Config.Agents[name] = ac
		}
	}
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/testorg/testrepo/pulls":
			if !withHold {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[
				{"number":11,"title":"level hold","labels":[{"name":"hold"}]},
				{"number":12,"title":"plain human hold","labels":[{"name":"hold"}]},
				{"number":13,"title":"paused","labels":[{"name":"hive-pause/test"}]}
			]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/testorg/testrepo/issues/11/comments":
			_, _ = w.Write([]byte(`[{"id":101,"body":"<!-- hive:level-hold {\"agent\":\"quality\"} -->\nheld","user":{"login":"hive[bot]"}}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/testorg/testrepo/issues/11/events":
			_, _ = w.Write([]byte(`[{"event":"labeled","created_at":"2026-10-01T14:00:00Z","actor":{"login":"hive[bot]"},"label":{"name":"hold"}}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/testorg/testrepo/issues/12/comments":
			_, _ = w.Write([]byte(`[{"id":102,"body":"human hold","user":{"login":"alice"}}]`))
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/testorg/testrepo/issues/11/labels/hold":
			calls.removes++
			calls.removedNumber = 11
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/testorg/testrepo/issues/11/comments":
			calls.releaseComments++
			_, _ = w.Write([]byte(`{"id":201}`))
		default:
			t.Fatalf("unexpected GitHub request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(ghSrv.Close)
	srv.deps.GHClient = github.NewClientForTest(ghSrv.URL, "testorg", []string{"testrepo"}, srv.logger)
	srv.deps.GHClient.SetAppBotLogin("hive[bot]")
	return srv, calls
}

func setDashboardACMMLevel(t *testing.T, srv *Server, level int) {
	t.Helper()
	srv.deps.Config.ACMMLevel = &level
	if srv.deps.AgentMgr != nil {
		srv.deps.AgentMgr.SetACMMLevel(level)
	}
}
