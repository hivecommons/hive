package automerge

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

func TestMergeSweepClockMarksOnlyEvaluatedPRInEveryLane(t *testing.T) {
	var client *hgithub.Client
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !client.MergeSweepClock().Snapshot("acme/widget", 1).SweepRunning {
			t.Error("PR evaluation did not publish running state")
		}
		if client.MergeSweepClock().Snapshot("acme/widget", 2).SweepRunning {
			t.Error("unrelated PR appears to be running")
		}
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()
	client = hgithub.NewClientForTest(srv.URL, "acme", []string{"widget"}, slog.Default())
	c := New(client, Options{})
	ctx := context.Background()
	for name, run := range map[string]func(){
		"queued":        func() { _, _, _ = c.trySweepQueuedPR(ctx, "acme/widget", "acme", "widget", 1, "automerge") },
		"self-authored": func() { _, _, _ = c.trySweepSelfAuthoredPR(ctx, "acme/widget", "acme", "widget", 1, false) },
		"trusted": func() {
			_, _, _ = c.trySweepTrustedAuthorPR(ctx, "acme/widget", "acme", "widget", 1, TrustedAuthorPolicy{})
		},
		"contributor": func() {
			_, _, _ = c.trySweepContributorLanePR(ctx, "acme/widget", "acme", "widget", 1, ContributorLanePolicy{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			run()
			if client.MergeSweepClock().Snapshot("acme/widget", 1).SweepRunning {
				t.Fatal("early return left the PR marked running")
			}
		})
	}
}
