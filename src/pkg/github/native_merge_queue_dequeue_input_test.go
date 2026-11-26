package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// GitHub's DequeuePullRequestInput names the pull request's node ID `id`
// (its fields are id and clientMutationId); `pullRequestId` belongs to
// EnqueuePullRequestInput. A dequeue sent with pullRequestId fails schema
// validation on every call, so a gate that turns on after the hive enqueued a
// PR never withdraws it and the queue merges the PR past that gate.
func TestDequeueNativePullRequestSendsID(t *testing.T) {
	var input map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string
			Variables map[string]json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if err := json.Unmarshal(body.Variables["input"], &input); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"dequeuePullRequest":{"clientMutationId":null}}}`))
	}))
	defer api.Close()
	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	// Test transport only: real clients obtain this from NewAppClient.
	c.appAuth = &AppAuth{}
	if err := c.DequeueNativePullRequest(context.Background(), "PR_7"); err != nil {
		t.Fatal(err)
	}
	if input["id"] != "PR_7" {
		t.Errorf("dequeue input %v: want id=PR_7 (DequeuePullRequestInput defines id and clientMutationId)", input)
	}
	if _, ok := input["pullRequestId"]; ok {
		t.Errorf("dequeue input %v carries pullRequestId, which DequeuePullRequestInput does not define", input)
	}
}
