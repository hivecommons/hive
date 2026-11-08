package agent

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/internal/testutil"
	"github.com/hivecommons/hive/crux/logscrub"
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
	scopes []crux.KnowledgeScope
}

func (f *fakeAdviser) Policy(string) crux.KnowledgeAdvisorPolicy { return f.policy }

func (f *fakeAdviser) Advise(_ string, scope crux.KnowledgeScope) []crux.KnowledgeAdvice {
	f.asked = append(f.asked, scope.Key)
	f.scopes = append(f.scopes, scope)
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

// blockingAdviser holds every Advise call until the test releases it, and
// records how many calls ran at once.
type blockingAdviser struct {
	policy  crux.KnowledgeAdvisorPolicy
	byKey   map[string][]crux.KnowledgeAdvice
	release chan struct{}

	mu        sync.Mutex
	asked     []string
	running   int
	maxActive int
}

func newBlockingAdviser(byKey map[string][]crux.KnowledgeAdvice) *blockingAdviser {
	return &blockingAdviser{policy: enabledPolicy(), byKey: byKey, release: make(chan struct{}, 16)}
}

func (b *blockingAdviser) Policy(string) crux.KnowledgeAdvisorPolicy { return b.policy }

func (b *blockingAdviser) Advise(_ string, scope crux.KnowledgeScope) []crux.KnowledgeAdvice {
	b.mu.Lock()
	b.asked = append(b.asked, scope.Key)
	b.running++
	if b.running > b.maxActive {
		b.maxActive = b.running
	}
	b.mu.Unlock()
	<-b.release
	b.mu.Lock()
	b.running--
	b.mu.Unlock()
	return b.byKey[scope.Key]
}

func (b *blockingAdviser) snapshot() ([]string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.asked...), b.maxActive
}

// waitKnowledgeIdle waits until no Advise call is in flight for agent.
func waitKnowledgeIdle(t *testing.T, m *Manager, agent string) {
	t.Helper()
	ka := m.knowledgeAdvisor.Load()
	testutil.Eventually(t, 5*time.Second, func() bool {
		ka.mu.Lock()
		defer ka.mu.Unlock()
		return ka.inFlight[agent] == nil
	}, "advise call still in flight")
}

func TestAdviseKnowledge_AsyncOneCallInFlight(t *testing.T) {
	adv := newBlockingAdviser(map[string][]crux.KnowledgeAdvice{
		"src/a": {{Slug: "a1", Title: "A1"}},
		"src/b": {{Slug: "b1", Title: "B1"}},
	})
	m, a, typed := advisorManager(t, adv)
	fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))
	m.adviseKnowledge(a, "", workingPane) // baseline

	// The adviser blocks far past the 3 s poll interval; the tick must not.
	start := time.Now()
	m.adviseKnowledge(a, "⏺ Read(src/a/x.go)", workingPane)
	if elapsed := time.Since(start); elapsed > knowledgeAdvisorAdviseGrace+time.Second {
		t.Fatalf("tick blocked on Advise for %v", elapsed)
	}
	// A second scope while the first call runs is queued, not called.
	m.adviseKnowledge(a, "⏺ Read(src/a/x.go)\n⏺ Read(src/b/y.go)", workingPane)
	if asked, _ := adv.snapshot(); !reflect.DeepEqual(asked, []string{"src/a"}) {
		t.Fatalf("asked = %v, want only src/a in flight", asked)
	}
	if len(*typed) != 0 {
		t.Fatalf("typed before any result: %v", *typed)
	}

	adv.release <- struct{}{}
	waitKnowledgeIdle(t, m, a.Name)
	// The held result goes out on the next tick, which also starts src/b.
	m.adviseKnowledge(a, "⏺ Read(src/a/x.go)\n⏺ Read(src/b/y.go)", workingPane)
	if len(*typed) != 1 || (*typed)[0] != "📚 Knowledge: A1" {
		t.Fatalf("typed = %v", *typed)
	}
	adv.release <- struct{}{}
	waitKnowledgeIdle(t, m, a.Name)
	asked, maxActive := adv.snapshot()
	if !reflect.DeepEqual(asked, []string{"src/a", "src/b"}) {
		t.Errorf("asked = %v", asked)
	}
	if maxActive != 1 {
		t.Errorf("%d Advise calls ran at once, want 1", maxActive)
	}
}

func TestAdviseKnowledge_QueueNewestFirstCappedAtAllowance(t *testing.T) {
	adv := newBlockingAdviser(nil)
	m, a, _ := advisorManager(t, adv)
	fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))
	m.adviseKnowledge(a, "", workingPane)
	m.adviseKnowledge(a, "⏺ Read(src/a/x.go)", workingPane) // in flight
	m.adviseKnowledge(a, "⏺ Read(src/a/x.go)\n⏺ Read(src/b/x.go)\n⏺ Read(src/c/x.go)\n⏺ Read(src/d/x.go)", workingPane)

	ka := m.knowledgeAdvisor.Load()
	ka.mu.Lock()
	var queued []string
	for _, sc := range ka.agents[a.Name].queue {
		queued = append(queued, sc.Key)
	}
	ka.mu.Unlock()
	// MaxPerKick is 2: the two newest scopes wait, newest first.
	if want := []string{"src/d", "src/c"}; !reflect.DeepEqual(queued, want) {
		t.Errorf("queue = %v, want %v", queued, want)
	}
	for i := 0; i < 3; i++ {
		adv.release <- struct{}{}
	}
	waitKnowledgeIdle(t, m, a.Name)
}

func TestAdviseKnowledge_ResultAfterNewKickDropped(t *testing.T) {
	adv := newBlockingAdviser(map[string][]crux.KnowledgeAdvice{
		"src/a": {{Slug: "a1", Title: "A1"}},
	})
	m, a, typed := advisorManager(t, adv)
	fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))
	m.adviseKnowledge(a, "", workingPane)
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane) // in flight

	later := a.LastKick.Add(time.Hour)
	a.LastKick = &later
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane) // new kick's baseline
	adv.release <- struct{}{}
	waitKnowledgeIdle(t, m, a.Name)
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
	if len(*typed) != 0 {
		t.Fatalf("advice from the previous kick was typed: %v", *typed)
	}
}

func TestObservedKnowledgeScopesAction(t *testing.T) {
	pane := strings.Join([]string{
		"⏺ Read(src/crux/agent/manager.go)",
		"● Edit src/pkg/config/config.go",
		"⏺ Read(src/pkg/config/load.go)",
		"⏺ Write(src/docs/new/page.md)",
		"$ cat src/scripts/check.sh",
		"$ gh pr checks 17153",
	}, "\n")
	got := map[string]string{}
	for _, sc := range observedKnowledgeScopes(pane) {
		got[sc.Key] = sc.Action
	}
	want := map[string]string{
		"src/crux/agent": crux.KnowledgeActionRead,
		"src/pkg/config": crux.KnowledgeActionEdit,
		"src/docs/new":   crux.KnowledgeActionEdit,
		"src/scripts":    crux.KnowledgeActionRun,
		"gh:pr":          crux.KnowledgeActionRun,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("actions = %v, want %v", got, want)
	}
	// A later edit upgrades a scope first seen read.
	if sc := observedKnowledgeScopes("⏺ Read(src/a/x.go)\n⏺ Update(src/a/y.go)")[0]; sc.Action != crux.KnowledgeActionEdit {
		t.Errorf("read then update = %q, want edit", sc.Action)
	}
}

func TestAdviseKnowledge_ReadviseOncePerScope(t *testing.T) {
	adv := &fakeAdviser{policy: enabledPolicy()}
	m, a, _ := advisorManager(t, adv)
	now := fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))
	m.adviseKnowledge(a, "", workingPane)

	pane := "⏺ Read(src/a/x.go)"
	m.adviseKnowledge(a, pane, workingPane)
	// Time alone never makes a scope eligible again.
	*now = now.Add(time.Hour)
	m.adviseKnowledge(a, pane, workingPane)
	pane += "\n● Edit src/a/x.go"
	m.adviseKnowledge(a, pane, workingPane)
	pane += "\n⏺ Write(src/a/z.go)"
	m.adviseKnowledge(a, pane, workingPane)

	pane += "\n$ gh pr view 1"
	m.adviseKnowledge(a, pane, workingPane)
	m.adviseKnowledge(a, pane, workingPane)
	pane += "\n$ gh pr checks 1"
	m.adviseKnowledge(a, pane, workingPane)
	pane += "\n$ gh pr merge 1"
	m.adviseKnowledge(a, pane, workingPane)

	var got []string
	for _, sc := range adv.scopes {
		got = append(got, sc.Key+"/"+sc.Action)
	}
	want := []string{"src/a/read", "src/a/edit", "gh:pr/run", "gh:pr/run"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("advised = %v, want %v", got, want)
	}
}

func TestAdviseKnowledge_PaneTailOnlyWhenWanted(t *testing.T) {
	adv := &fakeAdviser{policy: enabledPolicy()}
	m, a, _ := advisorManager(t, adv)
	fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))
	m.adviseKnowledge(a, "", workingPane)
	m.adviseKnowledge(a, "⏺ Read(src/a/x.go)\n$ gh pr view 1", workingPane)
	if len(adv.scopes) != 2 {
		t.Fatalf("asked %v", adv.asked)
	}
	for _, sc := range adv.scopes {
		if sc.PaneTail != "" {
			t.Errorf("scope %s carried pane text without WantsPaneTail: %q", sc.Key, sc.PaneTail)
		}
	}
}

func TestAdviseKnowledge_PaneTailScrubbedAndCapped(t *testing.T) {
	adv := &fakeAdviser{policy: enabledPolicy()}
	adv.policy.WantsPaneTail = true
	adv.policy.PaneLines = 3
	m, a, _ := advisorManager(t, adv)
	fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))
	m.adviseKnowledge(a, "", workingPane)

	agentKey := "sk-hive-" + a.Name
	ghToken := "ghp_" + strings.Repeat("A1b2", 9)
	pane := strings.Join([]string{
		"first line falls off the tail",
		"$ export ANTHROPIC_API_KEY='" + agentKey + "'",
		"$ GH_TOKEN=" + ghToken + " gh pr view 1",
		"⏺ Read(src/a/x.go)",
	}, "\n") + "\n"
	m.adviseKnowledge(a, pane, workingPane)
	if len(adv.scopes) == 0 {
		t.Fatal("adviser not asked")
	}
	for _, sc := range adv.scopes {
		tail := sc.PaneTail
		if strings.Contains(tail, agentKey) || strings.Contains(tail, ghToken) {
			t.Errorf("scope %s pane tail leaks a token: %q", sc.Key, tail)
		}
		if logscrub.RedactPaneTokens(tail) != tail {
			t.Errorf("scope %s pane tail is not scrubbed: %q", sc.Key, tail)
		}
		if strings.Contains(tail, "first line") || strings.Count(tail, "\n") != 2 {
			t.Errorf("scope %s pane tail not capped at 3 lines: %q", sc.Key, tail)
		}
		if !strings.Contains(tail, "⏺ Read(src/a/x.go)") {
			t.Errorf("scope %s pane tail lost the work line: %q", sc.Key, tail)
		}
	}
}

func TestKnowledgePaneTailDefaultLines(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, "line")
	}
	if got := strings.Count(knowledgePaneTail(strings.Join(lines, "\n"), 0), "\n") + 1; got != knowledgeAdvisorDefaultPaneLines {
		t.Errorf("default tail is %d lines, want %d", got, knowledgeAdvisorDefaultPaneLines)
	}
}

func TestAdviseKnowledge_AttachedHumanHoldsAdvice(t *testing.T) {
	adv := &fakeAdviser{policy: enabledPolicy(), byKey: map[string][]crux.KnowledgeAdvice{
		"src/a": {{Slug: "a1", Title: "A1"}},
	}}
	m, a, typed := advisorManager(t, adv)
	fakeClock(m, time.Date(2026, 11, 7, 12, 1, 0, 0, time.UTC))
	m.adviseKnowledge(a, "", workingPane)
	termSeams(m).sessionAttached = func(*AgentProcess) bool { return true }
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
	termSeams(m).sessionAttached = func(*AgentProcess) bool { return false }
	m.adviseKnowledge(a, "⏺ Edit(src/a/x.go)", workingPane)
	if len(*typed) != 1 || (*typed)[0] != "📚 Knowledge: A1" {
		t.Fatalf("held advice not delivered after the human left: %v", *typed)
	}
}
