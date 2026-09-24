package hub

import (
	"strings"
	"testing"
)

// retiredHubHost is the pre-migration hub hostname. It still resolves and its
// apex still answers, so nothing here fails loudly — which is exactly why it
// needs a test. Two separate breakages hide behind it:
//
//  1. The apex 301s to the current host but DROPS the path and query, so a
//     generated client pointed at it silently loses its request path.
//  2. Every hosted spoke subdomain on the retired domain is outside
//     the current wildcard certificate, so it fails the TLS handshake
//     outright rather than redirecting.
//
// A spoke that cannot complete a handshake sends no heartbeat, and a hive with
// no heartbeat is marked offline after maxHeartbeatAge.
var retiredHubHost = "hive." + "kubestellar.io"

// TestOpenAPIServerURLIsNotTheRetiredHost pins the server URL advertised by the
// embedded OpenAPI document.
//
// This reads the EMBEDDED file rather than the working tree: the working tree
// is not what ships, and a spec that is correct on disk but stale in the binary
// would still hand every generated client a redirecting host.
func TestOpenAPIServerURLIsNotTheRetiredHost(t *testing.T) {
	data, err := staticFS.ReadFile("static/openapi.yaml")
	if err != nil {
		t.Fatalf("read embedded openapi.yaml: %v", err)
	}
	body := string(data)

	if strings.Contains(body, "url: https://"+retiredHubHost) {
		t.Errorf("embedded openapi.yaml advertises the retired hub host %q as a server URL.\n"+
			"Clients generated from this spec would target a host that 301s and drops the request path.",
			retiredHubHost)
	}

	// Guard the replacement rather than only the absence, so deleting the
	// server block entirely does not quietly satisfy this test.
	if !strings.Contains(body, "url: https://hive.hivecommons.dev") {
		t.Error("embedded openapi.yaml no longer advertises the current hub host as a server URL")
	}
}

func TestCoupledHubDefaultsUseCurrentHost(t *testing.T) {
	for name, value := range map[string]string{
		"defaultHubPublicURL":     defaultHubPublicURL,
		"defaultHubCanonicalHost": defaultHubCanonicalHost,
		"defaultHubSpokeDomain":   defaultHubSpokeDomain,
	} {
		if strings.Contains(value, retiredHubHost) {
			t.Fatalf("%s still uses retired hub host in %q", name, value)
		}
	}
}
