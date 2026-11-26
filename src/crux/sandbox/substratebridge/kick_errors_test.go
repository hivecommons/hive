package substratebridge

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// serveKick sends one authorized request straight to k.ServeHTTP.
func serveKick(k *KickServer, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+kickTestToken)
	rec := httptest.NewRecorder()
	k.ServeHTTP(rec, req)
	return rec
}

// TestKickServerPushIntoAnUnusableWorkspaceFails: a workspace path that
// cannot be cleared answers 500 and leaves the bridge waiting for a push.
func TestKickServerPushIntoAnUnusableWorkspaceFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	writeFile(t, file, "x")
	k := &KickServer{Workspace: file, Command: []string{"true"}, Token: kickTestToken}
	k.pushed, k.done = make(chan struct{}), make(chan struct{})
	rec := serveKick(k, http.MethodPut, PathWorkspace, nil)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "clearing") {
		t.Errorf("push into a file = %d %q, want 500 naming the clear", rec.Code, rec.Body.String())
	}
	if k.phase != phaseWaitPush {
		t.Errorf("phase = %d after a failed push, want it still waiting for one", k.phase)
	}
}

// TestKickServerPullOfAMissingWorkspaceFails: a workspace that is gone when
// hive pulls answers 500, not an empty archive that would wipe hive's copy.
func TestKickServerPullOfAMissingWorkspaceFails(t *testing.T) {
	k := &KickServer{Workspace: filepath.Join(t.TempDir(), "gone"), Token: kickTestToken}
	k.phase = phaseExited
	rec := serveKick(k, http.MethodGet, PathWorkspace, nil)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "packing workspace") {
		t.Errorf("pull of a missing workspace = %d %q, want 500 naming the pack", rec.Code, rec.Body.String())
	}
}

// TestKickServerStopOutsideTheRun: /stop before the push or after the
// command exited is a harmless 204; while running it records the stop even
// before the command's cancel exists.
func TestKickServerStopOutsideTheRun(t *testing.T) {
	k := &KickServer{Token: kickTestToken}
	for _, phase := range []int{phaseWaitPush, phaseExited} {
		k.phase = phase
		if rec := serveKick(k, http.MethodPost, PathStop, nil); rec.Code != http.StatusNoContent {
			t.Errorf("stop in phase %d = %d, want 204", phase, rec.Code)
		}
		if k.stopped {
			t.Errorf("stop in phase %d marked the run stopped", phase)
		}
	}
	k.phase = phaseRunning
	if rec := serveKick(k, http.MethodPost, PathStop, nil); rec.Code != http.StatusNoContent || !k.stopped {
		t.Errorf("stop while running = %d stopped=%v, want 204 and stopped", rec.Code, k.stopped)
	}
}

func TestKickServerDefaultWorkspace(t *testing.T) {
	if got := (&KickServer{}).workspaceDir(); got != DefaultWorkspace {
		t.Errorf("workspaceDir() = %q, want DefaultWorkspace", got)
	}
	if got := (&KickServer{Workspace: "/w"}).workspaceDir(); got != "/w" {
		t.Errorf("workspaceDir() = %q, want /w", got)
	}
}

// TestKickServerRejectsAnEmptyToken: a bridge started without a token
// authorizes nothing, whatever the request carries.
func TestKickServerRejectsAnEmptyToken(t *testing.T) {
	k := &KickServer{}
	req := httptest.NewRequest(http.MethodPost, PathDone, nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	k.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("request to a token-less bridge = %d, want 401", rec.Code)
	}
}
