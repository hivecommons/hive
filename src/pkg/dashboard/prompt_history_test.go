package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestPromptHistory returns an in-memory store (no PVC writer), matching how
// newPromptHistory behaves when /data is absent.
func newTestPromptHistory() *PromptHistory {
	return &PromptHistory{ring: make([]PromptEntry, 0, promptHistoryRingCap)}
}

func TestRedactPromptSecrets(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantAbsent  []string
		wantPresent []string
	}{
		{
			name:        "github classic token",
			in:          "Use ghp_abcdefghijklmnopqrstuvwxyz0123456789 to auth",
			wantAbsent:  []string{"ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
			wantPresent: []string{secretRedactionPlaceholder, "to auth"},
		},
		{
			name:        "github fine-grained pat",
			in:          "token github_pat_11ABCDEFG0abcdefghijklmnop here",
			wantAbsent:  []string{"github_pat_11ABCDEFG0abcdefghijklmnop"},
			wantPresent: []string{secretRedactionPlaceholder},
		},
		{
			name:        "openai style key",
			in:          "OPENAI key sk-abcdefghijklmnopqrstuvwxyz012345",
			wantAbsent:  []string{"sk-abcdefghijklmnopqrstuvwxyz012345"},
			wantPresent: []string{secretRedactionPlaceholder},
		},
		{
			name:        "aws access key id",
			in:          "aws AKIAIOSFODNN7EXAMPLE end",
			wantAbsent:  []string{"AKIAIOSFODNN7EXAMPLE"},
			wantPresent: []string{secretRedactionPlaceholder},
		},
		{
			name:        "slack token",
			in:          "slack xoxb-1234567890-abcdefghij done",
			wantAbsent:  []string{"xoxb-1234567890-abcdefghij"},
			wantPresent: []string{secretRedactionPlaceholder},
		},
		{
			name:        "generic assignment with unknown vendor shape",
			in:          "GITHUB_TOKEN=s3cretValueGoesHere\nnext line",
			wantAbsent:  []string{"s3cretValueGoesHere"},
			wantPresent: []string{secretRedactionPlaceholder, "next line"},
		},
		{
			name:        "api_key colon form",
			in:          "api_key: superSecretValue123",
			wantAbsent:  []string{"superSecretValue123"},
			wantPresent: []string{secretRedactionPlaceholder},
		},
		{
			name:        "ordinary prompt text is untouched",
			in:          "Review issue #42 in hivecommons/hive and open a PR.",
			wantAbsent:  []string{secretRedactionPlaceholder},
			wantPresent: []string{"Review issue #42", "hivecommons/hive"},
		},
		{
			name:        "empty stays empty",
			in:          "",
			wantAbsent:  []string{secretRedactionPlaceholder},
			wantPresent: []string{},
		},
		// False-positive guards: redaction must not mangle ordinary prompt
		// content, or the view becomes misleading.
		{
			name:        "issue list line is untouched",
			in:          "  12m hivecommons/hive#2166 [kind/feature,triage/accepted] Add prompt history",
			wantAbsent:  []string{secretRedactionPlaceholder},
			wantPresent: []string{"hivecommons/hive#2166", "Add prompt history"},
		},
		{
			name:        "authorized repos section is untouched",
			in:          "AUTHORIZED REPOS (you may ONLY interact with these):\n  hivecommons/hive\n",
			wantAbsent:  []string{secretRedactionPlaceholder},
			wantPresent: []string{"hivecommons/hive"},
		},
		{
			name:        "prose containing the word token is untouched",
			in:          "The token bucket algorithm limits requests",
			wantAbsent:  []string{secretRedactionPlaceholder},
			wantPresent: []string{"token bucket algorithm"},
		},
		{
			name:        "unresolved placeholder is not a secret",
			in:          "api_key: ${OPENAI_API_KEY}",
			wantAbsent:  []string{secretRedactionPlaceholder},
			wantPresent: []string{"${OPENAI_API_KEY}"},
		},
		{
			name:        "colon separator is preserved when redacting",
			in:          "api_key: superSecretValue123",
			wantAbsent:  []string{"superSecretValue123"},
			wantPresent: []string{"api_key: " + secretRedactionPlaceholder},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactPromptSecrets(tc.in)
			for _, s := range tc.wantAbsent {
				if strings.Contains(got, s) {
					t.Errorf("expected %q to be redacted out, got %q", s, got)
				}
			}
			for _, s := range tc.wantPresent {
				if !strings.Contains(got, s) {
					t.Errorf("expected %q to be present, got %q", s, got)
				}
			}
		})
	}
}

func TestRecordRedactsBeforePersisting(t *testing.T) {
	p := newTestPromptHistory()
	p.Record("quality", "governor-eval", "auth with ghp_abcdefghijklmnopqrstuvwxyz0123456789 now")

	got := p.Query("quality", promptHistoryDefaultWindowHours, time.Now())
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if strings.Contains(got[0].Prompt, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("secret survived into stored prompt: %q", got[0].Prompt)
	}
	if !strings.Contains(got[0].Prompt, secretRedactionPlaceholder) {
		t.Fatalf("expected redaction placeholder, got %q", got[0].Prompt)
	}
}

func TestTruncatePrompt(t *testing.T) {
	tests := []struct {
		name          string
		size          int
		wantTruncated bool
	}{
		{name: "small prompt untouched", size: 100, wantTruncated: false},
		{name: "exactly at limit untouched", size: promptHistoryMaxPromptBytes, wantTruncated: false},
		{name: "one over limit truncated", size: promptHistoryMaxPromptBytes + 1, wantTruncated: true},
		{name: "far over limit truncated", size: promptHistoryMaxPromptBytes * 3, wantTruncated: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := strings.Repeat("a", tc.size)
			got, truncated := truncatePrompt(in)
			if truncated != tc.wantTruncated {
				t.Fatalf("truncated = %v, want %v", truncated, tc.wantTruncated)
			}
			if !tc.wantTruncated {
				if got != in {
					t.Fatalf("prompt was modified when it should not have been")
				}
				return
			}
			if !strings.Contains(got, promptHistoryTruncationMarker) {
				t.Fatalf("expected explicit truncation marker in output")
			}
			// The entry must be kept, not dropped.
			if len(got) == 0 {
				t.Fatalf("truncation dropped the prompt entirely")
			}
		})
	}
}

func TestTruncatePromptKeepsValidUTF8(t *testing.T) {
	// Multi-byte runes straddling the cut point must not produce invalid UTF-8,
	// which would break JSON round-tripping of the stored entry.
	in := strings.Repeat("é", promptHistoryMaxPromptBytes)
	got, truncated := truncatePrompt(in)
	if !truncated {
		t.Fatalf("expected truncation")
	}
	if !json.Valid(mustJSON(t, got)) {
		t.Fatalf("truncated prompt did not survive JSON encoding")
	}
}

func mustJSON(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestQueryTimeWindowFilter(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	p := newTestPromptHistory()

	// Seed entries at known ages directly so the test is time-independent.
	ages := []time.Duration{
		30 * time.Minute,
		6 * time.Hour,
		48 * time.Hour,      // outside a 24h window, inside a week
		10 * 24 * time.Hour, // outside a week
	}
	for i, age := range ages {
		p.ring = append(p.ring, PromptEntry{
			Timestamp: now.Add(-age).Format(time.RFC3339),
			Agent:     "quality",
			Trigger:   "governor-eval",
			Prompt:    fmt.Sprintf("prompt-%d", i),
		})
	}

	tests := []struct {
		name        string
		windowHours int
		want        int
	}{
		{name: "one hour window", windowHours: 1, want: 1},
		{name: "last day", windowHours: 24, want: 2},
		{name: "last week", windowHours: 168, want: 3},
		{name: "zero falls back to default day", windowHours: 0, want: 2},
		{name: "negative falls back to default day", windowHours: -5, want: 2},
		{name: "over max is clamped to a week", windowHours: 100000, want: 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Query("quality", tc.windowHours, now)
			if len(got) != tc.want {
				t.Fatalf("got %d entries, want %d", len(got), tc.want)
			}
		})
	}
}

func TestQueryAgentFilterAndOrdering(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	p := newTestPromptHistory()

	seed := []struct {
		agent string
		age   time.Duration
	}{
		{"quality", 3 * time.Hour},
		{"scanner", 2 * time.Hour},
		{"quality", 1 * time.Hour},
		{"architect", 30 * time.Minute},
	}
	for i, s := range seed {
		p.ring = append(p.ring, PromptEntry{
			Timestamp: now.Add(-s.age).Format(time.RFC3339),
			Agent:     s.agent,
			Trigger:   "governor-eval",
			Prompt:    fmt.Sprintf("p%d", i),
		})
	}

	tests := []struct {
		name  string
		agent string
		want  int
	}{
		{name: "single agent", agent: "quality", want: 2},
		{name: "other agent", agent: "scanner", want: 1},
		{name: "empty matches all", agent: "", want: 4},
		{name: "unknown agent matches none", agent: "nope", want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.Query(tc.agent, promptHistoryMaxWindowHours, now)
			if len(got) != tc.want {
				t.Fatalf("got %d, want %d", len(got), tc.want)
			}
			// Newest first.
			for i := 1; i < len(got); i++ {
				if got[i-1].Timestamp < got[i].Timestamp {
					t.Fatalf("entries not newest-first: %s before %s", got[i-1].Timestamp, got[i].Timestamp)
				}
			}
		})
	}
}

func TestQuerySkipsUnparseableTimestamps(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	p := newTestPromptHistory()
	p.ring = append(p.ring,
		PromptEntry{Timestamp: "not-a-timestamp", Agent: "quality", Prompt: "bad"},
		PromptEntry{Timestamp: now.Add(-time.Hour).Format(time.RFC3339), Agent: "quality", Prompt: "good"},
	)
	got := p.Query("quality", 24, now)
	if len(got) != 1 || got[0].Prompt != "good" {
		t.Fatalf("expected only the parseable entry, got %+v", got)
	}
}

func TestQueryCapsResults(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	p := newTestPromptHistory()
	for i := 0; i < promptHistoryMaxResults+50; i++ {
		p.ring = append(p.ring, PromptEntry{
			Timestamp: now.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339),
			Agent:     "quality",
			Prompt:    "p",
		})
	}
	got := p.Query("quality", promptHistoryMaxWindowHours, now)
	if len(got) != promptHistoryMaxResults {
		t.Fatalf("got %d, want cap of %d", len(got), promptHistoryMaxResults)
	}
}

func TestRecordRingRetention(t *testing.T) {
	p := newTestPromptHistory()
	// Overfill the ring; oldest entries must be evicted, size stays bounded.
	total := promptHistoryRingCap + 25
	for i := 0; i < total; i++ {
		p.Record("quality", "governor-eval", fmt.Sprintf("prompt number %d", i))
	}
	if len(p.ring) != promptHistoryRingCap {
		t.Fatalf("ring size = %d, want %d", len(p.ring), promptHistoryRingCap)
	}
	// The very first prompts must be gone.
	for _, e := range p.ring {
		if e.Prompt == "prompt number 0" {
			t.Fatalf("oldest entry was not evicted")
		}
	}
	// The newest must be present.
	found := false
	for _, e := range p.ring {
		if e.Prompt == fmt.Sprintf("prompt number %d", total-1) {
			found = true
		}
	}
	if !found {
		t.Fatalf("newest entry missing from ring")
	}
}

func TestRecordIgnoresEmptyInput(t *testing.T) {
	tests := []struct {
		name   string
		agent  string
		prompt string
	}{
		{name: "empty agent", agent: "", prompt: "hello"},
		{name: "empty prompt", agent: "quality", prompt: ""},
		{name: "both empty", agent: "", prompt: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPromptHistory()
			p.Record(tc.agent, "governor-eval", tc.prompt)
			if len(p.ring) != 0 {
				t.Fatalf("expected no entry recorded, got %d", len(p.ring))
			}
		})
	}
}

func TestRecordDefaultsMissingTrigger(t *testing.T) {
	p := newTestPromptHistory()
	p.Record("quality", "", "do the thing")
	if len(p.ring) != 1 {
		t.Fatalf("expected 1 entry")
	}
	if p.ring[0].Trigger != "unknown" {
		t.Fatalf("trigger = %q, want %q", p.ring[0].Trigger, "unknown")
	}
}

func TestLoadFromDiskMalformedLines(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{
			name:    "all good lines",
			content: `{"ts":"2026-07-29T10:00:00Z","agent":"quality","prompt":"a"}` + "\n" + `{"ts":"2026-07-29T11:00:00Z","agent":"scanner","prompt":"b"}` + "\n",
			want:    2,
		},
		{
			name: "truncated final line is skipped, earlier lines survive",
			content: `{"ts":"2026-07-29T10:00:00Z","agent":"quality","prompt":"a"}` + "\n" +
				`{"ts":"2026-07-29T11:00:00Z","agent":"scan`,
			want: 1,
		},
		{
			name: "garbage line in the middle does not abort the load",
			content: `{"ts":"2026-07-29T10:00:00Z","agent":"quality","prompt":"a"}` + "\n" +
				"this is not json at all\n" +
				`{"ts":"2026-07-29T12:00:00Z","agent":"quality","prompt":"c"}` + "\n",
			want: 2,
		},
		{
			name: "entries missing required fields are skipped",
			content: `{"ts":"","agent":"quality","prompt":"a"}` + "\n" +
				`{"ts":"2026-07-29T10:00:00Z","agent":"","prompt":"b"}` + "\n" +
				`{"ts":"2026-07-29T11:00:00Z","agent":"quality","prompt":"c"}` + "\n",
			want: 1,
		},
		{
			name:    "blank lines are ignored",
			content: "\n\n" + `{"ts":"2026-07-29T10:00:00Z","agent":"quality","prompt":"a"}` + "\n\n",
			want:    1,
		},
		{
			name:    "completely empty file",
			content: "",
			want:    0,
		},
		{
			name:    "only garbage yields nothing but does not panic",
			content: "nope\nstill nope\n{{{\n",
			want:    0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "prompt-history.jsonl")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			p := newTestPromptHistory()
			p.loadFromDisk(path)
			if len(p.ring) != tc.want {
				t.Fatalf("loaded %d entries, want %d", len(p.ring), tc.want)
			}
		})
	}
}

func TestLoadFromDiskMissingFile(t *testing.T) {
	p := newTestPromptHistory()
	p.loadFromDisk(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if len(p.ring) != 0 {
		t.Fatalf("expected empty ring")
	}
}

func TestLoadFromDiskCapsRing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt-history.jsonl")
	var b strings.Builder
	for i := 0; i < promptHistoryRingCap+40; i++ {
		fmt.Fprintf(&b, `{"ts":"2026-07-29T10:00:00Z","agent":"quality","prompt":"p%d"}`+"\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	p := newTestPromptHistory()
	p.loadFromDisk(path)
	if len(p.ring) != promptHistoryRingCap {
		t.Fatalf("ring = %d, want cap %d", len(p.ring), promptHistoryRingCap)
	}
}

func TestHandlePromptHistoryRoleGating(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		wantCode int
	}{
		{name: "owner allowed", role: "owner", wantCode: http.StatusOK},
		{name: "read-write allowed", role: "read-write", wantCode: http.StatusOK},
		{name: "missing role fails closed", role: "", wantCode: http.StatusForbidden},
		{name: "read-only forbidden", role: "read-only", wantCode: http.StatusForbidden},
		{name: "unknown role forbidden", role: "guest", wantCode: http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{promptHistory: newTestPromptHistory()}
			s.promptHistory.Record("quality", "governor-eval", "hello")

			req := httptest.NewRequest(http.MethodGet, "/api/prompt-history?agent=quality", nil)
			if tc.role != "" {
				req.Header.Set("X-Hive-Role", tc.role)
			}
			rec := httptest.NewRecorder()
			s.handlePromptHistory(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
		})
	}
}

func TestHandlePromptHistoryResponse(t *testing.T) {
	s := &Server{promptHistory: newTestPromptHistory()}
	s.promptHistory.Record("quality", "governor-eval", "review the queue")
	s.promptHistory.Record("scanner", "startup", "scan the repos")

	tests := []struct {
		name        string
		query       string
		wantEntries int
		wantWindow  int
	}{
		{name: "agent filter applied", query: "?agent=quality", wantEntries: 1, wantWindow: promptHistoryDefaultWindowHours},
		{name: "no agent returns all", query: "", wantEntries: 2, wantWindow: promptHistoryDefaultWindowHours},
		{name: "explicit week window", query: "?windowHours=168", wantEntries: 2, wantWindow: 168},
		{name: "garbage window falls back to default", query: "?windowHours=abc", wantEntries: 2, wantWindow: promptHistoryDefaultWindowHours},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/prompt-history"+tc.query, nil)
			req.Header.Set("X-Hive-Role", "owner")
			rec := httptest.NewRecorder()
			s.handlePromptHistory(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			var body struct {
				Entries     []PromptEntry `json:"entries"`
				WindowHours int           `json:"windowHours"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(body.Entries) != tc.wantEntries {
				t.Fatalf("entries = %d, want %d", len(body.Entries), tc.wantEntries)
			}
			if body.WindowHours != tc.wantWindow {
				t.Fatalf("windowHours = %d, want %d", body.WindowHours, tc.wantWindow)
			}
		})
	}
}

func TestRecordPromptNilSafe(t *testing.T) {
	// The manager fires this callback unconditionally; a Server without a store
	// (or a nil Server) must not panic.
	var nilServer *Server
	nilServer.RecordPrompt("quality", "governor-eval", "hi")

	s := &Server{}
	s.RecordPrompt("quality", "governor-eval", "hi")
}
