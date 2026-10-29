package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
					if input["pullRequestId"] != "PR_7" {
						t.Errorf("input=%v", input)
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
