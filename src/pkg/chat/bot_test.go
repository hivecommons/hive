package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recordingBackend struct {
	sent   []string
	topics []string
}

func (b *recordingBackend) Name() string { return "test" }
func (b *recordingBackend) Send(content string) error {
	b.sent = append(b.sent, content)
	return nil
}
func (b *recordingBackend) SetTopic(topic string) error {
	b.topics = append(b.topics, topic)
	return nil
}
func (b *recordingBackend) Listen(ctx context.Context, deliver func(Message)) {
	<-ctx.Done()
}

// newTestBot builds a Service wired to the given httptest server.
func newTestBot(ts *httptest.Server, channelID string) *Service {
	s := NewService(&recordingBackend{}, Config{
		DashboardURL: ts.URL,
		// Test messages are authored as "uid" (makeMsg) or "u1" (poll-loop
		// fixtures); allowlist both so command-dispatch tests exercise the handler
		// path. The allowlist gate itself is covered by
		// TestRouteMessage_NonAllowlistedUserBlocked / _EmptyAllowlistBlocksAll.
		AllowedUsers: []string{"uid", "u1"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s
}

// discardLogger returns a logger that drops all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func makeBotWithSendCapture(t *testing.T) (*Service, *[]string) {
	t.Helper()

	var sent []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)

	b := newTestBot(ts, "ch")
	return b, &sent
}

// drainQueue reads all pending messages from b.msgQueue (non-blocking) and
// appends their content to sent.
func drainQueue(b *Service, sent *[]string) {
	for {
		select {
		case item := <-b.msgQueue:
			*sent = append(*sent, item.content)
		default:
			return
		}
	}
}

func makeMsg(id, content string, isBot bool) Message {
	return Message{
		ID:       id,
		Text:     content,
		AuthorID: "uid",
		FromBot:  isBot,
	}
}

func TestStart_NilBackendReturnsError(t *testing.T) {
	s := NewService(nil, Config{}, discardLogger())
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("expected error for nil backend")
	}
}

func TestStart_WithBackendRegistersAndQueuesOnlineMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	backend := &recordingBackend{}
	s := NewService(backend, Config{}, discardLogger())
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}

	s.mu.RLock()
	_, hasStatus := s.commands["status"]
	s.mu.RUnlock()
	if !hasStatus {
		t.Fatal("Start did not register builtin commands")
	}

	var sent []string
	drainQueue(s, &sent)
	if len(sent) != 1 || !strings.Contains(sent[0], "Discord bot online") {
		t.Fatalf("online message = %v", sent)
	}
}

func TestDeliver_RoutesMessage(t *testing.T) {
	s, sent := makeBotWithSendCapture(t)
	s.RegisterCommand("ping", func(_ context.Context, args string) (string, error) {
		return "pong " + args, nil
	})
	s.Deliver(context.Background(), makeMsg("1", "!ping via-deliver", false))
	drainQueue(s, sent)
	if len(*sent) != 1 || (*sent)[0] != "pong via-deliver" {
		t.Fatalf("Deliver reply = %v", *sent)
	}
}

func TestDrainLoop_WrapperStopsOnCancel(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.DrainLoop(ctx)
}

// ──────────────────────────────────────────────────────────────────────────────
// NewBot
// ──────────────────────────────────────────────────────────────────────────────

func TestRegisterCommand_StoresHandler(t *testing.T) {
	b := NewService(&recordingBackend{}, Config{}, discardLogger())

	called := false
	b.RegisterCommand("ping", func(_ context.Context, _ string) (string, error) {
		called = true
		return "pong", nil
	})

	b.mu.RLock()
	h, ok := b.commands["ping"]
	b.mu.RUnlock()

	if !ok {
		t.Fatal("handler not found in commands map after RegisterCommand")
	}

	reply, err := h(context.Background(), "")
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if reply != "pong" {
		t.Errorf("handler reply: got %q, want %q", reply, "pong")
	}
	if !called {
		t.Error("handler was not actually invoked")
	}
}

func TestRegisterCommand_OverwritesExisting(t *testing.T) {
	b := NewService(&recordingBackend{}, Config{}, discardLogger())

	b.RegisterCommand("ping", func(_ context.Context, _ string) (string, error) {
		return "first", nil
	})
	b.RegisterCommand("ping", func(_ context.Context, _ string) (string, error) {
		return "second", nil
	})

	b.mu.RLock()
	h := b.commands["ping"]
	b.mu.RUnlock()

	reply, _ := h(context.Background(), "")
	if reply != "second" {
		t.Errorf("want second handler to win, got %q", reply)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Start
// ──────────────────────────────────────────────────────────────────────────────

func TestRouteMessage_NonAllowlistedUserBlocked(t *testing.T) {
	b, sent := makeBotWithSendCapture(t)
	called := false
	b.RegisterCommand("ping", func(_ context.Context, _ string) (string, error) {
		called = true
		return "pong", nil
	})
	// A message from a user NOT in the allowlist must be ignored — the command
	// handler never runs and nothing is sent.
	msg := makeMsg("1", "!ping", false)
	msg.AuthorID = "intruder"
	b.routeMessage(context.Background(), msg)
	drainQueue(b, sent)
	if called {
		t.Error("command handler ran for a non-allowlisted user")
	}
	if len(*sent) != 0 {
		t.Errorf("expected no messages for blocked user, got %v", *sent)
	}
}

func TestRouteMessage_EmptyAllowlistBlocksAll(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	t.Cleanup(ts.Close)
	// SECURITY (F8, CWE-862): an empty AllowedUsers FAILS CLOSED — no one is
	// authorized to drive commands, matching the documented contract ("Empty =
	// commands disabled"). Commands reach dashboardKick with the privileged
	// dashboard bearer, so an unconfigured allowlist must deny, not accept every
	// channel member. Operators enable command control by populating allowed_users.
	b := NewService(&recordingBackend{}, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.dashboardURL = ts.URL
	called := false
	b.RegisterCommand("ping", func(_ context.Context, _ string) (string, error) { called = true; return "pong", nil })
	b.routeMessage(context.Background(), makeMsg("1", "!ping", false))
	if called {
		t.Error("empty allowlist must block all commands (fail closed), but the handler ran")
	}
}

func TestRouteMessage_IgnoresBotMessages(t *testing.T) {
	b, sent := makeBotWithSendCapture(t)

	b.routeMessage(context.Background(), makeMsg("1", "!help", true))
	drainQueue(b, sent)

	if len(*sent) != 0 {
		t.Errorf("expected no messages sent for bot author, got %d", len(*sent))
	}
}

func TestRouteMessage_IgnoresNonCommandPrefix(t *testing.T) {
	b, sent := makeBotWithSendCapture(t)

	b.routeMessage(context.Background(), makeMsg("1", "just chatting", false))
	b.routeMessage(context.Background(), makeMsg("2", "hive status", false))
	b.routeMessage(context.Background(), makeMsg("3", "no bang prefix", false))
	drainQueue(b, sent)

	if len(*sent) != 0 {
		t.Errorf("expected no messages sent for non-command messages, got %d", len(*sent))
	}
}

func TestRouteMessage_DispatchesRegisteredCommand(t *testing.T) {
	b, sent := makeBotWithSendCapture(t)

	b.RegisterCommand("ping", func(_ context.Context, args string) (string, error) {
		return "pong " + args, nil
	})

	b.routeMessage(context.Background(), makeMsg("1", "!ping world", false))
	drainQueue(b, sent)

	if len(*sent) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(*sent))
	}
	if (*sent)[0] != "pong world" {
		t.Errorf("reply: got %q, want %q", (*sent)[0], "pong world")
	}
}

func TestRouteMessage_CommandWithNoArgs(t *testing.T) {
	b, sent := makeBotWithSendCapture(t)

	b.RegisterCommand("status", func(_ context.Context, args string) (string, error) {
		if args != "" {
			return "unexpected args: " + args, nil
		}
		return "all green", nil
	})

	b.routeMessage(context.Background(), makeMsg("1", "!status", false))
	drainQueue(b, sent)

	if len(*sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(*sent))
	}
	if (*sent)[0] != "all green" {
		t.Errorf("reply: got %q, want %q", (*sent)[0], "all green")
	}
}

func TestRouteMessage_UnknownCommandSendsError(t *testing.T) {
	b, sent := makeBotWithSendCapture(t)

	b.routeMessage(context.Background(), makeMsg("1", "!notacommand", false))
	drainQueue(b, sent)

	if len(*sent) != 1 {
		t.Fatalf("expected 1 message sent for unknown command, got %d", len(*sent))
	}
	if !strings.Contains((*sent)[0], "Unknown command") {
		t.Errorf("reply should mention Unknown command, got: %q", (*sent)[0])
	}
	if !strings.Contains((*sent)[0], "notacommand") {
		t.Errorf("reply should mention the bad command name, got: %q", (*sent)[0])
	}
}

func TestRouteMessage_HandlerErrorSendsErrorMessage(t *testing.T) {
	b, sent := makeBotWithSendCapture(t)

	b.RegisterCommand("boom", func(_ context.Context, args string) (string, error) {
		return "", errors.New("something went wrong")
	})

	b.routeMessage(context.Background(), makeMsg("1", "!boom", false))
	drainQueue(b, sent)

	if len(*sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(*sent))
	}
	if !strings.Contains((*sent)[0], "something went wrong") {
		t.Errorf("reply should include the error text, got: %q", (*sent)[0])
	}
}

func TestRouteMessage_LeadingWhitespaceIgnored(t *testing.T) {
	b, sent := makeBotWithSendCapture(t)

	b.RegisterCommand("trim", func(_ context.Context, _ string) (string, error) {
		return "trimmed", nil
	})

	// Content has leading/trailing spaces — TrimSpace is applied in routeMessage.
	b.routeMessage(context.Background(), makeMsg("1", "  !trim  ", false))
	drainQueue(b, sent)

	if len(*sent) != 1 || (*sent)[0] != "trimmed" {
		t.Errorf("expected trimmed reply, got: %v", *sent)
	}
}

// TestRouteMessage_EnqueueDoesNotPanic verifies that routeMessage does not
// panic when the message queue is full (the "queue full" log path).
func TestRouteMessage_EnqueueDoesNotPanic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	b.RegisterCommand("ok", func(_ context.Context, _ string) (string, error) {
		return "fine", nil
	})

	// Fill the message queue to capacity.
	const queueCap = 100
	for i := 0; i < queueCap; i++ {
		b.routeMessage(context.Background(), makeMsg(fmt.Sprintf("%d", i), "!ok", false))
	}

	// One more should not panic — it takes the "queue full" default branch.
	b.routeMessage(context.Background(), makeMsg("overflow", "!ok", false))
}

// ──────────────────────────────────────────────────────────────────────────────
// SendMessage – error branches
// ──────────────────────────────────────────────────────────────────────────────

// TestSendMessage_NewRequestError covers the http.NewRequest error branch in
// SendMessage by using a channelID containing a null byte, which makes the
// constructed URL invalid.
