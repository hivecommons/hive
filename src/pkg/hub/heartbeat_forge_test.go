package hub

import (
	"encoding/json"
	"testing"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// hivecommons/hive#20181: the hub drew a Forgejo-only spoke as a github.com
// hive because the heartbeat carried only github_host.

func TestSanitizeHeartbeatForge(t *testing.T) {
	cases := []struct {
		name, kind, baseURL, login       string
		wantKind, wantBaseURL, wantLogin string
	}{
		{"forgejo", "forgejo", "https://code.tswn.us/", "hive-bot", "forgejo", "https://code.tswn.us", "hive-bot"},
		{"upper-case kind and host", " Forgejo ", "HTTPS://Code.Example.ORG", "", "forgejo", "https://code.example.org", ""},
		{"port and subpath kept", "gitea", "http://git.example.org:3000/gitea/", "x", "gitea", "http://git.example.org:3000/gitea", "x"},
		{"github drops url and login", "github", "https://github.com", "bot", "github", "", ""},
		{"unknown kind dropped", "sourcehut", "https://git.sr.ht", "bot", "", "", ""},
		{"javascript scheme rejected", "forgejo", "javascript:alert(1)", "bot", "forgejo", "", "bot"},
		{"userinfo rejected", "forgejo", "https://user:pw@code.example.org", "", "forgejo", "", ""},
		{"bad host rejected", "forgejo", "https://code_example/", "", "forgejo", "", ""},
		{"markup stripped from login", "forgejo", "https://code.example.org", "<b>bot</b>", "forgejo", "https://code.example.org", "bbotb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeHeartbeatForgeKind(tc.kind); got != tc.wantKind {
				t.Errorf("kind = %q, want %q", got, tc.wantKind)
			}
			if got := sanitizeHeartbeatForgeBaseURL(tc.kind, tc.baseURL); got != tc.wantBaseURL {
				t.Errorf("base url = %q, want %q", got, tc.wantBaseURL)
			}
			if got := sanitizeHeartbeatForgeLogin(tc.kind, tc.login); got != tc.wantLogin {
				t.Errorf("login = %q, want %q", got, tc.wantLogin)
			}
		})
	}
}

// The spoke and hub payload types are separate structs; the forge fields
// must cross the wire under the same JSON names.
func TestHeartbeatForgeFieldsCrossTheWire(t *testing.T) {
	raw, err := json.Marshal(spoke.HeartbeatPayload{ForgeKind: "forgejo", ForgeBaseURL: "https://code.tswn.us", ForgeLogin: "hive-bot"})
	if err != nil {
		t.Fatal(err)
	}
	var got HeartbeatPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.ForgeKind != "forgejo" || got.ForgeBaseURL != "https://code.tswn.us" || got.ForgeLogin != "hive-bot" {
		t.Fatalf("decoded = %q %q %q", got.ForgeKind, got.ForgeBaseURL, got.ForgeLogin)
	}
}

// The identity-only minimal liveness beat (stats_stale) carries no forge
// fields. Without a carry-forward the Forgejo hive drew as github.com again
// until the next full collect.
func TestHeartbeatDegradedBeatCarriesForgeForward(t *testing.T) {
	srv := degradedCarryTestServer(t)
	const hiveID = "carry-forge-hive"

	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"tswn","primary_repo":"hive","repos":["hive"],"forge_kind":"forgejo","forge_base_url":"https://code.tswn.us","forge_login":"hive-bot"}`)
	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"tswn","primary_repo":"hive","stats_stale":true}`)

	entry := registryEntryByID(t, srv, hiveID)
	if entry.ForgeKind != "forgejo" || entry.ForgeBaseURL != "https://code.tswn.us" || entry.ForgeLogin != "hive-bot" {
		t.Fatalf("after stats_stale beat forge = %q %q %q, want forgejo https://code.tswn.us hive-bot carried forward", entry.ForgeKind, entry.ForgeBaseURL, entry.ForgeLogin)
	}

	// A reassignment to another org must not inherit the old tenant's forge.
	postBeat(t, srv, `{"hive_id":"`+hiveID+`","org":"available-x","stats_stale":true}`)
	if entry := registryEntryByID(t, srv, hiveID); entry.ForgeKind != "" {
		t.Fatalf("forge kind %q survived an org change", entry.ForgeKind)
	}
}
