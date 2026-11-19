package hub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/netguard"
)

// rebindingContributeHub registers one public contribute hive whose
// DashboardURL names host on upstream's loopback port, with the resolver
// answering public first and loopback after (stubRebindingResolver).
func rebindingContributeHub(t *testing.T, host string, upstream *httptest.Server) (*HubServer, func() int) {
	t.Helper()
	lookups := stubRebindingResolver(t, host)
	srv := newHubServerForTest(t)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "rebind-hive", Online: true, IsPublic: true, DashboardURL: "http://" + host + ":" + loopbackPort(t, upstream), Owner: "user1"},
	}
	srv.mu.Unlock()
	return srv, lookups
}

// findContributeHive admits the hive on the resolver's first (public) answer.
// The registration proxy must resolve again at dial time through the same
// seam, see the rebound loopback answer, and refuse it (#19221).
func TestHandleContributeProxyPinsResolvedAddress(t *testing.T) {
	var reached atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer upstream.Close()
	srv, lookups := rebindingContributeHub(t, "contrib.rebind.example", upstream)

	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.handleContributeProxy(rec, req)

	if reached.Load() {
		t.Fatal("contribute proxy reached the rebound loopback address")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("rebound proxy: status = %d, want 502", rec.Code)
	}
	if got := lookups(); got < 2 {
		t.Fatalf("resolver consulted %d times, want the guard's lookup and the pinned dial's own", got)
	}
}

func TestHandleContributeWSProxyPinsResolvedAddress(t *testing.T) {
	var reached atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer upstream.Close()
	srv, lookups := rebindingContributeHub(t, "contrib-ws.rebind.example", upstream)

	req := httptest.NewRequest(http.MethodGet, "/api/contribute/ws", nil)
	rec := httptest.NewRecorder()
	srv.handleContributeWSProxy(rec, req)

	if reached.Load() {
		t.Fatal("contribute WS proxy reached the rebound loopback address")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("rebound WS proxy: status = %d, want 502", rec.Code)
	}
	if got := lookups(); got < 2 {
		t.Fatalf("resolver consulted %d times, want the guard's lookup and the pinned dial's own", got)
	}
}

// validateLiteGitHubHost passes a github_host on the first (public) answer;
// the repo access request must not then connect to the rebound loopback one.
func TestVerifyGitHubRepoAccessPinsResolvedAddress(t *testing.T) {
	lookups := stubRebindingResolver(t, "ghe.rebind.example")

	ok, err := verifyGitHubRepoAccess(context.Background(), "tok", "ghe.rebind.example", "o", "r")
	if ok {
		t.Fatal("a rebound github_host reported access")
	}
	if !errors.Is(err, netguard.ErrRefused) {
		t.Fatalf("err = %v, want netguard.ErrRefused from the pinned dial", err)
	}
	if got := lookups(); got < 2 {
		t.Fatalf("resolver consulted %d times, want the guard's lookup and the pinned dial's own", got)
	}
}

// Without a replaced http.DefaultTransport the client carries the pinned
// transport; a replaced one (the test convention) is honoured instead.
func TestLiteRepoAccessHTTPClientUsesPinnedTransport(t *testing.T) {
	if got := liteRepoAccessHTTPClient().Transport; got != liteRepoAccessTransport {
		t.Fatalf("Transport = %T, want liteRepoAccessTransport", got)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	swapDefaultTransport(t, srv)
	if got := liteRepoAccessHTTPClient().Transport; got != http.DefaultTransport {
		t.Fatalf("Transport = %T, want the replaced http.DefaultTransport", got)
	}
}
