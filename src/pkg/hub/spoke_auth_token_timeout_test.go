package hub

import (
	"testing"
	"time"
)

func authTokenTestHub(t *testing.T) *HubServer {
	s := bulkTestHub(t)
	s.clusters = map[string]ClusterConfig{"hive-oke": {ID: "hive-oke", InCluster: true}}
	s.registry.Hives = []RegistryEntry{{ID: "h1", ClusterID: "hive-oke"}}
	return s
}

// A hung token load returns within the bound instead of blocking the request.
func TestLoadSpokeAuthTokenTimesOut(t *testing.T) {
	writeFakeKubectl(t, "#!/bin/sh\nexec sleep 30\n")
	s := authTokenTestHub(t)
	start := time.Now()
	if tok := s.loadSpokeAuthToken(&s.registry.Hives[0]); tok != "" {
		t.Fatalf("token = %q, want empty", tok)
	}
	if d := time.Since(start); d > spokeAuthTokenLoadTimeout+5*time.Second {
		t.Fatalf("hung load took %s, want about %s", d, spokeAuthTokenLoadTimeout)
	}
}

// Expired cache plus failed refresh serves the last good token inside the
// grace period, and nothing once the grace has passed.
func TestSpokeProxyAuthTokenStaleGrace(t *testing.T) {
	writeFakeKubectl(t, "#!/bin/sh\nexit 1\n")
	s := authTokenTestHub(t)
	now := time.Now()
	s.spokeProxyAuthCache["h1"] = spokeProxyAuthEntry{
		token: "good", expires: now.Add(-time.Minute), graceUntil: now.Add(time.Minute),
	}
	if got := s.spokeProxyAuthToken("h1"); got != "good" {
		t.Fatalf("within grace got %q, want good", got)
	}
	s.spokeProxyAuthCache["h1"] = spokeProxyAuthEntry{
		token: "good", expires: now.Add(-time.Hour), graceUntil: now.Add(-time.Minute),
	}
	if got := s.spokeProxyAuthToken("h1"); got != "" {
		t.Fatalf("past grace got %q, want empty (fail fast)", got)
	}
	if got := s.spokeProxyAuthToken("unknown"); got != "" {
		t.Fatalf("no token at all got %q, want empty", got)
	}
}
