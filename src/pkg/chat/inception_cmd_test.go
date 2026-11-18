package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func inceptionCtx(role string) context.Context {
	return context.WithValue(context.Background(), commandRoleContextKey{}, role)
}

// newInceptionStubService wires a dashboard that counts every request: the
// stub must never call it, since /api/inception/* is retired (410 Gone).
func newInceptionStubService(t *testing.T) (*Service, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusGone)
	}))
	t.Cleanup(ts.Close)
	s := NewService(&recordingBackend{}, Config{DashboardURL: ts.URL}, discardLogger())
	s.client = ts.Client()
	return s, &calls
}

func TestCmdInceptionRepliesWithSessionsPointerForEverySubcommand(t *testing.T) {
	s, calls := newInceptionStubService(t)
	for _, args := range []string{
		"", "start an idea", "scan https://github.com/o/r", "state", "answer 1 yes",
		"skip 2", "submit", `facts [{"title":"t"}]`, "approve", "reset", "bogus",
	} {
		got, err := s.cmdInception(inceptionCtx("owner"), args)
		if err != nil {
			t.Fatalf("%q: unexpected error %v", args, err)
		}
		if !strings.Contains(got, "Start an Inception session from the dashboard Sessions section") {
			t.Fatalf("%q reply = %q, want the Sessions pointer", args, got)
		}
		if strings.Contains(got, "❌") {
			t.Fatalf("%q reply = %q, an owner's pointer is not an error", args, got)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("stub made %d dashboard requests, want 0", n)
	}
}

func TestCmdInceptionOwnerGate(t *testing.T) {
	s, calls := newInceptionStubService(t)
	for _, sub := range []string{"start idea", "scan repo", "answer 1 x", "skip 1", "submit", "facts []", "approve", "RESET"} {
		for _, role := range []string{"read", ""} {
			got, err := s.cmdInception(inceptionCtx(role), sub)
			if err != nil || !strings.Contains(got, "owner role required") {
				t.Fatalf("role %q %q = %q, %v; want owner refusal", role, sub, got, err)
			}
		}
	}
	// Read-only forms keep answering below owner, as before the retirement.
	for _, sub := range []string{"", "state", "bogus"} {
		got, _ := s.cmdInception(inceptionCtx("read"), sub)
		if !strings.Contains(got, "dashboard Sessions section") {
			t.Fatalf("read role %q = %q, want the pointer", sub, got)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("stub made %d dashboard requests, want 0", n)
	}
}

func TestCmdInceptionRefusesMultilineInputAsAWhole(t *testing.T) {
	s, calls := newInceptionStubService(t)
	for _, args := range []string{
		"start line one\nline two",
		"start idea\r\nsecond",
		"state\nstart sneaky",
		"answer 1 a\nanswer 2 b",
	} {
		got, err := s.cmdInception(inceptionCtx("owner"), args)
		if err != nil || !strings.Contains(got, "single line") || !strings.Contains(got, "Nothing was run") {
			t.Fatalf("%q = %q, %v; want a whole-input multi-line refusal", args, got, err)
		}
	}
	// A trailing newline alone is not multi-line input.
	if got, _ := s.cmdInception(inceptionCtx("owner"), "state\n"); strings.Contains(got, "single line") {
		t.Fatalf("trailing newline refused: %q", got)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("stub made %d dashboard requests, want 0", n)
	}
}

func TestInceptionCommandStaysRegisteredAndRoutes(t *testing.T) {
	s, _ := newInceptionStubService(t)
	s.registerBuiltinCommands()
	s.mu.RLock()
	_, ok := s.commands["inception"]
	s.mu.RUnlock()
	if !ok {
		t.Fatal("inception command is no longer registered")
	}
}
