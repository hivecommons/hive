package dashboard

import (
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

// stubRebindingImportResolver answers host with a public address on the first
// lookup and loopback afterwards: the pre-fetch guard sees public, a client
// that resolves again sees 127.0.0.1 (#18992).
func stubRebindingImportResolver(t *testing.T, host string) {
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
			return []string{"93.184.216.34"}, nil
		}
		return []string{"127.0.0.1"}, nil
	}
	t.Cleanup(func() { privateURLResolver = orig })
}

func TestSSRFPinnedTransport_RefusesRebindingAfterPreflight(t *testing.T) {
	var reached atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	stubRebindingImportResolver(t, "agents.rebind.example")

	target := "http://agents.rebind.example:" + port + "/agent.yaml"
	if err := validateImportFetchURL(context.Background(), target); err != nil {
		t.Fatalf("preflight should pass on the first (public) answer: %v", err)
	}
	client := &http.Client{CheckRedirect: noRedirectToPrivate, Transport: ssrfPinnedTransport}
	resp, err := client.Get(target)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, netguard.ErrRefused) || reached.Load() {
		t.Fatalf("rebound import fetch: err=%v reached=%v; want netguard.ErrRefused and no fetch", err, reached.Load())
	}
}

// The pinned transport's dial-time check is isPrivateURL itself, so the
// exact host:port test exemption reaches an httptest server and nothing else.
func TestSSRFPinnedTransport_UsesIsPrivateURLAtDialTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	client := &http.Client{Transport: ssrfPinnedTransport}

	if resp, err := client.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("pinned transport dialled loopback without an exemption")
	} else if !errors.Is(err, netguard.ErrRefused) {
		t.Fatalf("err = %v, want netguard.ErrRefused", err)
	}

	prev := privateURLTestExemptHostPorts
	privateURLTestExemptHostPorts = map[string]struct{}{strings.TrimPrefix(srv.URL, "http://"): {}}
	t.Cleanup(func() { privateURLTestExemptHostPorts = prev })
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("exempted loopback test server: %v", err)
	}
	_ = resp.Body.Close()
}
