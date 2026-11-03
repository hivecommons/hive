package automerge

import (
	"context"
	"errors"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestIntegrationAdmissionOnlyBeforeNewJournaledEnqueue(t *testing.T) {
	for _, mode := range []string{"deny", "unavailable", "admit", "already-queued", "native-off"} {
		t.Run(mode, func(t *testing.T) {
			engine, transport, pr, claim := nativeQueueEngine()
			boundary := &recordingBoundary{}
			engine.mutation = boundary
			calls := 0
			engine.integrationAdmission = func(_ context.Context, repo string, observed *gh.PullRequest) (string, error) {
				calls++
				if repo != claim.Repo || observed != pr {
					t.Fatal("wrong live candidate")
				}
				if mode == "deny" {
					return "runner budget exhausted", nil
				}
				if mode == "unavailable" {
					return "unknown telemetry", errors.New("unavailable")
				}
				return "", nil
			}
			if mode == "already-queued" {
				transport.state.IsInMergeQueue = true
				transport.state.MergeQueueEntry = queueEntry(testHiveAppBotLogin)
			}
			if mode == "native-off" {
				engine.nativeMergeQueuePolicy = nil
			}
			reason, err := engine.tryNativeQueue(context.Background(), pr, claim)
			if mode == "deny" || mode == "unavailable" {
				if transport.enqueues != 0 || len(boundary.claims) != 0 || !strings.Contains(reason, "integration") {
					t.Fatalf("%s %v enqueues=%d", reason, err, transport.enqueues)
				}
			} else if mode == "admit" && (err != nil || transport.enqueues != 1 || len(boundary.claims) != 1) {
				t.Fatalf("existing journal writer not used: %s %v %+v", reason, err, boundary.claims)
			}
			wantCalls := 1
			if mode == "already-queued" || mode == "native-off" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want %d", calls, wantCalls)
			}
		})
	}
}
