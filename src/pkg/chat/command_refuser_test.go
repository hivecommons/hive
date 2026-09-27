package chat

import (
	"context"
	"strings"
	"testing"
)

// refusingBackend is a recordingBackend that also implements CommandRefuser.
type refusingBackend struct {
	recordingBackend
	refused []string
}

func (b *refusingBackend) CommandRefused(msg Message, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refused = append(b.refused, msg.AuthorID+": "+reason)
}

func TestRouteMessage_RefusalIsReportedOnlyToCommandRefusers(t *testing.T) {
	t.Run("denied author and empty allowlist are refused visibly", func(t *testing.T) {
		for name, cfg := range map[string]Config{
			"non-allowlisted": {AllowedUsers: []string{"alice"}},
			"empty allowlist": {},
		} {
			backend := &refusingBackend{}
			s := NewService(backend, cfg, discardLogger())
			called := false
			s.RegisterCommand("ping", func(context.Context, string) (string, error) {
				called = true
				return "pong", nil
			})
			s.routeMessage(context.Background(), Message{ID: "1", Text: "!ping", AuthorID: "mallory"})
			if called {
				t.Fatalf("%s: handler ran for a refused author", name)
			}
			if len(backend.refused) != 1 || !strings.HasPrefix(backend.refused[0], "mallory: ") {
				t.Fatalf("%s: refusals = %v, want one for mallory", name, backend.refused)
			}
		}
	})

	t.Run("allowed author and non-command text are not refused", func(t *testing.T) {
		backend := &refusingBackend{}
		s := NewService(backend, Config{AllowedUsers: []string{"alice"}}, discardLogger())
		s.RegisterCommand("ping", func(context.Context, string) (string, error) { return "pong", nil })
		s.routeMessage(context.Background(), Message{ID: "1", Text: "!ping", AuthorID: "alice"})
		s.routeMessage(context.Background(), Message{ID: "2", Text: "hello", AuthorID: "mallory"})
		if len(backend.refused) != 0 {
			t.Fatalf("refusals = %v, want none", backend.refused)
		}
	})

	t.Run("plain backends stay silent", func(t *testing.T) {
		backend := &recordingBackend{}
		s := NewService(backend, Config{AllowedUsers: []string{"alice"}}, discardLogger())
		s.routeMessage(context.Background(), Message{ID: "1", Text: "!ping", AuthorID: "mallory"})
		if len(backend.sent) != 0 || len(s.msgQueue) != 0 {
			t.Fatalf("sent = %v queued = %d, want nothing for a backend without CommandRefuser", backend.sent, len(s.msgQueue))
		}
	})
}
