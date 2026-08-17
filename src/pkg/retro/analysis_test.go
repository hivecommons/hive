package retro

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestParseAnalysisValid covers the happy path and confirms whitespace is
// trimmed on every string field.
func TestParseAnalysisValid(t *testing.T) {
	content := `some preamble text {"root_cause_hypothesis":"  CI failed late  ","process_improvement":" add pre-submit checks ","generalizable":true,"generalizable_lesson":"Run the same validation locally before opening PRs to catch failures earlier."} trailing`
	got, err := ParseAnalysis(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RootCauseHypothesis != "CI failed late" {
		t.Fatalf("RootCauseHypothesis = %q, want trimmed value", got.RootCauseHypothesis)
	}
	if got.ProcessImprovement != "add pre-submit checks" {
		t.Fatalf("ProcessImprovement = %q, want trimmed value", got.ProcessImprovement)
	}
	if !got.Generalizable {
		t.Fatal("Generalizable = false, want true")
	}
	if got.GeneralizableLesson == "" {
		t.Fatal("GeneralizableLesson should be preserved when generalizable is true")
	}
}

// TestParseAnalysisMalformed exercises the edge cases and boundary
// conditions the issue calls out: empty input, malformed JSON, missing
// required fields, over-length fields, and lesson-length boundaries.
func TestParseAnalysisMalformed(t *testing.T) {
	longField := strings.Repeat("x", MaxAnalysisFieldChars+1)
	shortLesson := strings.Repeat("y", MinLessonChars-1)
	longLesson := strings.Repeat("z", MaxLessonChars+1)
	exactLesson := strings.Repeat("a", MinLessonChars)

	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"empty input", "", true},
		{"no braces at all", "just some text with no json", true},
		{"unbalanced braces end before start", "} nonsense {", true},
		{"not valid json inside braces", `{not: valid json}`, true},
		{"two json objects concatenated", `{"root_cause_hypothesis":"a","process_improvement":"b","generalizable":false,"generalizable_lesson":""}{"root_cause_hypothesis":"c","process_improvement":"d","generalizable":false,"generalizable_lesson":""}`, true},
		{"unknown field rejected", `{"root_cause_hypothesis":"a","process_improvement":"b","generalizable":false,"generalizable_lesson":"","extra_field":"nope"}`, true},
		{"missing root cause", `{"root_cause_hypothesis":"","process_improvement":"b","generalizable":false,"generalizable_lesson":""}`, true},
		{"missing process improvement", `{"root_cause_hypothesis":"a","process_improvement":"","generalizable":false,"generalizable_lesson":""}`, true},
		{"root cause too long", `{"root_cause_hypothesis":"` + longField + `","process_improvement":"b","generalizable":false,"generalizable_lesson":""}`, true},
		{"process improvement too long", `{"root_cause_hypothesis":"a","process_improvement":"` + longField + `","generalizable":false,"generalizable_lesson":""}`, true},
		{"generalizable true but lesson too short", `{"root_cause_hypothesis":"a","process_improvement":"b","generalizable":true,"generalizable_lesson":"` + shortLesson + `"}`, true},
		{"generalizable true but lesson too long", `{"root_cause_hypothesis":"a","process_improvement":"b","generalizable":true,"generalizable_lesson":"` + longLesson + `"}`, true},
		{"generalizable true with lesson exactly at minimum", `{"root_cause_hypothesis":"a","process_improvement":"b","generalizable":true,"generalizable_lesson":"` + exactLesson + `"}`, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseAnalysis(tt.content)
			if tt.wantErr && err == nil {
				t.Fatalf("ParseAnalysis(%q) = nil error, want error", tt.content)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ParseAnalysis(%q) = %v, want no error", tt.content, err)
			}
		})
	}
}

// TestParseAnalysisNonGeneralizableClearsLesson confirms the lesson is
// discarded (not merely left alone) when generalizable is false, even if
// the model supplied lesson text.
func TestParseAnalysisNonGeneralizableClearsLesson(t *testing.T) {
	content := `{"root_cause_hypothesis":"a","process_improvement":"b","generalizable":false,"generalizable_lesson":"this text should be dropped because generalizable is false"}`
	got, err := ParseAnalysis(content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.GeneralizableLesson != "" {
		t.Fatalf("GeneralizableLesson = %q, want empty when not generalizable", got.GeneralizableLesson)
	}
}

// TestExtractJSONObject is a positive control on the brace-scanning helper:
// wrong extraction would break every downstream ParseAnalysis case above.
func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"no braces", "hello world", ""},
		{"only open brace", "hello { world", ""},
		{"only close brace", "hello } world", ""},
		{"close before open", "} { ", ""},
		{"simple object", `noise {"a":1} noise`, `{"a":1}`},
		{"nested object takes outermost span", `pre {"a":{"b":1}} post`, `{"a":{"b":1}}`},
		{"empty object", "{}", "{}"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJSONObject(tt.input)
			if got != tt.want {
				t.Fatalf("extractJSONObject(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestTruncate is a positive control: an off-by-one or missing rune-vs-byte
// handling would corrupt every prompt built from record/finding text.
func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"empty string", "", 10, ""},
		{"shorter than max unchanged", "hello", 10, "hello"},
		{"exactly at max unchanged", "hello", 5, "hello"},
		{"longer than max truncated", "hello world", 5, "hello"},
		{"max zero returns trimmed input unchanged", "hello world", 0, "hello world"},
		{"negative max returns trimmed input unchanged", "hello world", -1, "hello world"},
		{"leading and trailing whitespace trimmed first", "  hello  ", 10, "hello"},
		{"multi-byte runes truncate by rune not byte", "日本語のテスト", 3, "日本語"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.s, tt.max)
			if got != tt.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tt.s, tt.max, got, tt.want)
			}
		})
	}
}

// TestRuneLen is a positive control for the length checks ParseAnalysis
// relies on: byte-counting instead of rune-counting would let multi-byte
// content silently bypass the documented character bounds.
func TestRuneLen(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want int
	}{
		{"empty", "", 0},
		{"ascii", "hello", 5},
		{"trims whitespace before counting", "  hello  ", 5},
		{"multi-byte runes count as one each", "日本語", 3},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := runeLen(tt.s); got != tt.want {
				t.Fatalf("runeLen(%q) = %d, want %d", tt.s, got, tt.want)
			}
		})
	}
}

// TestCorrectiveAnalysisPromptIncludesError confirms the retry-guidance
// prompt actually surfaces the validation error, otherwise the model would
// retry blind.
func TestCorrectiveAnalysisPromptIncludesError(t *testing.T) {
	err := &testErr{"root_cause_hypothesis is required"}
	got := correctiveAnalysisPrompt(err)
	if !strings.Contains(got, "root_cause_hypothesis is required") {
		t.Fatalf("corrective prompt = %q, want it to contain underlying error text", got)
	}
	if !strings.Contains(got, "generalizable_lesson") {
		t.Fatal("corrective prompt should restate the required schema")
	}
}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }

// TestNewAnalyzer covers the exported constructor's branches: blank model
// (intentional no-op analyzer), blank endpoint (error), and success.
func TestNewAnalyzer(t *testing.T) {
	t.Run("blank model returns nil analyzer and nil error", func(t *testing.T) {
		a, err := NewAnalyzer("http://example.com", "key", "  ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a != nil {
			t.Fatalf("analyzer = %+v, want nil", a)
		}
	})
	t.Run("blank endpoint with non-blank model errors", func(t *testing.T) {
		a, err := NewAnalyzer("   ", "key", "gpt-x")
		if err == nil {
			t.Fatal("expected error for blank endpoint")
		}
		if a != nil {
			t.Fatalf("analyzer = %+v, want nil on error", a)
		}
	})
	t.Run("valid endpoint and model construct an analyzer", func(t *testing.T) {
		a, err := NewAnalyzer("http://example.com/", "key", " gpt-x ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if a == nil {
			t.Fatal("analyzer = nil, want non-nil")
		}
		if a.model != "gpt-x" {
			t.Fatalf("model = %q, want trimmed %q", a.model, "gpt-x")
		}
		if a.client == nil {
			t.Fatal("client should be constructed")
		}
	})
}

// TestAnalyzeNilReceiverAndEmptyConfig is a positive control on the
// fail-open guard at the top of Analyze: a nil analyzer, nil client, or
// blank model must all return a zero-value Analysis with no error rather
// than panicking or attempting a network call.
func TestAnalyzeNilReceiverAndEmptyConfig(t *testing.T) {
	var nilAnalyzer *Analyzer
	got, err := nilAnalyzer.Analyze(context.Background(), RetroRecord{}, nil)
	if err != nil {
		t.Fatalf("nil analyzer: unexpected error: %v", err)
	}
	if got != (Analysis{}) {
		t.Fatalf("nil analyzer: got %#v, want zero value", got)
	}

	blankModel := newAnalyzerWithClient("   ", &fakeAnalysisClient{replies: []string{"should never be called"}})
	got, err = blankModel.Analyze(context.Background(), RetroRecord{}, nil)
	if err != nil {
		t.Fatalf("blank model: unexpected error: %v", err)
	}
	if got != (Analysis{}) {
		t.Fatalf("blank model: got %#v, want zero value", got)
	}
}

// TestAnalyzeExhaustsRetries is a positive control: if Analyze looped
// forever or swallowed the terminal error, this would hang or return nil
// error incorrectly.
func TestAnalyzeExhaustsRetries(t *testing.T) {
	client := &fakeAnalysisClient{replies: []string{
		`not json at all`,
		`not json at all`,
		`not json at all`,
		`not json at all`,
		`not json at all`,
	}}
	analyzer := newAnalyzerWithClient("m", client)
	_, err := analyzer.Analyze(context.Background(), RetroRecord{BeadID: "b"}, nil)
	if err == nil {
		t.Fatal("expected error after exhausting retries on persistently invalid JSON")
	}
	if client.calls < 1 {
		t.Fatal("client should have been called at least once")
	}
}

// TestAnalyzePropagatesClientError confirms a transport-level error from the
// client short-circuits immediately without retrying.
func TestAnalyzePropagatesClientError(t *testing.T) {
	client := &fakeAnalysisClient{err: &testErr{"connection refused"}}
	analyzer := newAnalyzerWithClient("m", client)
	_, err := analyzer.Analyze(context.Background(), RetroRecord{BeadID: "b"}, nil)
	if err == nil {
		t.Fatal("expected error to propagate from client")
	}
	if client.calls != 1 {
		t.Fatalf("calls = %d, want exactly 1 (no retry on transport error)", client.calls)
	}
}

// TestOpenAIChatClientComplete exercises the HTTP request/response handling
// in openAIChatClient.Complete against a local httptest server: success,
// non-200 status, empty choices, and malformed response bodies.
func TestOpenAIChatClientComplete(t *testing.T) {
	t.Run("success returns message content", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer secret-key" {
				t.Errorf("missing/incorrect Authorization header: %q", r.Header.Get("Authorization"))
			}
			if r.URL.Path != "/v1/chat/completions" {
				t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
			}
			var req chatRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("failed to decode request body: %v", err)
			}
			if req.Model != "gpt-x" {
				t.Errorf("request model = %q, want gpt-x", req.Model)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello analysis"}}]}`))
		}))
		defer srv.Close()

		c := &openAIChatClient{endpoint: srv.URL, apiKey: "secret-key", model: "gpt-x", http: srv.Client()}
		got, err := c.Complete(context.Background(), []chatMessage{{Role: "user", Content: "hi"}}, 100)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "hello analysis" {
			t.Fatalf("content = %q, want %q", got, "hello analysis")
		}
	})

	t.Run("no api key omits authorization header", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				t.Errorf("Authorization header = %q, want empty", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}))
		defer srv.Close()

		c := &openAIChatClient{endpoint: srv.URL, apiKey: "", model: "gpt-x", http: srv.Client()}
		if _, err := c.Complete(context.Background(), nil, 10); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("non-200 status returns error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := &openAIChatClient{endpoint: srv.URL, model: "gpt-x", http: srv.Client()}
		_, err := c.Complete(context.Background(), nil, 10)
		if err == nil {
			t.Fatal("expected error on non-200 status")
		}
		if !strings.Contains(err.Error(), strconv.Itoa(http.StatusInternalServerError)) {
			t.Fatalf("error = %v, want it to mention status %d", err, http.StatusInternalServerError)
		}
	})

	t.Run("empty choices returns error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[]}`))
		}))
		defer srv.Close()

		c := &openAIChatClient{endpoint: srv.URL, model: "gpt-x", http: srv.Client()}
		_, err := c.Complete(context.Background(), nil, 10)
		if err == nil {
			t.Fatal("expected error on empty choices")
		}
	})

	t.Run("malformed json body returns error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{not valid json`))
		}))
		defer srv.Close()

		c := &openAIChatClient{endpoint: srv.URL, model: "gpt-x", http: srv.Client()}
		_, err := c.Complete(context.Background(), nil, 10)
		if err == nil {
			t.Fatal("expected error on malformed response body")
		}
	})
}

// TestBuildAnalysisPromptEmptyInput is a boundary case not covered by the
// existing TestBuildAnalysisPromptBounds in retro_test.go: a record and
// findings slice that are both entirely empty must still produce a
// well-formed two-message prompt rather than panicking.
func TestBuildAnalysisPromptEmptyInput(t *testing.T) {
	msgs := buildAnalysisPrompt(RetroRecord{}, nil)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("roles = (%q, %q), want (system, user)", msgs[0].Role, msgs[1].Role)
	}
	if !strings.Contains(msgs[1].Content, "DETERMINISTIC_FINDINGS") {
		t.Fatal("prompt should include the findings section header even with no findings")
	}
}
