package github

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIntegrationReadOnlyTransportRejectsAncillaryWrites(t *testing.T) {
	for _, test := range []struct {
		name, method, path, body string
		allowed                  bool
	}{
		{"ordinary", "POST", "/repos/o/r/issues/1/comments", `{}`, true},
		{"read", "GET", "/repos/o/r/pulls/1", "", true},
		{"head", "HEAD", "/repos/o/r/commits/head", "", true},
		{"comment", "POST", "/repos/o/r/issues/1/comments", `{}`, false},
		{"approve-held-run", "POST", "/repos/o/r/actions/runs/1/approve", `{}`, false},
		{"graphql-query", "POST", "/graphql", `{"query":"query { viewer { login } }"}`, true},
		{"graphql-shorthand", "POST", "/graphql", `{"query":"{ viewer { login } }"}`, true},
		{"graphql-mutation", "POST", "/graphql", `{"query":"mutation { removeLabel(input:{id:\"x\"}) {clientMutationId} }"}`, false},
		{"graphql-get", "GET", "/graphql?query=mutation", "", false},
		{"invalid-query", "POST", "/graphql", `{"query":"subscription { viewer }"}`, false},
		{"invalid-json", "POST", "/graphql", `not json`, false},
		{"empty-query", "POST", "/graphql", `{}`, false},
		{"large-query", "POST", "/graphql", strings.Repeat("x", integrationReadOnlyMaxQuery+1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.name != "ordinary" {
				ctx = WithIntegrationReadOnly(ctx)
			}
			request, err := http.NewRequestWithContext(ctx, test.method, "https://api.github.com"+test.path, strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			err = integrationReadOnlyRequest(request)
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v: %v", test.allowed, err)
			}
			if err == nil && test.method == "POST" && test.path == "/graphql" {
				body, readErr := io.ReadAll(request.Body)
				if readErr != nil || string(body) != test.body {
					t.Fatal("read-only validation consumed/replaced GraphQL body")
				}
			}
		})
	}
}
