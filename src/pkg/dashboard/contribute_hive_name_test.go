package dashboard

import (
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
)

func TestContributeHiveNameNormalizesAndSuppressesPlaceholder(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain name kept", "hivecommons hive", "hivecommons hive"},
		{"whitespace collapsed", "  hivecommons \t hive ", "hivecommons hive"},
		{"structure characters dropped", "a|b [c]", "ab c"},
		{"unnamed hive", "", ""},
		{"pool slot placeholder not advertised", "Available slot hosted-available-x", ""},
		{"private pool slot placeholder not advertised", "Available slot (private) hosted-available-x", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, deps := apiServer(t)
			deps.Config.Project.Name = c.in
			if got := s.contributeHiveName(); got != c.want {
				t.Fatalf("contributeHiveName(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	var nilServer *Server
	if got := nilServer.contributeHiveName(); got != "" {
		t.Fatalf("nil server must advertise no name, got %q", got)
	}
}

// The name rides auth_ok: a relay that connects to a named hive is told the
// name, so it need not derive one from a hostname minted before a rename.
func TestAuthOKCarriesHiveName(t *testing.T) {
	setupContributeEnv(t)
	token := "tok-" + randomHex(8)
	if err := saveContributorProfile(&ContributorProfile{
		GitHubUsername:    "alice",
		ContributorID:     "c-alice",
		RegistrationToken: sha256Hex(token),
		TrustTier:         "contributor",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, deps := apiServer(t)
	deps.Config.Project.Name = "hivecommons hive"
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if msg := readMsg(t, conn); msg.Type != "auth_challenge" {
		t.Fatalf("challenge = %+v", msg)
	}
	if err := conn.WriteJSON(WSMessage{Type: "auth_response", RegistrationToken: token, CLIBackend: "claude", Model: "sonnet"}); err != nil {
		t.Fatalf("auth write: %v", err)
	}
	auth := readMsg(t, conn)
	if auth.Type != "auth_ok" || auth.HiveName != "hivecommons hive" {
		t.Fatalf("auth_ok must carry the hive display name, got %+v", auth)
	}
}
