package advisor

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
		name    string
		in      string
		want    Verdict
		wantErr bool
	}{
		{"plain", `{"severity":"concern","text":"check the loop bound"}`,
			Verdict{Severity: SeverityConcern, Text: "check the loop bound"}, false},
		{"fenced", "```json\n{\"severity\":\"blocker\",\"text\":\"stop\"}\n```",
			Verdict{Severity: SeverityBlocker, Text: "stop"}, false},
		{"case and space", `{"severity":" Aside ","text":" fine "}`,
			Verdict{Severity: SeverityAside, Text: "fine"}, false},
		{"unknown severity", `{"severity":"panic","text":"x"}`, Verdict{}, true},
		{"no json", "no objection at all", Verdict{}, true},
		{"malformed", `{"severity":}`, Verdict{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseVerdict(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestVerdictDelivery(t *testing.T) {
	if (Verdict{Severity: SeverityAside}).Interrupting() {
		t.Error("aside must be non-interrupting")
	}
	if !(Verdict{Severity: SeverityConcern}).Interrupting() || !(Verdict{Severity: SeverityBlocker}).Interrupting() {
		t.Error("concern and blocker must be interrupting")
	}
	if (Verdict{Severity: SeverityAside}).Delivery() != DeliveryNonInterrupting {
		t.Error("aside delivery mode")
	}
	if (Verdict{Severity: SeverityBlocker}).Delivery() != DeliveryInterrupting {
		t.Error("blocker delivery mode")
	}
}

func TestNewEvaluatorValidation(t *testing.T) {
	if _, err := NewEvaluator(Config{Model: "m"}); err == nil {
		t.Error("missing endpoint must be rejected")
	}
	if _, err := NewEvaluator(Config{Endpoint: "http://x"}); err == nil {
		t.Error("missing model must be rejected")
	}
	e, err := NewEvaluator(Config{Endpoint: "http://x/", Model: " m "})
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if e.endpoint != "http://x" || e.Model() != "m" {
		t.Errorf("endpoint/model not normalized: %q %q", e.endpoint, e.Model())
	}
}

func TestTailLines(t *testing.T) {
	in := "a\nb\nc\nd"
	if got := tailLines(in, 2); got != "c\nd" {
		t.Errorf("tail = %q", got)
	}
	if got := tailLines(in, 10); got != in {
		t.Errorf("short input must pass through: %q", got)
	}
	long := strings.Repeat("x", maxTranscriptChars+100)
	if got := tailLines(long, 1); len(got) != maxTranscriptChars {
		t.Errorf("char ceiling not applied: %d", len(got))
	}
}

func TestBuildPromptCarriesInstructions(t *testing.T) {
	msgs := buildPrompt("scout", "watch the tests", "did a thing")
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Content, "STANDING INSTRUCTIONS") ||
		!strings.Contains(msgs[0].Content, "watch the tests") {
		t.Error("operator instructions must ride the system prompt")
	}
	if !strings.Contains(msgs[1].Content, "scout") || !strings.Contains(msgs[1].Content, "did a thing") {
		t.Error("user message must carry agent and transcript")
	}
	// No instructions → no instructions header.
	msgs = buildPrompt("scout", "", "turn")
	if strings.Contains(msgs[0].Content, "STANDING INSTRUCTIONS") {
		t.Error("empty instructions must not add the header")
	}
}

// fakeChatServer answers /v1/chat/completions with the given content and usage.
func fakeChatServer(t *testing.T, status int, content string, gotAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		resp := map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
			"usage":   map[string]any{"prompt_tokens": 42, "completion_tokens": 7},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestEvaluatorReview(t *testing.T) {
	var auth string
	srv := fakeChatServer(t, http.StatusOK, `{"severity":"concern","text":"careful"}`, &auth)
	defer srv.Close()
	e, err := NewEvaluator(Config{Endpoint: srv.URL, APIKey: "k", Model: "m", TimeoutS: 5})
	if err != nil {
		t.Fatal(err)
	}
	v, usage, err := e.Review(context.Background(), "scout", "the turn transcript")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if v.Severity != SeverityConcern || v.Text != "careful" {
		t.Errorf("verdict = %+v", v)
	}
	if usage.InputTokens != 42 || usage.OutputTokens != 7 || usage.Total() != 49 {
		t.Errorf("usage = %+v", usage)
	}
	if auth != "Bearer k" {
		t.Errorf("auth header = %q", auth)
	}
}

func TestEvaluatorReviewEmptyTranscript(t *testing.T) {
	e, err := NewEvaluator(Config{Endpoint: "http://127.0.0.1:1", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := e.Review(context.Background(), "scout", "   \n  ")
	if err != nil {
		t.Fatalf("empty transcript must not error: %v", err)
	}
	if v.Severity != SeverityAside {
		t.Errorf("empty transcript verdict = %+v", v)
	}
}

func TestEvaluatorReviewErrors(t *testing.T) {
	srv := fakeChatServer(t, http.StatusBadGateway, "", nil)
	defer srv.Close()
	e, _ := NewEvaluator(Config{Endpoint: srv.URL, Model: "m", TimeoutS: 5})
	if _, _, err := e.Review(context.Background(), "scout", "turn"); err == nil {
		t.Error("non-200 must error (caller records the skip and fails open)")
	}

	bad := fakeChatServer(t, http.StatusOK, "not json at all", nil)
	defer bad.Close()
	e2, _ := NewEvaluator(Config{Endpoint: bad.URL, Model: "m", TimeoutS: 5})
	if _, _, err := e2.Review(context.Background(), "scout", "turn"); err == nil {
		t.Error("unusable answer must error")
	}
}
