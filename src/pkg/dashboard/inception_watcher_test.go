package dashboard

import (
	"testing"
)

func TestInferFactType(t *testing.T) {
	tests := []struct {
		title string
		want  string
	}{
		{"Project Vision Statement", "vision"},
		{"Core Purpose", "vision"},
		{"Project Goal", "vision"},
		{"Architecture Constitution", "constitution"},
		{"Code style principles", "constitution"},
		{"Key Requirement: Fast startup", "requirement"},
		{"Must support HTTPS", "requirement"},
		{"New Feature: Dashboard", "requirement"},
		{"Constraint: No external deps", "constraint"},
		{"System Boundary", "constraint"},
		{"Limitation on memory", "constraint"},
		{"Primary Stakeholder", "stakeholder"},
		{"Target User group", "stakeholder"},
		{"Target Audience", "stakeholder"},
		{"Acceptance Criteria", "acceptance"},
		{"Success Metric", "acceptance"},
		{"Test Coverage Target", "acceptance"},
		{"Some random title", ""},
		{"Clarification: this is just a question", ""},
	}
	for _, tt := range tests {
		got := inferFactType(tt.title)
		if got != tt.want {
			t.Errorf("inferFactType(%q) = %q, want %q", tt.title, got, tt.want)
		}
	}
}

func TestParseQuestionTable(t *testing.T) {
	lines := []string{
		"┌──────────┬──────────────────────────────┬─────────────┐",
		"│ Category │ Question                     │ Default     │",
		"├──────────┼──────────────────────────────┼─────────────┤",
		"│ language │ What language will you use?   │ Go          │",
		"│ users    │ Who are the primary users?    │ developers  │",
		"│ features │ What must it do?              │             │",
		"└──────────┴──────────────────────────────┴─────────────┘",
	}

	questions := parseQuestionTable(lines)
	if len(questions) != 3 {
		t.Fatalf("expected 3 questions, got %d", len(questions))
	}
	if questions[0].Category != "language" {
		t.Errorf("q0 category = %q", questions[0].Category)
	}
	if questions[0].Text != "What language will you use?" {
		t.Errorf("q0 text = %q", questions[0].Text)
	}
	if questions[0].Default != "Go" {
		t.Errorf("q0 default = %q", questions[0].Default)
	}
}

func TestParseQuestionTableEmpty(t *testing.T) {
	questions := parseQuestionTable(nil)
	if len(questions) != 0 {
		t.Error("nil lines should return empty")
	}
}

func TestParseQuestionTableNoPipes(t *testing.T) {
	lines := []string{
		"This is just regular text",
		"No table formatting here",
	}
	questions := parseQuestionTable(lines)
	if len(questions) != 0 {
		t.Errorf("non-table lines should return empty, got %d", len(questions))
	}
}

func TestParseQuestionTableDedup(t *testing.T) {
	lines := []string{
		"│ language │ What language? │ Go │",
		"│ language │ What language? │ Go │",
	}
	questions := parseQuestionTable(lines)
	if len(questions) != 1 {
		t.Errorf("duplicate questions should be deduped, got %d", len(questions))
	}
}

func TestParseNumberedQuestions(t *testing.T) {
	lines := []string{
		"1. Primary users — who will use this and how?",
		"2. Must-have features — the 2-3 things it must do",
		"3. Language — what programming language?",
		"This is not a question",
		"4. Testing — how should we verify it works?",
	}

	questions := parseNumberedQuestions(lines)
	if len(questions) < 3 {
		t.Fatalf("expected at least 3 questions, got %d", len(questions))
	}
}

func TestParseNumberedQuestionsBullets(t *testing.T) {
	lines := []string{
		"- Primary users: who will use this?",
		"- Features: what must it do?",
		"* Language: what language?",
	}

	questions := parseNumberedQuestions(lines)
	if len(questions) < 2 {
		t.Fatalf("expected at least 2 questions, got %d", len(questions))
	}
}

func TestParseNumberedQuestionsEmpty(t *testing.T) {
	questions := parseNumberedQuestions(nil)
	if len(questions) != 0 {
		t.Error("nil lines should return empty")
	}
}

func TestParseNumberedQuestionsDedup(t *testing.T) {
	lines := []string{
		"1. Users — who uses this?",
		"2. Users — who uses this?",
	}
	questions := parseNumberedQuestions(lines)
	if len(questions) != 1 {
		t.Errorf("duplicates should be deduped, got %d", len(questions))
	}
}

func TestParseNumberedQuestionsBold(t *testing.T) {
	lines := []string{
		"1. **Primary users** — who will use this?",
		"2. **Features** — what must it do?",
	}

	questions := parseNumberedQuestions(lines)
	if len(questions) < 2 {
		t.Fatalf("expected at least 2 bold questions, got %d", len(questions))
	}
}
