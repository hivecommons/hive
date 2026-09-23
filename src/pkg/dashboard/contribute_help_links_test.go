package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/config"
)

func putHelpLinksWithRole(s *Server, role string, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/contribute/help-links", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

func TestContributeHelpLinksRenderingUsesTextContent(t *testing.T) {
	raw, err := os.ReadFile("contribute_landing.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		`id="onboarding-help-links"`,
		`id="ops-help-links"`,
		`a.textContent=l.label`,
		`/api/contribute/help-links`,
		`contribute.help_links`,
		`https://discord.gg/`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("contribute_landing.go missing %q", want)
		}
	}
	if strings.Contains(body, `a.innerHTML=l.label`) {
		t.Fatal("help link labels must not be rendered via innerHTML")
	}
}

func TestContributeHelpLinksAuthAndValidation(t *testing.T) {
	s, deps := apiServer(t)
	for _, tc := range []struct {
		role string
		want int
	}{
		{"read", http.StatusForbidden},
		{"read-write", http.StatusOK},
		{"owner", http.StatusOK},
	} {
		rec := putHelpLinksWithRole(s, tc.role, `{"help_links":[{"label":"Chat <b>now</b>","url":"https://discord.gg/hive"}]}`)
		if rec.Code != tc.want {
			t.Fatalf("role %q status = %d, want %d; body=%s", tc.role, rec.Code, tc.want, rec.Body.String())
		}
	}
	if deps.Config.Contribute.HelpLinks[0].Label != "Chat <b>now</b>" {
		t.Fatalf("label should be stored as plain text for textContent rendering: %#v", deps.Config.Contribute.HelpLinks[0])
	}
	rec := putHelpLinksWithRole(s, "owner", `{"help_links":[{"label":"Bad","url":"javascript:alert(1)"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("javascript URL status = %d, want 400", rec.Code)
	}
}

func TestContributeHelpLinksStatusDefaultAndGovernor(t *testing.T) {
	s, deps := apiServer(t)
	rec := doGet(s, "/api/contribute/status")
	var status map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	links, ok := status["help_links"].([]any)
	if !ok || len(links) < 2 {
		t.Fatalf("status help_links = %#v", status["help_links"])
	}

	deps.Config.Contribute.HelpLinks = []config.ContributeHelpLink{{Label: "Docs", URL: "https://example.test/docs"}}
	req := httptest.NewRequest(http.MethodGet, "/api/config/governor", nil)
	w := httptest.NewRecorder()
	s.handleGovernorConfigGet(w, req)
	var gov map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &gov); err != nil {
		t.Fatal(err)
	}
	hub := gov["hub"].(map[string]any)
	if got := hub["contribute_help_links"].([]any)[0].(map[string]any)["url"]; got != "https://example.test/docs" {
		t.Fatalf("governor help link url = %#v", got)
	}
}

func TestContributeHelpLinksOnAuthOK(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	s.deps = &Dependencies{Config: &config.Config{}}
	s.deps.Config.Contribute.HelpLinks = []config.ContributeHelpLink{{Label: "Contributor docs", URL: "https://example.test/docs"}}
	token, _ := registerWSUser(t, s, "help-links-user")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	readMsg(t, conn) // challenge
	conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: token, CLIBackend: "claude"})
	authOK := readMsg(t, conn)
	if authOK.Type != "auth_ok" {
		t.Fatalf("expected auth_ok, got %s", authOK.Type)
	}
	if len(authOK.HelpLinks) != 1 || authOK.HelpLinks[0].URL != "https://example.test/docs" {
		t.Fatalf("auth_ok help_links = %#v", authOK.HelpLinks)
	}
}
