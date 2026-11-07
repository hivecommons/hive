package agent

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

// knowledge_advisor_test.go covers the mid-task knowledge advisor (#17052):
// the pure pieces (scope extraction, the typed line, the pane gates) as tables,
// and the per-kick delivery loop against a fake adviser, a fake clock and the
// funcTerminal seams — no tmux, no time.Sleep beyond the package TestMain's
// shrunk textToEnterDelay.

// fakeAdviser answers Advise from a fixed table and records every scope asked.
type fakeAdviser struct {
	policy crux.KnowledgeAdvisorPolicy
	byKey  map[string][]crux.KnowledgeAdvice
	asked  []string
}

func (f *fakeAdviser) Policy(string) crux.KnowledgeAdvisorPolicy { return f.policy }

func (f *fakeAdviser) Advise(_ string, scope crux.KnowledgeScope) []crux.KnowledgeAdvice {
	f.asked = append(f.asked, scope.Key)
	return f.byKey[scope.Key]
}

// workingPane is a Claude Code pane mid-turn: a tool just finished and the
// model is generating, so the busy footer is up and no tool is in flight.
const workingPane = "⏺ Read(src/crux/agent/manager.go)\n⎿  Read 120 lines\n\n✶ Thinking… (4s · ↓ 94 tokens)\n\n❯ \n  esc to interrupt"

// toolPane is the same pane while a shell command runs.
const toolPane = "⏺ Running 1 shell command…\n\n❯ \n  esc to interrupt"

// idlePane is a finished turn: empty prompt, no busy footer.
const idlePane = "✻ Worked for 26s\n\n❯ "

func advisorManager(t *testing.T, adviser crux.KnowledgeAdviser) (*Manager, *AgentProcess, *[]string) {
	t.Helper()
	m := testManager(4)
	kickAt := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := &AgentProcess{
		Name:        "developer",
		Config:      config.AgentConfig{Backend: "claude"},
		tmuxSession: "hive-developer-test-nonexistent",
		LastKick:    &kickAt,
	}
	m.agents[a.Name] = a
	var typed []string
	termSeams(m).sessionAttached = func(*AgentProcess) bool { return false }
	termSeams(m).sendLiteral = func(_ *AgentProcess, text string) { typed = append(typed, text) }
	termSeams(m).sendKeys = func(*AgentProcess, ...string) {}
	m.SetKnowledgeAdviser(adviser)
	return m, a, &typed
}

// fakeClock replaces the advisor's clock; tests advance it by hand.
func fakeClock(m *Manager, start time.Time) *time.Time {
	now := start
	m.knowledgeAdvisor.Load().now = func() time.Time { return now }
	return &now
}

func enabledPolicy() crux.KnowledgeAdvisorPolicy {
	return crux.KnowledgeAdvisorPolicy{Enabled: true, MaxPerKick: 2, Cooldown: time.Minute}
}

func TestObservedKnowledgeScopes(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want []string
	}{
		{
			name: "claude code expanded tool calls",
			pane: "⏺ Read(src/crux/agent/manager.go)\n⏺ Update(src/pkg/config/config.go)",
			want: []string{"src/crux/agent", "src/pkg/config"},
		},
		{
			name: "copilot cli and shell shapes, deeper paths truncate to three segments",
			pane: "● Edit src/pkg/dashboard/static/app.js\n$ cat ./src/docs/operator-reference.md",
			want: []string{"src/pkg/dashboard", "src/docs"},
		},
		{
			name: "one key per scope, first-seen order",
			pane: "src/crux/agent/a.go src/crux/agent/b.go .github/workflows/ci.yml",
			want: []string{"src/crux/agent", ".github/workflows"},
		},
		{
			name: "gh families",
			pane: "$ gh pr checks 17052\n$ gh run view 12 --log-failed\n$ gh pr view 1",
			want: []string{"gh:pr", "gh:run"},
		},
		{
			name: "root files, urls, absolute paths and tooling dirs are not scopes",
			pane: "README.md https://github.com/hivecommons/hive/blob/v5/src/x.go /tmp/out.log node_modules/ws/index.js vendor/x/y.go",
			want: nil,
		},
		{
			name: "explain narration is not work",
			pane: config.ExplainLinePrefix + " reading src/crux/agent/manager.go next",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, sc := range observedKnowledgeScopes(tt.pane) {
				got = append(got, sc.Key)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("scopes = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKnowledgePathScopeKeywordsDropGenericSegments(t *testing.T) {
	sc := knowledgePathScope("./src/crux/agent/manager_kick.go")
	if sc.Kind != knowledgeAdvisorKindPath || sc.Key != "src/crux/agent" || sc.Path != "src/crux/agent/manager_kick.go" {
		t.Fatalf("scope = %+v", sc)
	}
	if want := []string{"crux", "agent"}; !reflect.DeepEqual(sc.Keywords, want) {
		t.Errorf("keywords = %v, want %v", sc.Keywords, want)
	}
	if gh := observedKnowledgeScopes("gh pr list")[0]; !reflect.DeepEqual(gh.Keywords, []string{"pull request", "pr"}) {
		t.Errorf("gh keywords = %v", gh.Keywords)
	}
}

func TestRenderKnowledgeAdvice(t *testing.T) {
	long := strings.Repeat("word ", 100)
	line := renderKnowledgeAdvice(crux.KnowledgeAdvice{
		Slug: "scripts-tree", Title: "Re-pin  TRUSTED_SCRIPTS_TREE", Summary: "line one\nline two " + long, Ref: "knowledge entry scripts-tree",
	})
	if !strings.HasPrefix(line, "📚 Knowledge: Re-pin TRUSTED_SCRIPTS_TREE — line one line two word") {
		t.Errorf("line = %q", line)
	}
	if !strings.HasSuffix(line, " (full: knowledge entry scripts-tree)") {
		t.Errorf("line = %q", line)
	}
	if strings.Contains(line, "\n") {
		t.Error("line must be a single line")
	}
	body := strings.TrimSuffix(strings.TrimPrefix(line, "📚 Knowledge: Re-pin TRUSTED_SCRIPTS_TREE — "), " (full: knowledge entry scripts-tree)")
	if n := len([]rune(body)); n > knowledgeAdvisorMaxSummary {
		t.Errorf("summary is %d runes, cap %d", n, knowledgeAdvisorMaxSummary)
	}
	if got := renderKnowledgeAdvice(crux.KnowledgeAdvice{Title: "T"}); got != "📚 Knowledge: T" {
		t.Errorf("bare title = %q", got)
	}
}

func TestKnowledgePaneGates(t *testing.T) {
	tests := []struct {
		name         string
		pane         string
		idle, inTool bool
	}{
		{"generating", workingPane, false, false},
		{"shell command running", toolPane, false, true},
		{"omp tool call", "⏺ Running… (esc to cancel)", false, true},
		{"turn over", idlePane, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := knowledgeTurnIdle(tt.pane); got != tt.idle {
				t.Errorf("idle = %v, want %v", got, tt.idle)
			}
			if got := knowledgeToolInFlight(tt.pane); got != tt.inTool {
				t.Errorf("toolInFlight = %v, want %v", got, tt.inTool)
			}
		})
	}
}

func TestAdviseKnowledge_BaselineThenNewScopeDelivers(t *testing.T) {
	adv := &fakeAdviser{policy: enabledPolicy(), byKey: map[string][]crux.KnowledgeAdvice{
		"src/scripts": {{Slug: "scripts-tree", Title: "Re-pin TRUSTED_SCRIPTS_TREE", Summary: "Any src/scripts edit must re-pin the tree."}},
	}}
	m, a, typed := advisorManager(t, adv)
	fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))

	// First tick after the kick: src/scripts is already in scrollback from
	// before the kick, so it is baseline, not a new scope.
	m.adviseKnowledge(a, "⏺ Read(src/scripts/old.sh)", workingPane)
	if len(adv.asked) != 0 || len(*typed) != 0 {
		t.Fatalf("baseline tick asked %v, typed %v", adv.asked, *typed)
	}
	m.adviseKnowledge(a, "⏺ Read(src/scripts/old.sh)", workingPane)
	if len(adv.asked) != 0 {
		t.Fatalf("baseline scope re-asked: %v", adv.asked)
	}

	// A new kick resets the baseline, so the same scope is new again.
	later := a.LastKick.Add(time.Hour)
	a.LastKick = &later
	m.adviseKnowledge(a, "⏺ Read(src/crux/agent/x.go)", workingPane)
	m.adviseKnowledge(a, "⏺ Read(src/crux/agent/x.go)\n⏺ Edit(src/scripts/ci.sh)", workingPane)
	if !reflect.DeepEqual(adv.asked, []string{"src/scripts"}) {
		t.Fatalf("asked = %v", adv.asked)
	}
	if len(*typed) != 1 || !strings.HasPrefix((*typed)[0], "📚 Knowledge: Re-pin TRUSTED_SCRIPTS_TREE — Any src/scripts edit") {
		t.Fatalf("typed = %v", *typed)
	}
	// Same slug never twice in a kick, even if another scope matches it.
	adv.byKey["gh:pr"] = adv.byKey["src/scripts"]
	m.adviseKnowledge(a, "⏺ Edit(src/scripts/ci.sh)\n$ gh pr checks 1", workingPane)
	if len(*typed) != 1 {
		t.Fatalf("duplicate slug typed: %v", *typed)
	}
}

func TestAdviseKnowledge_CapCooldownAndPaneGates(t *testing.T) {
	adv := &fakeAdviser{policy: enabledPolicy(), byKey: map[string][]crux.KnowledgeAdvice{
		"src/a": {{Slug: "a1", Title: "A1"}, {Slug: "a2", Title: "A2"}, {Slug: "a3", Title: "A3"}},
	}}
	m, a, typed := advisorManager(t, adv)
	now := fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))
	m.adviseKnowledge(a, "", workingPane) // baseline

	// Tool in flight: the advice queues, nothing is typed.
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", toolPane)
	if len(*typed) != 0 {
		t.Fatalf("typed during a tool call: %v", *typed)
	}
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
	if len(*typed) != 1 || (*typed)[0] != "📚 Knowledge: A1" {
		t.Fatalf("typed = %v", *typed)
	}
	// Cooldown holds the second line back until the clock moves.
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
	if len(*typed) != 1 {
		t.Fatalf("cooldown ignored: %v", *typed)
	}
	*now = now.Add(time.Minute)
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
	if len(*typed) != 2 || (*typed)[1] != "📚 Knowledge: A2" {
		t.Fatalf("typed = %v", *typed)
	}
	// MaxPerKick is 2: the third candidate never goes out this kick.
	*now = now.Add(time.Minute)
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
	if len(*typed) != 2 {
		t.Fatalf("cap ignored: %v", *typed)
	}
	if len(adv.asked) != 1 {
		t.Errorf("adviser asked %d times for one scope, want 1", len(adv.asked))
	}
}

func TestAdviseKnowledge_IdleTurnDropsPending(t *testing.T) {
	adv := &fakeAdviser{policy: enabledPolicy(), byKey: map[string][]crux.KnowledgeAdvice{
		"src/a": {{Slug: "a1", Title: "A1"}},
	}}
	m, a, typed := advisorManager(t, adv)
	fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))
	m.adviseKnowledge(a, "", workingPane)
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", toolPane) // queued
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", idlePane) // turn over: dropped
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
	if len(*typed) != 0 {
		t.Fatalf("advice for a finished turn was typed: %v", *typed)
	}
}

func TestAdviseKnowledge_Refusals(t *testing.T) {
	newCase := func() (*fakeAdviser, *Manager, *AgentProcess, *[]string) {
		adv := &fakeAdviser{policy: enabledPolicy(), byKey: map[string][]crux.KnowledgeAdvice{
			"src/a": {{Slug: "a1", Title: "A1"}},
		}}
		m, a, typed := advisorManager(t, adv)
		fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))
		m.adviseKnowledge(a, "", workingPane)
		return adv, m, a, typed
	}
	t.Run("disabled policy parses nothing", func(t *testing.T) {
		adv, m, a, typed := newCase()
		adv.policy = crux.KnowledgeAdvisorPolicy{}
		m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
		if len(adv.asked) != 0 || len(*typed) != 0 {
			t.Fatalf("asked %v typed %v", adv.asked, *typed)
		}
	})
	t.Run("kick being typed", func(t *testing.T) {
		_, m, a, typed := newCase()
		a.kickDelivering.Store(true)
		m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
		if len(*typed) != 0 {
			t.Fatal("typed into a kick in delivery")
		}
	})
	t.Run("no kick yet", func(t *testing.T) {
		_, m, a, typed := newCase()
		a.LastKick = nil
		m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
		if len(*typed) != 0 {
			t.Fatal("typed before any kick")
		}
	})
	t.Run("human attached", func(t *testing.T) {
		_, m, a, typed := newCase()
		termSeams(m).sessionAttached = func(*AgentProcess) bool { return true }
		m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
		if len(*typed) != 0 {
			t.Fatal("typed over an attached client")
		}
	})
	t.Run("no adviser installed", func(t *testing.T) {
		_, m, a, typed := newCase()
		m.SetKnowledgeAdviser(nil)
		m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
		if len(*typed) != 0 {
			t.Fatal("typed with the advisor off")
		}
	})
}
