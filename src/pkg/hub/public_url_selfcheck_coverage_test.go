package hub

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPublicURLSelfCheckCacheCloneAndErrors(t *testing.T) {
	publicURLSelfProbeCache.Lock()
	publicURLSelfProbeCache.url = ""
	publicURLSelfProbeCache.nextProbe = time.Time{}
	publicURLSelfProbeCache.result = nil
	publicURLSelfProbeCache.Unlock()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Host != "cached.example.com" {
			t.Errorf("Host = %q, want public host", r.Host)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	oldClient := publicURLSelfProbeClient
	oldPort := publicURLSelfProbePort
	publicURLSelfProbeClient = srv.Client()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	publicURLSelfProbePort, err = strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		publicURLSelfProbeClient = oldClient
		publicURLSelfProbePort = oldPort
	})

	first := publicURLSelfCheckFor(context.Background(), "  https://cached.example.com  ", nil)
	if first == nil || first.Status != PublicURLSelfCheckOK || first.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("first self-check = %+v", first)
	}
	first.Status = "mutated"
	second := publicURLSelfCheckFor(context.Background(), "https://cached.example.com", nil)
	if second.Status != PublicURLSelfCheckOK || calls != 1 {
		t.Fatalf("cached clone not used: second=%+v calls=%d", second, calls)
	}
	if publicURLSelfCheckFor(context.Background(), "   ", nil) != nil {
		t.Fatal("blank dashboard URL should skip the self-check")
	}

	long := strings.Repeat("世", 200)
	terse := terseProbeError(assertErr(long))
	if !strings.HasSuffix(terse, "…") || len([]rune(terse)) != 180 {
		t.Fatalf("terseProbeError did not truncate to 180 runes: %q (%d)", terse, len([]rune(terse)))
	}
	if terseProbeError(nil) != "" {
		t.Fatal("nil probe error should render empty")
	}
}

type assertErr string

func (e assertErr) Error() string { return string(e) }
