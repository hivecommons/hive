package proxy

import (
	"testing"

	"github.com/hivecommons/hive/crux/agent"
)

func TestGraphQLMutationGate_SameLinePrefixes(t *testing.T) {
	const mut = `mutation { addComment(input:{subjectId:"I_kw", body:"hi"}) { clientMutationId } }`
	cases := []struct {
		name, query string
		write       bool
	}{
		{"plain mutation", mut, true},
		{"fragment-first", `fragment F on Repository { id } ` + mut, true},
		{"leading comma", `,` + mut, true},
		{"leading BOM", "\uFEFF" + mut, true},
		{"plain query", `query { viewer { login } }`, false},
		{"fragment-first query", `fragment F on Repository { id } query { viewer { login } }`, false},
		{"mutation word in a string", `query { search(query: "mutation { x }") { issueCount } }`, false},
		{"escaped triple quote in block string", `fragment F on Repository { ref(qualifiedName: """\"""""") { id } }
mutation { addComment(input:{subjectId:"x", body:"y"}) { subject { ... on Issue { repository { ...F } } } } }`, true},
		{"unterminated string fails closed", `query { search(query: "abc) { issueCount } }`, true},
		{"unterminated block string fails closed", `query { search(query: """abc) { issueCount } }`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := graphQLBody(t, tc.query)
			allowed, write := GraphQLAllowedCaps(agent.ModeAdvisory, agent.AgentCapabilities{}, body)
			if allowed == tc.write || write != tc.write {
				t.Errorf("allowed=%v write=%v, want allowed=%v write=%v", allowed, write, !tc.write, tc.write)
			}
		})
	}
}

func TestGraphQLAutoMergeRefusal_SameLinePrefixes(t *testing.T) {
	enabled := autoMergeOffFor("acme/frozen")
	for name, q := range map[string]string{
		"fragment-first": `fragment F on Repository { id } ` + mergeMutationQuery,
		"leading comma":  `,` + mergeMutationQuery,
		"leading BOM":    "\uFEFF" + mergeMutationQuery,
	} {
		t.Run(name, func(t *testing.T) {
			body := string(graphQLBody(t, q))
			body = body[:len(body)-1] + `,"variables":{"repo":"acme/frozen"}}`
			if _, refused := GraphQLAutoMergeRefusal(enabled, []byte(body)); !refused {
				t.Fatal("merge on an auto-merge-off repo was not refused")
			}
		})
	}
}
