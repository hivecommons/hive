package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/effects"
)

func TestNativeMergeQueueGraphQL(t *testing.T) {
	for _, mode := range []string{"read", "enqueue", "dequeue", "graphql-error", "missing-entry"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/graphql" {
					t.Errorf("request=%s %s", r.Method, r.URL.Path)
				}
				var body struct {
					Query     string
					Variables map[string]json.RawMessage
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				switch mode {
				case "read":
					if !strings.Contains(body.Query, "isMergeQueueEnabled") || !strings.Contains(body.Query, "enqueuer { __typename login }") || string(body.Variables["id"]) != `"PR_7"` {
						t.Errorf("query=%+v", body)
					}
					_, _ = w.Write([]byte(`{"data":{"node":{"id":"PR_7","headRefOid":"validated","isMergeQueueEnabled":true,"isInMergeQueue":true,"mergeQueueEntry":{"id":"entry","position":3,"state":"AWAITING_CHECKS","enqueuer":{"__typename":"Bot","login":"hive[bot]"}}}}}`))
				case "graphql-error":
					_, _ = w.Write([]byte(`{"errors":[{"message":"expectedHeadOid mismatch"}],"data":null}`))
				case "missing-entry":
					_, _ = w.Write([]byte(`{"data":{"enqueuePullRequest":{"mergeQueueEntry":null}}}`))
				case "enqueue", "dequeue":
					var input map[string]any
					if err := json.Unmarshal(body.Variables["input"], &input); err != nil {
						t.Error(err)
					}
					// GitHub's EnqueuePullRequestInput names the PR pullRequestId,
					// but DequeuePullRequestInput names it id and rejects
					// pullRequestId as an undefined field (#20020).
					idField, badField := "pullRequestId", "id"
					if mode == "dequeue" {
						idField, badField = "id", "pullRequestId"
					}
					if _, ok := input[badField]; ok || input[idField] != "PR_7" || len(input) > 2 {
						t.Errorf("mode=%s input=%v", mode, input)
					}
					if mode == "enqueue" {
						if !strings.Contains(body.Query, "enqueuePullRequest") || input["expectedHeadOid"] != "validated" {
							t.Errorf("query=%s input=%v", body.Query, input)
						}
						_, _ = w.Write([]byte(`{"data":{"enqueuePullRequest":{"mergeQueueEntry":{"id":"entry","position":3,"state":"AWAITING_CHECKS"}}}}`))
					} else {
						if !strings.Contains(body.Query, "dequeuePullRequest") {
							t.Errorf("query=%s", body.Query)
						}
						_, _ = w.Write([]byte(`{"data":{"dequeuePullRequest":{"clientMutationId":null}}}`))
					}
				}
			}))
			defer api.Close()
			c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
			// Test transport only: real clients obtain this from NewAppClient.
			c.appAuth = &AppAuth{}
			switch mode {
			case "read":
				state, err := c.ReadNativeMergeQueue(context.Background(), "PR_7")
				if err != nil || !state.IsMergeQueueEnabled || state.MergeQueueEntry == nil || state.MergeQueueEntry.Enqueuer.Login != "hive[bot]" || state.MergeQueueEntry.Enqueuer.Typename != "Bot" {
					t.Fatalf("state=%+v error=%v", state, err)
				}
			case "dequeue":
				if err := c.DequeueNativePullRequest(context.Background(), "PR_7"); err != nil {
					t.Fatal(err)
				}
			default:
				entry, err := c.EnqueueNativePullRequest(context.Background(), "PR_7", "validated")
				if mode == "enqueue" && (err != nil || entry == nil || entry.Position != 3) {
					t.Fatalf("entry=%+v error=%v", entry, err)
				}
				if mode != "enqueue" && err == nil {
					t.Fatal("expected error")
				}
			}
			if calls != 1 {
				t.Fatalf("requests=%d", calls)
			}
		})
	}
}

func TestNativeMergeQueueRejectsStaticToken(t *testing.T) {
	c := NewClient("token", "acme", []string{"widget"}, nil, "http://unused.invalid")
	if c.NativeMergeQueueAvailable() {
		t.Fatal("static token must not enqueue")
	}
	if _, err := c.EnqueueNativePullRequest(context.Background(), "PR_7", "head"); err == nil {
		t.Fatal("static token accepted")
	}
	if err := c.DequeueNativePullRequest(context.Background(), "PR_7"); err == nil {
		t.Fatal("static token accepted")
	}
}

// The queue read asks for the PR's reviewDecision in the query it already
// makes, and decodes it: the three verdicts, and nothing when the base
// requires no review (GitHub answers null).
func TestReadNativeMergeQueueReviewDecision(t *testing.T) {
	for _, tc := range []struct{ served, want string }{
		{`"REVIEW_REQUIRED"`, "REVIEW_REQUIRED"},
		{`"CHANGES_REQUESTED"`, "CHANGES_REQUESTED"},
		{`"APPROVED"`, "APPROVED"},
		{`null`, ""},
	} {
		t.Run(tc.served, func(t *testing.T) {
			calls := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct{ Query string }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if !strings.Contains(body.Query, "reviewDecision") {
					t.Errorf("the queue read must ask for reviewDecision, query=%s", body.Query)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"node":{"id":"PR_7","headRefOid":"validated","reviewDecision":` + tc.served + `,"isMergeQueueEnabled":true,"isInMergeQueue":false,"mergeQueueEntry":null}}}`))
			}))
			defer api.Close()
			c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
			state, err := c.ReadNativeMergeQueue(context.Background(), "PR_7")
			if err != nil || state.ReviewDecision != tc.want || !state.IsMergeQueueEnabled || state.IsInMergeQueue {
				t.Fatalf("state=%+v error=%v, want reviewDecision %q", state, err, tc.want)
			}
			if calls != 1 {
				t.Fatalf("requests=%d, want the one queue read and no extra call", calls)
			}
		})
	}
}

// A GraphQL errors array is a definite refusal only when every entry
// carries a type that means GitHub turned the request down before changing
// anything. Everything else may or may not have been applied. Only
// UNPROCESSABLE says one of GitHub's rules for the object refused it: a
// missing permission or a missing node is a refusal of another kind.
func TestIsGraphQLRefusal(t *testing.T) {
	unprocessable := &graphQLErrors{Errors: []graphQLError{{Type: "UNPROCESSABLE", Message: "Pull request cannot be queued"}}}
	for _, tc := range []struct {
		name              string
		err               error
		want              bool
		wantUnprocessable bool
	}{
		{"unprocessable", unprocessable, true, true},
		{"forbidden", &graphQLErrors{Errors: []graphQLError{{Type: "FORBIDDEN", Message: "Resource not accessible by integration"}}}, true, false},
		{"not found", &graphQLErrors{Errors: []graphQLError{{Type: "NOT_FOUND", Message: "Could not resolve to a node"}}}, true, false},
		{"lower case type", &graphQLErrors{Errors: []graphQLError{{Type: "unprocessable"}}}, true, true},
		{"wrapped as not applied", effects.NotApplied(unprocessable), true, true},
		{"wrapped as uncertain", fmt.Errorf("effect outcome uncertain, reconciliation required: %w", unprocessable), true, true},
		{"two refusals", &graphQLErrors{Errors: []graphQLError{{Type: "UNPROCESSABLE"}, {Type: "FORBIDDEN"}}}, true, false},
		{"untyped entry", &graphQLErrors{Errors: []graphQLError{{Message: "the server could not answer"}}}, false, false},
		{"refusal beside an untyped entry", &graphQLErrors{Errors: []graphQLError{{Type: "UNPROCESSABLE"}, {Message: "the server could not answer"}}}, false, false},
		{"rate limited", &graphQLErrors{Errors: []graphQLError{{Type: "RATE_LIMITED", Message: "API rate limit exceeded"}}}, false, false},
		{"empty errors array", &graphQLErrors{}, false, false},
		{"flat string", errors.New("graphql: UNPROCESSABLE: Pull request cannot be queued"), false, false},
		{"rest server error", &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}, Message: "Server Error"}, false, false},
		{"rest unprocessable", &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnprocessableEntity}, Message: "Validation Failed"}, false, false},
		{"network error", errors.New("dial tcp: connection refused"), false, false},
		{"nil", nil, false, false},
	} {
		if got := IsGraphQLRefusal(tc.err); got != tc.want {
			t.Errorf("%s: IsGraphQLRefusal(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
		if got := IsGraphQLUnprocessable(tc.err); got != tc.wantUnprocessable {
			t.Errorf("%s: IsGraphQLUnprocessable(%v) = %v, want %v", tc.name, tc.err, got, tc.wantUnprocessable)
		}
	}
}
