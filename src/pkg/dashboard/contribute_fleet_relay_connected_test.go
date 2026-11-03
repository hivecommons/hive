package dashboard

import (
	"testing"
	"time"
)

// relayConnectedSince feeds the relay_audit_coverage posture check: it must
// report only relays that authenticated inside the window, never a connection
// that merely stayed up from before it (which writes no relay_* row and would
// otherwise produce a false FAIL).
func TestRelayConnectedSinceUsesOnlyTheAuthTimestamp(t *testing.T) {
	h := &ContributeWSHub{}
	since := time.Now().Add(-24 * time.Hour)

	if h.relayConnectedSince(since) {
		t.Fatal("fresh hub with no relay auth must not count as connected")
	}

	h.lastRelayConnectNanos.Store(since.Add(-time.Hour).UnixNano())
	if h.relayConnectedSince(since) {
		t.Fatal("relay authenticated before the window must not count, even if still connected")
	}

	h.lastRelayConnectNanos.Store(since.Add(time.Minute).UnixNano())
	if !h.relayConnectedSince(since) {
		t.Fatal("relay authenticated inside the window must count")
	}
}
