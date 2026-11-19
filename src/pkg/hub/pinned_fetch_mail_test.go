package hub

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// The mail relay checks the self-reported DashboardURL with the real guard
// (first, public answer) and then posts to it. The post must resolve again
// through the same seam, see the rebound loopback answer and refuse it, so
// the loopback spoke receives nothing (#19221).
func TestMailRelayPinsResolvedAddress(t *testing.T) {
	var reached atomic.Bool
	spoke := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer spoke.Close()
	const host = "mail.rebind.example"
	lookups := stubRebindingResolver(t, host)
	s := mailTestHub(t, "http://"+host+":"+loopbackPort(t, spoke)+"/")

	rec := httptest.NewRecorder()
	s.handleMailInbound(rec, mailRequest(http.MethodPost, wire.InboundPath, testMailToken, delivery(testDeliveryID, wire.KindInvite, "hive@acme.hive.hivecommons.dev")))

	if reached.Load() {
		t.Fatal("mail relay reached the rebound loopback address")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("rebound mail relay: status = %d, want 502", rec.Code)
	}
	if got := lookups(); got < 2 {
		t.Fatalf("resolver consulted %d times, want the guard's lookup and the pinned dial's own", got)
	}
}
