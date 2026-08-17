package hub

import "testing"

// TestFundedGatewayIsReOfferedUntilConfirmed pins that a gateway the user has
// PAID for is not lost to a single dropped beat.
//
// takePendingGateway deleted the record the moment it went on the wire, so a
// response lost in flight, a hub restart before the next beat, or a spoke-side
// ApplyDeliveredGateway error all dropped it permanently — no retry, and only a
// spoke log line to say the user's money bought nothing.
func TestFundedGatewayIsReOfferedUntilConfirmed(t *testing.T) {
	s := &HubServer{pendingGateways: map[string]*HeartbeatGatewayConfig{}}
	s.queuePendingGateway("h1", &HeartbeatGatewayConfig{Name: "openrouter", Key: "secret"})

	t.Run("offered while the spoke has not reported it", func(t *testing.T) {
		if got := s.pendingGatewayFor("h1", nil); got == nil {
			t.Fatal("not offered on the first beat")
		}
		// The beat is lost. It must still be offered.
		if got := s.pendingGatewayFor("h1", nil); got == nil {
			t.Fatal("a lost delivery was dropped — this is the paid-for-nothing case")
		}
	})

	t.Run("an empty report is UNKNOWN, not a confirmation", func(t *testing.T) {
		// A spoke too old to report gateways sends nothing. That must never be
		// read as "it has the gateway", or delivery silently stops.
		if got := s.pendingGatewayFor("h1", []string{}); got == nil {
			t.Fatal("an empty report cleared the pending gateway")
		}
	})

	t.Run("a DIFFERENT gateway is not a confirmation", func(t *testing.T) {
		if got := s.pendingGatewayFor("h1", []string{"some-other-gw"}); got == nil {
			t.Fatal("an unrelated gateway confirmed the delivery")
		}
	})

	t.Run("reporting it back clears the record and stops the offers", func(t *testing.T) {
		if got := s.pendingGatewayFor("h1", []string{"openrouter"}); got != nil {
			t.Fatal("still offered after the spoke confirmed it")
		}
		// And it stays cleared — no re-charge, no re-delivery.
		if got := s.pendingGatewayFor("h1", nil); got != nil {
			t.Fatal("the gateway came back after being confirmed")
		}
	})

	t.Run("case-insensitive, like every other name comparison here", func(t *testing.T) {
		s.queuePendingGateway("h2", &HeartbeatGatewayConfig{Name: "openrouter"})
		if got := s.pendingGatewayFor("h2", []string{"OpenRouter"}); got != nil {
			t.Fatal("a case-different report failed to confirm")
		}
	})
}
