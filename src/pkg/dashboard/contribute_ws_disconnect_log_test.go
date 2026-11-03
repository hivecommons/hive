package dashboard

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a goroutine-safe bytes.Buffer for capturing slog output
// written from the hub's HandleWS goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestContributeWS14163_AbnormalClosureIsNotAWarn: a contributor socket that
// ends without a close frame (gorilla synthesises 1006) is the peer's network,
// not a hub defect. It must not reach the WARN+ slog feed as a repeated
// "[contribute-ws] read error" (hivecommons/hive#14163); the drop is still
// recorded at INFO with the same close details.
func TestContributeWS14163_AbnormalClosureIsNotAWarn(t *testing.T) {
	s, ts := setupWSTest(t)
	defer ts.Close()
	logs := &lockedBuffer{}
	s.contributeHub.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	conn, reg := registerAndAuth(t, s, ts, "dropper")
	old := waitForLiveConn(t, s.contributeHub, reg["contributor_id"], 2*time.Second)

	// 1006: close the TCP connection under the websocket, no close frame.
	_ = conn.UnderlyingConn().Close()
	waitForOldConnGone(t, s.contributeHub, old, 5*time.Second)

	out := logs.String()
	if strings.Contains(out, "[contribute-ws] read error") {
		t.Fatalf("#14163: a 1006 abnormal closure was logged as a WARN read error:\n%s", out)
	}
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "dropped without close frame") || !strings.Contains(out, "close_code=1006") {
		t.Fatalf("#14163: expected an INFO drop line carrying close_code=1006, got:\n%s", out)
	}
}
