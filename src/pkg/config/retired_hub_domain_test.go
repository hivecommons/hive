package config

import (
	"strings"
	"testing"
)

// TestDefaultHubURLIsNotTheRetiredHost pins the hub URL a spoke falls back to
// when none is configured.
//
// This default is what an unconfigured or partially-configured hive actually
// dials. Pointed at the pre-migration host it does not fail loudly: the apex
// still answers and 301s, but the redirect DROPS the path, and the hosted
// spoke subdomains under it are outside the current wildcard certificate and
// fail the TLS handshake outright. Either way the spoke never completes a
// heartbeat, and a hive that misses maxHeartbeatAge is marked offline.
func TestDefaultHubURLIsNotTheRetiredHost(t *testing.T) {
	c := &Config{}
	c.applyDefaults()

	if c.Hub.URL == "" {
		t.Fatal("applyDefaults left Hub.URL empty; a spoke would have nothing to dial")
	}
	if strings.Contains(c.Hub.URL, "hive.kubestellar.io") {
		t.Errorf("default Hub.URL = %q, which is the retired hub host.\n"+
			"A spoke falling back to this default cannot heartbeat, and the hub marks it offline.",
			c.Hub.URL)
	}
	if !strings.Contains(c.Hub.URL, "hive.hivecommons.dev") {
		t.Errorf("default Hub.URL = %q, expected it to name the current hub host", c.Hub.URL)
	}
}
