package knowledge

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestClassifyComment(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		comment  string
		wantType FactType
		wantNil  bool
	}{
		{
			name:     "gotcha: always keyword",
			comment:  "You should always guard .join() against undefined arrays.",
			wantType: FactGotcha,
		},
		{
			name:     "gotcha: never keyword",
			comment:  "Never call setState inside a useEffect cleanup function.",
			wantType: FactGotcha,
		},
		{
			name:     "regression: broke keyword",
			comment:  "This broke again after the last deploy — the webhook port override was lost.",
			wantType: FactRegression,
		},
		{
			name:     "pattern: convention keyword",
			comment:  "Our convention is to use factory functions from test/factories.ts for all mocks.",
			wantType: FactPattern,
		},
		{
			name:     "test_scaffold: test keyword",
			comment:  "Make sure to add a test for the edge case when the array is empty.",
			wantType: FactTestScaff,
		},
		{
			name:     "decision: decided keyword",
			comment:  "We decided to use Zustand instead of Redux for state management going forward.",
			wantType: FactDecision,
		},
		{
			name:    "nil: conversational comment",
			comment: "Looks good to me, thanks for the update!",
			wantNil: true,
		},
		{
			name:    "nil: too short for title",
			comment: "ok lgtm",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fact := classifyComment(tt.comment, "org/repo#42", now)
			if tt.wantNil {
				if fact != nil {
					t.Errorf("expected nil, got %+v", fact)
				}
				return
			}
			if fact == nil {
				t.Fatal("expected non-nil fact")
			}
			if fact.Type != tt.wantType {
				t.Errorf("type = %s, want %s", fact.Type, tt.wantType)
			}
			if fact.SourcePR != "org/repo#42" {
				t.Errorf("source_pr = %s, want org/repo#42", fact.SourcePR)
			}
			if fact.Title == "" {
				t.Error("expected non-empty title")
			}
		})
	}
}

func TestExtractTitle(t *testing.T) {
	tests := []struct {
		name    string
		comment string
		want    string
	}{
		{
			name:    "simple sentence",
			comment: "Always guard .join() against undefined arrays to prevent crashes.",
			want:    "Always guard .join() against undefined arrays to prevent crashes.",
		},
		{
			name:    "skips markdown headers",
			comment: "# Review\nAlways guard .join() against undefined arrays.",
			want:    "Always guard .join() against undefined arrays.",
		},
		{
			name:    "skips code blocks",
			comment: "```go\nfmt.Println()\n```\nAlways guard .join() against undefined.",
			want:    "Always guard .join() against undefined.",
		},
		{
			name:    "skips short lines",
			comment: "ok\nAlways guard .join() against undefined arrays to prevent crashes.",
			want:    "Always guard .join() against undefined arrays to prevent crashes.",
		},
		{
			name:    "truncates long lines",
			comment: "This is a very long comment that goes on and on and on and on and on and on and on and on and on and on and on and on and on and keeps going past the limit.",
			want:    "This is a very long comment that goes on and on and on and on and on and on and on and on and on and on and on and on an...",
		},
		{
			name:    "empty when all short",
			comment: "ok\nlgtm\nnice",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTitle(tt.comment)
			if got != tt.want {
				t.Errorf("extractTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractTags(t *testing.T) {
	comment := "This React component uses TypeScript and needs a test with Docker."
	tags := extractTags(comment)

	expected := map[string]bool{
		"react":      false,
		"typescript": false,
		"testing":    false,
		"docker":     false,
	}

	for _, tag := range tags {
		if _, ok := expected[tag]; ok {
			expected[tag] = true
		}
	}

	for tag, found := range expected {
		if !found {
			t.Errorf("expected tag %q not found in %v", tag, tags)
		}
	}
}

func TestExtractTagsDedup(t *testing.T) {
	comment := "go and golang are the same language for testing purposes"
	tags := extractTags(comment)

	goCount := 0
	for _, tag := range tags {
		if tag == "go" {
			goCount++
		}
	}
	if goCount != 1 {
		t.Errorf("expected 1 'go' tag, got %d in %v", goCount, tags)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate short = %q", got)
	}
	if got := truncate("exactly ten", 11); got != "exactly ten" {
		t.Errorf("truncate exact = %q", got)
	}
	if got := truncate("this is too long", 7); got != "this is..." {
		t.Errorf("truncate long = %q, want %q", got, "this is...")
	}
}

func TestContainsAny(t *testing.T) {
	if !containsAny("always use guards", "always", "never") {
		t.Error("should match 'always'")
	}
	if containsAny("looks good", "always", "never") {
		t.Error("should not match")
	}
}

func TestShouldExtractFrom(t *testing.T) {
	c := &Curator{
		config: CuratorConfig{
			ExtractFrom: []string{"review_comments", "pr_comments"},
		},
	}

	if !c.shouldExtractFrom("review_comments") {
		t.Error("should extract from review_comments")
	}
	if !c.shouldExtractFrom("pr_comments") {
		t.Error("should extract from pr_comments")
	}
	if c.shouldExtractFrom("ci_failures") {
		t.Error("should not extract from ci_failures")
	}
}

func TestFetchMergedPRsFiltering(t *testing.T) {
	now := time.Now()
	since := now.Add(-24 * time.Hour)

	mergedRecent := now.Add(-1 * time.Hour)
	mergedOld := now.Add(-48 * time.Hour)
	recentTimestamp := &gh.Timestamp{Time: mergedRecent}
	oldTimestamp := &gh.Timestamp{Time: mergedOld}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prs := []*gh.PullRequest{
			{
				Number:   gh.Ptr(1),
				MergedAt: recentTimestamp,
			},
			{
				Number:   gh.Ptr(2),
				MergedAt: oldTimestamp,
			},
			{
				Number: gh.Ptr(3),
				// not merged
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(prs)
	}))
	defer srv.Close()

	ghClient, err := gh.NewClient(nil).WithEnterpriseURLs(srv.URL+"/", srv.URL+"/")
	if err != nil {
		t.Fatalf("creating test client: %v", err)
	}

	c := &Curator{
		ghClient: ghClient,
		logger:   slog.Default(),
	}

	merged, err := c.fetchMergedPRs(context.Background(), "org", "repo", since)
	if err != nil {
		t.Fatalf("fetchMergedPRs: %v", err)
	}

	if len(merged) != 1 {
		t.Fatalf("expected 1 merged PR since cutoff, got %d", len(merged))
	}
	if merged[0].GetNumber() != 1 {
		t.Errorf("expected PR #1, got #%d", merged[0].GetNumber())
	}
}

func TestIngestSuccess(t *testing.T) {
	var receivedFacts []ExtractedFact

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ingest" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected content-type: %s", r.Header.Get("Content-Type"))
		}

		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedFacts)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := &Curator{
		wikiURL: srv.URL,
		logger:  slog.Default(),
	}

	facts := []ExtractedFact{
		{
			Title:      "Guard .join()",
			Body:       "Always guard .join() against undefined",
			Type:       FactGotcha,
			Confidence: 0.8,
			Tags:       []string{"typescript"},
			SourcePR:   "org/repo#42",
			SourceDate: time.Now(),
		},
	}

	err := c.Ingest(context.Background(), facts)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if len(receivedFacts) != 1 {
		t.Fatalf("expected 1 fact ingested, got %d", len(receivedFacts))
	}
	if receivedFacts[0].Title != "Guard .join()" {
		t.Errorf("title = %q, want %q", receivedFacts[0].Title, "Guard .join()")
	}
}

func TestIngestEmpty(t *testing.T) {
	c := &Curator{logger: slog.Default()}
	err := c.Ingest(context.Background(), nil)
	if err != nil {
		t.Errorf("Ingest(nil) should return nil, got %v", err)
	}
}

func TestIngestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Curator{
		wikiURL: srv.URL,
		logger:  slog.Default(),
	}

	facts := []ExtractedFact{{Title: "test", Body: "test body for length"}}
	err := c.Ingest(context.Background(), facts)
	if err == nil {
		t.Error("expected error for HTTP 500")
	}
}
