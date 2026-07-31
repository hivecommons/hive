package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func auditTestClient() *http.Client {
	return &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func TestProbeWideOpen_OpenIsFlagged(t *testing.T) {
	// A spoke that serves /api/status unauthenticated (200) is WIDE OPEN.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	_, open, reachable := probeWideOpen(context.Background(), auditTestClient(), srv.URL)
	if !reachable {
		t.Fatal("expected reachable")
	}
	if !open {
		t.Fatal("200 must be flagged wide open")
	}
}

func TestProbeWideOpen_RedirectIsProtected(t *testing.T) {
	// A protected spoke redirects to login (30x) — NOT open.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}))
	defer srv.Close()
	_, open, reachable := probeWideOpen(context.Background(), auditTestClient(), srv.URL)
	if !reachable {
		t.Fatal("expected reachable")
	}
	if open {
		t.Fatal("a 30x redirect must NOT be flagged open (it's the protected signal)")
	}
}

func TestProbeWideOpen_UnauthorizedIsProtected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, open, reachable := probeWideOpen(context.Background(), auditTestClient(), srv.URL)
	if !reachable || open {
		t.Fatalf("401 must be reachable+protected, got open=%v reachable=%v", open, reachable)
	}
}

func TestProbeWideOpen_UnreachableIsNotOpen(t *testing.T) {
	// A firewalled/down spoke: transport error → unreachable, never "open".
	_, open, reachable := probeWideOpen(context.Background(), auditTestClient(), "https://127.0.0.1:1")
	if reachable {
		t.Fatal("expected unreachable for a dead endpoint")
	}
	if open {
		t.Fatal("an unreachable spoke must never be flagged open")
	}
}
