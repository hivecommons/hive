package github

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestLabelReplayTracksCurrentState(t *testing.T) {
	var mu sync.Mutex
	present, writes, gets := false, 0, 0
	unreadable := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			gets++
			if unreadable {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			labels := []map[string]string{}
			if present {
				labels = append(labels, map[string]string{"name": "X"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 63, "labels": labels})
		case http.MethodPost:
			writes++
			present = true
			_, _ = w.Write([]byte(`[{"name":"X"}]`))
		case http.MethodDelete:
			writes++
			present = false
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	c := NewClientForTest(srv.URL, "o", nil, logger)
	b := replayTestBoundary(t)
	b.Logger = logger
	c.SetMutationBoundary(b)
	ctx := context.Background()
	for cycle := 0; cycle < 2; cycle++ {
		if err := c.AddLabels(ctx, "o/r", 63, []string{"X"}); err != nil {
			t.Fatal(err)
		}
		if err := c.RemoveLabel(ctx, "o/r", 63, "X"); err != nil {
			t.Fatal(err)
		}
		// Retrying the same removal must observe the absent label and skip DELETE.
		if err := c.RemoveLabel(ctx, "o/r", 63, "X"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	if present || writes != 4 || gets != 4 {
		t.Errorf("present=%v writes=%d gets=%d, want false/4/4", present, writes, gets)
	}
	unreadable = true
	mu.Unlock()
	if err := c.RemoveLabel(ctx, "o/r", 63, "X"); err == nil {
		t.Fatal("unreadable cached state returned success")
	}
	mu.Lock()
	if writes != 4 {
		t.Errorf("unreadable state caused write: %d", writes)
	}
	mu.Unlock()
	for _, want := range []string{"applied=true", "applied=false"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing observation %q in %s", want, logs.String())
		}
	}
}
