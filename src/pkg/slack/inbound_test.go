package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hivecommons/hive/pkg/chat"
)

// A blocked first handler must not delay acks or pongs for later envelopes.
// Retried B uses a different envelope ID but the same message ID.
func TestListenSlowHandlerAcksRetriesAndPongs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	serverResult := make(chan error, 1)
	calls := make(chan string, 10)
	var apiBase string
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps.connections.open" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": strings.Replace(apiBase+"/socket", "http", "ws", 1)})
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverResult <- err
			return
		}
		defer conn.Close()
		pong := false
		conn.SetPongHandler(func(string) error { pong = true; return nil })
		for i, id := range []string{"A", "B", "B", "C"} {
			if i == 1 {
				select {
				case <-entered:
				case <-ctx.Done():
					return
				}
				if err := conn.WriteControl(websocket.PingMessage, []byte("alive"), time.Now().Add(time.Second)); err != nil {
					serverResult <- err
					return
				}
			}
			payload, _ := json.Marshal(eventPayload{Event: slackEvent{Type: "message", Channel: "C1", ClientMsgID: id, User: "U1", Text: "!ping " + id}})
			retry := 0
			if i == 2 {
				retry = 1
			}
			envID := fmt.Sprint(i)
			if err := conn.WriteJSON(map[string]any{"envelope_id": envID, "type": "events_api", "payload": json.RawMessage(payload), "retry_attempt": retry}); err != nil {
				serverResult <- err
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			var ack map[string]string
			if err := conn.ReadJSON(&ack); err != nil {
				serverResult <- fmt.Errorf("ack %s while handler blocked: %w", envID, err)
				return
			}
			if ack["envelope_id"] != envID {
				serverResult <- fmt.Errorf("wrong ack: %v", ack)
				return
			}
		}
		if !pong {
			serverResult <- fmt.Errorf("no pong while handler blocked")
			return
		}
		serverResult <- nil
		<-ctx.Done()
	}))
	defer ts.Close()
	apiBase = ts.URL
	b := newTestBot(ts.URL)
	service := chat.NewService(b.slackBackend, chat.Config{AllowedUsers: []string{"U1"}}, discardLogger())
	service.RegisterCommand("ping", func(_ context.Context, args string) (string, error) {
		if args == "A" {
			close(entered)
			<-release
		}
		calls <- args
		return "", nil
	})
	go func() { defer close(finished); b.Listen(ctx, func(msg chat.Message) { service.Deliver(ctx, msg) }) }()
	defer func() { close(release); cancel(); <-finished }()
	select {
	case err := <-serverResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("socket reader stalled")
	}
	release <- struct{}{}
	for _, want := range []string{"A", "B", "C"} {
		select {
		case got := <-calls:
			if got != want {
				t.Fatalf("command = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("delivery stalled")
		}
	}
	cancel()
	<-finished
	if len(calls) != 0 {
		t.Fatal("duplicate command executed")
	}
}

func TestConsumeSocketFullQueueDoesNotAck(t *testing.T) {
	var apiBase string
	result := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps.connections.open" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": strings.Replace(apiBase+"/socket", "http", "ws", 1)})
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		payload, _ := json.Marshal(eventPayload{Event: slackEvent{Type: "message", Channel: "C1", TS: "2"}})
		_ = conn.WriteJSON(socketEnvelope{EnvelopeID: "overflow", Type: "events_api", Payload: payload})
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, _, err = conn.ReadMessage()
		result <- err
	}))
	defer ts.Close()
	apiBase = ts.URL
	queue := make(chan chat.Message, 1)
	queue <- chat.Message{ID: "1"}
	err := consumeOnce(context.Background(), newTestBot(ts.URL), queue)
	if err == nil || !strings.Contains(err.Error(), "queue full") {
		t.Fatalf("consumeOnce = %v, want queue full", err)
	}
	if err := <-result; err == nil {
		t.Fatal("overflow envelope was acknowledged")
	}
	if msg := <-queue; msg.ID != "1" {
		t.Fatalf("queued message replaced: %+v", msg)
	}
}
