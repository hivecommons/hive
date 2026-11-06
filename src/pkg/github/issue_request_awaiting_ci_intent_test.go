package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah#1091: 22 s after the adopted fixer's push the relay
// posted "hive/awaiting-ci: CI pending — sweep will merge when green." for a
// head that undid the PR. On an in-place adopted PR the note promises a
// merge only once the head passed the intent check.
func TestAwaitingCINoteOnAdoptedPRWaitsForIntentCheck(t *testing.T) {
	const agentNote = "hive/awaiting-ci: CI pending — sweep will merge when green."
	head := "a75a567"
	pusher := "hive[bot]"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/1091"):
			_, _ = w.Write([]byte(`{"number":1091,"head":{"sha":"` + head + `"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/commits/"+head):
			_, _ = w.Write([]byte(`{"sha":"` + head + `","author":{"login":"` + pusher + `"},"committer":{"login":"` + pusher + `"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.SetAppBotLogin("hive[bot]")

	store := stagelabels.Shared()
	key := stagelabels.Key("o/utah", 1091)
	t.Cleanup(func() { store.Forget(key) })
	store.Observe(key, stagelabels.AdoptedByHive, "adopted")
	now := time.Date(2026, 11, 3, 21, 0, 0, 0, time.UTC)
	store.MarkAdoptedInPlace(key, "7e9dfd7", "contributor-offline", now)

	ctx := context.Background()
	if got := c.adoptedAwaitingCINote(ctx, "o/utah", 1091, agentNote); got != awaitingCIIntentPendingNote {
		t.Fatalf("unchecked hive head (App-linked commit, not yet recorded): %q", got)
	}
	// The author pushed: that head is never intent-checked, so the note
	// must not claim it is.
	pusher = "thatguywob"
	if got := c.adoptedAwaitingCINote(ctx, "o/utah", 1091, agentNote); got != agentNote {
		t.Fatalf("author push: %q", got)
	}
	// A recorded hive push (bare git identity) is checked.
	store.MarkHivePushedHead(key, head)
	if got := c.adoptedAwaitingCINote(ctx, "o/utah", 1091, agentNote); got != awaitingCIIntentPendingNote {
		t.Fatalf("recorded hive push: %q", got)
	}
	store.MarkAdoptionEscalated(key, head, stagelabels.LaneReasonAdoptionFixerReverts)
	if got := c.adoptedAwaitingCINote(ctx, "o/utah", 1091, agentNote); got != awaitingCIIntentRefusedNote {
		t.Fatalf("refused head: %q", got)
	}
	store.ClearAdoptionEscalation(key)
	store.MarkAdoptedHead(key, head)
	if got := c.adoptedAwaitingCINote(ctx, "o/utah", 1091, agentNote); got != agentNote {
		t.Fatalf("checked head: %q", got)
	}
	if got := c.adoptedAwaitingCINote(ctx, "o/utah", 1091, "Parking on stage/045-needs-decision: why"); !strings.HasPrefix(got, "Parking on") {
		t.Fatalf("another note rewritten: %q", got)
	}
	if got := c.adoptedAwaitingCINote(ctx, "o/utah", 9999, agentNote); got != agentNote {
		t.Fatalf("not adopted: %q", got)
	}
	if relayStatusNoteKey(awaitingCIIntentPendingNote) == "" || relayStatusNoteKey(awaitingCIIntentRefusedNote) == "" {
		t.Fatal("rewritten notes must stay awaiting-ci status notes")
	}
}
