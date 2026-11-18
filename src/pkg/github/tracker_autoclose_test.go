package github

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestParseTrackerTasks(t *testing.T) {
	body := "Intro #99\n- [ ] #11 first\n* [x] acme/other#12 done\n- [ ] no reference here\n- [ ] see issue#5 not a ref\n  - [ ] nested #13\n"
	got := parseTrackerTasks(body)
	if len(got) != 5 {
		t.Fatalf("items = %+v", got)
	}
	if !got[0].HasRef || got[0].Ref != (ChildRef{Number: 11}) || got[0].Checked {
		t.Errorf("item0 = %+v", got[0])
	}
	if !got[1].HasRef || got[1].Ref != (ChildRef{Repo: "acme/other", Number: 12}) || !got[1].Checked {
		t.Errorf("item1 = %+v", got[1])
	}
	if got[2].HasRef || got[3].HasRef {
		t.Errorf("lines without a bare or repo-qualified ref must not carry one: %+v %+v", got[2], got[3])
	}
	if !got[4].HasRef || got[4].Ref.Number != 13 {
		t.Errorf("item4 = %+v", got[4])
	}
}

func TestParseCloseWhen(t *testing.T) {
	body := "- [ ] #1 outside\n\n## close_when\n\n- [x] ship docs\n- [ ] #2\n\nMore prose\n- [ ] #3 after prose\n"
	items, ok := parseCloseWhen(body)
	if !ok || len(items) != 2 {
		t.Fatalf("ok=%v items=%+v", ok, items)
	}
	if !items[0].Checked || items[0].HasRef || items[1].Ref.Number != 2 {
		t.Errorf("items = %+v", items)
	}
	if _, ok := parseCloseWhen("- [ ] #1\n"); ok {
		t.Error("a body without a close_when marker must not report one")
	}
	if items, ok := parseCloseWhen("close_when:\n- [ ] #4"); !ok || len(items) != 1 {
		t.Errorf("close_when: line form = %+v %v", items, ok)
	}
}

func TestTrackerSignal(t *testing.T) {
	cases := []struct {
		name   string
		title  string
		labels []string
		body   string
		want   bool
	}{
		{"plain issue with Refs PRs only", "Betty on Zoom without a Zoom app", []string{"help wanted"}, "design text", false},
		{"tracker label", "x", []string{"Tracker"}, "", true},
		{"audit label", "x", []string{"audit"}, "", true},
		{"umbrella title prefix", "[Umbrella] fail-closed defaults", nil, "", true},
		{"task list of two references", "x", nil, "- [ ] #4\n- [ ] #5", true},
		{"a single reference is a blocked-on note", "x", nil, "- [ ] #4", false},
		{"task list without a reference", "x", nil, "- [ ] write docs", false},
		{"close_when", "x", nil, "close_when:\n- [x] done", true},
	}
	for _, tc := range cases {
		if got := trackerSignal(tc.title, tc.labels, tc.body); got != tc.want {
			t.Errorf("%s: signal = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTrackerRefersTo(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"Refs #7", true},
		{"refs: #7", true},
		{"Part of #7", true},
		{"Fixes acme/widgets#7", true},
		{"Refs #70", false},
		{"Refs other/repo#7", false},
		{"mentions #7 in passing", false},
	}
	for _, tc := range cases {
		if got := trackerRefersTo(tc.body, "acme/widgets", 7); got != tc.want {
			t.Errorf("%q: refers = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func resetTrackerReadsForTest(t *testing.T) {
	t.Helper()
	clear := func() {
		trackerMu.Lock()
		trackerReads = map[string]trackerReadRecord{}
		trackerMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
	resetParentReopenCacheForTest(t)
}

type trackerCase struct {
	title  string
	labels []string
	body   string
	policy string
	// closedRefs are referenced issues/PRs that are closed.
	closedRefs []int
	// prBodies are referencing PR bodies; every key also gets a
	// cross-referenced timeline event on the tracker.
	prBodies map[int]string
	extra    []map[string]any
}

// runTrackerSweeps sweeps tracker #7 at stageNow() and again one settle
// interval later, and returns whether it closed after each sweep and the
// evidence comments posted.
func runTrackerSweeps(t *testing.T, tc trackerCase) (afterFirst, afterSecond bool, comments []string) {
	t.Helper()
	resetTrackerReadsForTest(t)
	fake := newIssueStageServer(t)
	c := NewClientForTest(fake.srv.URL, "acme", []string{"widgets"}, testLogger())
	policy := tc.policy
	c.SetIssueStageLabels(func() IssueStagePolicy {
		return IssueStagePolicy{Enabled: true, ParentAutoClose: func(string) string { return policy }}
	})
	store := stagelabels.NewStore("", stageNow)
	store.Observe(stagelabels.Key("acme/widgets", 7), stagelabels.Actionable, "actionable")
	fake.mu.Lock()
	fake.bodies[7] = tc.body
	for _, n := range tc.closedRefs {
		fake.closed[n] = "completed"
	}
	var events []map[string]any
	for pr, body := range tc.prBodies {
		fake.bodies[pr] = body
		events = append(events, map[string]any{
			"event":      "cross-referenced",
			"created_at": "2026-10-01T00:00:00Z",
			"source": map[string]any{"type": "issue", "issue": map[string]any{
				"number": pr, "pull_request": map[string]any{"url": "https://api.example/pulls"},
				"repository": map[string]any{"full_name": "acme/widgets"},
			}},
		})
	}
	fake.timelines = map[int][]map[string]any{7: append(events, tc.extra...)}
	fake.mu.Unlock()

	build := func() *ActionableResult {
		result := &ActionableResult{}
		result.Issues = IssueResultFromItems([]Issue{{Repo: "acme/widgets", Number: 7, Title: tc.title, Body: tc.body,
			Labels: append([]string{stagelabels.Actionable.Label()}, tc.labels...)}})
		return result
	}
	syncSeeded(t, fake, c, build(), store, stageNow())
	fake.mu.Lock()
	_, afterFirst = fake.closed[7]
	fake.mu.Unlock()
	syncSeeded(t, fake, c, build(), store, stageNow().Add(trackerSettleInterval+time.Minute))
	fake.mu.Lock()
	defer fake.mu.Unlock()
	_, afterSecond = fake.closed[7]
	return afterFirst, afterSecond, append([]string(nil), fake.commentBodies[7]...)
}

func TestTrackerAutoClose_AllReferencesDoneClosesAfterSettleWithEvidence(t *testing.T) {
	first, second, comments := runTrackerSweeps(t, trackerCase{
		title: "x", labels: []string{"umbrella"}, body: "- [ ] #11\n- [ ] #12\n", policy: "trackers_too",
		closedRefs: []int{11, 12},
	})
	if first {
		t.Fatal("the first read must only record the evidence, not close")
	}
	if !second {
		t.Fatal("the same finished evidence on a second read must close the tracker")
	}
	if len(comments) != 1 || !strings.Contains(comments[0], parentAutoCloseMarker) ||
		!strings.Contains(comments[0], "#11") || !strings.Contains(comments[0], "#12") || !strings.Contains(comments[0], "closed") {
		t.Fatalf("evidence comment = %q", comments)
	}
}

func TestTrackerAutoClose_OneOpenReferenceKeepsItOpen(t *testing.T) {
	_, second, comments := runTrackerSweeps(t, trackerCase{
		title: "x", labels: []string{"umbrella"}, body: "- [ ] #11\n- [ ] #12\n", policy: "trackers_too",
		closedRefs: []int{11},
	})
	if second || len(comments) != 0 {
		t.Fatalf("an open reference must keep the tracker open: closed=%v comments=%q", second, comments)
	}
}

func TestTrackerAutoClose_DefaultAndEpicsPoliciesNeverCloseTrackers(t *testing.T) {
	for _, policy := range []string{"true", "epics_too", "false", ""} {
		_, second, _ := runTrackerSweeps(t, trackerCase{
			title: "[Umbrella] x", body: "- [ ] #11\n", policy: policy, closedRefs: []int{11},
		})
		if second {
			t.Fatalf("policy %q must not close a tracker", policy)
		}
	}
}

func TestTrackerAutoClose_TrackingIssueNeverCloses(t *testing.T) {
	for name, tc := range map[string]trackerCase{
		"label":   {title: "[Umbrella] x", labels: []string{"hive/tracking"}, body: "- [ ] #11\n"},
		"marker":  {title: "[Umbrella] x", body: "<!-- hive:tracking -->\n- [ ] #11\n"},
		"keep":    {title: "[Umbrella] x", body: "Keep this issue open.\n- [ ] #11\n"},
		"hold":    {title: "[Umbrella] x", labels: []string{"hold"}, body: "- [ ] #11\n"},
		"on hold": {title: "[Umbrella] x", labels: []string{"on-hold"}, body: "- [ ] #11\n"},
	} {
		tc.policy = "trackers_too"
		tc.closedRefs = []int{11}
		if _, second, _ := runTrackerSweeps(t, tc); second {
			t.Fatalf("%s: a tracking or held issue must never be closed", name)
		}
	}
}

func TestTrackerAutoClose_ReopenedByPersonNeverRecloses(t *testing.T) {
	_, second, _ := runTrackerSweeps(t, trackerCase{
		title: "[Umbrella] x", body: "- [ ] #11\n", policy: "trackers_too", closedRefs: []int{11},
		extra: []map[string]any{
			timelineEvent("commented", "hive[bot]", "Bot", parentAutoCloseMarker+"\nclosed", "2026-10-02T00:00:00Z"),
			timelineEvent("closed", "hive[bot]", "Bot", "", "2026-10-02T00:00:01Z"),
			timelineEvent("reopened", "alice", "User", "", "2026-10-03T00:00:00Z"),
		},
	})
	if second {
		t.Fatal("a tracker a person reopened after an auto-close must stay open")
	}
}

func TestTrackerAutoClose_EmptyReferenceSetNeverCloses(t *testing.T) {
	for name, tc := range map[string]trackerCase{
		"tracker label, nothing referenced": {title: "x", labels: []string{"tracker"}, body: "free text"},
		"close_when with no items":          {title: "x", body: "close_when:\n\nprose"},
	} {
		tc.policy = "trackers_too"
		if _, second, _ := runTrackerSweeps(t, tc); second {
			t.Fatalf("%s: an empty reference set must never close", name)
		}
	}
}

func TestTrackerAutoClose_PlainIssueWithRefsPRsIsNotACandidate(t *testing.T) {
	_, second, _ := runTrackerSweeps(t, trackerCase{
		title: "Betty on Zoom without a Zoom app", body: "design text", policy: "trackers_too",
		closedRefs: []int{21}, prBodies: map[int]string{21: "Refs #7"},
	})
	if second {
		t.Fatal("an issue that does not announce itself as a tracker must not close from Refs PRs alone")
	}
}

func TestTrackerAutoClose_InfersFromRefsPRs(t *testing.T) {
	_, second, comments := runTrackerSweeps(t, trackerCase{
		title: "x", labels: []string{"tracker"}, body: "free text", policy: "trackers_too",
		closedRefs: []int{21, 22},
		prBodies:   map[int]string{21: "Refs #7", 22: "Part of #7", 23: "only mentions #7 in passing"},
	})
	if !second {
		t.Fatal("every referencing PR closed must close the tracker; a PR that merely mentions it is not a reference")
	}
	if len(comments) != 1 || strings.Contains(comments[0], "#23") {
		t.Fatalf("comment = %q", comments)
	}
}

func TestTrackerAutoClose_OpenRefsPRKeepsItOpen(t *testing.T) {
	_, second, _ := runTrackerSweeps(t, trackerCase{
		title: "x", labels: []string{"tracker"}, body: "free text", policy: "trackers_too",
		closedRefs: []int{21}, prBodies: map[int]string{21: "Refs #7", 22: "Refs #7"},
	})
	if second {
		t.Fatal("an open PR that references the tracker must keep it open")
	}
}

func TestTrackerAutoClose_CloseWhenReplacesInferredSet(t *testing.T) {
	// #12 is an open task-list item and PR 22 is an open Refs PR, but the
	// explicit close_when checklist names only #11 and a ticked step.
	_, second, comments := runTrackerSweeps(t, trackerCase{
		title: "x", policy: "trackers_too", closedRefs: []int{11},
		body:     "- [ ] #12 not part of close_when\n\n## close_when\n- [x] docs shipped\n- [ ] #11\n",
		prBodies: map[int]string{22: "Refs #7"},
	})
	if !second {
		t.Fatal("close_when must replace the inferred set")
	}
	if len(comments) != 1 || !strings.Contains(comments[0], "close_when") || strings.Contains(comments[0], "#12") {
		t.Fatalf("comment = %q", comments)
	}
	_, second, _ = runTrackerSweeps(t, trackerCase{
		title: "x", policy: "trackers_too", closedRefs: []int{11},
		body: "close_when:\n- [ ] docs shipped\n- [ ] #11\n",
	})
	if second {
		t.Fatal("an unticked close_when step without a reference keeps the tracker open")
	}
}
