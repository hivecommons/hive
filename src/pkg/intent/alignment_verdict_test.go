package intent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAlignmentVerdictMisaligned(t *testing.T) {
	tests := []struct {
		name string
		v    AlignmentVerdict
		want bool
	}{
		{"aligned", AlignmentVerdict{Status: AlignmentStatusAligned}, false},
		{"top-level misaligned", AlignmentVerdict{Status: AlignmentStatusMisaligned}, true},
		{"unclear is not misaligned", AlignmentVerdict{Status: AlignmentStatusUnclear}, false},
		{
			"deterministic finding misaligned",
			AlignmentVerdict{
				Status:                AlignmentStatusAligned,
				DeterministicFindings: []AlignmentFinding{{Status: AlignmentStatusMisaligned}},
			},
			true,
		},
		{
			"aligned finding does not flip",
			AlignmentVerdict{
				Status:                AlignmentStatusAligned,
				DeterministicFindings: []AlignmentFinding{{Status: AlignmentStatusAligned}},
			},
			false,
		},
		{
			"model misaligned",
			AlignmentVerdict{
				Status: AlignmentStatusAligned,
				Model:  &ModelAlignmentVerdict{Status: AlignmentStatusMisaligned},
			},
			true,
		},
		{
			"model aligned stays aligned",
			AlignmentVerdict{
				Status: AlignmentStatusAligned,
				Model:  &ModelAlignmentVerdict{Status: AlignmentStatusAligned},
			},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.Misaligned(); got != tt.want {
				t.Fatalf("Misaligned() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergeAlignment(t *testing.T) {
	base := AlignmentVerdict{Status: AlignmentStatusAligned, Rationale: "deterministic ok"}

	t.Run("model misaligned overrides", func(t *testing.T) {
		got := MergeAlignment(base, &ModelAlignmentVerdict{Status: AlignmentStatusMisaligned, Rationale: "scope creep"}, nil)
		if got.Status != AlignmentStatusMisaligned {
			t.Fatalf("Status = %q, want misaligned", got.Status)
		}
		if got.Rationale != "scope creep" {
			t.Fatalf("Rationale = %q, want model rationale", got.Rationale)
		}
	})

	t.Run("model misaligned with empty rationale keeps base rationale", func(t *testing.T) {
		got := MergeAlignment(base, &ModelAlignmentVerdict{Status: AlignmentStatusMisaligned}, nil)
		if got.Rationale != "deterministic ok" {
			t.Fatalf("Rationale = %q, want base rationale", got.Rationale)
		}
	})

	t.Run("model unclear downgrades aligned", func(t *testing.T) {
		got := MergeAlignment(base, &ModelAlignmentVerdict{Status: AlignmentStatusUnclear}, nil)
		if got.Status != AlignmentStatusUnclear {
			t.Fatalf("Status = %q, want unclear", got.Status)
		}
		if got.Rationale != "model could not determine alignment" {
			t.Fatalf("Rationale = %q, want default unclear rationale", got.Rationale)
		}
	})

	t.Run("model unclear does not downgrade misaligned base", func(t *testing.T) {
		mis := AlignmentVerdict{Status: AlignmentStatusMisaligned, Rationale: "drift"}
		got := MergeAlignment(mis, &ModelAlignmentVerdict{Status: AlignmentStatusUnclear}, nil)
		if got.Status != AlignmentStatusMisaligned {
			t.Fatalf("Status = %q, want misaligned preserved", got.Status)
		}
	})

	t.Run("model aligned keeps base", func(t *testing.T) {
		got := MergeAlignment(base, &ModelAlignmentVerdict{Status: AlignmentStatusAligned}, nil)
		if got.Status != AlignmentStatusAligned || got.Model == nil {
			t.Fatalf("got %+v, want aligned with model attached", got)
		}
	})

	t.Run("model error recorded", func(t *testing.T) {
		got := MergeAlignment(base, nil, context.DeadlineExceeded)
		if got.ModelError != context.DeadlineExceeded.Error() {
			t.Fatalf("ModelError = %q", got.ModelError)
		}
		if got.Status != AlignmentStatusAligned {
			t.Fatalf("Status = %q, want base status preserved on error", got.Status)
		}
	})

	t.Run("empty status defaults to aligned", func(t *testing.T) {
		got := MergeAlignment(AlignmentVerdict{}, nil, nil)
		if got.Status != AlignmentStatusAligned {
			t.Fatalf("Status = %q, want aligned default", got.Status)
		}
	})
}

func TestParseModelAlignmentVerdict(t *testing.T) {
	t.Run("valid statuses", func(t *testing.T) {
		for _, s := range []string{AlignmentStatusAligned, AlignmentStatusMisaligned, AlignmentStatusUnclear} {
			v, err := ParseModelAlignmentVerdict(`{"status":"` + s + `","confidence":0.9,"rationale":"r"}`)
			if err != nil {
				t.Fatalf("status %q: %v", s, err)
			}
			if v.Status != s || v.Confidence != 0.9 || v.Rationale != "r" {
				t.Fatalf("got %+v", v)
			}
		}
	})

	t.Run("JSON embedded in prose", func(t *testing.T) {
		v, err := ParseModelAlignmentVerdict("Sure, here is my verdict:\n```json\n{\"status\":\"aligned\"}\n```\nDone.")
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != AlignmentStatusAligned {
			t.Fatalf("Status = %q", v.Status)
		}
	})

	t.Run("confidence clamped", func(t *testing.T) {
		v, err := ParseModelAlignmentVerdict(`{"status":"aligned","confidence":1.7}`)
		if err != nil {
			t.Fatal(err)
		}
		if v.Confidence != 1 {
			t.Fatalf("Confidence = %v, want clamped to 1", v.Confidence)
		}
		v, err = ParseModelAlignmentVerdict(`{"status":"aligned","confidence":-0.4}`)
		if err != nil {
			t.Fatal(err)
		}
		if v.Confidence != 0 {
			t.Fatalf("Confidence = %v, want clamped to 0", v.Confidence)
		}
	})

	t.Run("invalid status rejected", func(t *testing.T) {
		if _, err := ParseModelAlignmentVerdict(`{"status":"maybe"}`); err == nil {
			t.Fatal("want error for invalid status")
		}
		if _, err := ParseModelAlignmentVerdict(`{"confidence":0.5}`); err == nil {
			t.Fatal("want error for empty status")
		}
	})

	t.Run("no JSON object", func(t *testing.T) {
		if _, err := ParseModelAlignmentVerdict("aligned"); err == nil {
			t.Fatal("want error when reply has no JSON")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		if _, err := ParseModelAlignmentVerdict(`{"status": aligned}`); err == nil {
			t.Fatal("want error for malformed JSON")
		}
	})
}

func TestExtractJSONObject(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`{"a":1}`, `{"a":1}`},
		{"prefix {\"a\":1} suffix", `{"a":1}`},
		{"nested {\"a\":{\"b\":2}}", `{"a":{"b":2}}`},
		{"no braces", ""},
		{"} reversed {", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := extractJSONObject(tt.in); got != tt.want {
			t.Fatalf("extractJSONObject(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTruncateStringAndFirstNonEmpty(t *testing.T) {
	if got := truncateString("hello", 10); got != "hello" {
		t.Fatalf("truncateString short = %q", got)
	}
	if got := truncateString("hello", 3); got != "hel" {
		t.Fatalf("truncateString long = %q", got)
	}
	if got := firstNonEmpty("", "  ", "x", "y"); got != "x" {
		t.Fatalf("firstNonEmpty = %q, want x", got)
	}
	if got := firstNonEmpty("", "   "); got != "" {
		t.Fatalf("firstNonEmpty all blank = %q, want empty", got)
	}
}

func TestNewAlignmentReviewer(t *testing.T) {
	if _, err := NewAlignmentReviewer(AlignmentReviewerConfig{Model: "m"}); err == nil {
		t.Fatal("want error for missing endpoint")
	}
	if _, err := NewAlignmentReviewer(AlignmentReviewerConfig{Endpoint: "http://x"}); err == nil {
		t.Fatal("want error for missing model")
	}
	r, err := NewAlignmentReviewer(AlignmentReviewerConfig{Endpoint: "http://x/", Model: " m "})
	if err != nil {
		t.Fatal(err)
	}
	if r.endpoint != "http://x" {
		t.Fatalf("endpoint = %q, want trailing slash trimmed", r.endpoint)
	}
	if r.model != "m" {
		t.Fatalf("model = %q, want trimmed", r.model)
	}
}

func TestAlignmentReviewerReview(t *testing.T) {
	newReviewer := func(t *testing.T, handler http.HandlerFunc) *AlignmentReviewer {
		t.Helper()
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		r, err := NewAlignmentReviewer(AlignmentReviewerConfig{Endpoint: srv.URL, APIKey: "test-key", Model: "test-model"})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ac := AlignmentContext{AuthorizedIntent: "fix pkg/foo", PRTitle: "fix foo", DiffSummary: "pkg/foo/foo.go +1 -1\n"}

	t.Run("success", func(t *testing.T) {
		var gotPath, gotAuth string
		r := newReviewer(t, func(w http.ResponseWriter, req *http.Request) {
			gotPath = req.URL.Path
			gotAuth = req.Header.Get("Authorization")
			w.Write([]byte(`{"choices":[{"message":{"content":"{\"status\":\"misaligned\",\"confidence\":0.8,\"rationale\":\"drift\"}"}}]}`))
		})
		v, err := r.Review(context.Background(), ac)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != AlignmentStatusMisaligned || v.Confidence != 0.8 {
			t.Fatalf("got %+v", v)
		}
		if gotPath != "/v1/chat/completions" {
			t.Fatalf("path = %q", gotPath)
		}
		if gotAuth != "Bearer test-key" {
			t.Fatalf("auth = %q", gotAuth)
		}
	})

	t.Run("non-200 status", func(t *testing.T) {
		r := newReviewer(t, func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		})
		if _, err := r.Review(context.Background(), ac); err == nil || !strings.Contains(err.Error(), "502") {
			t.Fatalf("err = %v, want 502 error", err)
		}
	})

	t.Run("no choices", func(t *testing.T) {
		r := newReviewer(t, func(w http.ResponseWriter, req *http.Request) {
			w.Write([]byte(`{"choices":[]}`))
		})
		if _, err := r.Review(context.Background(), ac); err == nil || !strings.Contains(err.Error(), "no choices") {
			t.Fatalf("err = %v, want no-choices error", err)
		}
	})

	t.Run("undecodable body", func(t *testing.T) {
		r := newReviewer(t, func(w http.ResponseWriter, req *http.Request) {
			w.Write([]byte("not json"))
		})
		if _, err := r.Review(context.Background(), ac); err == nil {
			t.Fatal("want decode error")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		r := newReviewer(t, func(w http.ResponseWriter, req *http.Request) {})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := r.Review(ctx, ac); err == nil {
			t.Fatal("want error for cancelled context")
		}
	})
}

func TestBuildAlignmentPrompt(t *testing.T) {
	msgs := buildAlignmentPrompt(AlignmentContext{
		AuthorizedIntent: "intent-text",
		PRTitle:          "title-text",
		PRBody:           "body-text",
		DiffSummary:      "diff-text",
	})
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("roles = %q,%q", msgs[0].Role, msgs[1].Role)
	}
	for _, want := range []string{"intent-text", "title-text", "body-text", "diff-text"} {
		if !strings.Contains(msgs[1].Content, want) {
			t.Fatalf("user prompt missing %q", want)
		}
	}
}
