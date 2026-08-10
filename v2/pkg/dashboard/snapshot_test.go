package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSnapshotAPIMatchesStatus(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = true

	// Seed status data
	status := &StatusPayload{
		Timestamp: "2026-01-01T00:00:00Z",
		Agents: []FrontendAgent{
			{Name: "scanner", State: "running"},
			{Name: "guide", State: "paused"},
		},
		Governor: FrontendGovernor{Mode: "busy", Issues: 5, PRs: 3},
	}
	srv.statusMu.Lock()
	srv.status = status
	srv.statusMu.Unlock()

	// Fetch /api/snapshot
	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var snapData StatusPayload
	if err := json.Unmarshal(w.Body.Bytes(), &snapData); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if snapData.Timestamp != status.Timestamp {
		t.Errorf("timestamp mismatch: snapshot=%q status=%q", snapData.Timestamp, status.Timestamp)
	}
	if len(snapData.Agents) != len(status.Agents) {
		t.Errorf("agent count mismatch: snapshot=%d status=%d", len(snapData.Agents), len(status.Agents))
	}
	for i, a := range snapData.Agents {
		if a.Name != status.Agents[i].Name || a.State != status.Agents[i].State {
			t.Errorf("agent %d mismatch: snapshot=%s/%s status=%s/%s",
				i, a.Name, a.State, status.Agents[i].Name, status.Agents[i].State)
		}
	}
	if snapData.Governor.Mode != status.Governor.Mode {
		t.Errorf("governor mode mismatch: snapshot=%q status=%q", snapData.Governor.Mode, status.Governor.Mode)
	}
}

func TestSnapshotAPIDisabled(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = false

	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 when snapshots disabled, got %d", w.Code)
	}
}

func TestSnapshotAPINoData(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = true
	srv.statusMu.Lock()
	srv.status = nil
	srv.statusMu.Unlock()

	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when no data, got %d", w.Code)
	}
}

func TestSnapshotReadOnly(t *testing.T) {
	srv := newFullServer(t)

	methods := []string{"POST", "PUT", "PATCH", "DELETE"}
	paths := []string{"/snapshot", "/api/snapshot"}

	for _, method := range methods {
		for _, path := range paths {
			req := httptest.NewRequest(method, path, nil)
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)

			if w.Code == http.StatusOK {
				t.Errorf("%s %s should not return 200 (got %d) — snapshot must be read-only",
					method, path, w.Code)
			}
		}
	}
}

func TestSnapshotIsPublicPath(t *testing.T) {
	tests := []struct {
		path   string
		public bool
	}{
		{"/snapshot", true},
		{"/snapshot/light", true},
		{"/api/snapshot", true},
		{"/api/status", false},
		{"/api/agents", false},
	}
	for _, tt := range tests {
		got := isPublicPath(tt.path)
		if got != tt.public {
			t.Errorf("isPublicPath(%q) = %v, want %v", tt.path, got, tt.public)
		}
	}
}

func TestSnapshotCacheHeaders(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.AutoSnapshot = true
	srv.statusMu.Lock()
	srv.status = &StatusPayload{Timestamp: "2026-01-01T00:00:00Z"}
	srv.statusMu.Unlock()

	req := httptest.NewRequest("GET", "/api/snapshot", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotAPI(w, req)

	cc := w.Header().Get("Cache-Control")
	if cc == "" {
		t.Error("snapshot API should set Cache-Control header")
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}
}

func TestSnapshotFrameAncestorsEndpoint(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Dashboard.SnapshotFrameAncestors = []string{"https://docs.projectbluefin.io"}

	req := httptest.NewRequest(http.MethodGet, "/api/snapshot/frame-ancestors", nil)
	w := httptest.NewRecorder()
	srv.handleSnapshotFrameAncestors(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Origins []string `json:"origins"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(body.Origins) != 1 || body.Origins[0] != "https://docs.projectbluefin.io" {
		t.Fatalf("origins = %#v", body.Origins)
	}
}
