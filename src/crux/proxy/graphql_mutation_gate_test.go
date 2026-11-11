package proxy

import (
	"testing"

	"github.com/hivecommons/hive/crux/agent"
)

func TestGraphQLAllowedCapsMutationPrefixes(t *testing.T) {
	const m = `mutation { addComment(input:{}) { clientMutationId } }`
	cases := []struct {
		name, query  string
		wantAdvisory bool
		wantWrite    bool
	}{
		{"fragment prefix", `fragment F on Repository { id } ` + m, false, true},
		{"comma prefix", `,` + m, false, true},
		{"BOM prefix", "\uFEFF" + m, false, true},
		{"comment prefix read", "# note\n{ viewer { login } }", true, false},
		{"mutation only in string", `{ search(query: "mutation") { issueCount } }`, true, false},
		{"mutation only in comment", "# mutation\n{ viewer { login } }", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := graphQLBody(t, tc.query)
			allowed, write := GraphQLAllowedCaps(agent.ModeAdvisory, agent.AgentCapabilities{}, body)
			if allowed != tc.wantAdvisory || write != tc.wantWrite {
				t.Errorf("advisory: allowed=%v write=%v, want %v/%v", allowed, write, tc.wantAdvisory, tc.wantWrite)
			}
			if tc.wantWrite {
				if allowed, _ := GraphQLAllowedCaps(agent.ModeIssuesOnly, agent.AgentCapabilities{}, body); !allowed {
					t.Errorf("issues-only: addComment mutation should be allowed")
				}
			}
		})
	}
}
