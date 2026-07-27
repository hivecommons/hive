package agentparse

import (
	"reflect"
	"testing"
)

var testCategories = map[string]bool{
	"users": true, "features": true, "constraints": true,
	"testing": true, "deployment": true, "storage": true,
	"language": true, "general": true,
}

func TestParseTable(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  []Item
	}{
		{
			name:  "nil",
			lines: nil,
			want:  nil,
		},
		{
			name:  "no pipes",
			lines: []string{"just text", "no table here"},
			want:  nil,
		},
		{
			name: "basic three rows",
			lines: []string{
				"│ language │ What language? │ Go         │",
				"│ users    │ Who uses it?   │ developers │",
				"│ features │ What must do?  │            │",
			},
			want: []Item{
				{Category: "language", Title: "What language?", Default: "Go"},
				{Category: "users", Title: "Who uses it?", Default: "developers"},
				{Category: "features", Title: "What must do?", Default: ""},
			},
		},
		{
			name: "category not first column",
			lines: []string{
				"│ 1 │ storage │ Where to persist? │ disk │",
			},
			want: []Item{
				{Category: "storage", Title: "Where to persist?", Default: "disk"},
			},
		},
		{
			name: "missing default column",
			lines: []string{
				"│ testing │ How to verify?",
			},
			want: []Item{
				{Category: "testing", Title: "How to verify?", Default: ""},
			},
		},
		{
			name: "continuation row merges",
			lines: []string{
				"│ features │ What must it │ nothing │",
				"│          │ absolutely do │ special │",
			},
			want: []Item{
				{Category: "features", Title: "What must it absolutely do", Default: "nothing special"},
			},
		},
		{
			name: "dedup identical",
			lines: []string{
				"│ users │ Who? │ devs │",
				"│ users │ Who? │ devs │",
			},
			want: []Item{
				{Category: "users", Title: "Who?", Default: "devs"},
			},
		},
		{
			name: "continuation before any category is ignored",
			lines: []string{
				"│ some │ stray │ row │",
				"│ users │ Who? │ devs │",
			},
			want: []Item{
				{Category: "users", Title: "Who?", Default: "devs"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseTable(tt.lines, testCategories)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseTable() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseNumberedList(t *testing.T) {
	tests := []struct {
		name     string
		lines    []string
		fallback string
		want     []Item
	}{
		{name: "nil", lines: nil, fallback: "general", want: nil},
		{
			name: "numbered",
			lines: []string{
				"1. Primary users — who will use this?",
				"not a list item",
				"3. Language — which language?",
			},
			fallback: "general",
			want: []Item{
				{Category: "users", Title: "who will use this?"},
				{Category: "language", Title: "which language?"},
			},
		},
		{
			name:     "bullets",
			lines:    []string{"- Users: who?", "* Language: what?"},
			fallback: "general",
			want: []Item{
				{Category: "users", Title: "who?"},
				{Category: "language", Title: "what?"},
			},
		},
		{
			name:     "bold labels",
			lines:    []string{"1. **Features** — what must it do?"},
			fallback: "general",
			want: []Item{
				{Category: "features", Title: "what must it do?"},
			},
		},
		{
			name:     "unknown label uses fallback",
			lines:    []string{"1. Timeline — when to ship?"},
			fallback: "general",
			want: []Item{
				{Category: "general", Title: "when to ship?"},
			},
		},
		{
			name:     "empty question skipped",
			lines:    []string{"1. Users — "},
			fallback: "general",
			want:     nil,
		},
		{
			name:     "dedup",
			lines:    []string{"1. Users — who?", "2. Users — who?"},
			fallback: "general",
			want: []Item{
				{Category: "users", Title: "who?"},
			},
		},
		{
			name:     "empty fallback stays empty",
			lines:    []string{"1. Timeline — when?"},
			fallback: "",
			want: []Item{
				{Category: "", Title: "when?"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseNumberedList(tt.lines, testCategories, tt.fallback)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseNumberedList() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestNumberedListRe(t *testing.T) {
	re := NumberedListRe()
	if re == nil {
		t.Fatal("NumberedListRe returned nil")
	}
	m := re.FindStringSubmatch("1. Users — who?")
	if m == nil || m[2] != "who?" {
		t.Errorf("unexpected match: %v", m)
	}
}

func TestIsTemplatePlaceholder(t *testing.T) {
	tests := map[string]bool{
		"<fact title>":       true,
		"<question title>":   true,
		"<anything>":         true,
		"<>":                 true,
		"your question here": true,
		"fact_title":         true,
		"QUESTION_TITLE":     true, // case-insensitive
		"real title":         false,
		"Primary users":      false,
		"":                   false,
	}
	for in, want := range tests {
		if got := IsTemplatePlaceholder(in); got != want {
			t.Errorf("IsTemplatePlaceholder(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTitleFromBdCreate(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		want   string
		wantOK bool
	}{
		{"basic", `bd create --title "What db?" --type advisory`, "What db?", true},
		{"no title flag", `bd create --type advisory`, "", false},
		{"empty title", `bd create --title ""`, "", false},
		{"placeholder", `bd create --title "<fact title>"`, "", false},
		{"trims whitespace", `bd create --title "  spaced  "`, "spaced", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := TitleFromBdCreate(tt.line)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("TitleFromBdCreate(%q) = (%q,%v), want (%q,%v)", tt.line, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseTaskList(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  []Task
	}{
		{name: "nil", lines: nil, want: nil},
		{name: "no list items", lines: []string{"prose", "more prose"}, want: nil},
		{
			name: "ids deps and execution tags",
			lines: []string{
				"1. [T1] Design data model [agent_suitable]",
				"2. [T2] Get legal sign-off (human_required)",
				"3. [T3] Implement persistence (depends: T1) [agent_suitable]",
			},
			want: []Task{
				{Ref: "T1", Title: "Design data model", Execution: "agent_suitable"},
				{Ref: "T2", Title: "Get legal sign-off", Execution: "human_required"},
				{Ref: "T3", Title: "Implement persistence", Execution: "agent_suitable", DependsOn: []string{"T1"}},
			},
		},
		{
			name: "multiple deps and depends on phrasing",
			lines: []string{
				"1. [A] first",
				"2. [B] second (depends on: A)",
				"3. [C] third (depends: A, B)",
			},
			want: []Task{
				{Ref: "A", Title: "first"},
				{Ref: "B", Title: "second", DependsOn: []string{"A"}},
				{Ref: "C", Title: "third", DependsOn: []string{"A", "B"}},
			},
		},
		{
			name: "execution parenthetical variant with space",
			lines: []string{
				"- [X] do a thing (execution: agent suitable)",
			},
			want: []Task{
				{Ref: "X", Title: "do a thing", Execution: "agent_suitable"},
			},
		},
		{
			name: "unknown execution tag dropped",
			lines: []string{
				"1. [T1] task (execution: maybe)",
			},
			want: []Task{
				// "(execution: maybe)" does not match the execution regex, so it
				// remains part of the title.
				{Ref: "T1", Title: "task (execution: maybe)"},
			},
		},
		{
			name: "no id token",
			lines: []string{
				"1. plain task without id [human_required]",
			},
			want: []Task{
				{Ref: "", Title: "plain task without id", Execution: "human_required"},
			},
		},
		{
			name: "bracketed agent_suitable tag",
			lines: []string{
				"1. [T1] build the thing [agent_suitable]",
			},
			want: []Task{
				{Ref: "T1", Title: "build the thing", Execution: "agent_suitable"},
			},
		},
		{
			name: "dedup by title",
			lines: []string{
				"1. [T1] same task",
				"2. [T2] same task",
			},
			want: []Task{
				{Ref: "T1", Title: "same task"},
			},
		},
		{
			name: "empty remainder skipped",
			lines: []string{
				"1. [T1] (depends: T0)",
			},
			want: nil,
		},
		{
			name: "empty deps clause yields no deps",
			lines: []string{
				"1. [T1] task (depends: )",
			},
			want: []Task{
				{Ref: "T1", Title: "task"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseTaskList(tt.lines)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseTaskList() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single line no newline", "hello", []string{"hello"}},
		{"trailing newline dropped", "a\nb\n", []string{"a", "b"}},
		{"crlf normalized", "a\r\nb", []string{"a", "b"}},
		{"trailing spaces trimmed", "a   \nb\t", []string{"a", "b"}},
		{"internal blank preserved", "a\n\nb\n", []string{"a", "", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitLines(tt.in)
			// SplitLines never returns nil for non-empty input; normalize the
			// empty case where a single "" element is expected to drop out.
			if tt.in == "" {
				if len(got) != 1 || got[0] != "" {
					// "" -> Split gives [""], trailing-empty rule drops it -> [].
					if len(got) != 0 {
						t.Errorf("SplitLines(%q) = %+v, want empty", tt.in, got)
					}
					return
				}
			}
			if tt.want == nil {
				if len(got) != 0 {
					t.Errorf("SplitLines(%q) = %+v, want empty", tt.in, got)
				}
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitLines(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeExecution(t *testing.T) {
	tests := map[string]string{
		"agent_suitable": ExecutionAgentSuitable,
		"agent suitable": ExecutionAgentSuitable,
		"HUMAN_REQUIRED": ExecutionHumanRequired,
		"human required": ExecutionHumanRequired,
		"unknown":        "",
		"":               "",
	}
	for in, want := range tests {
		if got := normalizeExecution(in); got != want {
			t.Errorf("normalizeExecution(%q) = %q, want %q", in, got, want)
		}
	}
}
