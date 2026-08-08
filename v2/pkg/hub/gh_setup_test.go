package hub

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newGitHubSetupTestServer(resolve func(context.Context, int64) (string, error), hives ...RegistryEntry) *HubServer {
	return &HubServer{
		mux:                       http.NewServeMux(),
		logger:                    slog.Default(),
		registry:                  Registry{Hives: hives},
		githubInstallationAccount: resolve,
	}
}

func TestGitHubAppSetupRouterRedirectsMatchingSpoke(t *testing.T) {
	s := newGitHubSetupTestServer(func(_ context.Context, id int64) (string, error) {
		if id != 12345 {
			t.Fatalf("installation id = %d, want 12345", id)
		}
		return "AcmeOrg", nil
	}, RegistryEntry{ID: "h1", Org: "acmeorg", DashboardURL: "https://spoke.example.com/dashboard"})

	req := httptest.NewRequest("GET", "/gh-setup?installation_id=12345&setup_action=install&state=abc", nil)
	w := httptest.NewRecorder()
	s.handleGitHubAppSetupRouter(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusFound, w.Body.String())
	}
	want := "https://spoke.example.com/gh-setup?installation_id=12345&setup_action=install&state=abc"
	if got := w.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestGitHubAppSetupRouterSkipsEnterpriseHiveWithSameOrg(t *testing.T) {
	s := newGitHubSetupTestServer(func(context.Context, int64) (string, error) {
		return "acme", nil
	},
		RegistryEntry{ID: "ghe", Org: "acme", GitHubHost: "github.ibm.com", DashboardURL: "https://ghe.example.com"},
		RegistryEntry{ID: "public", Org: "acme", GitHubHost: "github.com", DashboardURL: "https://public.example.com"},
	)

	req := httptest.NewRequest("GET", "/gh-setup?installation_id=55&setup_action=install", nil)
	w := httptest.NewRecorder()
	s.handleGitHubAppSetupRouter(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusFound, w.Body.String())
	}
	want := "https://public.example.com/gh-setup?installation_id=55&setup_action=install"
	if got := w.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestGitHubAppSetupRouterRequestRendersFriendlyPage(t *testing.T) {
	s := newGitHubSetupTestServer(func(context.Context, int64) (string, error) {
		t.Fatal("resolver should not be called for a pure request callback")
		return "", nil
	})

	req := httptest.NewRequest("GET", "/gh-setup?setup_action=request", nil)
	w := httptest.NewRecorder()
	s.handleGitHubAppSetupRouter(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Install requested/received") {
		t.Fatalf("friendly page missing expected text: %s", w.Body.String())
	}
}

func TestGitHubAppSetupRouterUnknownOrgRendersNoMatch(t *testing.T) {
	s := newGitHubSetupTestServer(func(context.Context, int64) (string, error) {
		return "missing-org", nil
	}, RegistryEntry{ID: "h1", Org: "other", DashboardURL: "https://spoke.example.com"})

	req := httptest.NewRequest("GET", "/gh-setup?installation_id=99&setup_action=install", nil)
	w := httptest.NewRecorder()
	s.handleGitHubAppSetupRouter(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "No hive registered for org missing-org") {
		t.Fatalf("no-match page missing expected text: %s", w.Body.String())
	}
}

func TestGitHubAppSetupRouterRejectsNonNumericInstallationID(t *testing.T) {
	called := false
	s := newGitHubSetupTestServer(func(context.Context, int64) (string, error) {
		called = true
		return "", errors.New("unexpected")
	})

	req := httptest.NewRequest("GET", "/gh-setup?installation_id=abc&setup_action=install", nil)
	w := httptest.NewRecorder()
	s.handleGitHubAppSetupRouter(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if called {
		t.Fatal("resolver called for invalid installation id")
	}
}

func TestGitHubAppSetupRouterMultipleMatchesRendersChooser(t *testing.T) {
	s := newGitHubSetupTestServer(func(context.Context, int64) (string, error) {
		return "acme", nil
	},
		RegistryEntry{ID: "h1", Name: "First hive", Org: "acme", DashboardURL: "https://one.example.com"},
		RegistryEntry{ID: "h2", ProjectName: "Second project", Org: "ACME", DashboardURL: "two.example.com/root"},
	)

	req := httptest.NewRequest("GET", "/gh-setup?installation_id=7&setup_action=install", nil)
	w := httptest.NewRecorder()
	s.handleGitHubAppSetupRouter(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Choose a hive",
		`https://one.example.com/gh-setup?installation_id=7&amp;setup_action=install`,
		`https://two.example.com/gh-setup?installation_id=7&amp;setup_action=install`,
		"First hive",
		"Second project",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("chooser missing %q: %s", want, body)
		}
	}
}
