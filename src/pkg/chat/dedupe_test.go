package chat

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDeliverDeduplicatesConcurrentMessages(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{AllowedUsers: []string{"U1"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var calls atomic.Int64
	s.RegisterCommand("ping", func(context.Context, string) (string, error) { calls.Add(1); return "", nil })
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Deliver(context.Background(), Message{ID: "same", AuthorID: "U1", Text: "!ping"})
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	for _, id := range []string{"different", "", ""} {
		s.Deliver(context.Background(), Message{ID: id, AuthorID: "U1", Text: "!ping"})
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("calls = %d, want 4 (distinct and empty IDs execute)", got)
	}
}

func TestRecentMessagesEvictsOldest(t *testing.T) {
	var recent recentMessages
	for i := range recentMessageLimit {
		if !recent.remember(fmt.Sprint(i)) {
			t.Fatal("new ID rejected")
		}
	}
	if recent.remember("0") {
		t.Fatal("duplicate accepted")
	}
	if !recent.remember("next") || !recent.remember("0") {
		t.Fatal("oldest ID not evicted")
	}
	if recent.remember("next") {
		t.Fatal("recent ID evicted")
	}
	if len(recent.seen) != recentMessageLimit || len(recent.order) != recentMessageLimit {
		t.Fatal("cache is not bounded")
	}
}
