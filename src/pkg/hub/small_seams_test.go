package hub

import (
	"testing"
	"time"
)

func TestDefaultTokenEnvForKind(t *testing.T) {
	cases := []struct {
		kind ForgeKind
		want string
	}{
		{ForgeGitLab, "GITLAB_TOKEN"},
		{ForgeGitea, "GITEA_TOKEN"},
		{ForgeKind("github"), "GH_TOKEN"},
		{ForgeKind(""), "GH_TOKEN"},
	}
	for _, c := range cases {
		if got := defaultTokenEnvForKind(c.kind); got != c.want {
			t.Errorf("defaultTokenEnvForKind(%q) = %q, want %q", c.kind, got, c.want)
		}
	}
}

func TestRevokedSessionsAtCapacity(t *testing.T) {
	var nilStore *revokedSessions
	if nilStore.atCapacity() {
		t.Error("nil store must never report at-capacity")
	}
	store := &revokedSessions{sid: map[string]int64{"s1": time.Now().Unix()}}
	if store.atCapacity() {
		t.Error("one entry must not report at-capacity")
	}
}

// spokeDomainKey resolution order: injected env var wins, then derivation
// from HIVE_HUB_SECRET, then empty (each caller's fail-closed path).
func TestSpokeSessionKeyResolution(t *testing.T) {
	t.Setenv(EnvSessionKey, "injected-key")
	t.Setenv("HIVE_HUB_SECRET", "master")
	if got := SpokeSessionKey(); got != "injected-key" {
		t.Errorf("injected env must win, got %q", got)
	}

	t.Setenv(EnvSessionKey, "")
	derived := SpokeSessionKey()
	if derived == "" || derived == "master" {
		t.Errorf("fallback must derive a domain-separated key from the master, got %q", derived)
	}
	if derived != deriveDomainKey("master", infoSessionKey) {
		t.Error("fallback derivation must match deriveDomainKey(master, infoSessionKey)")
	}

	t.Setenv("HIVE_HUB_SECRET", "")
	if got := SpokeSessionKey(); got != "" {
		t.Errorf("with neither source configured the key must be empty (fail closed), got %q", got)
	}
}
