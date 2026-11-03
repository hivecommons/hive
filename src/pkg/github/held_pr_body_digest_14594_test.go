package github

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

// A held PR carries the same body digest as an actionable one, so the review
// swarm's kick for it names the digest and its dispatch record can stamp
// every head's verdict into one aggregate (hivecommons/hive#14594).
func TestFetchPRsHeldPRCarriesBodyDigest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/pulls", prsHandler(t, []wirePR{
		{
			Number:    101,
			Title:     "fix: actionable",
			Body:      "actionable body",
			User:      wireUser{Login: "bot"},
			CreatedAt: "2026-09-01T00:00:00Z",
		},
		{
			Number:    102,
			Title:     "fix: held",
			Body:      "held body",
			User:      wireUser{Login: "bot"},
			Labels:    []wireLabel{{Name: "hold"}},
			CreatedAt: "2026-09-01T00:00:00Z",
		},
	}))
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})

	actionable, _, heldPRs, _, _, _, _, err := c.fetchPRs(t.Context(), "widget", nil)
	if err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	if len(actionable) != 1 {
		t.Fatalf("actionable = %d, want 1", len(actionable))
	}
	if want := review.PullRequestBodyDigest("fix: actionable", "actionable body"); actionable[0].BodyDigest != want {
		t.Fatalf("actionable BodyDigest = %q, want %q", actionable[0].BodyDigest, want)
	}
	if len(heldPRs) != 1 {
		t.Fatalf("heldPRs = %d, want 1", len(heldPRs))
	}
	held := heldPRs[0]
	want := review.PullRequestBodyDigest("fix: held", "held body")
	if held.BodyDigest != want {
		t.Fatalf("held BodyDigest = %q, want %q", held.BodyDigest, want)
	}

	prompt := review.BuildPerspectivePromptWith(review.PerspectiveCorrectness, review.PullRequest{
		Repo:       "acme/widget",
		Number:     held.Number,
		Title:      held.Title,
		BodyDigest: held.BodyDigest,
	}, review.PromptOptions{})
	if !strings.Contains(prompt, "Review body digest: "+want) {
		t.Fatalf("held PR kick has no body digest line:\n%s", prompt)
	}
}
