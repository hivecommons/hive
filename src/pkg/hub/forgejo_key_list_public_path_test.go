package hub

import (
	"testing"

	"github.com/hivecommons/hive/crux/forgejwt"
)

// A hosted spoke's forgejo.integration key list must reach the spoke with no
// hub session (#19996): Forgejo fetches it anonymously and follows no
// redirect. Only the two exact paths pass; the spoke 404s them unless the
// setting is on.
func TestForgejoKeyListPathsBypassTheHubGate(t *testing.T) {
	if forgejoDiscoveryPath != "/"+forgejwt.DiscoveryPath || forgejoKeyListPath != "/"+forgejwt.KeyListPath {
		t.Fatalf("hub paths %q %q drifted from forgejwt %q %q", forgejoDiscoveryPath, forgejoKeyListPath, forgejwt.DiscoveryPath, forgejwt.KeyListPath)
	}
	for _, uri := range []string{"/.well-known/openid-configuration", "/jwks.json", "/jwks.json?x=1"} {
		if !isSaaSPublicPath(uri) {
			t.Errorf("%s should reach the spoke", uri)
		}
	}
	for _, uri := range []string{"/.well-known/", "/.well-known/openid-configuration/x", "/jwks.json/", "/api/jwks.json", "/.well-known/other"} {
		if isSaaSPublicPath(uri) {
			t.Errorf("%s must stay behind the hub gate", uri)
		}
	}
}
