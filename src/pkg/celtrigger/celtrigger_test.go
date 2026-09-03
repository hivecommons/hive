package celtrigger

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// ruleNames extracts the ordered Name list from a Match result for assertions.
func ruleNames(rules []Rule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.Name)
	}
	return out
}

func TestCompile_ValidRules(t *testing.T) {
	rules := []Rule{
		{Name: "bug-triage", Expr: `event.kind == "issue.opened" && hasLabel(event.labels, "bug")`, Agent: "triager"},
		{Name: "pr-review", Expr: `event.kind == "pr.opened" && !event.is_draft`, Agent: "reviewer"},
		{Name: "in-check", Expr: `"urgent" in event.labels`, Agent: "escalator"},
	}
	eng, err := Compile(rules)
	if err != nil {
		t.Fatalf("Compile returned error for valid rules: %v", err)
	}
	if eng == nil {
		t.Fatal("Compile returned nil engine")
	}
	if got := eng.Len(); got != len(rules) {
		t.Fatalf("engine Len = %d, want %d", got, len(rules))
	}
}

func TestCompile_InvalidCEL_ReturnsErrorNoPanic(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{"syntax-error", `event.kind == `},
		{"unknown-field", `event.does_not_exist == "x"`},
		{"unknown-func", `bogusFunc(event.title)`},
		{"non-bool-result", `event.title`},
		{"empty-expr", ``},
		{"garbage", `@@@!!!`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Compile panicked on invalid expr %q: %v", tc.expr, r)
				}
			}()
			eng, err := Compile([]Rule{{Name: tc.name, Expr: tc.expr, Agent: "a"}})
			if err == nil {
				t.Fatalf("Compile accepted invalid expr %q (expected error)", tc.expr)
			}
			if eng != nil {
				t.Fatalf("Compile returned non-nil engine alongside error for %q", tc.expr)
			}
		})
	}
}

func TestCompile_EmptyRules(t *testing.T) {
	eng, err := Compile(nil)
	if err != nil {
		t.Fatalf("Compile(nil) error: %v", err)
	}
	if eng.Len() != 0 {
		t.Fatalf("empty engine Len = %d, want 0", eng.Len())
	}
	got := eng.Match(NormalizedEvent{Kind: KindIssueOpened})
	if len(got) != 0 {
		t.Fatalf("empty engine matched %d rules, want 0", len(got))
	}
}

func TestMatch_LabelMatch(t *testing.T) {
	eng, err := Compile([]Rule{
		{Name: "bug", Expr: `hasLabel(event.labels, "bug")`, Agent: "triager"},
		{Name: "in-op", Expr: `"enhancement" in event.labels`, Agent: "planner"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	ev := NormalizedEvent{Kind: KindIssueLabeled, Labels: []string{"bug", "p1"}}
	got := eng.Match(ev)
	if names := ruleNames(got); len(names) != 1 || names[0] != "bug" {
		t.Fatalf("Match labels=[bug,p1] = %v, want [bug]", ruleNames(got))
	}

	ev2 := NormalizedEvent{Kind: KindIssueLabeled, Labels: []string{"enhancement"}}
	if names := ruleNames(eng.Match(ev2)); len(names) != 1 || names[0] != "in-op" {
		t.Fatalf("Match labels=[enhancement] = %v, want [in-op]", ruleNames(eng.Match(ev2)))
	}
}

func TestMatch_KindMatch(t *testing.T) {
	eng, err := Compile([]Rule{
		{Name: "issue", Expr: `event.kind == "issue.opened"`, Agent: "a"},
		{Name: "pr", Expr: `event.kind == "pr.opened"`, Agent: "b"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got := eng.Match(NormalizedEvent{Kind: KindPROpened})
	if names := ruleNames(got); len(names) != 1 || names[0] != "pr" {
		t.Fatalf("Match kind=pr.opened = %v, want [pr]", ruleNames(got))
	}
}

func TestMatch_AuthorMatch(t *testing.T) {
	eng, err := Compile([]Rule{
		{Name: "from-bot", Expr: `event.author == "dependabot[bot]"`, Agent: "bothandler"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := eng.Match(NormalizedEvent{Author: "dependabot[bot]"}); len(got) != 1 {
		t.Fatalf("author match failed: got %v", ruleNames(got))
	}
	if got := eng.Match(NormalizedEvent{Author: "alice"}); len(got) != 0 {
		t.Fatalf("author non-match returned %v, want none", ruleNames(got))
	}
}

func TestMatch_BooleanCombos(t *testing.T) {
	eng, err := Compile([]Rule{
		{Name: "ready-pr", Expr: `event.kind == "pr.opened" && !event.is_draft && event.base_branch == "main"`, Agent: "reviewer"},
		{Name: "urgent-or-sec", Expr: `hasLabel(event.labels, "urgent") || hasLabel(event.labels, "security")`, Agent: "escalator"},
		{Name: "body-contains", Expr: `event.body.contains("/retry")`, Agent: "retrier"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	// ready-pr: non-draft PR to main should match; draft should not.
	if got := eng.Match(NormalizedEvent{Kind: KindPROpened, IsDraft: false, BaseBranch: "main"}); !containsName(got, "ready-pr") {
		t.Fatalf("expected ready-pr to match, got %v", ruleNames(got))
	}
	if got := eng.Match(NormalizedEvent{Kind: KindPROpened, IsDraft: true, BaseBranch: "main"}); containsName(got, "ready-pr") {
		t.Fatalf("draft PR should not match ready-pr, got %v", ruleNames(got))
	}

	// urgent-or-sec: either label triggers.
	if got := eng.Match(NormalizedEvent{Labels: []string{"security"}}); !containsName(got, "urgent-or-sec") {
		t.Fatalf("expected urgent-or-sec to match on security, got %v", ruleNames(got))
	}

	// body-contains: string function usage.
	if got := eng.Match(NormalizedEvent{Body: "please /retry the build"}); !containsName(got, "body-contains") {
		t.Fatalf("expected body-contains to match, got %v", ruleNames(got))
	}
	if got := eng.Match(NormalizedEvent{Body: "no command here"}); containsName(got, "body-contains") {
		t.Fatalf("body-contains should not match plain body, got %v", ruleNames(got))
	}
}

func TestMatch_PriorityOrdering(t *testing.T) {
	// All three rules match a labeled event; assert descending priority order,
	// with declaration order preserved on ties.
	eng, err := Compile([]Rule{
		{Name: "low", Expr: `true`, Agent: "a", Priority: 1},
		{Name: "high", Expr: `true`, Agent: "b", Priority: 100},
		{Name: "mid-a", Expr: `true`, Agent: "c", Priority: 50},
		{Name: "mid-b", Expr: `true`, Agent: "d", Priority: 50},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got := ruleNames(eng.Match(NormalizedEvent{}))
	want := []string{"high", "mid-a", "mid-b", "low"}
	if len(got) != len(want) {
		t.Fatalf("Match returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("priority order = %v, want %v", got, want)
		}
	}
}

func TestMatch_NoMatches(t *testing.T) {
	eng, err := Compile([]Rule{
		{Name: "never", Expr: `event.kind == "pr.merged"`, Agent: "a"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := eng.Match(NormalizedEvent{Kind: KindIssueOpened}); len(got) != 0 {
		t.Fatalf("expected no matches, got %v", ruleNames(got))
	}
}

func TestMatch_NilEngineSafe(t *testing.T) {
	var eng *Engine
	if got := eng.Match(NormalizedEvent{Kind: KindIssueOpened}); got != nil {
		t.Fatalf("nil engine Match should return nil, got %v", got)
	}
	if eng.Len() != 0 {
		t.Fatalf("nil engine Len should be 0")
	}
}

func TestMatch_CommentEvent(t *testing.T) {
	eng, err := Compile([]Rule{
		{Name: "slash-cmd", Expr: `event.kind == "comment.created" && event.comment.startsWith("/hive")`, Agent: "cmd"},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := eng.Match(NormalizedEvent{Kind: KindCommentCreated, Comment: "/hive run"}); !containsName(got, "slash-cmd") {
		t.Fatalf("expected slash-cmd to match, got %v", ruleNames(got))
	}
	if got := eng.Match(NormalizedEvent{Kind: KindCommentCreated, Comment: "just a comment"}); containsName(got, "slash-cmd") {
		t.Fatalf("slash-cmd should not match plain comment, got %v", ruleNames(got))
	}
}

func containsName(rules []Rule, name string) bool {
	for _, r := range rules {
		if r.Name == name {
			return true
		}
	}
	return false
}

func TestMatchSkipsRuleThatExceedsCostLimit(t *testing.T) {
	var logs bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	rules := []Rule{{
		Name:  "expensive",
		Agent: "scanner",
		Expr:  `event.labels.all(a, event.labels.all(b, event.labels.all(c, a != "" && b != "" && c != "")))`,
	}}
	engine, err := Compile(rules)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	labels := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		labels = append(labels, fmt.Sprintf("label-%03d", i))
	}
	if got := engine.Match(NormalizedEvent{Kind: KindIssueOpened, Labels: labels}); len(got) != 0 {
		t.Fatalf("expensive CEL rule matched despite exceeding cost limit: %+v", got)
	}
	gotLog := logs.String()
	if !strings.Contains(gotLog, "celtrigger: rule exceeded evaluation cost budget; treating as no-match") ||
		!strings.Contains(gotLog, "rule=expensive") ||
		!strings.Contains(gotLog, fmt.Sprintf("budget=%d", maxEvalCost)) {
		t.Fatalf("cost-limit overrun was not observable in logs: %q", gotLog)
	}
}

func TestMatchAllowsNormalRuleWithinCostLimit(t *testing.T) {
	engine, err := Compile([]Rule{{Name: "bug", Agent: "scanner", Expr: `hasLabel(event.labels, "bug")`}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got := engine.Match(NormalizedEvent{Kind: KindIssueOpened, Labels: []string{"bug"}})
	if len(got) != 1 || got[0].Agent != "scanner" {
		t.Fatalf("normal CEL rule did not match: %+v", got)
	}
}

func TestMatchAllowsRealisticOperatorRuleWithinCostLimit(t *testing.T) {
	engine, err := Compile([]Rule{{
		Name:  "realistic",
		Agent: "scanner",
		Expr:  `event.repo.startsWith("hivecommons/") && event.title.matches("(?i).*(bug|fix|security).*") && event.labels.exists(l, l == "security")`,
	}})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got := engine.Match(NormalizedEvent{
		Kind:   KindIssueOpened,
		Repo:   "hivecommons/hive",
		Title:  "Security fix for CEL trigger limits",
		Labels: []string{"help wanted", "security", "v4"},
	})
	if len(got) != 1 || got[0].Name != "realistic" {
		t.Fatalf("realistic operator rule did not match within cost budget %d: %+v", maxEvalCost, got)
	}
}
