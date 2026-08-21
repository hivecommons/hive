package intent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMisaligned(t *testing.T) {
	tests := []struct {
		name string
		v    AlignmentVerdict
		want bool
	}{
		{"aligned empty", AlignmentVerdict{Status: AlignmentStatusAligned}, false},
		{"top-level misaligned", AlignmentVerdict{Status: AlignmentStatusMisaligned}, true},
		{"deterministic finding misaligned", AlignmentVerdict{
			Status:                AlignmentStatusAligned,
			DeterministicFindings: []AlignmentFinding{{Code: "scope-drift", Status: AlignmentStatusMisaligned}},
		}, true},
		{"deterministic finding aligned", AlignmentVerdict{
			Status:                AlignmentStatusAligned,
			DeterministicFindings: []AlignmentFinding{{Code: "note", Status: AlignmentStatusAligned}},
		}, false},
		{"model misaligned", AlignmentVerdict{
			Status: AlignmentStatusAligned,
			Model:  &ModelAlignmentVerdict{Status: AlignmentStatusMisaligned},
		}, true},
		{"model aligned", AlignmentVerdict{
			Status: AlignmentStatusAligned,
			Model:  &ModelAlignmentVerdict{Status: AlignmentStatusAligned},
		}, false},
		{"unclear is not misaligned", AlignmentVerdict{
			Status: AlignmentStatusUnclear,
			Model:  &ModelAlignmentVerdict{Status: AlignmentStatusUnclear},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.Misaligned(); got != tt.want {
				t.Errorf("Misaligned() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergeAlignment(t *testing.T) {
	base := AlignmentVerdict{Status: AlignmentStatusAligned, Rationale: "deterministic ok"}

	t.Run("model misaligned overrides", func(t *testing.T) {
		got := MergeAlignment(base, &ModelAlignmentVerdict{Status: AlignmentStatusMisaligned, Rationale: "scope creep"}, nil)
		if got.Status != AlignmentStatusMisaligned {
			t.Errorf("Status = %q, want misaligned", got.Status)
		}
		if got.Rationale != "scope creep" {
			t.Errorf("Rationale = %q, want model rationale", got.Rationale)
		}
		if got.Model == nil {
			t.Error("Model verdict not attached")
		}
	})

	t.Run("model misaligned with empty rationale keeps base rationale", func(t *testing.T) {
		got := MergeAlignment(base, &ModelAlignmentVerdict{Status: AlignmentStatusMisaligned}, nil)
		if got.Rationale != "deterministic ok" {
			t.Errorf("Rationale = %q, want base rationale", got.Rationale)
		}
	})

	t.Run("model unclear downgrades aligned", func(t *testing.T) {
		got := MergeAlignment(base, &ModelAlignmentVerdict{Status: AlignmentStatusUnclear}, nil)
		if got.Status != AlignmentStatusUnclear {
			t.Errorf("Status = %q, want unclear", got.Status)
		}
		if got.Rationale != "model could not determine alignment" {
			t.Errorf("Rationale = %q, want fallback rationale", got.Rationale)
		}
	})

	t.Run("model unclear does not upgrade misaligned base", func(t *testing.T) {
		misBase := AlignmentVerdict{Status: AlignmentStatusMisaligned, Rationale: "drift"}
		got := MergeAlignment(misBase, &ModelAlignmentVerdict{Status: AlignmentStatusUnclear}, nil)
		if got.Status != AlignmentStatusMisaligned {
			t.Errorf("Status = %q, want misaligned preserved", got.Status)
		}
	})

	t.Run("model aligned keeps base", func(t *testing.T) {
		got := MergeAlignment(base, &ModelAlignmentVerdict{Status: AlignmentStatusAligned}, nil)
		if got.Status != AlignmentStatusAligned {
			t.Errorf("Status = %q, want aligned", got.Status)
		}
	})

	t.Run("model error recorded", func(t *testing.T) {
		got := MergeAlignment(base, nil, errors.New("reviewer down"))
		if got.ModelError != "reviewer down" {
			t.Errorf("ModelError = %q", got.ModelError)
		}
		if got.Status != AlignmentStatusAligned {
			t.Errorf("Status = %q, want aligned base preserved", got.Status)
		}
	})

	t.Run("empty status defaults to aligned", func(t *testing.T) {
		got := MergeAlignment(AlignmentVerdict{}, nil, nil)
		if got.Status != AlignmentStatusAligned {
			t.Errorf("Status = %q, want aligned default", got.Status)
		}
	})
}

func TestParseModelAlignmentVerdict(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		v, err := ParseModelAlignmentVerdict(`{"status":"aligned","confidence":0.9,"rationale":"matches intent"}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v.Status != AlignmentStatusAligned || v.Confidence != 0.9 || v.Rationale != "matches intent" {
			t.Errorf("unexpected verdict: %+v", v)
		}
	})

	t.Run("JSON wrapped in prose", func(t *testing.T) {
		v, err := ParseModelAlignmentVerdict("Sure, here is my verdict:\n```json\n{\"status\":\"misaligned\",\"confidence\":1}\n```\nDone.")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v.Status != AlignmentStatusMisaligned {
			t.Errorf("Status = %q, want misaligned", v.Status)
		}
	})

	t.Run("confidence clamped low", func(t *testing.T) {
		v, err := ParseModelAlignmentVerdict(`{"status":"unclear","confidence":-3}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v.Confidence != 0 {
			t.Errorf("Confidence = %v, want 0", v.Confidence)
		}
	})

	t.Run("confidence clamped high", func(t *testing.T) {
		v, err := ParseModelAlignmentVerdict(`{"status":"unclear","confidence":42}`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v.Confidence != 1 {
			t.Errorf("Confidence = %v, want 1", v.Confidence)
		}
	})

	t.Run("invalid status rejected", func(t *testing.T) {
		if _, err := ParseModelAlignmentVerdict(`{"status":"maybe"}`); err == nil {
			t.Error("expected error for invalid status")
		}
	})

	t.Run("empty status rejected", func(t *testing.T) {
		if _, err := ParseModelAlignmentVerdict(`{"confidence":0.5}`); err == nil {
			t.Error("expected error for missing status")
		}
	})

	t.Run("no JSON object", func(t *testing.T) {
		if _, err := ParseModelAlignmentVerdict("no json here"); err == nil {
			t.Error("expected error for missing JSON")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		if _, err := ParseModelAlignmentVerdict(`{"status": aligned}`); err == nil {
			t.Error("expected error for malformed JSON")
		}
	})
}

func TestExtractJSONObject(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`{"a":1}`, `{"a":1}`},
		{`prefix {"a":1} suffix`, `{"a":1}`},
		{`no braces`, ""},
		{``, ""},
		{`}{`, ""},
		{`only open {`, ""},
		{`{"outer":{"inner":1}}`, `{"outer":{"inner":1}}`},
	}
	for _, tt := range tests {
		if got := extractJSONObject(tt.in); got != tt.want {
			t.Errorf("extractJSONObject(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTruncateString(t *testing.T) {
	if got := truncateString("hello", 10); got != "hello" {
		t.Errorf("short string modified: %q", got)
	}
	if got := truncateString("hello", 3); got != "hel" {
		t.Errorf("truncateString = %q, want %q", got, "hel")
	}
	if got := truncateString("", 5); got != "" {
		t.Errorf("empty string modified: %q", got)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "x", "y"); got != "x" {
		t.Errorf("firstNonEmpty = %q, want %q", got, "x")
	}
	if got := firstNonEmpty("", "   "); got != "" {
		t.Errorf("firstNonEmpty = %q, want empty", got)
	}
}

func TestNewAlignmentReviewer(t *testing.T) {
	t.Run("missing endpoint", func(t *testing.T) {
		if _, err := NewAlignmentReviewer(AlignmentReviewerConfig{Model: "m"}); err == nil {
			t.Error("expected error for missing endpoint")
		}
	})
	t.Run("missing model", func(t *testing.T) {
		if _, err := NewAlignmentReviewer(AlignmentReviewerConfig{Endpoint: "http://x"}); err == nil {
			t.Error("expected error for missing model")
		}
	})
	t.Run("trims endpoint and model", func(t *testing.T) {
		r, err := NewAlignmentReviewer(AlignmentReviewerConfig{Endpoint: " http://x/// ", Model: " m "})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r.endpoint != "http://x" {
			t.Errorf("endpoint = %q, want trimmed", r.endpoint)
		}
		if r.model != "m" {
			t.Errorf("model = %q, want trimmed", r.model)
		}
	})
}

func TestBuildAlignmentPrompt(t *testing.T) {
	ac := AlignmentContext{
		AuthorizedIntent: "fix the login bug",
		PRTitle:          "fix login",
		PRBody:           "body text",
		DiffSummary:      "M auth.go +3 -1",
	}
	msgs := buildAlignmentPrompt(ac)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Errorf("roles = %q,%q", msgs[0].Role, msgs[1].Role)
	}
	for _, want := range []string{"fix the login bug", "fix login", "body text", "M auth.go +3 -1"} {
		if !strings.Contains(msgs[1].Content, want) {
			t.Errorf("user prompt missing %q", want)
		}
	}
	if !strings.Contains(msgs[0].Content, "unclear") {
		t.Error("system prompt missing unclear instruction")
	}
}

func newTestReviewer(t *testing.T, handler http.HandlerFunc) (*AlignmentReviewer, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	r, err := NewAlignmentReviewer(AlignmentReviewerConfig{Endpoint: srv.URL, APIKey: "test-key", Model: "test-model"})
	if err != nil {
		t.Fatalf("NewAlignmentReviewer: %v", err)
	}
	return r, srv
}

func TestReview(t *testing.T) {
	ac := AlignmentContext{AuthorizedIntent: "intent", PRTitle: "title", DiffSummary: "diff"}

	t.Run("success", func(t *testing.T) {
		var gotAuth, gotPath string
		r, _ := newTestReviewer(t, func(w http.ResponseWriter, req *http.Request) {
			gotAuth = req.Header.Get("Authorization")
			gotPath = req.URL.Path
			fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"status\":\"aligned\",\"confidence\":0.8}"}}]}`)
		})
		v, err := r.Review(context.Background(), ac)
		if err != nil {
			t.Fatalf("Review: %v", err)
		}
		if v.Status != AlignmentStatusAligned || v.Confidence != 0.8 {
			t.Errorf("verdict = %+v", v)
		}
		if gotAuth != "Bearer test-key" {
			t.Errorf("Authorization = %q", gotAuth)
		}
		if gotPath != "/v1/chat/completions" {
			t.Errorf("path = %q", gotPath)
		}
	})

	t.Run("non-200 status", func(t *testing.T) {
		r, _ := newTestReviewer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		})
		if _, err := r.Review(context.Background(), ac); err == nil || !strings.Contains(err.Error(), "502") {
			t.Errorf("expected 502 error, got %v", err)
		}
	})

	t.Run("no choices", func(t *testing.T) {
		r, _ := newTestReviewer(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"choices":[]}`)
		})
		if _, err := r.Review(context.Background(), ac); err == nil || !strings.Contains(err.Error(), "no choices") {
			t.Errorf("expected no-choices error, got %v", err)
		}
	})

	t.Run("malformed response body", func(t *testing.T) {
		r, _ := newTestReviewer(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `not json`)
		})
		if _, err := r.Review(context.Background(), ac); err == nil {
			t.Error("expected decode error")
		}
	})

	t.Run("invalid model verdict propagates", func(t *testing.T) {
		r, _ := newTestReviewer(t, func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"status\":\"bogus\"}"}}]}`)
		})
		if _, err := r.Review(context.Background(), ac); err == nil {
			t.Error("expected invalid-status error")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		r, _ := newTestReviewer(t, func(http.ResponseWriter, *http.Request) {})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := r.Review(ctx, ac); err == nil {
			t.Error("expected error for cancelled context")
		}
	})

	t.Run("connection error", func(t *testing.T) {
		r, srv := newTestReviewer(t, func(w http.ResponseWriter, _ *http.Request) {})
		srv.Close()
		if _, err := r.Review(context.Background(), ac); err == nil {
			t.Error("expected connection error")
		}
	})

	t.Run("no auth header without api key", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			gotAuth = req.Header.Get("Authorization")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"status\":\"unclear\"}"}}]}`)
		}))
		t.Cleanup(srv.Close)
		r, err := NewAlignmentReviewer(AlignmentReviewerConfig{Endpoint: srv.URL, Model: "m"})
		if err != nil {
			t.Fatalf("NewAlignmentReviewer: %v", err)
		}
		if _, err := r.Review(context.Background(), ac); err != nil {
			t.Fatalf("Review: %v", err)
		}
		if gotAuth != "" {
			t.Errorf("Authorization = %q, want empty", gotAuth)
		}
	})
}
