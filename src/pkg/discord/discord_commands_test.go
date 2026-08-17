package discord

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegisterBuiltinCommandsAllRegistered(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "test-channel")
	b.registerBuiltinCommands()

	expectedCommands := []string{"status", "governor", "help", "kick", "pause", "resume"}
	for _, cmd := range expectedCommands {
		b.mu.RLock()
		_, ok := b.commands[cmd]
		b.mu.RUnlock()
		if !ok {
			t.Errorf("command %q not registered", cmd)
		}
	}
}

func TestCommandPauseViaHandler(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "pause") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "paused"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "test-channel")
	b.dashboardURL = ts.URL
	b.registerBuiltinCommands()

	b.mu.RLock()
	handler := b.commands["pause"]
	b.mu.RUnlock()

	result, err := handler(context.Background(), "scanner")
	if err != nil {
		t.Fatalf("pause handler error: %v", err)
	}
	_ = result
}

func TestCommandResumeViaHandler(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "resume") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "resumed"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "test-channel")
	b.dashboardURL = ts.URL
	b.registerBuiltinCommands()

	b.mu.RLock()
	handler := b.commands["resume"]
	b.mu.RUnlock()

	result, err := handler(context.Background(), "scanner")
	if err != nil {
		t.Fatalf("resume handler error: %v", err)
	}
	_ = result
}

func TestCommandKickViaHandler(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.Contains(r.URL.Path, "kick") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "kicked"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "test-channel")
	b.dashboardURL = ts.URL
	b.registerBuiltinCommands()

	b.mu.RLock()
	handler := b.commands["kick"]
	b.mu.RUnlock()

	result, err := handler(context.Background(), "scanner review bugs")
	if err != nil {
		t.Fatalf("kick handler error: %v", err)
	}
	_ = result
}

func TestDrainLoopCancelStops(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "test-channel")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		b.drainLoop(ctx)
		close(done)
	}()

	// Enqueue a message then cancel
	b.enqueue("test message")
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("drainLoop should stop on context cancel")
	}
}

func TestHeartbeatLoopCancelStops(t *testing.T) {
	statusResp := `{"governor":{"mode":"idle","issues":0,"prs":0},"agents":[]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(statusResp))
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "test", ChannelID: "ch"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.client = &http.Client{Transport: &redirectTransport{target: ts.URL}, Timeout: 5 * time.Second}
	b.dashboardURL = ts.URL

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		b.heartbeatLoop(ctx)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("heartbeatLoop should stop on cancel")
	}
}
