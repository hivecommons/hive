package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func jamLiveDial(t *testing.T, srv *httptest.Server, user, role string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("X-Hive-User", user)
	header.Set("X-Hive-Role", role)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/campaigns/spec-live/jam/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial jam websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readJamLiveUntil(t *testing.T, conn *websocket.Conn, want string, match func(jamLiveMessage) bool) jamLiveMessage {
	t.Helper()
	// One deadline for the whole wait: gorilla/websocket marks the connection
	// permanently failed after any read error (a deadline timeout included), so
	// retrying ReadJSON after a short timeout panics with "repeated read on
	// failed websocket connection" instead of waiting for the next frame.
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		var msg jamLiveMessage
		if err := conn.ReadJSON(&msg); err != nil {
			if strings.Contains(err.Error(), "i/o timeout") {
				t.Fatalf("timed out waiting for live message %s", want)
			}
			t.Fatalf("read live message %s: %v", want, err)
		}
		if msg.Type == want && (match == nil || match(msg)) {
			return msg
		}
	}
}

// jamLiveQuietGap is the silence TestReadJamLiveUntilWaitsPastShortGaps puts
// between the client's "ready" frame and the server's snapshot. It must stay
// longer than any per-read poll interval readJamLiveUntil might apply (the
// retired implementation used 250 ms); the test fails if it is not honoured.
const jamLiveQuietGap = 300 * time.Millisecond

// TestReadJamLiveUntilWaitsPastShortGaps guards the helper above: a frame that
// arrives after a quiet gap longer than any per-read poll interval must still
// be delivered rather than tripping gorilla's failed-connection panic.
func TestReadJamLiveUntilWaitsPastShortGaps(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Hand-off by signal: the client writes a frame once it is about to
		// block in readJamLiveUntil, so the quiet gap below is measured from a
		// known point rather than from an arbitrary moment after the dial.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		// Simulate a slow producer: the socket stays silent for longer than the
		// per-read poll the helper used to apply before the snapshot lands.
		<-time.After(jamLiveQuietGap)
		_ = conn.WriteJSON(jamLiveMessage{Type: jamLiveSnapshot})
		// Keep the socket open until the client is done reading.
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// The test goroutine is the only reader on conn: gorilla/websocket allows
	// one concurrent reader per connection, so the ready signal is a write.
	if err := conn.WriteJSON(jamLiveMessage{Type: jamLiveFocus}); err != nil {
		t.Fatalf("write ready signal: %v", err)
	}
	started := time.Now()
	got := readJamLiveUntil(t, conn, jamLiveSnapshot, nil)
	if got.Type != jamLiveSnapshot {
		t.Fatalf("message type = %q, want %q", got.Type, jamLiveSnapshot)
	}
	if waited := time.Since(started); waited < jamLiveQuietGap {
		t.Fatalf("snapshot arrived after %v, want a quiet gap of at least %v", waited, jamLiveQuietGap)
	}
}

func TestCampaignJamLiveRejectsCrossOriginHandshake(t *testing.T) {
	s := jamTestServer(t)
	httpSrv := httptest.NewServer(s.mux)
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/api/campaigns/spec-live/jam/ws"

	header := http.Header{}
	header.Set("X-Hive-Role", "read-write")
	header.Set("Origin", "https://evil.example")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err == nil {
		conn.Close()
		t.Fatal("cross-origin jam websocket handshake succeeded, want rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin handshake response = %+v, want 403", resp)
	}

	header.Set("Origin", httpSrv.URL)
	conn, _, err = websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("same-origin jam websocket handshake failed: %v", err)
	}
	conn.Close()
}

func TestCampaignJamLivePresenceFocusAndReconnect(t *testing.T) {
	s := jamTestServer(t)
	httpSrv := httptest.NewServer(s.mux)
	t.Cleanup(httpSrv.Close)

	alice := jamLiveDial(t, httpSrv, "alice", "read-write")
	_ = readJamLiveUntil(t, alice, jamLiveSnapshot, nil)
	bob := jamLiveDial(t, httpSrv, "bob", "read-write")
	_ = readJamLiveUntil(t, bob, jamLiveSnapshot, nil)
	_ = readJamLiveUntil(t, alice, jamLivePresence, func(msg jamLiveMessage) bool {
		return len(msg.Presence) == 2
	})

	if err := alice.WriteJSON(jamLiveMessage{Type: jamLiveFocus, Section: "Goals"}); err != nil {
		t.Fatalf("write focus: %v", err)
	}
	focused := readJamLiveUntil(t, bob, jamLivePresence, func(msg jamLiveMessage) bool {
		for _, participant := range msg.Presence {
			if participant.Actor.Name == "alice" && participant.Section == "Goals" {
				return true
			}
		}
		return false
	})
	if len(focused.Presence) != 2 {
		t.Fatalf("focused presence participants = %d, want 2", len(focused.Presence))
	}

	if err := bob.Close(); err != nil {
		t.Fatalf("close bob: %v", err)
	}
	_ = readJamLiveUntil(t, alice, jamLivePresence, func(msg jamLiveMessage) bool {
		return len(msg.Presence) == 1
	})
	bob = jamLiveDial(t, httpSrv, "bob", "read-write")
	reconnected := readJamLiveUntil(t, bob, jamLiveSnapshot, nil)
	if len(reconnected.Presence) != 2 {
		t.Fatalf("reconnected presence = %+v, want alice and bob", reconnected.Presence)
	}
}

func TestCampaignJamLiveConflictingEdits(t *testing.T) {
	s := jamTestServer(t)
	httpSrv := httptest.NewServer(s.mux)
	t.Cleanup(httpSrv.Close)

	alice := jamLiveDial(t, httpSrv, "alice", "read-write")
	bob := jamLiveDial(t, httpSrv, "bob", "read-write")
	aliceSnapshot := readJamLiveUntil(t, alice, jamLiveSnapshot, nil)
	bobSnapshot := readJamLiveUntil(t, bob, jamLiveSnapshot, nil)
	if aliceSnapshot.Jam.SpecRevisionID != "" || bobSnapshot.Jam.SpecRevisionID != "" {
		t.Fatalf("new jam revisions = %q/%q, want empty", aliceSnapshot.Jam.SpecRevisionID, bobSnapshot.Jam.SpecRevisionID)
	}

	if err := alice.WriteJSON(jamLiveMessage{Type: jamLiveEdit, BaseRevisionID: "", Content: "## Goals\nAlice"}); err != nil {
		t.Fatalf("write alice edit: %v", err)
	}
	applied := readJamLiveUntil(t, alice, jamLiveEditApplied, nil)
	if applied.RevisionID == "" || applied.Jam.SpecContent != "## Goals\nAlice" {
		t.Fatalf("applied edit = %+v", applied)
	}

	if err := bob.WriteJSON(jamLiveMessage{Type: jamLiveEdit, BaseRevisionID: "", Content: "## Goals\nBob"}); err != nil {
		t.Fatalf("write stale bob edit: %v", err)
	}
	conflict := readJamLiveUntil(t, bob, jamLiveConflict, nil)
	if conflict.Expected != applied.RevisionID || conflict.Jam.SpecContent != "## Goals\nAlice" {
		t.Fatalf("conflict = %+v, want expected %s and alice content", conflict, applied.RevisionID)
	}

	get := doOwnerGet(s, "/api/campaigns/spec-live/jam")
	jam := decodeJam(t, get)
	if jam.SpecContent != "## Goals\nAlice" || len(jam.Revisions) != 1 {
		t.Fatalf("stale edit overwrote state: %+v", jam)
	}
}

func TestCampaignJamLiveEnforcesReadLimit(t *testing.T) {
	s := jamTestServer(t)
	httpSrv := httptest.NewServer(s.mux)
	t.Cleanup(httpSrv.Close)

	// A read role is enough to open this socket, so the limit is what stops a
	// reader from making the server decode an unbounded frame.
	reader := jamLiveDial(t, httpSrv, "reader", "read")
	_ = readJamLiveUntil(t, reader, jamLiveSnapshot, nil)

	oversized := jamLiveMessage{Type: jamLiveFocus, Section: strings.Repeat("x", wsMaxMessageSize)}
	// A write error is an outcome here, not a failure: the server closes as
	// soon as it has read past the limit, possibly before the last byte lands.
	_ = reader.WriteJSON(oversized)
	if err := reader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		var msg jamLiveMessage
		err := reader.ReadJSON(&msg)
		if err != nil {
			break
		}
		if msg.Type == jamLivePresence || msg.Type == jamLiveSnapshot {
			continue
		}
		t.Fatalf("oversized frame produced %+v, want the connection to end", msg)
	}
}

func TestCampaignJamLiveRejectsEmptyEdit(t *testing.T) {
	s := jamTestServer(t)
	httpSrv := httptest.NewServer(s.mux)
	t.Cleanup(httpSrv.Close)

	alice := jamLiveDial(t, httpSrv, "alice", "read-write")
	_ = readJamLiveUntil(t, alice, jamLiveSnapshot, nil)
	if err := alice.WriteJSON(jamLiveMessage{Type: jamLiveEdit, BaseRevisionID: "", Content: "## Goals\nAlice"}); err != nil {
		t.Fatalf("write alice edit: %v", err)
	}
	applied := readJamLiveUntil(t, alice, jamLiveEditApplied, nil)

	if err := alice.WriteJSON(jamLiveMessage{Type: jamLiveEdit, BaseRevisionID: applied.RevisionID, Content: "   "}); err != nil {
		t.Fatalf("write blank edit: %v", err)
	}
	blank := readJamLiveUntil(t, alice, jamLiveError, nil)
	if blank.Error != "content required" {
		t.Fatalf("blank live edit error = %q, want content required", blank.Error)
	}

	jam := decodeJam(t, doOwnerGet(s, "/api/campaigns/spec-live/jam"))
	if jam.SpecContent != "## Goals\nAlice" || len(jam.Revisions) != 1 {
		t.Fatalf("blank live edit blanked the spec: %+v", jam)
	}
}
