package dashboard

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCovB_NewSessionID(t *testing.T) {
	id := newSessionID()
	if id == "" {
		t.Fatal("newSessionID returned empty")
	}
	if _, err := hex.DecodeString(id); err != nil {
		t.Fatalf("session id not hex: %v", err)
	}
	if len(id) != sessionIDBytes*2 {
		t.Fatalf("session id length = %d, want %d", len(id), sessionIDBytes*2)
	}
}

func TestCovB_SessionLifecycle(t *testing.T) {
	s := NewServer(0, covBLogger())

	id := s.createUserSession("alice", "admin")
	if id == "" {
		t.Fatal("createUserSession returned empty")
	}

	sess := s.lookupSession(id)
	if sess == nil || sess.Username != "alice" || sess.Role != "admin" {
		t.Fatalf("lookupSession = %+v, want alice/admin", sess)
	}

	// Empty and unknown ids resolve to nil.
	if s.lookupSession("") != nil {
		t.Fatal("lookupSession(\"\") != nil")
	}
	if s.lookupSession("deadbeef") != nil {
		t.Fatal("lookupSession(unknown) != nil")
	}

	// Delete removes it.
	s.deleteSession(id)
	if s.lookupSession(id) != nil {
		t.Fatal("session survived deleteSession")
	}
	// deleteSession("") is a no-op.
	s.deleteSession("")
}

func TestCovB_SessionExpiryEviction(t *testing.T) {
	s := NewServer(0, covBLogger())
	id := s.createUserSession("bob", "viewer")

	// Force expiry in the past, then lookup must evict and return nil.
	s.sessionMu.Lock()
	s.userSessions[id].ExpiresAt = time.Now().Add(-time.Hour)
	s.sessionMu.Unlock()

	if s.lookupSession(id) != nil {
		t.Fatal("expired session was not evicted")
	}
	// Confirm it is gone from the map.
	s.sessionMu.RLock()
	_, present := s.userSessions[id]
	s.sessionMu.RUnlock()
	if present {
		t.Fatal("expired session still in map after lookup")
	}
}

func TestCovB_CreateUserSessionReapsExpired(t *testing.T) {
	s := NewServer(0, covBLogger())
	stale := s.createUserSession("stale", "viewer")
	s.sessionMu.Lock()
	s.userSessions[stale].ExpiresAt = time.Now().Add(-time.Hour)
	s.sessionMu.Unlock()

	// A fresh login sweeps expired sessions.
	s.createUserSession("fresh", "admin")
	s.sessionMu.RLock()
	_, present := s.userSessions[stale]
	s.sessionMu.RUnlock()
	if present {
		t.Fatal("stale session not reaped by new login")
	}
}

func TestCovB_SessionFromRequest(t *testing.T) {
	s := NewServer(0, covBLogger())
	id := s.createUserSession("carol", "admin")

	// No cookie → nil.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if s.sessionFromRequest(req) != nil {
		t.Fatal("sessionFromRequest with no cookie != nil")
	}

	// Valid cookie → session.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: id})
	sess := s.sessionFromRequest(req)
	if sess == nil || sess.Username != "carol" {
		t.Fatalf("sessionFromRequest = %+v, want carol", sess)
	}

	// Empty-value cookie → nil.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: ""})
	if s.sessionFromRequest(req) != nil {
		t.Fatal("sessionFromRequest with empty cookie != nil")
	}
}

func TestCovB_SessionCookies(t *testing.T) {
	// setSessionCookie over HTTPS (Secure=true) via X-Forwarded-Proto.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	setSessionCookie(rec, req, "abc123")
	sc := rec.Header().Get("Set-Cookie")
	if sc == "" {
		t.Fatal("setSessionCookie wrote no Set-Cookie header")
	}

	// clearSessionCookie expires it.
	rec2 := httptest.NewRecorder()
	clearSessionCookie(rec2)
	if rec2.Header().Get("Set-Cookie") == "" {
		t.Fatal("clearSessionCookie wrote no Set-Cookie header")
	}
}

func TestCovB_SessionPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess.json")

	// First server enables persistence with no existing file (no error).
	s1 := NewServer(0, covBLogger())
	s1.EnableSessionPersistence(path)
	id := s1.createUserSession("dave", "admin") // writes the store file
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session store not written: %v", err)
	}

	// A fresh server restores the live session from disk.
	s2 := NewServer(0, covBLogger())
	s2.EnableSessionPersistence(path)
	if sess := s2.lookupSession(id); sess == nil || sess.Username != "dave" {
		t.Fatalf("restored session = %+v, want dave", sess)
	}
}

func TestCovB_SessionPersistence_ExpiredNotRestored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess.json")
	s1 := NewServer(0, covBLogger())
	s1.EnableSessionPersistence(path)
	id := s1.createUserSession("erin", "viewer")
	// Expire it and rewrite the store.
	s1.sessionMu.Lock()
	s1.userSessions[id].ExpiresAt = time.Now().Add(-time.Hour)
	s1.persistSessionsLocked()
	s1.sessionMu.Unlock()

	s2 := NewServer(0, covBLogger())
	s2.EnableSessionPersistence(path)
	if s2.lookupSession(id) != nil {
		t.Fatal("expired session should not be restored")
	}
}

func TestCovB_SessionPersistence_CorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(path, []byte("not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, covBLogger())
	// Must not panic and must start with no restored sessions.
	s.EnableSessionPersistence(path)
	if len(s.userSessions) != 0 {
		t.Fatalf("corrupt store restored %d sessions, want 0", len(s.userSessions))
	}
}
