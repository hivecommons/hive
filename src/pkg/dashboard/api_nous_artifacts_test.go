package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNousArtifactSyncRequiresOwner(t *testing.T) {
	s, deps := apiServerWithNous(t)
	deps.Nous.ArtifactSync = func(context.Context) (interface{}, error) { t.Fatal("must not run"); return nil, nil }
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/nous/artifacts/sync", nil)
	s.mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("anonymous sync = %d, want rejection", rec.Code)
	}
}

func TestNousArtifactSyncNotConfigured(t *testing.T) {
	s, _ := apiServerWithNous(t)
	if rec := doPostOwner(s, "/api/nous/artifacts/sync", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	s2, _ := apiServer(t)
	if rec := doPostOwner(s2, "/api/nous/artifacts/sync", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("nil nous code = %d", rec.Code)
	}
}

func TestNousArtifactSyncRunsAndAudits(t *testing.T) {
	s, deps := apiServerWithNous(t)
	calls := 0
	deps.Nous.ArtifactSync = func(context.Context) (interface{}, error) {
		calls++
		return map[string]interface{}{"pushed": []string{"run-1"}}, nil
	}
	rec := doPostOwner(s, "/api/nous/artifacts/sync", nil)
	if rec.Code != http.StatusOK || calls != 1 {
		t.Fatalf("code = %d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"synced"`) || !strings.Contains(body, "run-1") {
		t.Fatalf("body = %s", body)
	}
	entries := s.GetAudit().Recent(10)
	if len(entries) == 0 || entries[0].Action != nousArtifactSyncAudit {
		t.Fatalf("audit = %+v", entries)
	}
}

func TestNousArtifactSyncError(t *testing.T) {
	s, deps := apiServerWithNous(t)
	deps.Nous.ArtifactSync = func(context.Context) (interface{}, error) { return nil, errors.New("budget conserve") }
	rec := doPostOwner(s, "/api/nous/artifacts/sync", nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "budget conserve") {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	entries := s.GetAudit().Recent(10)
	if len(entries) == 0 || !strings.Contains(entries[0].Detail, "budget conserve") {
		t.Fatalf("audit = %+v", entries)
	}
}
