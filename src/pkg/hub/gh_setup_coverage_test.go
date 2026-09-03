package hub

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestGitHubSetupMatchesAndRedirects(t *testing.T) {
	s := &HubServer{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.registry.Hives = []RegistryEntry{
		{ID: "match", Org: "ExampleOrg", ProjectName: "Prod", DashboardURL: "console.example.com/base", GitHubAppID: config.PublicGitHubAppID},
		{ID: "wrong-app", Org: "ExampleOrg", DashboardURL: "wrong.example.com", GitHubAppID: 12345},
		{ID: "enterprise", Org: "ExampleOrg", DashboardURL: "ghe.example.com", GitHubHost: "github.example.com", GitHubAppID: config.PublicGitHubAppID},
		{ID: "other", Org: "OtherOrg", DashboardURL: "other.example.com", GitHubAppID: config.PublicGitHubAppID},
	}
	matches := s.githubSetupMatches(" exampleorg ")
	if len(matches) != 1 || matches[0].ID != "match" {
		t.Fatalf("githubSetupMatches = %+v, want only public github.com match", matches)
	}
	target, ok := spokeGitHubSetupURL(matches[0], "installation_id=42&setup_action=install")
	if !ok || target != "https://console.example.com/gh-setup?installation_id=42&setup_action=install" {
		t.Fatalf("spokeGitHubSetupURL = %q, %v", target, ok)
	}
	if _, ok := spokeGitHubSetupURL(RegistryEntry{}, "q=1"); ok {
		t.Fatal("hive without dashboard/snapshot URL should not redirect")
	}
	if _, ok := spokeGitHubSetupURL(RegistryEntry{DashboardURL: ":// bad"}, "q=1"); ok {
		t.Fatal("invalid dashboard URL should not redirect")
	}

	s.githubInstallationAccount = func(ctx context.Context, installationID int64) (string, error) {
		if installationID != 42 {
			return "", fmt.Errorf("unexpected installation %d", installationID)
		}
		return "ExampleOrg", nil
	}
	req := httptest.NewRequest(http.MethodGet, "/gh-setup?installation_id=42&setup_action=install", nil)
	rec := httptest.NewRecorder()
	s.handleGitHubAppSetupRouter(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != target {
		t.Fatalf("setup callback code=%d location=%q body=%q", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
}

func TestGitHubSetupRouterFallbackPages(t *testing.T) {
	s := &HubServer{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, tc := range []struct {
		name string
		url  string
		code int
		body string
	}{
		{"missing installation", "/gh-setup?setup_action=install", http.StatusOK, "Install requested"},
		{"bad installation", "/gh-setup?installation_id=0042", http.StatusBadRequest, "Invalid setup callback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.handleGitHubAppSetupRouter(rec, httptest.NewRequest(http.MethodGet, tc.url, nil))
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("code=%d body=%q, want %d containing %q", rec.Code, rec.Body.String(), tc.code, tc.body)
			}
		})
	}

	s.githubInstallationAccount = func(context.Context, int64) (string, error) { return "", fmt.Errorf("api down") }
	rec := httptest.NewRecorder()
	s.handleGitHubAppSetupRouter(rec, httptest.NewRequest(http.MethodGet, "/gh-setup?installation_id=42", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Install received") {
		t.Fatalf("lookup error fallback code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestGitHubSetupChooserEscapesLabels(t *testing.T) {
	s := &HubServer{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	matches := []RegistryEntry{
		{ID: "one", ProjectName: "<Prod>", DashboardURL: "https://one.example.com"},
		{ID: "two", Name: "Two", SnapshotURL: "two.example.com"},
		{ID: "skip", ProjectName: "Skip", DashboardURL: "http:// bad host"},
	}
	rec := httptest.NewRecorder()
	s.renderGitHubSetupChooser(rec, matches, "installation_id=42")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "&lt;Prod&gt;") || !strings.Contains(body, "https://one.example.com/gh-setup?installation_id=42") || !strings.Contains(body, "https://two.example.com/gh-setup?installation_id=42") {
		t.Fatalf("unexpected chooser body: code=%d body=%q", rec.Code, body)
	}
	if strings.Contains(body, "Skip") {
		t.Fatalf("unroutable hive should be omitted from chooser: %q", body)
	}
}

func TestGitHubSetupRouterNoMatchAndChooser(t *testing.T) {
	s := &HubServer{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.githubInstallationAccount = func(context.Context, int64) (string, error) { return "ExampleOrg", nil }

	rec := httptest.NewRecorder()
	s.handleGitHubAppSetupRouter(rec, httptest.NewRequest(http.MethodGet, "/gh-setup?installation_id=42", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No matching hive yet") {
		t.Fatalf("no-match callback code=%d body=%q", rec.Code, rec.Body.String())
	}

	s.registry.Hives = []RegistryEntry{
		{ID: "one", Org: "ExampleOrg", ProjectName: "One", DashboardURL: "one.example.com", GitHubAppID: config.PublicGitHubAppID},
		{ID: "two", Org: "ExampleOrg", ProjectName: "Two", DashboardURL: "two.example.com", GitHubAppID: config.PublicGitHubAppID},
	}
	rec = httptest.NewRecorder()
	s.handleGitHubAppSetupRouter(rec, httptest.NewRequest(http.MethodGet, "/gh-setup?installation_id=42&setup_action=update", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Choose a hive") || !strings.Contains(body, "https://one.example.com/gh-setup?installation_id=42&amp;setup_action=update") || !strings.Contains(body, "https://two.example.com/gh-setup?installation_id=42&amp;setup_action=update") {
		t.Fatalf("chooser callback code=%d body=%q", rec.Code, body)
	}
}
