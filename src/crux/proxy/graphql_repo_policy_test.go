package proxy

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agent"
)

// autoMergeOffFor is a repo-policy predicate that reports auto-merge disabled
// for exactly the given repos (case-insensitive) and enabled for every other.
func autoMergeOffFor(repos ...string) func(string) bool {
	off := make(map[string]bool, len(repos))
	for _, r := range repos {
		off[strings.ToLower(r)] = true
	}
	return func(repo string) bool { return !off[strings.ToLower(repo)] }
}

const mergeMutationQuery = `mutation($input:MergePullRequestInput!){ mergePullRequest(input:$input){clientMutationId} }`

// The refusal must never turn into a blanket merge denial: an attributable
// merge on a repo whose auto-merge is ON, an unattributable merge, a read
// query and a non-merge mutation all pass through to the ACMM merge gate.
func TestGraphQLAutoMergeRefusal_AllowsWhenPolicyPermits(t *testing.T) {
	enabled := autoMergeOffFor("acme/frozen")

	cases := map[string]string{
		"merge on enabled repo via variables":       `{"query":"` + mergeMutationQuery + `","variables":{"repo":"acme/open"}}`,
		"merge on enabled repo via owner+name":      `{"query":"` + mergeMutationQuery + `","variables":{"input":{"owner":"acme","name":"open"}}}`,
		"merge with no repo attribution at all":     `{"query":"` + mergeMutationQuery + `","variables":{"input":{"pullRequestId":"PR_kw"}}}`,
		"read query naming the frozen repo":         `{"query":"query{ repository(owner:\"acme\", name:\"frozen\"){ id } }","variables":{"repo":"acme/frozen"}}`,
		"non-merge mutation naming the frozen repo": `{"query":"mutation{ addComment(input:{subjectId:\"I_kw\", body:\"hi\"}){clientMutationId} }","variables":{"repo":"acme/frozen"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if msg, refused := GraphQLAutoMergeRefusal(enabled, []byte(body)); refused {
				t.Fatalf("refused (%q); want pass-through to the ACMM merge gate", msg)
			}
		})
	}
}

// With no repo-policy predicate configured (every pre-policy hive) or a body
// the proxy cannot parse, the refusal declines to judge rather than deny:
// the ACMM merge gate downstream still applies.
func TestGraphQLAutoMergeRefusal_NoPredicateOrUnparseableBodyPassesThrough(t *testing.T) {
	body := []byte(`{"query":"` + mergeMutationQuery + `","variables":{"repo":"acme/frozen"}}`)
	if msg, refused := GraphQLAutoMergeRefusal(nil, body); refused {
		t.Fatalf("nil predicate refused (%q); want pass-through", msg)
	}
	enabled := autoMergeOffFor("acme/frozen")
	if msg, refused := GraphQLAutoMergeRefusal(enabled, []byte(`{"query": not json`)); refused {
		t.Fatalf("unparseable body refused (%q); want pass-through", msg)
	}
	if msg, refused := GraphQLAutoMergeRefusal(enabled, nil); refused {
		t.Fatalf("empty body refused (%q); want pass-through", msg)
	}
}

// When the variables do not attribute the merge to a repo, the body-literal
// fallback still can: a repo spelled anywhere in the request (here under
// extensions) denies a merge on a frozen repo.
func TestGraphQLAutoMergeRefusal_BodyLiteralFallback(t *testing.T) {
	enabled := autoMergeOffFor("acme/frozen")

	body := []byte(`{"query":"` + mergeMutationQuery + `","variables":{"input":{"pullRequestId":"PR_kw"}},"extensions":{"repository":"acme/frozen"}}`)
	msg, refused := GraphQLAutoMergeRefusal(enabled, body)
	if !refused || !strings.Contains(msg, "acme/frozen") {
		t.Fatalf("literal fallback = (%q,%v), want refusal naming acme/frozen", msg, refused)
	}

	body = []byte(`{"query":"` + mergeMutationQuery + `","variables":{"input":{"pullRequestId":"PR_kw"}},"extensions":{"owner":"acme","repo":"frozen"}}`)
	msg, refused = GraphQLAutoMergeRefusal(enabled, body)
	if !refused || !strings.Contains(msg, "acme/frozen") {
		t.Fatalf("owner+repo literal fallback = (%q,%v), want refusal naming acme/frozen", msg, refused)
	}

	// The fallback attributes to the OPEN repo: not refused.
	body = []byte(`{"query":"` + mergeMutationQuery + `","variables":{"input":{"pullRequestId":"PR_kw"}},"extensions":{"repository":"acme/open"}}`)
	if msg, refused := GraphQLAutoMergeRefusal(enabled, body); refused {
		t.Fatalf("literal fallback naming an enabled repo refused (%q)", msg)
	}
}

func TestExtractRepoFromGraphQLBody(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"repo literal", `{"repo":"acme/widgets"}`, "acme/widgets"},
		{"repository literal", `{"repository": "acme/widgets"}`, "acme/widgets"},
		{"repositoryNameWithOwner literal", `{"repositoryNameWithOwner":"acme/widgets"}`, "acme/widgets"},
		{"owner then name", `{"owner":"acme","name":"widgets"}`, "acme/widgets"},
		{"owner then repo with spaces", `{"owner" : "acme" , "repo" : "widgets"}`, "acme/widgets"},
		{"owner then repository", `{"owner":"acme","repository":"widgets"}`, "acme/widgets"},
		{"case-insensitive key", `{"Repository":"acme/widgets"}`, "acme/widgets"},
		{"literal without a slash is not a repo", `{"repo":"widgets"}`, ""},
		{"owner without a name", `{"owner":"acme"}`, ""},
		{"name before owner is not matched by the literal regex", `{"name":"widgets","owner":"acme"}`, ""},
		{"empty body", ``, ""},
		{"unrelated body", `{"query":"query{ viewer { login } }"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractRepoFromGraphQLBody([]byte(tc.body)); got != tc.want {
				t.Fatalf("extractRepoFromGraphQLBody(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// setAutonomyRepoLevelForTest registers a per-repo autonomy override and
// removes it when the test ends. autonomyRepoLevels is package-global, so a
// test that leaves its key behind changes what every later test sees.
func setAutonomyRepoLevelForTest(t *testing.T, repo string, level int) {
	t.Helper()
	SetAutonomyRepoLevel(repo, level)
	t.Cleanup(func() { autonomyRepoLevels.Delete(strings.ToLower(repo)) })
}

// A GraphQL document that names a demoted repo inline
// (repository(owner:"…", name:"…")) resolves to that repo's demoted mode,
// not the hive-wide one. This is the branch the proxy relies on to apply
// project.repo_policies autonomy levels to GraphQL traffic.
func TestAutonomyGraphQLMode_InlineRepositoryRefResolvesDemotedRepo(t *testing.T) {
	setAutonomyRepoLevelForTest(t, "acme/demoted-graphql", 4)
	hiveMode := agent.DefaultAgentMode("scanner", 5)
	demoted := agent.DefaultAgentMode("scanner", 4)
	if demoted == hiveMode {
		t.Fatal("fixture: scanner L4 and L5 must map to different modes")
	}

	body := []byte(`{"query":"mutation { repository(owner: \"acme\", name: \"demoted-graphql\") { id } createPullRequest(input:{repositoryId:\"R\"}){ pullRequest { id } } }"}`)
	if got := autonomyGraphQLMode("scanner", body, hiveMode); got != demoted {
		t.Fatalf("mode = %v, want demoted %v", got, demoted)
	}

	// Case and whitespace in the inline ref do not defeat attribution.
	body = []byte(`{"query":"query {\n  Repository (\n owner : \"ACME\" ,\n name : \"Demoted-GraphQL\" ) { id }\n}"}`)
	if got := autonomyGraphQLMode("scanner", body, hiveMode); got != demoted {
		t.Fatalf("mode with spaced/cased ref = %v, want demoted %v", got, demoted)
	}
}

// An inline ref to a repo WITHOUT an override keeps the hive-wide mode for
// read queries, while a PR write that cannot be attributed to an overridden
// repo fails closed to advisory while any override is active.
func TestAutonomyGraphQLMode_UnmatchedRepoKeepsFallbackForReadsOnly(t *testing.T) {
	setAutonomyRepoLevelForTest(t, "acme/demoted-graphql", 4)
	hiveMode := agent.DefaultAgentMode("scanner", 5)

	read := []byte(`{"query":"query { repository(owner: \"acme\", name: \"other\") { id } }"}`)
	if got := autonomyGraphQLMode("scanner", read, hiveMode); got != hiveMode {
		t.Fatalf("read on non-overridden repo = %v, want hive-wide %v", got, hiveMode)
	}
	write := []byte(`{"query":"mutation { repository(owner: \"acme\", name: \"other\") { id } createPullRequest(input:{repositoryId:\"R\"}){ pullRequest { id } } }"}`)
	if got := autonomyGraphQLMode("scanner", write, hiveMode); got != agent.ModeAdvisory {
		t.Fatalf("unattributed PR write = %v, want fail-closed %v", got, agent.ModeAdvisory)
	}
	merge := []byte(`{"query":"mutation { mergePullRequest(input:{pullRequestId:\"PR_kw\"}){ clientMutationId } }"}`)
	if got := autonomyGraphQLMode("scanner", merge, hiveMode); got != agent.ModeAdvisory {
		t.Fatalf("unattributed merge = %v, want fail-closed %v", got, agent.ModeAdvisory)
	}
}

// A body the proxy cannot parse carries no attribution and no mutation
// classification: the hive-wide mode stands (the generic GraphQL gate
// downstream still sees the raw body).
func TestAutonomyGraphQLMode_UnparseableBodyKeepsFallback(t *testing.T) {
	setAutonomyRepoLevelForTest(t, "acme/demoted-graphql", 4)
	hiveMode := agent.DefaultAgentMode("scanner", 5)
	if got := autonomyGraphQLMode("scanner", []byte(`{"query": not json`), hiveMode); got != hiveMode {
		t.Fatalf("unparseable body mode = %v, want fallback %v", got, hiveMode)
	}
}

// With no overrides registered, autonomyGraphQLMode is a no-op. The package
// map is global, so snapshot and clear it for the duration of this test.
func TestAutonomyGraphQLMode_NoOverridesIsNoop(t *testing.T) {
	saved := map[any]any{}
	autonomyRepoLevels.Range(func(k, v any) bool {
		saved[k] = v
		autonomyRepoLevels.Delete(k)
		return true
	})
	t.Cleanup(func() {
		for k, v := range saved {
			autonomyRepoLevels.Store(k, v)
		}
	})
	if hasAutonomyRepoLevels() {
		t.Fatal("fixture: overrides should be cleared")
	}
	hiveMode := agent.DefaultAgentMode("scanner", 5)
	body := []byte(`{"query":"mutation { mergePullRequest(input:{pullRequestId:\"PR_kw\"}){ clientMutationId } }"}`)
	if got := autonomyGraphQLMode("scanner", body, hiveMode); got != hiveMode {
		t.Fatalf("mode with no overrides = %v, want fallback %v", got, hiveMode)
	}
}
