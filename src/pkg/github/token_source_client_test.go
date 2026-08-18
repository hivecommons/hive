package github

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

func TestClientWithTokenSourceUsesLatestMemoryToken(t *testing.T) {
	var (
		mu    sync.RWMutex
		token = "first"
		seen  []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = append(seen, request.Header.Get("Authorization"))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"login":"visual-hive[bot]","id":1,"type":"Bot"}`))
	}))
	defer server.Close()
	client := NewClientWithTokenSource(func(context.Context) (string, error) {
		mu.RLock()
		defer mu.RUnlock()
		return token, nil
	}, "owner", []string{"repo"}, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	base, _ := url.Parse(server.URL + "/")
	client.client.BaseURL = base
	if _, _, err := client.client.Users.Get(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	token = "second"
	mu.Unlock()
	if _, _, err := client.client.Users.Get(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "Bearer first" || seen[1] != "Bearer second" {
		t.Fatalf("authorization headers = %#v", seen)
	}
}
