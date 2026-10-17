package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestAppBotUserIDResolvesAndCachesBotUser(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/users/hivecommons-hive[bot]", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"login":"hivecommons-hive[bot]","id":280983584,"type":"Bot"}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "hivecommons", []string{"hivecommons/hive"})
	c.SetAppBotLogin("hivecommons-hive[bot]")

	if got := c.AppBotUserID(context.Background()); got != 280983584 {
		t.Fatalf("AppBotUserID = %d, want 280983584 (bot USER id, not App id)", got)
	}
	if got := c.AppBotUserID(context.Background()); got != 280983584 {
		t.Fatalf("second AppBotUserID = %d, want cached 280983584", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("users lookup calls = %d, want 1 (cached after first success)", n)
	}
}

func TestAppBotUserIDZeroWithoutBotOrOnFailure(t *testing.T) {
	var nilClient *Client
	if got := nilClient.AppBotUserID(context.Background()); got != 0 {
		t.Fatalf("nil client AppBotUserID = %d, want 0", got)
	}

	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"acme/widgets"})
	if got := c.AppBotUserID(context.Background()); got != 0 {
		t.Fatalf("AppBotUserID without bot login = %d, want 0", got)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("lookup without bot login made %d calls, want 0", n)
	}

	c.SetAppBotLogin("missing[bot]")
	if got := c.AppBotUserID(context.Background()); got != 0 {
		t.Fatalf("AppBotUserID on 404 = %d, want 0", got)
	}
	if got := c.AppBotUserID(context.Background()); got != 0 {
		t.Fatalf("AppBotUserID on repeated 404 = %d, want 0", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("failed lookups = %d calls, want 2 (failures are retried, not cached)", n)
	}
}
