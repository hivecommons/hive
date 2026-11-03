package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #12039: the Experiment Lab mode/scope buttons "did nothing" because the
// PUT handlers wrote NousState.Mode/Scope while GET /api/nous/status served
// NousState.Status["mode"]/["scope"], which only ApplyPanelConfig refreshed
// at boot. Every mutation must be visible on the very next status read.
func TestNousModeAndScopeRoundTripThroughStatus(t *testing.T) {
	s, deps := apiServer(t)
	var persisted []NousPanelSnapshot
	deps.Nous = &NousState{
		Mode:  "observe",
		Scope: "governor",
		Status: map[string]interface{}{
			"status": "active", "mode": "observe", "scope": "governor", "baseline_pct": 100,
		},
		PersistPanel: func(snap NousPanelSnapshot) error { persisted = append(persisted, snap); return nil },
	}

	readStatus := func() map[string]interface{} {
		t.Helper()
		rec := doOwnerGet(s, "/api/nous/status")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status = %d: %s", rec.Code, rec.Body.String())
		}
		var got map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("status body: %v", err)
		}
		return got
	}

	// The lab is repo-only (#12207): status always reports scope "repo".
	if got := readStatus(); got["mode"] != "observe" || got["scope"] != "repo" {
		t.Fatalf("precondition status = %v", got)
	}

	rec := doPut(s, "/api/nous/mode", map[string]interface{}{"mode": "suggest", "force": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT mode = %d: %s", rec.Code, rec.Body.String())
	}
	if got := readStatus(); got["mode"] != "suggest" {
		t.Fatalf("status.mode after PUT = %v, want suggest (the #12039 regression)", got["mode"])
	}
	if got := readStatus(); got["baseline_pct"] != float64(100) || got["status"] != "active" {
		t.Fatalf("unrelated status keys must survive a mode change: %v", got)
	}

	if rec = doPut(s, "/api/nous/scope", map[string]interface{}{"scope": "both"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT scope both = %d, want 400 (repo-only, #12207): %s", rec.Code, rec.Body.String())
	}
	rec = doPut(s, "/api/nous/scope", map[string]interface{}{"scope": "repo"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT scope = %d: %s", rec.Code, rec.Body.String())
	}
	got := readStatus()
	if got["scope"] != "repo" || got["mode"] != "suggest" {
		t.Fatalf("status after scope PUT = mode %v scope %v, want suggest/repo", got["mode"], got["scope"])
	}

	if len(persisted) != 2 || persisted[1].Mode != "suggest" || persisted[1].Scope != "repo" {
		t.Fatalf("persisted snapshots = %+v, want two with final suggest/repo", persisted)
	}

	// Downgrade back to observe (the UI's confirm path sends force=true).
	rec = doPut(s, "/api/nous/mode", map[string]interface{}{"mode": "observe", "force": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT mode observe = %d: %s", rec.Code, rec.Body.String())
	}
	if got := readStatus(); got["mode"] != "observe" {
		t.Fatalf("status.mode after downgrade = %v, want observe", got["mode"])
	}
}

// A bare NousState (nil Status) must still surface the mode after a PUT
// instead of serving an empty status map.
func TestNousSetModeCreatesStatusMirrorWhenNil(t *testing.T) {
	ns := &NousState{Mode: "observe", Scope: "governor"}
	ns.SetMode("evolve")
	ns.SetScope("repo")
	if ns.Status["mode"] != "evolve" || ns.Status["scope"] != "repo" {
		t.Fatalf("Status mirror = %+v, want evolve/repo", ns.Status)
	}
	if ns.Mode != "evolve" || ns.Scope != "repo" {
		t.Fatalf("fields = %q/%q", ns.Mode, ns.Scope)
	}
}

func TestNousModeAndScopeAuthAndValidation(t *testing.T) {
	s, deps := apiServer(t)
	deps.Nous = &NousState{Mode: "observe", Scope: "governor", Status: map[string]interface{}{"mode": "observe", "scope": "governor"}}

	send := func(path, body string, mark func(*http.Request)) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		mark(req)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		return rec
	}
	asOwner := markOwnerRequest
	asReadWrite := func(req *http.Request) { req.Header.Set("X-Hive-Role", "read-write") }
	asProoflessOwner := func(req *http.Request) { req.Header.Set("X-Hive-Role", "owner") }

	cases := []struct {
		name string
		path string
		body string
		mark func(*http.Request)
		want int
	}{
		{"owner mode ok", "/api/nous/mode", `{"mode":"suggest"}`, asOwner, http.StatusOK},
		{"owner scope ok", "/api/nous/scope", `{"scope":"repo"}`, asOwner, http.StatusOK},
		{"read-write mode 403", "/api/nous/mode", `{"mode":"evolve"}`, asReadWrite, http.StatusForbidden},
		{"read-write scope 403", "/api/nous/scope", `{"scope":"both"}`, asReadWrite, http.StatusForbidden},
		{"proofless owner mode 403", "/api/nous/mode", `{"mode":"evolve"}`, asProoflessOwner, http.StatusForbidden},
		{"proofless owner scope 403", "/api/nous/scope", `{"scope":"both"}`, asProoflessOwner, http.StatusForbidden},
		{"malformed mode body 400", "/api/nous/mode", `{"mode":`, asOwner, http.StatusBadRequest},
		{"malformed scope body 400", "/api/nous/scope", `not json`, asOwner, http.StatusBadRequest},
		{"blank mode 400", "/api/nous/mode", `{"mode":"   "}`, asOwner, http.StatusBadRequest},
		{"blank scope 400", "/api/nous/scope", `{"scope":""}`, asOwner, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := send(tc.path, tc.body, tc.mark)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK {
				var j map[string]interface{}
				if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil || j["error"] == "" {
					t.Fatalf("rejections must carry a JSON error the UI can toast, got %q", rec.Body.String())
				}
			}
		})
	}

	// A rejected request must not have moved the served status.
	if deps.Nous.Status["mode"] != "suggest" || deps.Nous.Status["scope"] != "repo" {
		t.Fatalf("rejected PUTs changed status: %+v", deps.Nous.Status)
	}
}
