package knowledge

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

// allowDocFetchTo lets document fetches dial srv's loopback listener (and
// nothing else private) for the duration of a test. The exemption is one
// exact ip:port, so it cannot widen to all of loopback.
func allowDocFetchTo(t *testing.T, srv *httptest.Server) {
	t.Helper()
	allowed := strings.TrimPrefix(srv.URL, "http://")
	orig := docFetchRefuse
	docFetchRefuse = func(ctx context.Context, target string) bool {
		if target == allowed {
			return false
		}
		return netguard.IsPrivateURL(ctx, "http://"+target, nil)
	}
	t.Cleanup(func() { docFetchRefuse = orig })
}

// A document URL that passed the dashboard's pre-fetch isPrivateURL check
// (first lookup: public) must not be connected to when the name answers
// loopback on the client's own lookup (#18992). Before the pin, http.Client
// resolved the name again and dialled whatever came back.
func TestFetchURL_PinsAddressAcrossRebinding(t *testing.T) {
	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		_, _ = w.Write([]byte("internal secret"))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	var mu sync.Mutex
	lookups := 0
	orig := docRedirectResolver
	t.Cleanup(func() { docRedirectResolver = orig })
	docRedirectResolver = func(_ context.Context, host string) ([]string, error) {
		if host != "rebind.attacker.example" {
			return nil, errors.New("no such host")
		}
		mu.Lock()
		defer mu.Unlock()
		lookups++
		if lookups == 1 {
			return []string{"93.184.216.34"}, nil
		}
		return []string{"127.0.0.1"}, nil
	}

	target := "http://rebind.attacker.example:" + port + "/doc"
	if docRedirectHostIsPrivate(context.Background(), "rebind.attacker.example") {
		t.Fatal("first lookup should look public to the pre-fetch check")
	}
	ds := &DocumentSource{}
	_, _, err := ds.fetchURL(context.Background(), target)
	if err == nil || hit.Load() {
		t.Fatalf("fetchURL reached the rebound loopback address: err=%v hit=%v", err, hit.Load())
	}
	if !errors.Is(err, netguard.ErrRefused) {
		t.Fatalf("err = %v, want netguard.ErrRefused", err)
	}
}

// The dial-time check uses the netip classifier, so an answer the redirect
// guard's older classifier misses (carrier-grade NAT) is refused too.
func TestFetchURL_RefusesAnyBarredAnswer(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{
		"mixed.attacker.example": {"93.184.216.34", "100.100.100.200"},
	})
	ds := &DocumentSource{}
	_, _, err := ds.fetchURL(context.Background(), "http://mixed.attacker.example/doc")
	if !errors.Is(err, netguard.ErrRefused) {
		t.Fatalf("err = %v, want netguard.ErrRefused", err)
	}
}
