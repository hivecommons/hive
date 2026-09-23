package dashboard

import (
	"context"
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

func TestKnowledgeExportContributorTokenGetsBoundedSummary(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTOR_KNOWLEDGE_EXPORT_MAX_FACTS", "2")
	const token = "contributor-summary-token"
	covSeedContributor(t, &ContributorProfile{
		GitHubUsername:    "summary-user",
		ContributorID:     "c-summary",
		RegistrationToken: sha256Hex(token),
		TrustTier:         "contributor",
	})
	s := serverWithKnowledgeExport(t, "dashboard-secret")
	vaultDir := t.TempDir()
	if err := s.deps.Knowledge.ConnectVault(vaultDir, "project"); err != nil {
		t.Fatalf("ConnectVault: %v", err)
	}
	longBody := strings.Repeat("long contributor knowledge body ", 30)
	if _, err := s.deps.Knowledge.ImportFacts(context.Background(), knowledge.LayerType("project"), strings.Join([]string{
		"# First fact\n" + longBody,
		"# Second fact\nshort body",
		"# Third fact\nthis fact should be omitted from contributor startup",
	}, "\n\n"), "markdown"); err != nil {
		t.Fatalf("ImportFacts: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/knowledge/export", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("contributor export = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"bounded startup summary",
		"First fact",
		"Second fact",
		"Hive knowledge export truncated: 1 entries omitted",
		"hive knowledge",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("contributor summary missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Third fact") {
		t.Fatalf("contributor summary included an omitted fact:\n%s", body)
	}
	if strings.Contains(body, strings.Repeat("long contributor knowledge body ", 20)) {
		t.Fatalf("contributor summary included an unbounded fact body:\n%s", body)
	}

	ownerReq := httptest.NewRequest(http.MethodGet, "/api/knowledge/export", nil)
	markOwnerRequest(ownerReq)
	ownerRec := httptest.NewRecorder()
	s.handleKnowledgeExport(ownerRec, ownerReq)
	ownerBody := ownerRec.Body.String()
	if !strings.Contains(ownerBody, "Third fact") {
		t.Fatalf("owner export should remain full; got:\n%s", ownerBody)
	}
	if strings.Contains(ownerBody, "bounded startup summary") {
		t.Fatalf("owner export should not use the contributor summary marker:\n%s", ownerBody)
	}
}
