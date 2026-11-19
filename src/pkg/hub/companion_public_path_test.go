package hub

import "testing"

// The desktop companion's routes bypass the hub gate (the spoke checks the
// companion token); the token page stays behind it (#18596).
func TestCompanionRoutesBypassTheHubGateButNotTheTokenPage(t *testing.T) {
	for _, uri := range []string{
		"/api/companion/info",
		"/api/companion/meetings",
		"/api/companion/meetings/mtg-1/audio?channel=mic&seq=0",
		"/api/companion/meetings/mtg-1/replies?after=3",
	} {
		if !isSaaSPublicPath(uri) {
			t.Errorf("%s should reach the spoke", uri)
		}
	}
	for _, uri := range []string{"/companion/token", "/companion/token/revoke", "/api/companion/token", "/api/companion/meetingsx", "/api/meetings"} {
		if isSaaSPublicPath(uri) {
			t.Errorf("%s must stay behind the hub gate", uri)
		}
	}
}
