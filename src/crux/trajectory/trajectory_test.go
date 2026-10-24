package trajectory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantDiv  bool
		wantErr  bool
		wantConf float64
	}{
		{"clean json", `{"divergent":true,"confidence":0.9,"reason":"escaping sandbox"}`, true, false, 0.9},
		{"non-divergent", `{"divergent":false,"confidence":0.2,"reason":"normal work"}`, false, false, 0.2},
		{"code fenced", "```json\n{\"divergent\":true,\"confidence\":1.0,\"reason\":\"x\"}\n```", true, false, 1.0},
		{"prose wrapped", `Here is my verdict: {"divergent":false,"confidence":0.1,"reason":"ok"} done.`, false, false, 0.1},
		{"confidence clamp high", `{"divergent":true,"confidence":2.5,"reason":"x"}`, true, false, 1.0},
		{"confidence clamp low", `{"divergent":true,"confidence":-1,"reason":"x"}`, true, false, 0.0},
		{"no json", `I cannot answer.`, false, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := ParseVerdict(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", v)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if v.Divergent != c.wantDiv {
				t.Errorf("divergent = %v, want %v", v.Divergent, c.wantDiv)
			}
			if v.Confidence != c.wantConf {
				t.Errorf("confidence = %v, want %v", v.Confidence, c.wantConf)
			}
		})
	}
}

func TestTailLines(t *testing.T) {
	in := "a\nb\nc\nd\ne"
	if got := tailLines(in, 2); got != "d\ne" {
		t.Errorf("tailLines(_,2) = %q, want %q", got, "d\ne")
	}
	if got := tailLines(in, 100); got != in {
		t.Errorf("tailLines wants full string when n exceeds length")
	}
	// char cap
	long := strings.Repeat("x", maxTranscriptChars+500)
	if got := tailLines(long, 10); len(got) != maxTranscriptChars {
		t.Errorf("tailLines char cap = %d, want %d", len(got), maxTranscriptChars)
	}
}

func TestNewReviewerValidation(t *testing.T) {
	if _, err := NewReviewer(Config{Endpoint: "", Model: "m"}); err == nil {
		t.Error("expected error for empty endpoint")
	}
	if _, err := NewReviewer(Config{Endpoint: "http://x", Model: ""}); err == nil {
		t.Error("expected error for empty model")
	}
	r, err := NewReviewer(Config{Endpoint: "http://x/", Model: "m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.endpoint != "http://x" {
		t.Errorf("endpoint trailing slash not trimmed: %q", r.endpoint)
	}
	if r.transcriptLines != DefaultTranscriptLines {
		t.Errorf("default transcript lines not applied")
	}
}

func TestReviewAgainstServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("missing/wrong auth header: %q", r.Header.Get("Authorization"))
		}
		var req chatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "review-model" {
			t.Errorf("model = %q, want review-model", req.Model)
		}
		// intent + transcript both present in the user message
		var userMsg string
		for _, m := range req.Messages {
			if m.Role == "user" {
				userMsg = m.Content
			}
		}
		if !strings.Contains(userMsg, "open a PR") || !strings.Contains(userMsg, "split the token") {
			t.Errorf("user message missing intent/transcript: %q", userMsg)
		}
		json.NewEncoder(w).Encode(chatResponse{Choices: []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}{{Message: struct {
			Content string `json:"content"`
		}{Content: `{"divergent":true,"confidence":0.95,"reason":"credential obfuscation"}`}}}})
	}))
	defer srv.Close()

	r, err := NewReviewer(Config{Endpoint: srv.URL, APIKey: "k", Model: "review-model"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.Review(context.Background(), "scanner", "open a PR that fixes the bug", "I will split the token into two parts")
	if err != nil {
		t.Fatalf("review error: %v", err)
	}
	if !v.Divergent || v.Confidence != 0.95 {
		t.Errorf("unexpected verdict: %+v", v)
	}
}

func TestReviewEmptyTranscriptSkips(t *testing.T) {
	r, _ := NewReviewer(Config{Endpoint: "http://unused", Model: "m"})
	v, err := r.Review(context.Background(), "a", "intent", "   \n  ")
	if err != nil || v.Divergent {
		t.Errorf("empty transcript should be a no-op non-divergent verdict, got %+v err=%v", v, err)
	}
}

// TestReviewNoChoicesErrors: a 200 response with an empty choices array must
// error rather than panic on index access.
func TestReviewNoChoicesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatResponse{Choices: nil})
	}))
	defer srv.Close()
	r, err := NewReviewer(Config{Endpoint: srv.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.Review(context.Background(), "a", "intent", "some work")
	if err == nil {
		t.Error("expected error on empty choices")
	}
	if v.Divergent {
		t.Error("verdict must be non-divergent on error")
	}
}

// TestReviewMalformedJSONBody: a 200 response whose body is not valid JSON
// must surface a decode error, not a panic.
func TestReviewMalformedJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json at all"))
	}))
	defer srv.Close()
	r, err := NewReviewer(Config{Endpoint: srv.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Review(context.Background(), "a", "intent", "some work"); err == nil {
		t.Error("expected decode error on malformed body")
	}
}

// TestReviewTransportError: an unreachable endpoint (nothing listening)
// surfaces a transport error from http.Client.Do.
func TestReviewTransportError(t *testing.T) {
	r, err := NewReviewer(Config{Endpoint: "http://127.0.0.1:1", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Review(context.Background(), "a", "intent", "some work"); err == nil {
		t.Error("expected transport error against unreachable endpoint")
	}
}

// TestReviewBuildRequestError: a malformed endpoint URL (control characters
// in the scheme/host) makes http.NewRequestWithContext itself fail.
func TestReviewBuildRequestError(t *testing.T) {
	r, err := NewReviewer(Config{Endpoint: "http://\x7f", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Review(context.Background(), "a", "intent", "some work"); err == nil {
		t.Error("expected request-build error on malformed endpoint")
	}
}

func TestReviewFailsOpenOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	r, _ := NewReviewer(Config{Endpoint: srv.URL, Model: "m"})
	v, err := r.Review(context.Background(), "a", "intent", "some work")
	if err == nil {
		t.Error("expected error on 500")
	}
	if v.Divergent {
		t.Error("verdict must be non-divergent on error (fail open)")
	}
}
