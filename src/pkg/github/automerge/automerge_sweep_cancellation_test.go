package automerge

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestContributorSweepPassCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{name: "generation canceled", cancel: true},
		{name: "github failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var requests atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if tc.cancel {
					cancel()
					<-r.Context().Done()
					return
				}
				http.Error(w, "upstream failure", http.StatusInternalServerError)
			}))
			defer api.Close()

			var logs bytes.Buffer
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, defaultContributorLanePolicy(), clock, Options{
				Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
			})
			result := engine.sweepPass(ctx, 1, false, nil, "tick")
			if requests.Load() == 0 {
				t.Fatal("contributor lane did not attempt to list pull requests")
			}
			failed := strings.Contains(logs.String(), "contributor-lane automerge sweep failed")
			if tc.cancel {
				if result != nil || failed || strings.Contains(logs.String(), "stage labels reconcile failed") {
					t.Fatalf("canceled pass: result=%+v logs=%s", result, logs.String())
				}
			} else if result == nil || !failed {
				t.Fatalf("GitHub failure must remain visible: result=%+v logs=%s", result, logs.String())
			}
		})
	}
}
