package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedOperatorMessageProfile(t *testing.T, username, cid, tier string) *ContributorProfile {
	t.Helper()
	p := &ContributorProfile{GitHubUsername: username, ContributorID: cid, TrustTier: tier, RegisteredAt: time.Now().UTC().Format(time.RFC3339)}
	if err := saveContributorProfile(p); err != nil {
		t.Fatalf("save profile: %v", err)
	}
	return p
}

func TestOperatorMessageHandlerAuthValidationAndAck(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	operatorMessageRateLimiter.Lock()
	operatorMessageRateLimiter.buckets = map[string][]time.Time{}
	operatorMessageRateLimiter.Unlock()
	seedOperatorMessageProfile(t, "alice", "c-alice", "contributor")
	s := NewServer(0, nil)

	readOnly := httptest.NewRequest(http.MethodPost, "/api/contribute/operators/message", strings.NewReader(`{"contributor":"alice","text":"hi"}`))
	readOnly.Header.Set("X-Hive-Role", "read")
	rec := httptest.NewRecorder()
	s.handleContributeOperatorMessage(rec, readOnly)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only send status = %d, want 403", rec.Code)
	}

	tooLong := strings.Repeat("x", operatorMessageMaxRunes+1)
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/operators/message", bytes.NewBufferString(`{"contributor":"alice","text":"`+tooLong+`"}`))
	req.Header.Set("X-Hive-Role", "read-write")
	req.Header.Set("X-Hive-User", "op")
	rec = httptest.NewRecorder()
	s.handleContributeOperatorMessage(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("too-long send status = %d, want 400", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/contribute/operators/message", strings.NewReader(`{"contributor":"alice","text":"hello <b>Alice</b>\u001b[2J"}`))
	req.Header.Set("X-Hive-Role", "read-write")
	req.Header.Set("X-Hive-User", "op")
	rec = httptest.NewRecorder()
	s.handleContributeOperatorMessage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("send status = %d body=%s", rec.Code, rec.Body.String())
	}
	p, err := loadContributorProfile("alice")
	if err != nil || len(p.OperatorMessages) != 1 {
		t.Fatalf("stored messages = %#v err=%v", p, err)
	}
	if strings.ContainsRune(p.OperatorMessages[0].Text, '\x1b') || !strings.Contains(p.OperatorMessages[0].Text, "<b>Alice</b>") {
		t.Fatalf("message sanitization lost literal html or kept ESC: %q", p.OperatorMessages[0].Text)
	}

	get := httptest.NewRequest(http.MethodGet, "/api/contribute/operators/message", nil)
	get.Header.Set("X-Hive-User", "bob")
	rec = httptest.NewRecorder()
	s.handleContributeOperatorMessage(rec, get)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("other contributor status = %d, want 403", rec.Code)
	}
	seedOperatorMessageProfile(t, "bob", "c-bob", "contributor")
	rec = httptest.NewRecorder()
	s.handleContributeOperatorMessage(rec, get)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "Alice") {
		t.Fatalf("other contributor leaked message: status=%d body=%s", rec.Code, rec.Body.String())
	}

	badAck := httptest.NewRequest(http.MethodPost, "/api/contribute/operators/message/ack", strings.NewReader(`{}`))
	badAck.Header.Set("X-Hive-User", "alice")
	rec = httptest.NewRecorder()
	s.handleContributeOperatorMessageAck(rec, badAck)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty ack status = %d, want 400", rec.Code)
	}

	ack := httptest.NewRequest(http.MethodPost, "/api/contribute/operators/message/ack", strings.NewReader(`{"id":"`+p.OperatorMessages[0].ID+`","reply":"thanks <ok>"}`))
	ack.Header.Set("X-Hive-User", "alice")
	rec = httptest.NewRecorder()
	s.handleContributeOperatorMessageAck(rec, ack)
	if rec.Code != http.StatusOK {
		t.Fatalf("ack status = %d body=%s", rec.Code, rec.Body.String())
	}
	p, _ = loadContributorProfile("alice")
	if p.OperatorMessages[0].AcknowledgedAt == "" || p.OperatorMessages[0].Reply != "thanks <ok>" {
		t.Fatalf("ack not persisted: %#v", p.OperatorMessages[0])
	}
}

func TestOperatorMessagesStrippedFromPublicFleet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	alice := seedOperatorMessageProfile(t, "alice", "c-alice", "contributor")
	alice.OperatorMessages = append(alice.OperatorMessages, ContributorOperatorMessage{ID: "m1", Text: "private", CreatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err := saveContributorProfile(alice); err != nil {
		t.Fatal(err)
	}
	s := NewServer(0, nil)
	s.contributeHub = NewContributeWSHub(nil, s)
	s.contributeHub.connections["a"] = &ContributorConnection{profile: alice, connectedAt: time.Now(), lastPong: time.Now()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/fleet", nil)
	s.handleContributeFleet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("public fleet status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "private") || strings.Contains(rec.Body.String(), "operator_messages") {
		t.Fatalf("public fleet leaked operator message: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/contribute/fleet", nil)
	req.Header.Set("X-Hive-Role", "read-write")
	s.handleContributeFleet(rec, req)
	if !strings.Contains(rec.Body.String(), "private") {
		t.Fatalf("operator fleet did not include message state: %s", rec.Body.String())
	}
}

func TestOperatorMessageDeliveryTargetsMatchingContributorOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	alice := seedOperatorMessageProfile(t, "alice", "c-alice", "contributor")
	bob := seedOperatorMessageProfile(t, "bob", "c-bob", "contributor")
	h := NewContributeWSHub(nil, NewServer(0, nil))
	sa, ca := wsPipe(t)
	defer sa.Close()
	defer ca.Close()
	sb, cb := wsPipe(t)
	defer sb.Close()
	defer cb.Close()
	h.connections["a"] = &ContributorConnection{ws: sa, profile: alice}
	h.connections["b"] = &ContributorConnection{ws: sb, profile: bob}
	msg := ContributorOperatorMessage{ID: "m1", Text: "hello", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if got := h.DeliverContributorOperatorMessage("c-alice", msg); got != 1 {
		t.Fatalf("delivered = %d, want 1", got)
	}
	var frame WSMessage
	if err := ca.ReadJSON(&frame); err != nil {
		t.Fatalf("alice read: %v", err)
	}
	if frame.OperatorMessage == nil || frame.OperatorMessage.ID != "m1" || !strings.Contains(frame.Message, "hive operator") {
		t.Fatalf("bad frame: %#v", frame)
	}
	_ = cb.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if err := cb.ReadJSON(&frame); err == nil {
		t.Fatalf("bob received leaked frame: %#v", frame)
	}
}

func TestOperatorMessageProfilePersistsOnDisk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	p := seedOperatorMessageProfile(t, "alice", "c-alice", "contributor")
	p.OperatorMessages = append(p.OperatorMessages, ContributorOperatorMessage{ID: "m1", Text: "hello", CreatedAt: "now"})
	if err := saveContributorProfile(p); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "alice.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk ContributorProfile
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if len(disk.OperatorMessages) != 1 || disk.OperatorMessages[0].Text != "hello" {
		t.Fatalf("message not persisted: %#v", disk.OperatorMessages)
	}
}
