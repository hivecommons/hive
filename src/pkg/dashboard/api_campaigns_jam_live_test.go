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
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		var msg jamLiveMessage
		if err := conn.ReadJSON(&msg); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) || strings.Contains(err.Error(), "i/o timeout") {
				continue
			}
			t.Fatalf("read live message %s: %v", want, err)
		}
		if msg.Type == want && (match == nil || match(msg)) {
			return msg
		}
	}
	t.Fatalf("timed out waiting for live message %s", want)
	return jamLiveMessage{}
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
