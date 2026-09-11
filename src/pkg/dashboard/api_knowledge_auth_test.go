package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

func serverWithKnowledgeExport(t *testing.T, authToken string) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, authToken, logger)
	s.RegisterAPI(testDeps(t))
	dir := t.TempDir()
	layers := []knowledge.LayerConfig{{Type: knowledge.LayerType("project"), Path: dir}}
	s.deps.Config.Knowledge = config.KnowledgeConfig{
		Enabled: true,
		Layers:  []config.KnowledgeLayer{{Type: "project", Path: dir}},
	}
	s.deps.Knowledge = knowledge.NewKnowledgeAPI(layers, knowledge.KnowledgeConfig{Enabled: true, Layers: layers}, logger)
	return s
}

func TestKnowledgeExportRejectsUnauthenticatedRequest(t *testing.T) {
	s := serverWithKnowledgeExport(t, "dashboard-secret")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/knowledge/export", nil)

	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated export = %d, want 401; body=%q", rec.Code, rec.Body.String())
	}
}

func TestKnowledgeExportAcceptsContributorRegistrationToken(t *testing.T) {
	const token = "contributor-registration-token"
	covSeedContributor(t, &ContributorProfile{
		GitHubUsername:    "knowledge-user",
		ContributorID:     "c-knowledge",
		RegistrationToken: sha256Hex(token),
		TrustTier:         "contributor",
	})
	s := serverWithKnowledgeExport(t, "dashboard-secret")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/knowledge/export", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated contributor export = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Body.String(), "# Agent Knowledge") {
		t.Fatalf("authenticated contributor export body = %q", rec.Body.String())
	}
}
