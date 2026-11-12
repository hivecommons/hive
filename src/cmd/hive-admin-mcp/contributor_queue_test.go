package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

// The stdio binary reads the contributor queue through the same GET as the
// dashboard endpoint: the read tool pages the card's rows, and the write
// previews' state read asks for ?controls=1&withheld=1.
func TestStdioContributorQueueReadsTheCardsGET(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		body := map[string]any{"queue": []any{
			map[string]any{"key": "acme/widgets#3", "repo": "acme/widgets", "number": 3},
			map[string]any{"key": "acme/widgets#1", "repo": "acme/widgets", "number": 1, "held": true, "held_reason": "waiting on a design call"},
		}, "queue_total": 1, "held_total": 1}
		if r.URL.Query().Get("controls") == "1" {
			body["controls"] = map[string]any{"order": []string{"acme/widgets#3"}, "hold": []string{"acme/widgets#1"}, "hold_reasons": []any{}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	provider := readProvider{roster: &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, timeout: time.Second}}

	data, err := provider.Read(context.Background(), adminmcp.ToolContributorQueue, map[string]any{"withheld": true})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := data.(map[string]any)["rows"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["key"] != "acme/widgets#3" || rows[1].(map[string]any)["held_reason"] != "waiting on a design call" {
		t.Fatalf("rows = %#v", rows)
	}
	state, err := provider.Read(context.Background(), adminmcp.ContributorQueueStateRead, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.(map[string]any)["controls"]; !ok {
		t.Fatalf("state read = %#v", state)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "/api/contribute/queue?withheld=1" || !strings.HasPrefix(seen[1], "/api/contribute/queue?") ||
		!strings.Contains(seen[1], "controls=1") || !strings.Contains(seen[1], "withheld=1") {
		t.Fatalf("requests = %q", seen)
	}
}
