package hub

import "testing"

// TestImpersonateKeyMethod covers HubServer.impersonateKey (was 0%, #4716):
// the impersonation domain key must be the HKDF derivation of the hub master
// under infoImpersonateKey — nothing else — so it stays distinct from every
// other domain (C2) and fail-closed on a missing master.
func TestImpersonateKeyMethod(t *testing.T) {
	const master = "impersonate-method-master-secret"
	s := &HubServer{hubSecret: master}

	got := s.impersonateKey()
	if got == "" {
		t.Fatal("impersonateKey() derived empty for a non-empty master")
	}
	if want := deriveDomainKey(master, infoImpersonateKey); got != want {
		t.Errorf("impersonateKey() = %q, want deriveDomainKey(master, infoImpersonateKey) = %q", got, want)
	}
	if got == master {
		t.Error("impersonateKey() must never equal the master secret itself")
	}
	// Distinct from the other domain accessors on the same server.
	for name, other := range map[string]string{
		"session seed": s.sessionSigningSeed(),
		"sso seed":     s.ssoSigningSeed(),
	} {
		if other == got {
			t.Errorf("impersonateKey() collides with the %s", name)
		}
	}

	// Fail-closed: an empty master must never yield a usable impersonation key.
	empty := &HubServer{}
	if k := empty.impersonateKey(); k != "" {
		t.Errorf("impersonateKey() with empty master = %q, want empty (fail-closed)", k)
	}
}
