package dashboard

import "testing"

// ContributorTrustTierByLogin is the registry read behind the contributor-lane
// auto-merge registry admission path: it must return the operator-set tier
// for a login regardless of case, and ok=false for an unknown login so the
// lane falls through to the claim-marker path.
func TestContributorTrustTierByLogin(t *testing.T) {
	covSeedContributor(t, &ContributorProfile{GitHubUsername: "MrBobbyTables", ContributorID: "c-bob", TrustTier: "trusted"})
	for _, login := range []string{"MrBobbyTables", "mrbobbytables", "@mrbobbytables", " MRBOBBYTABLES "} {
		tier, ok := ContributorTrustTierByLogin(login)
		if !ok || tier != "trusted" {
			t.Fatalf("login %q: tier=%q ok=%v, want trusted/true", login, tier, ok)
		}
	}
	for _, login := range []string{"", "@", "nobody", "c-bob", "../MrBobbyTables"} {
		if tier, ok := ContributorTrustTierByLogin(login); ok || tier != "" {
			t.Fatalf("login %q: tier=%q ok=%v, want no entry", login, tier, ok)
		}
	}
}
