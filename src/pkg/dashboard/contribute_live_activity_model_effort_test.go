package dashboard

import (
	"encoding/json"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestActivityEntry_EffortJSONSerialization(t *testing.T) {
	entry := ActivityEntry{
		Username: "alice",
		Action:   "picked up",
		Role:     "contributor",
		CLI:      "codex",
		Model:    "gpt-5.6-terra",
		Effort:   "high",
		Task:     "task-123",
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal ActivityEntry failed: %v", err)
	}
	if !strings.Contains(string(raw), `"effort":"high"`) {
		t.Errorf("expected effort in JSON, got %s", string(raw))
	}

	var decoded ActivityEntry
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal ActivityEntry failed: %v", err)
	}
	if decoded.Effort != "high" || decoded.CLI != "codex" || decoded.Model != "gpt-5.6-terra" {
		t.Errorf("decoded = %+v, want effort=high, cli=codex, model=gpt-5.6-terra", decoded)
	}

	// Empty effort should be omitted from JSON
	entryNoEffort := ActivityEntry{
		Username: "bob",
		Action:   "joined",
		CLI:      "bob",
	}
	rawNoEffort, err := json.Marshal(entryNoEffort)
	if err != nil {
		t.Fatalf("marshal ActivityEntry without effort failed: %v", err)
	}
	if strings.Contains(string(rawNoEffort), `"effort"`) {
		t.Errorf("empty effort should be omitted, got %s", string(rawNoEffort))
	}
}

func TestContributeWSHub_AddActivity_EffortStored(t *testing.T) {
	hub := &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         slog.Default(),
	}
	hub.addActivity("alice", "picked up", "contributor", "codex", "gpt-5.6-terra", "high", "task-1")

	acts := hub.RecentActivity()
	if len(acts) != 1 {
		t.Fatalf("expected 1 activity, got %d", len(acts))
	}
	if acts[0].Effort != "high" || acts[0].CLI != "codex" || acts[0].Model != "gpt-5.6-terra" {
		t.Errorf("activity entry = %+v, want effort=high, cli=codex, model=gpt-5.6-terra", acts[0])
	}
}

func TestContributePage_LoadoutFormattingPresence(t *testing.T) {
	body := renderContributePage(t)

	// ccFormatLoadout function must exist and be exported on window
	if !strings.Contains(body, "function ccFormatLoadout(e,cls){") {
		t.Error("ccFormatLoadout helper is missing from /contribute page")
	}
	if !strings.Contains(body, "window.ccFormatLoadout=ccFormatLoadout;") {
		t.Error("ccFormatLoadout is not published on window")
	}

	// ccNarrate must format loadout and append to all event types
	if !strings.Contains(body, "var loadout=ccFormatLoadout(e,'ref');") {
		t.Error("ccNarrate does not compute loadout via ccFormatLoadout")
	}
	if !strings.Contains(body, "return {ic:ic,body:body+loadout,ts:e.timestamp};") {
		t.Error("ccNarrate does not append loadout to body")
	}

	// Onboarding activity feed must use the SHARED formatter and escape
	// username/task/role. It reaches both through window-resolved locals rather
	// than bare cross-IIFE identifiers — see
	// TestOnboardingFeed_NoBareCrossIIFEReference for why that matters.
	if !strings.Contains(body, "const cliModel=feedLoadout(e,'feed-cli');") {
		t.Error("onboarding activity feed does not use the shared loadout formatter")
	}
	if !strings.Contains(body, "<b>'+feedEsc(e.username)+'</b>") {
		t.Error("onboarding activity feed does not escape username")
	}
	if !strings.Contains(body, "feedEsc(e.task)") || !strings.Contains(body, "feedEsc(e.role)") {
		t.Error("onboarding activity feed does not escape task/role")
	}
}

func TestFleetClanker_ReasoningEffortJSONSerialization(t *testing.T) {
	clanker := FleetClanker{
		ContributorID:   "c1",
		CLIBackend:      "codex",
		Model:           "gpt-5.6-terra",
		ReasoningEffort: "high",
	}
	raw, err := json.Marshal(clanker)
	if err != nil {
		t.Fatalf("marshal FleetClanker failed: %v", err)
	}
	if !strings.Contains(string(raw), `"reasoning_effort":"high"`) {
		t.Errorf("expected reasoning_effort in FleetClanker JSON, got %s", string(raw))
	}
}

// extractJSFunc pulls one top-level JS function out of the rendered page by
// brace-matching from its declaration, so the test exercises the SHIPPED source
// rather than a copy that can drift away from it.
func extractJSFunc(t *testing.T, page, decl string) string {
	t.Helper()
	start := strings.Index(page, decl)
	if start < 0 {
		t.Fatalf("could not find %q in the rendered page", decl)
	}
	depth := 0
	for i := start; i < len(page); i++ {
		switch page[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return page[start : i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces while extracting %q", decl)
	return ""
}

// TestCCFormatLoadout_Behaviour runs the REAL ccFormatLoadout against the cases
// #4084 specified. The sibling presence test only asserts that certain source
// literals exist, which cannot catch a formatter that renders the wrong string —
// notably dropping the effort, or emitting a bare "()" / dangling "with".
func TestCCFormatLoadout_Behaviour(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed; the presence assertions elsewhere still ran")
	}
	page := renderContributePage(t)

	script := extractJSFunc(t, page, "function esc(s){") + "\n" +
		extractJSFunc(t, page, "function ccFormatLoadout(e,cls){") + "\n" +
		`const cases=[
			{cli:'codex',model:'gpt-5.6-terra',effort:'high'},
			{cli:'codex',model:'gpt-5.6-terra'},
			{cli:'codex',effort:'high'},
			{cli:'bob'},
			{},
			null,
			{cli:'<img src=x onerror=alert(1)>',model:'a"b',effort:"c'd"}
		];
		console.log(JSON.stringify(cases.map(function(c){return ccFormatLoadout(c,'feed-cli');})));`

	cmd := exec.Command("node", "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	var got []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("could not decode node output %q: %v", out, err)
	}

	want := []string{
		` <span class="feed-cli">via codex CLI with gpt-5.6-terra (high)</span>`,
		` <span class="feed-cli">via codex CLI with gpt-5.6-terra</span>`,
		// Effort without a model must still render: a codex contributor can set
		// AGENT_REASONING_EFFORT without AGENT_MODEL, and dropping it here would
		// lose exactly the value this feature exists to surface.
		` <span class="feed-cli">via codex CLI (high)</span>`,
		` <span class="feed-cli">via bob CLI</span>`,
		// No backend: nothing honest to say, so say nothing.
		``,
		``,
		` <span class="feed-cli">via &lt;img src=x onerror=alert(1)&gt; CLI with a&quot;b (c&#39;d)</span>`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d results, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("case %d:\n got  %q\n want %q", i, got[i], want[i])
		}
	}

	for _, g := range got {
		if strings.Contains(g, "()") {
			t.Errorf("a bare empty parenthesis was rendered: %q", g)
		}
		if strings.HasSuffix(strings.TrimSuffix(g, "</span>"), " with ") {
			t.Errorf("a dangling 'with' was rendered: %q", g)
		}
	}
}

// TestOnboardingFeed_NoBareCrossIIFEReference guards the failure this page has
// already had three times (#2603/#2604/#2606): the Onboarding feed lives in its
// own script block, so a BARE reference to esc/ccFormatLoadout — which are
// declared inside the Operations IIFE — throws ReferenceError the moment that
// IIFE fails before its window.* republish. poll() swallows every throw, so the
// symptom is a feed that silently stops updating.
func TestOnboardingFeed_NoBareCrossIIFEReference(t *testing.T) {
	body := renderContributePage(t)

	if !strings.Contains(body, "const feedLoadout=(typeof window!=='undefined'&&window.ccFormatLoadout)||function(){return '';};") {
		t.Error("the onboarding feed must resolve ccFormatLoadout through window with a local fallback")
	}
	if !strings.Contains(body, "const feedEsc=(typeof window!=='undefined'&&window.esc)||function(s){") {
		t.Error("the onboarding feed must resolve esc through window with a local fallback")
	}
	// The throwing shape: window.X||X, where the right operand is the very
	// identifier that is out of scope. It can never act as a fallback.
	if strings.Contains(body, "(window.ccFormatLoadout||ccFormatLoadout)") {
		t.Error("(window.ccFormatLoadout||ccFormatLoadout) throws ReferenceError instead of falling back")
	}
	// The fallback escaper must be a real escaper — falling back to a
	// pass-through would turn a rendering failure into an injection bug.
	if !strings.Contains(body, `.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')`) {
		t.Error("the onboarding feed's fallback escaper must actually escape")
	}
}
