package advisor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// chatBodyServer answers every review with an aside and keeps the last
// request body it read.
func chatBodyServer(t *testing.T, got *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		*got = map[string]any{}
		if err := json.Unmarshal(raw, got); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": `{"severity":"aside","text":"ok"}`}}},
			"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 2},
		})
	}))
}

// A role's reasoning effort reaches the endpoint as reasoning_effort, with
// room for the reasoning before the verdict. Without one the request is the
// one sent before: no reasoning_effort key and the 400-token verdict cap.
func TestEvaluatorSendsReasoningEffort(t *testing.T) {
	var got map[string]any
	srv := chatBodyServer(t, &got)
	defer srv.Close()

	for _, tc := range []struct {
		effort    string
		maxTokens float64
	}{
		{"high", reviewMaxTokensWithEffort},
		{"", reviewMaxTokens},
	} {
		e, err := NewEvaluator(Config{Endpoint: srv.URL, Model: "m", TimeoutS: 5, ReasoningEffort: tc.effort})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.Review(context.Background(), "scout", "the turn"); err != nil {
			t.Fatalf("effort %q: review: %v", tc.effort, err)
		}
		effort, present := got["reasoning_effort"]
		if tc.effort == "" && present {
			t.Errorf("no effort configured, but the request carries reasoning_effort %v", effort)
		}
		if tc.effort != "" && effort != tc.effort {
			t.Errorf("reasoning_effort = %v, want %q", effort, tc.effort)
		}
		if got["max_tokens"] != tc.maxTokens {
			t.Errorf("effort %q: max_tokens = %v, want %v", tc.effort, got["max_tokens"], tc.maxTokens)
		}
	}
}

// The record names the effort the review asked for beside the model.
func TestServerRecordsReasoningEffort(t *testing.T) {
	var got map[string]any
	chat := chatBodyServer(t, &got)
	defer chat.Close()
	store := NewStore("")
	srv := newTestServer(store, &testUsage{}, chat.URL, Runtime{ReasoningEffort: "max"})

	if rec, _ := postAdvise(t, srv.Handler(), "scout", `{"turn":"t1","transcript":"did a thing"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	records := store.List("scout", time.Time{}, 0)
	if len(records) != 1 || records[0].ReasoningEffort != "max" || records[0].Model != "test-model" {
		t.Fatalf("records = %+v", records)
	}
	if got["reasoning_effort"] != "max" {
		t.Fatalf("request reasoning_effort = %v, want max", got["reasoning_effort"])
	}
}
