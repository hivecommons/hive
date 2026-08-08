package dashboard

import (
	"os"
	"strings"
	"testing"
)

func TestContributeTrustTierListingsIncludeMerger(t *testing.T) {
	body := renderContributePage(t)
	for _, want := range []string{
		`<tr><td>Merger</td><td>Granted by a maintainer/owner</td><td>Queue others' PRs for auto-merge — never your own</td></tr>`,
		`var ADMIN_TIER_ORDER=['newcomer','contributor','trusted','merger','advisor'];`,
		`.tier-badge.tier-merger`,
		`var known={newcomer:1,contributor:1,trusted:1,merger:1,advisor:1};`,
		`var opts=['newcomer','contributor','trusted','merger','advisor'].map(function(t){`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/contribute missing merger trust-tier UI fragment %q", want)
		}
	}
}

func TestGovernorHubTierListingsIncludeMerger(t *testing.T) {
	body, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		`const tiers = ['newcomer', 'contributor', 'trusted', 'merger', 'advisor'];`,
		`merger:{mph:30,mpd:200,mc:5}`,
		`merger:      'var(--pink, #f778ba)'`,
		`const CONTRIBUTOR_FILTER_TIERS = ['newcomer', 'contributor', 'trusted', 'merger', 'advisor', 'revoked'];`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("static Governor Hub UI missing merger tier fragment %q", want)
		}
	}
}
