package dashboard

import (
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// This file "pins" the current behavior of the inception output parsers
// (parseQuestionTable, parseNumberedQuestions, parseQuestionFromBdCreate, and
// isTemplatePlaceholder) BEFORE they are lifted into the shared pkg/agentparse
// package. The lift-and-generalize refactor must keep every assertion below
// green, guaranteeing Level-1 ideation parsing is unchanged.

func TestPin_ParseQuestionTable_FullTable(t *testing.T) {
	lines := []string{
		"┌──────────┬──────────────────────────────┬─────────────┐",
		"│ Category │ Question                     │ Default     │",
		"├──────────┼──────────────────────────────┼─────────────┤",
		"│ language │ What language will you use?   │ Go          │",
		"│ users    │ Who are the primary users?    │ developers  │",
		"│ features │ What must it do?              │             │",
		"└──────────┴──────────────────────────────┴─────────────┘",
	}
	got := parseQuestionTable(lines)
	if len(got) != 3 {
		t.Fatalf("want 3 questions, got %d: %+v", len(got), got)
	}
	want := []knowledge.Question{
		{ID: "language", Text: "What language will you use?", Default: "Go", Category: "language"},
		{ID: "users", Text: "Who are the primary users?", Default: "developers", Category: "users"},
		{ID: "features", Text: "What must it do?", Default: "", Category: "features"},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("q[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestPin_ParseQuestionTable_BeadColumnLayout(t *testing.T) {
	// The category is found dynamically regardless of the leading column.
	lines := []string{
		"│ 1 │ storage │ Where do we persist state? │ local disk │",
		"│ 2 │ testing │ How do we verify?          │ unit tests │",
	}
	got := parseQuestionTable(lines)
	if len(got) != 2 {
		t.Fatalf("want 2, got %d: %+v", len(got), got)
	}
	if got[0].Category != "storage" || got[0].Text != "Where do we persist state?" || got[0].Default != "local disk" {
		t.Errorf("q0 = %+v", got[0])
	}
	if got[1].Category != "testing" {
		t.Errorf("q1 category = %q", got[1].Category)
	}
}

func TestPin_ParseQuestionTable_ContinuationRow(t *testing.T) {
	lines := []string{
		"│ features │ What must it            │ nothing │",
		"│          │ absolutely do          │ special │",
	}
	got := parseQuestionTable(lines)
	if len(got) != 1 {
		t.Fatalf("want 1 merged question, got %d: %+v", len(got), got)
	}
	if got[0].Text != "What must it absolutely do" {
		t.Errorf("continuation text = %q", got[0].Text)
	}
	if got[0].Default != "nothing special" {
		t.Errorf("continuation default = %q", got[0].Default)
	}
}

func TestPin_ParseQuestionTable_EmptyAndNoPipes(t *testing.T) {
	if got := parseQuestionTable(nil); len(got) != 0 {
		t.Errorf("nil -> %d", len(got))
	}
	if got := parseQuestionTable([]string{"regular text", "no table"}); len(got) != 0 {
		t.Errorf("no-pipes -> %d", len(got))
	}
}

func TestPin_ParseQuestionTable_Dedup(t *testing.T) {
	lines := []string{
		"│ language │ What language? │ Go │",
		"│ language │ What language? │ Go │",
	}
	if got := parseQuestionTable(lines); len(got) != 1 {
		t.Errorf("dedup -> %d", len(got))
	}
}

func TestPin_ParseNumberedQuestions_Numbered(t *testing.T) {
	lines := []string{
		"1. Primary users — who will use this and how?",
		"2. Must-have features — the 2-3 things it must do",
		"3. Language — what programming language?",
		"This is not a question",
		"4. Testing — how should we verify it works?",
	}
	got := parseNumberedQuestions(lines)
	if len(got) != 4 {
		t.Fatalf("want 4, got %d: %+v", len(got), got)
	}
	// Category inference from the captured label. Note the regex captures the
	// label up to the first non-\w/\s char, so "Must-have features" captures
	// only "must" (-> general) and the question text keeps "have features ...".
	if got[0].Category != "users" {
		t.Errorf("q0 category = %q, want users", got[0].Category)
	}
	if got[0].Text != "who will use this and how?" {
		t.Errorf("q0 text = %q", got[0].Text)
	}
	if got[1].Category != "general" {
		t.Errorf("q1 category = %q, want general (label captured as 'must')", got[1].Category)
	}
	if got[1].Text != "have features — the 2-3 things it must do" {
		t.Errorf("q1 text = %q", got[1].Text)
	}
	if got[2].Category != "language" {
		t.Errorf("q2 category = %q, want language", got[2].Category)
	}
	if got[3].Category != "testing" {
		t.Errorf("q3 category = %q, want testing", got[3].Category)
	}
}

func TestPin_ParseNumberedQuestions_Bullets(t *testing.T) {
	lines := []string{
		"- Primary users: who will use this?",
		"* Language: what language?",
	}
	got := parseNumberedQuestions(lines)
	if len(got) != 2 {
		t.Fatalf("want 2, got %d: %+v", len(got), got)
	}
}

func TestPin_ParseNumberedQuestions_Bold(t *testing.T) {
	lines := []string{
		"1. **Primary users** — who will use this?",
		"2. **Features** — what must it do?",
	}
	got := parseNumberedQuestions(lines)
	if len(got) != 2 {
		t.Fatalf("want 2, got %d: %+v", len(got), got)
	}
	if got[0].Category != "users" {
		t.Errorf("q0 category = %q", got[0].Category)
	}
}

func TestPin_ParseNumberedQuestions_UnknownLabelGeneral(t *testing.T) {
	lines := []string{"1. Timeline — when must it ship?"}
	got := parseNumberedQuestions(lines)
	if len(got) != 1 || got[0].Category != "general" {
		t.Fatalf("unknown label -> %+v", got)
	}
}

func TestPin_ParseNumberedQuestions_EmptyAndDedup(t *testing.T) {
	if got := parseNumberedQuestions(nil); len(got) != 0 {
		t.Errorf("nil -> %d", len(got))
	}
	dup := []string{"1. Users — who uses this?", "2. Users — who uses this?"}
	if got := parseNumberedQuestions(dup); len(got) != 1 {
		t.Errorf("dedup -> %d", len(got))
	}
}

func TestPin_ParseQuestionFromBdCreate(t *testing.T) {
	w := &InceptionWatcher{logger: slog.Default()}

	q := w.parseQuestionFromBdCreate(`bd create --title "What database should we use?" --type advisory`)
	if q == nil || q.Text != "What database should we use?" {
		t.Fatalf("basic -> %+v", q)
	}

	q = w.parseQuestionFromBdCreate(`bd create --title "Who are the primary users?" --actor brainstorm`)
	if q == nil || q.Category != "users" {
		t.Fatalf("users category -> %+v", q)
	}

	q = w.parseQuestionFromBdCreate(`bd create --title "Clarification: What language?"`)
	if q == nil || q.Text != "What language?" {
		t.Fatalf("clarification strip -> %+v", q)
	}
	if q.Category != "language" {
		t.Errorf("language category -> %q", q.Category)
	}
	if q.Default != "Yes, use best practices" {
		t.Errorf("default -> %q", q.Default)
	}

	if got := w.parseQuestionFromBdCreate(`bd create --type advisory`); got != nil {
		t.Errorf("no title -> %+v", got)
	}
	if got := w.parseQuestionFromBdCreate(`bd create --title "<fact title>"`); got != nil {
		t.Errorf("placeholder -> %+v", got)
	}
	if got := w.parseQuestionFromBdCreate(`bd create --title ""`); got != nil {
		t.Errorf("empty title -> %+v", got)
	}
}

func TestPin_IsTemplatePlaceholder(t *testing.T) {
	cases := map[string]bool{
		"<fact title>":         true,
		"<question title>":     true,
		"<anything angled>":    true,
		"your question here":   true,
		"What database to use?": false,
		"Primary users":        false,
	}
	for in, want := range cases {
		if got := isTemplatePlaceholder(in); got != want {
			t.Errorf("isTemplatePlaceholder(%q) = %v, want %v", in, got, want)
		}
	}
}
