package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCoverageFloorAttributionAndHoldHelpers(t *testing.T) {
	if got := ModelFamily(" GPT-5.4-mini[preview] "); got != "gpt" {
		t.Fatalf("ModelFamily preview = %q, want gpt", got)
	}
	if got := ModelFamily("auto"); got != "unknown" {
		t.Fatalf("ModelFamily auto = %q, want unknown", got)
	}
	c := &Client{}
	if c.IsHeldLabels([]string{"ready"}) {
		t.Fatal("ready label should not be held")
	}
	c.SetHoldLabels([]string{"hive-pause/test-hive", "blocked"})
	if !c.IsHeldLabels([]string{"hive-pause/test-hive"}) {
		t.Fatal("exact configured hive-pause label should be held")
	}
	if !c.IsHeldLabels([]string{"needs-blocked-review"}) {
		t.Fatal("configured substring hold label should be held")
	}
	if c.IsHeldLabels([]string{"hive-pause/other"}) {
		t.Fatal("other hive-pause label should not be held")
	}
}

func TestSignedHeadMovedErrorUnwrap(t *testing.T) {
	wrapped := signedHeadMovedError{error: errSignedCommitSkip}
	if !errors.Is(wrapped, errSignedCommitSkip) {
		t.Fatal("signedHeadMovedError should unwrap to errSignedCommitSkip")
	}
}

func TestGetPRState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/7" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number":    7,
			"state":     "closed",
			"merged_at": "2026-10-01T15:00:00Z",
			"closed_at": "2026-10-01T15:01:00Z",
		})
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	state, err := c.GetPRState(context.Background(), "acme/widget", 7)
	if err != nil {
		t.Fatalf("GetPRState: %v", err)
	}
	if state.State != "closed" || state.MergedAt.IsZero() || state.ClosedAt.IsZero() {
		t.Fatalf("state = %+v, want closed with merged/closed times", state)
	}
	if _, err := (*Client)(nil).GetPRState(context.Background(), "acme/widget", 7); err == nil {
		t.Fatal("nil client GetPRState should fail")
	}
}
