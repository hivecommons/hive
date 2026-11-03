package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeIntegrationGroupExactPrefix(t *testing.T) {
	for _, mode := range []string{"valid", "gap", "truncated", "moved-ref", "missing-group", "read-error"} {
		t.Run(mode, func(t *testing.T) {
			base, head, tree, member := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/graphql":
					if mode == "read-error" {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					if mode == "missing-group" {
						_, _ = w.Write([]byte(`{"data":{"repository":{"mergeQueue":null}}}`))
						return
					}
					position := 1
					if mode == "gap" {
						position = 2
					}
					_, _ = fmt.Fprintf(w, `{"data":{"repository":{"mergeQueue":{"entries":{"pageInfo":{"hasNextPage":%t},"nodes":[{"id":"entry","position":%d,"baseCommit":{"oid":%q},"headCommit":{"oid":%q},"pullRequest":{"number":7,"headRefOid":%q,"baseRefName":"v5"}}]}}}}}`, mode == "truncated", position, base, head, member)
				case strings.Contains(r.URL.Path, "/git/commits/"):
					_, _ = fmt.Fprintf(w, `{"sha":%q,"tree":{"sha":%q}}`, head, tree)
				case strings.Contains(r.URL.Path, "/git/ref/"):
					if mode == "moved-ref" {
						head = strings.Repeat("f", 40)
					}
					_, _ = fmt.Fprintf(w, `{"object":{"sha":%q}}`, head)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer api.Close()
			c := NewClient("token", "o", []string{"r"}, nil, api.URL)
			got, err := c.ReadNativeIntegrationGroup(context.Background(), "o/r", "v5", base, "refs/heads/gh-readonly-queue/v5/pr-7", head)
			if mode == "valid" {
				if err != nil || got.Tree != tree || len(got.Members) != 1 || got.Members[0].Head != member {
					t.Fatalf("%+v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("incomplete or changed group accepted")
			}
		})
	}
	c := NewClient("token", "o", []string{"r"}, nil, "https://example.invalid")
	if _, err := c.ReadNativeIntegrationGroup(context.Background(), "bad", "", "", "", ""); err == nil {
		t.Fatal("incomplete identity accepted")
	}
}
