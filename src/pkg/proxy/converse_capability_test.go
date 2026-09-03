package proxy

import (
	"encoding/json"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

// The `converse` capability (#4492) is an ORTHOGONAL grant: it widens the two
// conversational routes and nothing else, and it is off by default so no
// existing hive changes behaviour.
//
// These tests are written as two halves that must BOTH hold:
//
//  1. with no capabilities, every route answers exactly as it did before —
//     asserted here as well as by the pre-existing rules_test.go, which still
//     calls AllowedByMode/GraphQLAllowed unchanged;
//  2. with converse, exactly the conversational routes open, and every
//     artifact-producing, code-writing, merging and hard-denied route stays
//     shut at the same tier.
//
// Half 2 without half 1 would let a widening slip past as "the feature works".

var (
	noCaps       = agent.AgentCapabilities{}
	converseCaps = agent.AgentCapabilities{Converse: true}
)

// TestConverseOpensOnlyConversationalRoutes walks the REST table at ADVISORY —
// the mode with the least to lose and the one the RFC's motivating case uses —
// and pins which routes converse opens.
func TestConverseOpensOnlyConversationalRoutes(t *testing.T) {
	cases := []struct {
		name           string
		method, path   string
		wantNoCaps     bool
		wantConverse   bool
		conversational bool
	}{
		// The two routes converse exists for.
		{"issue comment", "POST", "/repos/o/r/issues/12/comments", false, true, true},
		{"PR review", "POST", "/repos/o/r/pulls/12/reviews", false, true, true},

		// Artifact production stays on the mode ladder. This is the half of the
		// finding that says an ISSUES_ONLY agent should not have to accept
		// issue-rewrite rights just to be allowed to speak.
		{"create issue", "POST", "/repos/o/r/issues", false, false, false},
		{"edit issue body", "PATCH", "/repos/o/r/issues/12", false, false, false},
		{"relabel issue", "POST", "/repos/o/r/issues/12/labels", false, false, false},

		// Code writes stay on the mode ladder.
		{"edit PR", "PATCH", "/repos/o/r/pulls/12", false, false, false},
		{"create ref", "POST", "/repos/o/r/git/refs", false, false, false},
		{"create commit", "POST", "/repos/o/r/git/commits", false, false, false},
		{"delete ref", "DELETE", "/repos/o/r/git/refs/heads/x", false, false, false},
		{"git push", "POST", "/o/r.git/git-receive-pack", false, false, false},
		{"update branch", "PUT", "/repos/o/r/pulls/12/update-branch", false, false, false},

		// Hard denies are not reachable by any capability.
		{"create PR (hard deny)", "POST", "/repos/o/r/pulls", false, false, false},
		{"merge PR (hard deny)", "PUT", "/repos/o/r/pulls/12/merge", false, false, false},

		// Reads were already open; converse must not be load-bearing for them.
		{"read issue", "GET", "/repos/o/r/issues/12", true, true, false},

		// Deny-by-default still holds: converse widens a rule that exists, it
		// does not invent one.
		{"unmatched write", "POST", "/repos/o/r/some/unknown/route", false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AllowedByModeCaps(agent.ModeAdvisory, noCaps, tc.method, tc.path); got != tc.wantNoCaps {
				t.Errorf("ADVISORY without caps: got %v, want %v", got, tc.wantNoCaps)
			}
			if got := AllowedByModeCaps(agent.ModeAdvisory, converseCaps, tc.method, tc.path); got != tc.wantConverse {
				t.Errorf("ADVISORY with converse: got %v, want %v", got, tc.wantConverse)
			}
			// The capability may only ever widen. Whatever the old answer was,
			// the new one is never more restrictive.
			if tc.wantNoCaps && !tc.wantConverse {
				t.Errorf("converse NARROWED %s %s — capabilities must only widen", tc.method, tc.path)
			}
			// And it only differs where the route is conversational.
			if (tc.wantNoCaps != tc.wantConverse) != tc.conversational {
				t.Errorf("converse changed the answer for a non-conversational route (or failed to change a conversational one)")
			}
		})
	}
}

// TestConverseIsIdenticalToTodayWithoutTheCapability is the compatibility
// contract: for every mode and every route in the table, the zero capability
// set answers exactly what the old tier-only comparison answered.
func TestConverseIsIdenticalToTodayWithoutTheCapability(t *testing.T) {
	paths := []struct{ method, path string }{
		{"POST", "/repos/o/r/issues"},
		{"PATCH", "/repos/o/r/issues/1"},
		{"POST", "/repos/o/r/issues/1/comments"},
		{"POST", "/repos/o/r/issues/1/labels"},
		{"POST", "/repos/o/r/pulls/1/reviews"},
		{"PATCH", "/repos/o/r/pulls/1"},
		{"POST", "/repos/o/r/git/refs"},
		{"GET", "/repos/o/r"},
	}
	modes := []agent.AgentMode{
		agent.ModeAdvisory, agent.ModeIssuesOnly,
		agent.ModeIssuesAndPRs, agent.ModeIssuesPRsMerge,
	}
	for _, m := range modes {
		for _, p := range paths {
			legacy := AllowedByMode(m, p.method, p.path)
			withZero := AllowedByModeCaps(m, noCaps, p.method, p.path)
			if legacy != withZero {
				t.Errorf("%s %s %s: AllowedByMode=%v but AllowedByModeCaps(zero)=%v — the zero capability set must be a no-op",
					m, p.method, p.path, legacy, withZero)
			}
		}
	}
}

// TestConverseDoesNotSubstituteForAHigherTier: an agent that already has the
// tier keeps it, and converse does not become a way around the tiers that
// govern the same objects.
func TestConverseDoesNotSubstituteForAHigherTier(t *testing.T) {
	// A merge-capable agent with converse still cannot merge directly (hard
	// deny), and still can do everything its tier allows.
	if AllowedByModeCaps(agent.ModeIssuesPRsMerge, converseCaps, "PUT", "/repos/o/r/pulls/1/merge") {
		t.Error("hard deny bypassed by a capability")
	}
	if !AllowedByModeCaps(agent.ModeIssuesPRsMerge, converseCaps, "POST", "/repos/o/r/issues") {
		t.Error("converse must not remove what the tier already granted")
	}
	// An ISSUES_ONLY agent gains PR reviews from converse but still no pushes.
	if !AllowedByModeCaps(agent.ModeIssuesOnly, converseCaps, "POST", "/repos/o/r/pulls/1/reviews") {
		t.Error("converse should open PR reviews at ISSUES_ONLY")
	}
	if AllowedByModeCaps(agent.ModeIssuesOnly, converseCaps, "POST", "/repos/o/r/git/refs") {
		t.Error("converse must not open code writes")
	}
}

func graphQLBody(t *testing.T, query string) []byte {
	t.Helper()
	b, err := json.Marshal(graphQLRequest{Query: query})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestConverseGraphQL covers the path that actually matters in practice:
// `gh issue comment` and `gh pr review` go through GraphQL, not REST, so a
// converse capability that stopped at the REST table would look broken.
func TestConverseGraphQL(t *testing.T) {
	cases := []struct {
		name         string
		query        string
		wantNoCaps   bool
		wantConverse bool
	}{
		{
			name:         "addComment — what `gh issue comment` sends",
			query:        `mutation($input: AddCommentInput!) { addComment(input: $input) { clientMutationId } }`,
			wantNoCaps:   false,
			wantConverse: true,
		},
		{
			name:         "addPullRequestReview — what `gh pr review` sends",
			query:        `mutation AddReview($input: AddPullRequestReviewInput!) { addPullRequestReview(input: $input) { clientMutationId } }`,
			wantNoCaps:   false,
			wantConverse: true,
		},
		{
			name:         "aliased addComment resolves to the real field",
			query:        `mutation { reply: addComment(input: {body: "hi"}) { clientMutationId } }`,
			wantNoCaps:   false,
			wantConverse: true,
		},
		{
			name:         "read query is unaffected",
			query:        `query { repository(owner: "o", name: "r") { id } }`,
			wantNoCaps:   true,
			wantConverse: true,
		},
		// The reason converse is granted on the WHOLE document rather than a
		// substring match. Each of these MENTIONS a conversational mutation and
		// must still be refused.
		{
			name:         "batched comment + issue edit is not conversation",
			query:        `mutation { addComment(input: {body: "x"}) { clientMutationId } issueUpdate(input: {title: "y"}) { clientMutationId } }`,
			wantNoCaps:   false,
			wantConverse: false,
		},
		{
			name:         "batched comment + merge is a merge",
			query:        `mutation { addComment(input: {body: "x"}) { clientMutationId } mergePullRequest(input: {}) { clientMutationId } }`,
			wantNoCaps:   false,
			wantConverse: false,
		},
		{
			name:         "batched review + createPullRequest is a PR write",
			query:        `mutation { addPullRequestReview(input: {}) { clientMutationId } createPullRequest(input: {}) { clientMutationId } }`,
			wantNoCaps:   false,
			wantConverse: false,
		},
		{
			name:         "a comment name inside a string argument is not a field",
			query:        `mutation { issueUpdate(input: {body: "please addComment(here)"}) { clientMutationId } }`,
			wantNoCaps:   false,
			wantConverse: false,
		},
		{
			name:         "editing someone else's comment is not conversation",
			query:        `mutation { updateIssueComment(input: {body: "x"}) { clientMutationId } }`,
			wantNoCaps:   false,
			wantConverse: false,
		},
		{
			name:         "multi-operation document is refused rather than guessed at",
			query:        `mutation A { addComment(input: {}) { clientMutationId } } mutation B { issueUpdate(input: {}) { clientMutationId } }`,
			wantNoCaps:   false,
			wantConverse: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := graphQLBody(t, tc.query)
			got, _ := GraphQLAllowedCaps(agent.ModeAdvisory, noCaps, body)
			if got != tc.wantNoCaps {
				t.Errorf("ADVISORY without caps: got %v, want %v", got, tc.wantNoCaps)
			}
			got, _ = GraphQLAllowedCaps(agent.ModeAdvisory, converseCaps, body)
			if got != tc.wantConverse {
				t.Errorf("ADVISORY with converse: got %v, want %v", got, tc.wantConverse)
			}
			// Legacy entry point must be unchanged.
			legacy, _ := GraphQLAllowed(agent.ModeAdvisory, body)
			if legacy != tc.wantNoCaps {
				t.Errorf("GraphQLAllowed drifted from GraphQLAllowedCaps(zero): got %v, want %v", legacy, tc.wantNoCaps)
			}
		})
	}
}

// TestTopLevelMutationFields pins the parser directly, including the inputs it
// must refuse to read rather than guess at. A parser that returned a partial
// field list on a document it did not understand would grant converse on the
// half it saw.
func TestTopLevelMutationFields(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"simple", `mutation { addComment(input: {}) { id } }`, []string{"addComment"}},
		{"named with vars", `mutation Foo($i: X!) { addComment(input: $i) { id } }`, []string{"addComment"}},
		{"alias", `mutation { r: addComment(input: {}) { id } }`, []string{"addComment"}},
		{"two fields", `mutation { addComment(input: {}) { id } issueUpdate(input: {}) { id } }`, []string{"addComment", "issueUpdate"}},
		{"nested selections are not top level", `mutation { addComment(input: {}) { comment { id author { login } } } }`, []string{"addComment"}},
		{"braces inside a string argument", `mutation { addComment(input: {body: "a } b"}) { id } }`, []string{"addComment"}},
		{"comment line", "mutation {\n  # addComment is not here\n  issueUpdate(input: {}) { id }\n}", []string{"issueUpdate"}},
		{"directive is not a field", `mutation { addComment(input: {}) @include(if: true) { id } }`, []string{"addComment"}},
		{"not a mutation", `query { viewer { login } }`, nil},
		{"two operations", `mutation A { addComment(input: {}) { id } } mutation B { x(input: {}) { id } }`, nil},
		{"unterminated", `mutation { addComment(input: {}) { id }`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := topLevelMutationFields(tc.query)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestReadAgentCapsDefaultsClosed: every failure path must yield no
// capabilities, because the file is consulted on the request path and an
// unreadable file must not be able to grant anything.
func TestReadAgentCapsDefaultsClosed(t *testing.T) {
	if got := readAgentCaps(""); got.Any() {
		t.Error("unidentified agent must hold no capabilities")
	}
	if got := readAgentCaps("no-such-agent-xyz-4492"); got.Any() {
		t.Error("missing caps file must yield no capabilities")
	}
	if got := agent.ParseCapabilities(""); got.Any() {
		t.Error("empty caps file must yield no capabilities")
	}
	if got := agent.ParseCapabilities("a-capability-from-a-newer-hive"); got.Any() {
		t.Error("unknown token must yield no capabilities, not a parse that grants something")
	}
	if got := agent.ParseCapabilities("converse"); !got.CanConverse() {
		t.Error("converse token should parse")
	}
	// Round-trip, since the manager writes what the proxy reads.
	if got := agent.ParseCapabilities(converseCaps.String()); got != converseCaps {
		t.Errorf("round-trip lost the capability: %q -> %+v", converseCaps.String(), got)
	}
	if s := (agent.AgentCapabilities{}).String(); s != "" {
		t.Errorf("zero capabilities should render empty, got %q", s)
	}
}
