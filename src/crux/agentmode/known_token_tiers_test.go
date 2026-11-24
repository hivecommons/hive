package agentmode

import (
	"slices"
	"testing"
)

func TestKnownTokenTiers_CoversEveryTokenTierForRole(t *testing.T) {
	roles := []string{"", "scanner", ReviewerRole, SecCheckRole}
	for m := AgentMode(0); int(m) < agentModeCount; m++ {
		for _, role := range roles {
			got := TokenTierForRole(m, role)
			if !slices.Contains(KnownTokenTiers, got) {
				t.Errorf("TokenTierForRole(%v, %q) = %q, not in KnownTokenTiers", m, role, got)
			}
		}
	}
}

func TestLessPrivilegedTokenTier(t *testing.T) {
	tests := []struct {
		tier string
		want string
		ok   bool
	}{
		{"reviewer", "advisor", true},
		{"shared-uid-read", "advisor", true},
		{SecCheckTier, "contributor", true},
		{SecCheckTrustedTier, "trusted", true},
		{"trusted", "contributor", true},
		{"advisor", "", false},
		{"newcomer", "", false},
		{"contributor", "", false},
		{"Contributor", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := LessPrivilegedTokenTier(tt.tier)
		if got != tt.want || ok != tt.ok {
			t.Errorf("LessPrivilegedTokenTier(%q) = (%q, %v), want (%q, %v)", tt.tier, got, ok, tt.want, tt.ok)
		}
	}
}

func TestLessPrivilegedTokenTier_NeverEscalatesAndEndsInKnownTier(t *testing.T) {
	for _, start := range KnownTokenTiers {
		tier := start
		startMode, startKnown := ModeForTokenTier(start)
		for steps := 0; ; steps++ {
			if steps > len(KnownTokenTiers) {
				t.Fatalf("chain from %q does not terminate", start)
			}
			next, ok := LessPrivilegedTokenTier(tier)
			if !ok {
				break
			}
			if !slices.Contains(KnownTokenTiers, next) {
				t.Errorf("chain from %q reaches unknown tier %q", start, next)
			}
			if !startKnown {
				if next != "advisor" {
					t.Errorf("chain from %q, which ModeForTokenTier does not know, goes to %q, want advisor", start, next)
				}
			} else if m, _ := ModeForTokenTier(next); m > startMode {
				t.Errorf("chain from %q escalates to %q", start, next)
			}
			tier = next
		}
	}
}
