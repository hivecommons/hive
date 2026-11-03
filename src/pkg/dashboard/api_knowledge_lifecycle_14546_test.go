package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// lifecycle14546Server holds one fact in each lifecycle state, all tagged lc
// so a tag search reaches every one of them.
func lifecycle14546Server(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "", logger)
	s.RegisterAPI(testDeps(t))

	vault := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	files := [][2]string{
		{"lc-approved.md", "---\ntitle: LC Approved Fact\ntype: pattern\ntags: [lc]\nlayer: project\n---\nApproved body.\n"},
		{"lc-draft.md", "---\ntitle: LC Draft Fact\ntype: pattern\ntags: [lc]\nstate: draft\nlayer: project\n---\nDraft body.\n"},
		{"lc-deprecated.md", "---\ntitle: LC Deprecated Fact\ntype: gotcha\ntags: [lc]\nstate: deprecated\nlayer: project\n---\nDeprecated body.\n"},
		{"lc-superseded.md", "---\ntitle: LC Superseded Fact\ntype: gotcha\ntags: [lc]\nstate: superseded\nsuperseded_by: lc-approved\nlayer: project\n---\nSuperseded body.\n"},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(vault, f[0]), []byte(f[1]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	api := knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, logger)
	if err := api.ConnectVault(vault, "vault"); err != nil {
		t.Fatalf("ConnectVault: %v", err)
	}
	s.deps.Knowledge = api
	return s
}

var lifecycle14546Hidden = []string{"LC Draft Fact", "LC Deprecated Fact", "LC Superseded Fact"}

func lifecycle14546Get(t *testing.T, s *Server, target, role string, verified bool) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	s.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestKnowledgeExportApprovedOnly: the export seeds contributor agents'
// context, so it never carries draft, deprecated or superseded facts — not
// even for a verified owner asking for them (#14546).
func TestKnowledgeExportApprovedOnly(t *testing.T) {
	s := lifecycle14546Server(t)
	for _, tc := range []struct {
		role     string
		verified bool
	}{{"read", false}, {"owner", true}} {
		code, body := lifecycle14546Get(t, s, "/api/knowledge/export?include_states=all", tc.role, tc.verified)
		if code != http.StatusOK {
			t.Fatalf("%s export status = %d body=%s", tc.role, code, body)
		}
		if !strings.Contains(body, "LC Approved Fact") {
			t.Errorf("%s export missing approved fact:\n%s", tc.role, body)
		}
		for _, hidden := range lifecycle14546Hidden {
			if strings.Contains(body, hidden) {
				t.Errorf("%s export leaked %q:\n%s", tc.role, hidden, body)
			}
		}
	}
}

// TestKnowledgeReadsClampNonOwnerToApproved: every agent-reachable read
// returns approved facts only to a caller that is not a verified owner, even
// when it asks for every state (#14546).
func TestKnowledgeReadsClampNonOwnerToApproved(t *testing.T) {
	s := lifecycle14546Server(t)
	targets := []string{
		"/api/knowledge?include_states=all",
		"/api/knowledge/search?tag=lc&include_states=all",
		"/api/knowledge/project?include_states=all",
		"/api/knowledge/vaults/vault/facts?include_states=all",
		"/api/knowledge/toc?include_states=all",
	}
	callers := []struct {
		name     string
		role     string
		verified bool
	}{
		{"anonymous", "", false},
		{"read", "read", false},
		{"read-write", "read-write", false},
		{"proofless owner", "owner", false},
	}
	for _, c := range callers {
		for _, target := range targets {
			code, body := lifecycle14546Get(t, s, target, c.role, c.verified)
			if code != http.StatusOK {
				t.Fatalf("%s %s status = %d body=%s", c.name, target, code, body)
			}
			for _, hidden := range lifecycle14546Hidden {
				if strings.Contains(body, hidden) {
					t.Errorf("%s %s leaked %q: %s", c.name, target, hidden, body)
				}
			}
		}
		for _, slug := range []string{"lc-draft", "lc-deprecated", "lc-superseded"} {
			for _, target := range []string{
				"/api/knowledge/project/" + slug + "?include_states=all",
				"/api/knowledge/entry/" + slug + "?include_states=all",
			} {
				if code, body := lifecycle14546Get(t, s, target, c.role, c.verified); code != http.StatusNotFound {
					t.Errorf("%s %s status = %d, want 404 (body=%s)", c.name, target, code, body)
				}
			}
		}
	}
	if code, _ := lifecycle14546Get(t, s, "/api/knowledge/search?tag=lc&include_states=bogus", "read", false); code != http.StatusBadRequest {
		t.Errorf("malformed include_states status = %d, want 400", code)
	}
}

// TestKnowledgeReadsOwnerOptsIntoStates: a verified owner still sees approved
// knowledge by default and every state on request.
func TestKnowledgeReadsOwnerOptsIntoStates(t *testing.T) {
	s := lifecycle14546Server(t)
	_, body := lifecycle14546Get(t, s, "/api/knowledge", "owner", true)
	for _, hidden := range lifecycle14546Hidden {
		if strings.Contains(body, hidden) {
			t.Errorf("owner default list leaked %q: %s", hidden, body)
		}
	}
	for _, target := range []string{"/api/knowledge?include_states=all", "/api/knowledge/search?tag=lc&include_states=all"} {
		_, body = lifecycle14546Get(t, s, target, "owner", true)
		for _, want := range append([]string{"LC Approved Fact"}, lifecycle14546Hidden...) {
			if !strings.Contains(body, want) {
				t.Errorf("owner %s missing %q: %s", target, want, body)
			}
		}
	}
	if code, body := lifecycle14546Get(t, s, "/api/knowledge/project/lc-draft?include_states=draft", "owner", true); code != http.StatusOK || !strings.Contains(body, "LC Draft Fact") {
		t.Errorf("owner draft fact read = %d %s", code, body)
	}
	if code, _ := lifecycle14546Get(t, s, "/api/knowledge/project/lc-draft", "owner", true); code != http.StatusNotFound {
		t.Errorf("owner draft fact read without include_states = %d, want 404", code)
	}
}
