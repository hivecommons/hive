package automerge

// The trusted-bot lane lets the self-authored sweep merge dependency-bot PRs
// through the same gates as the App's own. These tests pin the lane boundary:
// only logins in the live TrustedBotAuthors set qualify, the match is
// case-insensitive on the exact login, a nil resolver keeps the sweep App-only,
// and the lane string recorded on the mutation claim distinguishes the two.

import (
	"testing"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

func newTrustedBotEngine(t *testing.T, trusted func() map[string]bool) *Engine {
	t.Helper()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "http://127.0.0.1:0")
	client.SetAppBotLogin(testHiveAppBotLogin)
	return New(client, Options{TrustedBotAuthors: trusted})
}

func TestSweepLaneForAuthor(t *testing.T) {
	trusted := func() map[string]bool { return map[string]bool{"dependabot[bot]": true} }
	c := newTrustedBotEngine(t, trusted)

	cases := []struct {
		author   string
		wantLane string
		wantOK   bool
	}{
		{testHiveAppBotLogin, "self-authored", true},
		{"dependabot[bot]", "trusted-bot", true},
		{"Dependabot[bot]", "trusted-bot", true},
		{"renovate[bot]", "", false},
		{"mallory", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		lane, ok := c.sweepLaneForAuthor(tc.author)
		if lane != tc.wantLane || ok != tc.wantOK {
			t.Errorf("sweepLaneForAuthor(%q) = (%q, %v), want (%q, %v)", tc.author, lane, ok, tc.wantLane, tc.wantOK)
		}
	}
}

func TestSweepLaneForAuthorNilResolverIsAppOnly(t *testing.T) {
	c := newTrustedBotEngine(t, nil)
	if _, ok := c.sweepLaneForAuthor("dependabot[bot]"); ok {
		t.Fatal("nil TrustedBotAuthors must not admit dependabot")
	}
	if lane, ok := c.sweepLaneForAuthor(testHiveAppBotLogin); !ok || lane != "self-authored" {
		t.Fatalf("App login must still qualify: got (%q, %v)", lane, ok)
	}
}

func TestPrefilterSelfAuthoredPRAdmitsTrustedBot(t *testing.T) {
	c := newTrustedBotEngine(t, func() map[string]bool { return map[string]bool{"dependabot[bot]": true} })
	pr := selfAuthoredListPR(func(pr *gh.PullRequest) { pr.User = &gh.User{Login: gh.Ptr("dependabot[bot]")} })
	if got := c.prefilterSelfAuthoredPR(pr); got != "" {
		t.Fatalf("trusted bot PR should pass prefilter, got %q", got)
	}
	// The other gates still apply to the bot lane.
	pr.Draft = gh.Ptr(true)
	if got := c.prefilterSelfAuthoredPR(pr); got != "draft" {
		t.Fatalf("draft trusted bot PR should be skipped as draft, got %q", got)
	}
	// Lane is live: an emptied set (operator reload) revokes admission.
	c.trustedBotAuthors = func() map[string]bool { return map[string]bool{} }
	pr.Draft = nil
	if got := c.prefilterSelfAuthoredPR(pr); got != "not-app-authored" {
		t.Fatalf("revoked trusted bot should be not-app-authored, got %q", got)
	}
}
