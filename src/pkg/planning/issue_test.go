package planning

import (
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
)

// TestEpicFromIssue_CreateError exercises the persistence error path (the epic
// create's WriteFile fails) by making the store dir read-only. Skipped as root,
// which bypasses filesystem permission checks.
func TestEpicFromIssue_CreateError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; filesystem permission enforcement is bypassed")
	}
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	_, err = EpicFromIssue(store, github.Issue{Repo: "a/b", Number: 1, Title: "t"}, "body")
	if err == nil {
		t.Fatal("want persistence error when store dir is read-only")
	}
	if !strings.Contains(err.Error(), "creating epic") {
		t.Errorf("error = %v, want creating-epic wrap", err)
	}
}

func TestIssueRef(t *testing.T) {
	cases := []struct {
		name  string
		issue github.Issue
		want  string
	}{
		{"repo+number", github.Issue{Repo: "kubestellar/hive", Number: 42}, "gh-kubestellar/hive#42"},
		{"number zero falls to url", github.Issue{Repo: "kubestellar/hive", Number: 0, URL: "https://x/1"}, "https://x/1"},
		{"no repo falls to url", github.Issue{Number: 42, URL: "https://x/2"}, "https://x/2"},
		{"nothing", github.Issue{}, ""},
		{"whitespace repo", github.Issue{Repo: "  ", Number: 5, URL: "https://x/3"}, "https://x/3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IssueRef(tc.issue); got != tc.want {
				t.Errorf("IssueRef = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEpicFromIssue_CreatesEpic(t *testing.T) {
	store := newStore(t)
	issue := github.Issue{
		Repo:   "kubestellar/hive",
		Number: 7,
		Title:  "Add planning intelligence phase 4",
		URL:    "https://github.com/hivecommons/hive/issues/7",
		Labels: []string{"enhancement", "plan"},
	}

	epic, err := EpicFromIssue(store, issue, "Detailed body describing the work.")
	if err != nil {
		t.Fatalf("EpicFromIssue: %v", err)
	}
	if epic.Type != beads.TypeEpic {
		t.Errorf("type = %q, want epic", epic.Type)
	}
	if epic.Title != issue.Title {
		t.Errorf("title = %q", epic.Title)
	}
	if epic.ExternalRef != "gh-kubestellar/hive#7" {
		t.Errorf("external ref = %q", epic.ExternalRef)
	}
	if epic.Notes != "Detailed body describing the work." {
		t.Errorf("notes = %q", epic.Notes)
	}
	if epic.Meta(MetaSource) != SourceGitHubIssue {
		t.Errorf("source meta = %q", epic.Meta(MetaSource))
	}
	if epic.Meta(MetaIssueRepo) != "kubestellar/hive" {
		t.Errorf("issue_repo = %q", epic.Meta(MetaIssueRepo))
	}
	if epic.Meta(MetaIssueNumber) != "7" {
		t.Errorf("issue_number = %q", epic.Meta(MetaIssueNumber))
	}
	if epic.Meta(MetaIssueURL) != issue.URL {
		t.Errorf("issue_url = %q", epic.Meta(MetaIssueURL))
	}
	if epic.Meta(MetaIssueLabels) != "enhancement,plan" {
		t.Errorf("labels = %q", epic.Meta(MetaIssueLabels))
	}
	// Accepted-but-not-built state: draft + decompose_pending, no children yet.
	if epic.Meta(MetaPlanStatus) != PlanStatusDraft {
		t.Errorf("plan_status = %q, want draft", epic.Meta(MetaPlanStatus))
	}
	if !DecomposePending(epic) {
		t.Error("fresh issue-sourced epic should be decompose_pending")
	}
	// The epic must be decomposable by BuildPrompt (body flows through).
	if body := epicBody(epic); body == "" {
		t.Error("epicBody empty; BuildPrompt would lack DETAILS")
	}
}

func TestEpicFromIssue_Idempotent(t *testing.T) {
	store := newStore(t)
	issue := github.Issue{Repo: "org/repo", Number: 3, Title: "Idempotent epic"}

	first, err := EpicFromIssue(store, issue, "")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := EpicFromIssue(store, issue, "different body")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected same epic id, got %s and %s (duplicate!)", first.ID, second.ID)
	}
	// Only ONE epic must exist for this ref.
	all := store.List(beads.ListFilter{})
	epics := 0
	for _, b := range all {
		if b.Type == beads.TypeEpic {
			epics++
		}
	}
	if epics != 1 {
		t.Fatalf("expected exactly 1 epic, got %d", epics)
	}
}

func TestEpicFromIssue_UrlOnlyRef(t *testing.T) {
	store := newStore(t)
	issue := github.Issue{Title: "no repo/number", URL: "https://example/issues/99"}
	epic, err := EpicFromIssue(store, issue, "")
	if err != nil {
		t.Fatalf("EpicFromIssue: %v", err)
	}
	if epic.ExternalRef != "https://example/issues/99" {
		t.Errorf("ref = %q", epic.ExternalRef)
	}
	if epic.Meta(MetaIssueNumber) != "" {
		t.Errorf("issue_number should be empty when number is 0, got %q", epic.Meta(MetaIssueNumber))
	}
}

func TestEpicFromIssue_Errors(t *testing.T) {
	store := newStore(t)
	cases := []struct {
		name  string
		store *beads.Store
		issue github.Issue
	}{
		{"nil store", nil, github.Issue{Repo: "a/b", Number: 1, Title: "t"}},
		{"empty title", store, github.Issue{Repo: "a/b", Number: 1}},
		{"no ref", store, github.Issue{Title: "has title but no ref"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EpicFromIssue(tc.store, tc.issue, ""); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestHasPlanLabel(t *testing.T) {
	cases := []struct {
		labels []string
		want   bool
	}{
		{[]string{"plan"}, true},
		{[]string{"epic"}, true},
		{[]string{"Plan"}, true},    // case-insensitive
		{[]string{"  EPIC "}, true}, // trimmed + case-insensitive
		{[]string{"bug", "epic"}, true},
		{[]string{"enhancement"}, false},
		{nil, false},
		{[]string{}, false},
	}
	for _, tc := range cases {
		got := HasPlanLabel(github.Issue{Labels: tc.labels})
		if got != tc.want {
			t.Errorf("HasPlanLabel(%v) = %v, want %v", tc.labels, got, tc.want)
		}
	}
}

func TestDecomposePendingAndClear(t *testing.T) {
	store := newStore(t)
	epic, err := EpicFromIssue(store, github.Issue{Repo: "a/b", Number: 1, Title: "t"}, "")
	if err != nil {
		t.Fatalf("EpicFromIssue: %v", err)
	}
	if !DecomposePending(epic) {
		t.Fatal("issue-sourced epic should be pending")
	}
	if DecomposePending(nil) {
		t.Error("nil epic should not be pending")
	}

	if err := ClearDecomposePending(store, epic.ID); err != nil {
		t.Fatalf("ClearDecomposePending: %v", err)
	}
	got, _ := store.Get(epic.ID)
	if DecomposePending(got) {
		t.Error("epic should not be pending after clear")
	}
}

func TestDecomposeFromOutput_ClearsPending(t *testing.T) {
	store := newStore(t)
	epic, err := EpicFromIssue(store, github.Issue{Repo: "a/b", Number: 2, Title: "t"}, "")
	if err != nil {
		t.Fatalf("EpicFromIssue: %v", err)
	}
	// Re-read so the in-memory epic carries the pending marker.
	epic, _ = store.Get(epic.ID)
	if _, err := DecomposeFromOutput(store, epic, "1. [T1] a [agent_suitable]\n", Options{}); err != nil {
		t.Fatalf("DecomposeFromOutput: %v", err)
	}
	got, _ := store.Get(epic.ID)
	if DecomposePending(got) {
		t.Error("decompose_pending should be cleared once children are materialized")
	}
}

// fakeDecomposeKicker records SendKick calls and reports a configurable paused
// state, so RequestDecompose can be tested without a live agent.Manager.
type fakeDecomposeKicker struct {
	paused  bool
	kickErr error
	kicks   int
	lastMsg string
}

func (f *fakeDecomposeKicker) IsPaused(string) bool { return f.paused }
func (f *fakeDecomposeKicker) SendKick(_, message string) error {
	f.kicks++
	f.lastMsg = message
	return f.kickErr
}

// recordingSink captures LabelPlanSink callbacks.
type recordingSink struct {
	kicked       []string
	queuedPaused []string
	queuedNo     []string
}

func (s *recordingSink) KickedPlan(epic *beads.Bead) { s.kicked = append(s.kicked, epic.ID) }
func (s *recordingSink) QueuedPlan(epic *beads.Bead, paused bool) {
	if paused {
		s.queuedPaused = append(s.queuedPaused, epic.ID)
	} else {
		s.queuedNo = append(s.queuedNo, epic.ID)
	}
}

func TestPlanIssuesFromLabels_MintsKicksAndSkips(t *testing.T) {
	store := newStore(t)
	issues := []github.Issue{
		{Repo: "a/b", Number: 1, Title: "labeled plan", Labels: []string{"plan"}},
		{Repo: "a/b", Number: 2, Title: "no label", Labels: []string{"bug"}}, // skipped
		{Repo: "a/b", Number: 3, Title: "labeled epic", Labels: []string{"epic"}},
	}
	sink := &recordingSink{}
	res := PlanIssuesFromLabels(store, &fakeDecomposeKicker{}, issues, sink, nil, PlanningMinACMMLevel)

	if res.Minted != 2 || res.Kicked != 2 {
		t.Fatalf("res = %+v, want Minted=2 Kicked=2", res)
	}
	if len(sink.kicked) != 2 {
		t.Errorf("expected 2 kicked plans, got %d", len(sink.kicked))
	}
	// Two epics exist, both pending → draft.
	epics := 0
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Type == beads.TypeEpic {
			epics++
		}
	}
	if epics != 2 {
		t.Errorf("expected 2 epics, got %d", epics)
	}
}

func TestPlanIssuesFromLabels_Idempotent(t *testing.T) {
	store := newStore(t)
	issues := []github.Issue{{Repo: "a/b", Number: 1, Title: "once", Labels: []string{"plan"}}}
	sink := &recordingSink{}

	first := PlanIssuesFromLabels(store, &fakeDecomposeKicker{}, issues, sink, nil, PlanningMinACMMLevel)
	if first.Minted != 1 {
		t.Fatalf("first Minted = %d, want 1", first.Minted)
	}
	// Second pass: epic already exists AND is still pending (kicker did nothing
	// real), so it is NOT minted again but IS re-kicked (still pending).
	second := PlanIssuesFromLabels(store, &fakeDecomposeKicker{}, issues, sink, nil, PlanningMinACMMLevel)
	if second.Minted != 0 {
		t.Errorf("second Minted = %d, want 0 (no duplicate)", second.Minted)
	}
}

func TestPlanIssuesFromLabels_PausedQueues(t *testing.T) {
	store := newStore(t)
	issues := []github.Issue{{Repo: "a/b", Number: 1, Title: "plan me", Labels: []string{"plan"}}}
	sink := &recordingSink{}
	res := PlanIssuesFromLabels(store, &fakeDecomposeKicker{paused: true}, issues, sink, nil, PlanningMinACMMLevel)

	if res.Kicked != 0 || res.QueuedPaused != 1 {
		t.Fatalf("res = %+v, want Kicked=0 QueuedPaused=1", res)
	}
	if len(sink.queuedPaused) != 1 {
		t.Errorf("expected 1 paused-queued plan, got %d", len(sink.queuedPaused))
	}
	// The epic is minted and still pending (queued, not dropped).
	epic := store.FindByExternalRef("gh-a/b#1")
	if epic == nil || !DecomposePending(epic) {
		t.Error("paused plan should be minted and left pending")
	}
}

func TestPlanIssuesFromLabels_NilStoreAndMintErr(t *testing.T) {
	// Nil store → no-op zero result.
	if res := PlanIssuesFromLabels(nil, &fakeDecomposeKicker{}, nil, nil, nil, PlanningMinACMMLevel); res != (LabelPlanResult{}) {
		t.Errorf("nil store: got %+v, want zero", res)
	}
	// Mint error (issue with a title but no ref) → mintErr callback fires.
	store := newStore(t)
	called := false
	PlanIssuesFromLabels(store,
		&fakeDecomposeKicker{},
		[]github.Issue{{Title: "no ref", Labels: []string{"plan"}}},
		nil,
		func(string, error) { called = true },
		PlanningMinACMMLevel)
	if !called {
		t.Error("expected mintErr callback on unref-able labeled issue")
	}
}

func TestPlanIssuesFromLabels_QueuedNoAgent(t *testing.T) {
	store := newStore(t)
	issues := []github.Issue{{Repo: "a/b", Number: 1, Title: "plan", Labels: []string{"plan"}}}
	sink := &recordingSink{}
	// Kick fails → queued (no agent) path + sink.QueuedPlan(paused=false).
	PlanIssuesFromLabels(store, &fakeDecomposeKicker{kickErr: errFake}, issues, sink, nil, PlanningMinACMMLevel)
	if len(sink.queuedNo) != 1 {
		t.Errorf("expected 1 no-agent-queued plan, got %d", len(sink.queuedNo))
	}
}

func TestPlanningAllowedAtLevel(t *testing.T) {
	tests := []struct {
		level int
		want  bool
	}{
		{1, false}, {2, false}, {3, false}, {4, false}, // architect has no cadence
		{5, true}, {6, true}, // architect scheduled (4h / 15m)
	}
	for _, tc := range tests {
		if got := PlanningAllowedAtLevel(tc.level); got != tc.want {
			t.Errorf("PlanningAllowedAtLevel(%d) = %v, want %v", tc.level, got, tc.want)
		}
	}
}

// TestPlanIssuesFromLabels_LevelGate proves the L5+ gate: below L5 the pass is a
// hard no-op that mints nothing (the architect has no cadence to decompose it);
// at L5/L6 it mints as before. Covers the boundary (L4 blocked, L5 allowed).
func TestPlanIssuesFromLabels_LevelGate(t *testing.T) {
	tests := []struct {
		name       string
		level      int
		wantMinted int
	}{
		{"L1 blocked", 1, 0},
		{"L4 blocked (boundary)", 4, 0},
		{"L5 allowed (boundary)", 5, 1},
		{"L6 allowed", 6, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			issues := []github.Issue{{Repo: "a/b", Number: 1, Title: "plan me", Labels: []string{"plan"}}}
			res := PlanIssuesFromLabels(store, &fakeDecomposeKicker{}, issues, &recordingSink{}, nil, tc.level)
			if res.Minted != tc.wantMinted {
				t.Errorf("level %d: Minted = %d, want %d", tc.level, res.Minted, tc.wantMinted)
			}
			// Below the gate, no epic should exist at all.
			epic := store.FindByExternalRef("gh-a/b#1")
			if tc.wantMinted == 0 && epic != nil {
				t.Errorf("level %d: expected no epic minted, but found %s", tc.level, epic.ID)
			}
			if tc.wantMinted > 0 && epic == nil {
				t.Errorf("level %d: expected an epic minted, found none", tc.level)
			}
		})
	}
}

func TestRequestDecompose(t *testing.T) {
	store := newStore(t)
	epic, _ := EpicFromIssue(store, github.Issue{Repo: "a/b", Number: 3, Title: "plan me"}, "body")

	// nil kicker → queued (no agent).
	if got := RequestDecompose(nil, epic); got != DecomposeQueuedNoAgent {
		t.Errorf("nil kicker: got %q, want queued_no_agent", got)
	}

	// paused architect → queued, NOT kicked (respect the pause).
	paused := &fakeDecomposeKicker{paused: true}
	if got := RequestDecompose(paused, epic); got != DecomposeQueuedPaused {
		t.Errorf("paused: got %q, want queued_paused", got)
	}
	if paused.kicks != 0 {
		t.Error("paused architect must NOT be kicked")
	}

	// available architect → kicked with the decomposition prompt.
	ok := &fakeDecomposeKicker{}
	if got := RequestDecompose(ok, epic); got != DecomposeKicked {
		t.Errorf("available: got %q, want kicked", got)
	}
	if ok.kicks != 1 || ok.lastMsg == "" {
		t.Errorf("expected one kick with a prompt, got %d msg=%q", ok.kicks, ok.lastMsg)
	}

	// kick error → queued (no agent).
	failing := &fakeDecomposeKicker{kickErr: errFake}
	if got := RequestDecompose(failing, epic); got != DecomposeQueuedNoAgent {
		t.Errorf("kick error: got %q, want queued_no_agent", got)
	}
}
