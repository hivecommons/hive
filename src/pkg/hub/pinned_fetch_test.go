package hub

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/netguard"
)

// stubRebindingResolver answers host with public on the first lookup and
// loopback on every later one, the DNS rebinding shape #18992 closes: the
// SSRF guard's lookup sees a public address, the client's own lookup sees
// 127.0.0.1. It returns a lookup counter for host.
func stubRebindingResolver(t *testing.T, host string) func() int {
	t.Helper()
	var mu sync.Mutex
	n := 0
	orig := privateURLResolver
	privateURLResolver = func(_ context.Context, h string) ([]string, error) {
		if !strings.EqualFold(h, host) {
			return nil, errors.New("no such host")
		}
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return []string{publicTestResolvedAddr}, nil
		}
		return []string{"127.0.0.1"}, nil
	}
	t.Cleanup(func() { privateURLResolver = orig })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

func loopbackPort(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// The hive config proxy checks the self-reported DashboardURL with the real
// guard and then fetches it. The fetch must resolve through the same seam and
// dial only a checked address, so the rebound loopback answer is refused.
func TestHandleProxyHiveConfigPinsResolvedAddress(t *testing.T) {
	var reached atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer upstream.Close()
	lookups := stubRebindingResolver(t, "spoke.rebind.example")

	mux := proxyConfigServer(t, "alice", "http://spoke.rebind.example:"+loopbackPort(t, upstream))
	req := httptest.NewRequest("GET", "/api/saas/hive-config/cfg-hive", nil)
	req.AddCookie(testAuthCookie("alice"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if reached.Load() {
		t.Fatal("hive config proxy reached the rebound loopback address")
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("rebound fetch: expected 502, got %d (body: %s)", w.Code, w.Body.String())
	}
	if got := lookups(); got < 2 {
		t.Fatalf("resolver consulted %d times, want the guard's lookup and the pinned dial's own", got)
	}
}

func TestSpokeWebhookRelayClientPinsResolvedAddress(t *testing.T) {
	var reached atomic.Bool
	spoke := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer spoke.Close()
	stubRebindingResolver(t, "relay.rebind.example")

	dashboardURL := "http://relay.rebind.example:" + loopbackPort(t, spoke)
	if spokeWebhookRelayGuard(context.Background(), dashboardURL) {
		t.Fatal("the guard's lookup should see the first, public answer")
	}
	req, err := http.NewRequest(http.MethodPost, dashboardURL+spokeWebhookPath, bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := spokeWebhookRelayClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, netguard.ErrRefused) || reached.Load() {
		t.Fatalf("relay: err=%v reached=%v; want netguard.ErrRefused and no delivery", err, reached.Load())
	}
}

// The relay tests swap spokeWebhookRelayGuard to reach an httptest spoke; the
// pinned transport must honour that seam rather than capture the original.
func TestSpokeWebhookRelayClientHonoursGuardSeam(t *testing.T) {
	var reached atomic.Bool
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer spoke.Close()
	allowLoopbackRelay(t)

	req, err := http.NewRequest(http.MethodPost, spoke.URL+spokeWebhookPath, bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := spokeWebhookRelayClient.Do(req)
	if err != nil {
		t.Fatalf("relay with guard overridden: %v", err)
	}
	_ = resp.Body.Close()
	if !reached.Load() {
		t.Fatal("relay did not reach the allowed spoke")
	}
}
