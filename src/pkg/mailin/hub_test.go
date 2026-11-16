package mailin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/mailin/wire"
)

func tokenFile(t *testing.T, tok string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeHubServer serves the hub mail API, checking the bearer token.
func fakeHubServer(t *testing.T, hosts []string, inboundStatus *atomic.Int32, got chan<- wire.Delivery) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == wire.HostsPath:
			_ = json.NewEncoder(w).Encode(wire.HostsResponse{Hosts: hosts})
		case r.Method == http.MethodPost && r.URL.Path == wire.InboundPath:
			if r.Header.Get("Content-Type") != "application/json" {
				http.Error(w, "bad type", http.StatusUnsupportedMediaType)
				return
			}
			var d wire.Delivery
			if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if got != nil {
				got <- d
			}
			code := http.StatusAccepted
			if inboundStatus != nil && inboundStatus.Load() != 0 {
				code = int(inboundStatus.Load())
			}
			w.WriteHeader(code)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHubClientHosts(t *testing.T) {
	srv := fakeHubServer(t, []string{"Acme.hive.hivecommons.dev.", " ", strings.Repeat("a", maxHostLen+1), "b.example"}, nil, nil)
	h := &HubClient{BaseURL: srv.URL + "/", TokenFile: tokenFile(t, "s3cret")}
	hosts, err := h.FetchHosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(hosts, ",") != "acme.hive.hivecommons.dev,b.example" {
		t.Fatalf("hosts %v", hosts)
	}
	bad := &HubClient{BaseURL: srv.URL, TokenFile: tokenFile(t, "wrong")}
	if _, err := bad.FetchHosts(context.Background()); err == nil {
		t.Fatal("unauthorized fetch succeeded")
	}
	missing := &HubClient{BaseURL: srv.URL, TokenFile: filepath.Join(t.TempDir(), "none")}
	if _, err := missing.FetchHosts(context.Background()); err == nil {
		t.Fatal("missing token file accepted")
	}
	empty := &HubClient{BaseURL: srv.URL, TokenFile: tokenFile(t, "  ")}
	if _, err := empty.FetchHosts(context.Background()); err == nil {
		t.Fatal("empty token accepted")
	}
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{not json") }))
	defer junk.Close()
	if _, err := (&HubClient{BaseURL: junk.URL, TokenFile: tokenFile(t, "s3cret")}).FetchHosts(context.Background()); err == nil {
		t.Fatal("junk body accepted")
	}
	if _, err := (&HubClient{BaseURL: "http://127.0.0.1:1", TokenFile: tokenFile(t, "s3cret")}).FetchHosts(context.Background()); err == nil {
		t.Fatal("unreachable hub accepted")
	}
	if _, err := (&HubClient{BaseURL: "://bad", TokenFile: tokenFile(t, "s3cret")}).FetchHosts(context.Background()); err == nil {
		t.Fatal("bad URL accepted")
	}
}

func TestHubClientDeliver(t *testing.T) {
	var status atomic.Int32
	got := make(chan wire.Delivery, 8)
	srv := fakeHubServer(t, nil, &status, got)
	h := &HubClient{BaseURL: srv.URL, TokenFile: tokenFile(t, "s3cret"), HTTP: srv.Client()}
	d := wire.Delivery{ID: "d1", Kind: wire.KindFiles, Recipient: "hive@acme", Files: &wire.Files{Title: "t",
		Attachments: []wire.Attachment{{Name: "a.png", Data: pngData}}}}
	if err := h.Deliver(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if r := <-got; r.ID != "d1" || string(r.Files.Attachments[0].Data) != string(pngData) {
		t.Fatalf("round trip %+v", r)
	}
	for code, rejected := range map[int32]bool{404: true, 422: true, 429: false, 408: false, 502: false} {
		status.Store(code)
		err := h.Deliver(context.Background(), d)
		<-got
		if err == nil || errors.Is(err, ErrRejected) != rejected {
			t.Errorf("HTTP %d: %v", code, err)
		}
	}
	if err := (&HubClient{BaseURL: srv.URL, TokenFile: "/nonexistent"}).Deliver(context.Background(), d); err == nil {
		t.Fatal("missing token accepted")
	}
	if err := (&HubClient{BaseURL: "http://127.0.0.1:1", TokenFile: tokenFile(t, "s3cret")}).Deliver(context.Background(), d); err == nil || errors.Is(err, ErrRejected) {
		t.Fatalf("unreachable hub: %v", err)
	}
}

type flakyFetcher struct {
	n     atomic.Int32
	hosts []string
}

func (f *flakyFetcher) FetchHosts(context.Context) ([]string, error) {
	if f.n.Add(1)%2 == 0 {
		return nil, errors.New("hub down")
	}
	return f.hosts, nil
}

func TestHostSetRefresh(t *testing.T) {
	var s HostSet
	if s.Ready() || s.Has("x") || s.Len() != 0 {
		t.Fatal("empty set")
	}
	f := &flakyFetcher{hosts: []string{"a.example"}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Refresh(ctx, f, 5*time.Millisecond, slog.New(slog.DiscardHandler)); close(done) }()
	testutil.Eventually(t, 5*time.Second, func() bool { return f.n.Load() >= 4 }, "host list refreshed four times")
	cancel()
	<-done
	// A failed refresh keeps the last good list.
	if !s.Ready() || !s.Has("A.example") || s.Len() != 1 {
		t.Fatalf("after refresh: ready=%v len=%d", s.Ready(), s.Len())
	}

	// A zero interval falls back to the default and still loads once.
	var s2 HostSet
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	s2.Refresh(ctx2, &flakyFetcher{hosts: []string{"b.example"}}, 0, slog.New(slog.DiscardHandler))
	if !s2.Has("b.example") {
		t.Fatal("initial load")
	}
}
