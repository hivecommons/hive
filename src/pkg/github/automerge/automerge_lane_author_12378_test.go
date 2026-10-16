package automerge

import (
	"testing"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#12378: the your-turn decision asks whether any merge lane
// admits a PR's author, through the author gates the sweeps skip on
// (not-app-authored, untrusted-author-role).
func TestMergeLaneTakesAuthor12378(t *testing.T) {
	newEngine := func(trusted TrustedAuthorPolicy, contributor ContributorLanePolicy) *Engine {
		client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "http://127.0.0.1:0")
		client.SetAppBotLogin(testHiveAppBotLogin)
		return New(client, Options{
			TrustedBotAuthors:     func() map[string]bool { return map[string]bool{"dependabot[bot]": true} },
			TrustedAuthorPolicy:   func() TrustedAuthorPolicy { return trusted },
			ContributorLanePolicy: func() ContributorLanePolicy { return contributor },
			TrustedAuthorizer: func(login, _ string) TrustedAuthorDecision {
				return TrustedAuthorDecision{Allowed: login == "maintainer"}
			},
		})
	}
	off := newEngine(TrustedAuthorPolicy{}, ContributorLanePolicy{})
	trustedOn := newEngine(TrustedAuthorPolicy{Enabled: true}, ContributorLanePolicy{})
	trustedOtherRepo := newEngine(TrustedAuthorPolicy{Enabled: true, Repos: map[string]bool{"other": true}}, ContributorLanePolicy{})
	contributorOn := newEngine(TrustedAuthorPolicy{}, ContributorLanePolicy{Enabled: true})

	cases := []struct {
		name   string
		engine *Engine
		author string
		want   bool
	}{
		{"App's own PR", off, testHiveAppBotLogin, true},
		{"allow-listed bot", off, "dependabot[bot]", true},
		{"person, no lane for others", off, "alice", false},
		{"authorized author, trusted-author tier off", off, "maintainer", false},
		{"authorized author, trusted-author tier on", trustedOn, "maintainer", true},
		{"unauthorized author, trusted-author tier on", trustedOn, "alice", false},
		{"authorized author, tier serves another repo", trustedOtherRepo, "maintainer", false},
		{"any author on a contributor-lane repo", contributorOn, "alice", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.engine.MergeLaneTakesAuthor("widget", tc.author); got != tc.want {
				t.Fatalf("MergeLaneTakesAuthor(widget, %q) = %v, want %v", tc.author, got, tc.want)
			}
		})
	}
}
