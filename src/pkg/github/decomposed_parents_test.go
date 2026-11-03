package github

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestParentMarker_RoundTrip(t *testing.T) {
	body := withParentMarker("Child body.\n", 12361)
	if !strings.HasSuffix(body, ParentMarker(12361)) || !strings.Contains(body, "Child body.") {
		t.Fatalf("body = %q", body)
	}
	if ParseParentMarker(body) != 12361 {
		t.Fatalf("parse = %d", ParseParentMarker(body))
	}
	if again := withParentMarker(body, 12361); again != body {
		t.Fatalf("marker must not repeat: %q", again)
	}
	if withParentMarker("x", 0) != "x" || ParseParentMarker("no marker") != 0 {
		t.Fatal("zero parent / absent marker must be no-ops")
	}
	if ParseParentMarker("<!-- hive:parent #42 -->") != 42 {
		t.Fatal("hash-prefixed marker must parse")
	}
}

func TestChildrenOf_SummaryThenLedgerAndMarkerFallback(t *testing.T) {
	open := map[int]*gh.Issue{
		1: {Number: gh.Ptr(1)},
		2: {Number: gh.Ptr(2), Body: gh.Ptr("work\n\n" + ParentMarker(1))},
		9: {Number: gh.Ptr(9)},
	}
	ledger := map[string]SplitParent{
		splitParentKey("acme/widgets", 3): {Parent: 1},
		splitParentKey("acme/widgets", 4): {Parent: 1},
		splitParentKey("acme/widgets", 5): {Parent: 9},
		splitParentKey("acme/other", 6):   {Parent: 1},
	}
	summaries := map[int]IssueChildren{1: {Total: 4, Completed: 4}}

	if got := childrenOf("acme/widgets", 1, summaries, ledger, open); got == nil || *got != (IssueChildren{Total: 4, Completed: 4}) {
		t.Fatalf("summary must win: %+v", got)
	}
	// No summary: ledger children 3 and 4 (closed: absent from the snapshot)
	// plus body-marked open child 2; the other repo's #6 is ignored.
	got := childrenOf("acme/widgets", 1, nil, ledger, open)
	if got == nil || *got != (IssueChildren{Total: 3, Completed: 2}) {
		t.Fatalf("fallback = %+v", got)
	}
	if got.Open() != 1 || got.AllClosed() {
		t.Fatalf("open=%d allClosed=%v", got.Open(), got.AllClosed())
	}
	if childrenOf("acme/widgets", 2, nil, ledger, open) != nil {
		t.Fatal("a leaf has no children")
	}
	if got := splitChildrenOf(ledger, "ACME/Widgets", 1); len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("splitChildrenOf = %v", got)
	}
}

func TestDecomposedParentCloseComment(t *testing.T) {
	got := decomposedParentCloseComment([]int{3, 4, 5}, map[int]int{3: 30, 5: 50})
	if !strings.HasPrefix(got, parentAutoCloseMarker) {
		t.Fatalf("missing marker: %q", got)
	}
	if !strings.Contains(got, "All children delivered: #3 → PR #30, #4, #5 → PR #50.") {
		t.Fatalf("comment = %q", got)
	}
}

func parentInput(children *IssueChildren, labels ...string) Issue {
	return Issue{Repo: "acme/widgets", Number: 7, Labels: labels, Children: children}
}

func TestSyncIssueStageLabels_ClosesParentWhenAllChildrenClosed(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	key := stagelabels.Key("acme/widgets", 7)
	store.Observe(key, stagelabels.Actionable, "actionable")

	fake.mu.Lock()
	fake.subIssues[7] = []subIssueRef{{Number: 3, State: "closed"}, {Number: 4, State: "closed"}}
	fake.closers[3] = 30
	fake.closers[4] = 40
	fake.mu.Unlock()

	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{parentInput(&IssueChildren{Total: 2, Completed: 2}, stagelabels.Actionable.Label(), stagelabels.StalledLabel, "enhancement")})
	res := syncSeeded(t, fake, c, result, store, stageNow())

	if res.ParentsClosed != 1 || res.ParentsAwaitingChildren != 0 {
		t.Fatalf("result = %+v", res)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if reason, ok := fake.closed[7]; !ok || reason != IssueStateReasonCompleted {
		t.Fatalf("parent not closed as completed: %v %q", ok, reason)
	}
	// The close path's best-effort removal of the reporter-confirmation
	// label follows the two stage labels.
	if got := strings.Join(fake.removed[7], ","); !strings.HasPrefix(got, stagelabels.Actionable.Label()+","+stagelabels.StalledLabel) {
		t.Fatalf("removed = %v", fake.removed[7])
	}
	if len(fake.added[7]) != 0 {
		t.Fatalf("no label must be added to a closed parent: %v", fake.added[7])
	}
	if len(fake.commentBodies[7]) != 1 || !strings.Contains(fake.commentBodies[7][0], "All children delivered: #3 → PR #30, #4 → PR #40.") {
		t.Fatalf("comments = %q", fake.commentBodies[7])
	}
	if _, ok := store.Get(key); ok {
		t.Fatal("closed parent must be forgotten by the store")
	}
}

func TestSyncIssueStageLabels_ParentWithOpenChildNeverStalls(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	now := stageNow()
	store := stagelabels.NewStore("", func() time.Time { return now })
	key := stagelabels.Key("acme/widgets", 7)
	// Entered 010 three days ago: well past the actionable dwell.
	store.Observe(key, stagelabels.Actionable, "actionable")
	now = now.Add(72 * time.Hour)

	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{parentInput(&IssueChildren{Total: 2, Completed: 1}, stagelabels.Actionable.Label(), stagelabels.StalledLabel)})
	res := syncSeeded(t, fake, c, result, store, now)

	if res.ParentsClosed != 0 || res.ParentsAwaitingChildren != 1 {
		t.Fatalf("result = %+v", res)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, closed := fake.closed[7]; closed {
		t.Fatal("parent with an open child must not be closed")
	}
	if got := strings.Join(fake.removed[7], ","); got != stagelabels.StalledLabel {
		t.Fatalf("existing stage/stalled must come off and nothing else: removed = %v", fake.removed[7])
	}
	if len(fake.added[7]) != 0 || fake.comments[7] != 0 {
		t.Fatalf("stage untouched, no comment: added=%v comments=%d", fake.added[7], fake.comments[7])
	}
	if rec, ok := store.Get(key); !ok || rec.Stage != stagelabels.Actionable || !strings.Contains(rec.Reason, "waiting on 1 open child") {
		t.Fatalf("record = %+v", rec)
	}
}

func TestSyncIssueStageLabels_ParentSnapshotLagsOpenChild(t *testing.T) {
	// The summary says all closed but the live sub-issue list still has an
	// open child (a reopen the list payload has not caught up with): the
	// parent stays open and is not stalled.
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	fake.mu.Lock()
	fake.subIssues[7] = []subIssueRef{{Number: 3, State: "closed"}, {Number: 4, State: "open"}}
	fake.mu.Unlock()

	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{parentInput(&IssueChildren{Total: 2, Completed: 2}, stagelabels.Actionable.Label())})
	res := syncSeeded(t, fake, c, result, store, stageNow())
	if res.ParentsClosed != 0 || res.ParentsAwaitingChildren != 1 {
		t.Fatalf("result = %+v", res)
	}
	writes := fake.writes()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, closed := fake.closed[7]; closed || fake.comments[7] != 0 || writes != 0 {
		t.Fatalf("no close, comment or write expected: closed=%v comments=%d writes=%d", closed, fake.comments[7], writes)
	}
}

func TestSyncIssueStageLabels_ParentLedgerFallbackClose(t *testing.T) {
	// No native sub-issue list (the link failed at create time): the ledger
	// names the children and each is read live.
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)
	prev := SplitParentsPath
	SplitParentsPath = filepath.Join(t.TempDir(), "split-parents.json")
	t.Cleanup(func() { SplitParentsPath = prev })
	for _, child := range []int{3, 4} {
		if err := RecordSplitParent("", "acme/widgets", child, 7, "scanner"); err != nil {
			t.Fatal(err)
		}
	}
	fake.mu.Lock()
	fake.closed[3] = IssueStateReasonCompleted
	fake.closed[4] = "not_planned"
	fake.closers[3] = 30
	fake.mu.Unlock()

	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{parentInput(&IssueChildren{Total: 2, Completed: 2}, stagelabels.Actionable.Label())})
	res := syncSeeded(t, fake, c, result, store, stageNow())
	if res.ParentsClosed != 1 {
		t.Fatalf("result = %+v", res)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.closed[7] != IssueStateReasonCompleted {
		t.Fatalf("closed = %v", fake.closed)
	}
	if len(fake.commentBodies[7]) != 1 || !strings.Contains(fake.commentBodies[7][0], "All children delivered: #3 → PR #30, #4.") {
		t.Fatalf("comments = %q", fake.commentBodies[7])
	}
}

func TestSyncIssueStageLabels_NoChildrenUnchanged(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	store := stagelabels.NewStore("", stageNow)

	result := &ActionableResult{}
	result.Issues = IssueResultFromItems([]Issue{parentInput(nil, stagelabels.Actionable.Label())})
	res := syncSeeded(t, fake, c, result, store, stageNow())
	writes := fake.writes()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if res.ParentsClosed != 0 || res.ParentsAwaitingChildren != 0 || writes != 0 || len(fake.closed) != 0 || fake.comments[7] != 0 {
		t.Fatalf("leaf issue must be untouched: res=%+v writes=%d closed=%v", res, writes, fake.closed)
	}
}

func TestAnnotateIssueChildren_SetsParentsOnly(t *testing.T) {
	actionable := []Issue{{Repo: "acme/widgets", Number: 1}, {Repo: "acme/widgets", Number: 2}}
	details := []RepoWorkIssue{{Repo: "acme/widgets", Number: 3}, {Repo: "acme/widgets"}}
	summaries := map[int]IssueChildren{1: {Total: 2, Completed: 1}, 3: {Total: 1, Completed: 1}}
	annotateIssueChildren("acme/widgets", summaries, nil, nil, actionable, details)
	if actionable[0].Children == nil || actionable[0].Children.Total != 2 || actionable[1].Children != nil {
		t.Fatalf("actionable = %+v", actionable)
	}
	if details[0].Children == nil || !details[0].Children.AllClosed() || details[1].Children != nil {
		t.Fatalf("details = %+v", details)
	}
	row := repoWorkIssueFromIssue(actionable[0], workBucketClaimed, "x")
	if row.Children != actionable[0].Children {
		t.Fatal("repoWorkIssueFromIssue must carry Children")
	}
	in := IssueStageInputs(&ActionableResult{Issues: IssueResultFromItems(actionable)})
	if in[0].Children == nil || in[1].Children != nil {
		t.Fatalf("inputs = %+v", in)
	}
}

func TestEnumerateActionable_SubIssueSummaryBecomesChildren(t *testing.T) {
	org, repo := "org", "repo"
	issues := []wireIssue{
		{Number: 40, Title: "parent, one child open", User: wireUser{"u"}, CreatedAt: hoursAgo(1), SubIssuesSummary: &IssueChildren{Total: 2, Completed: 1}},
		{Number: 41, Title: "parent, all done", User: wireUser{"u"}, CreatedAt: hoursAgo(1), SubIssuesSummary: &IssueChildren{Total: 3, Completed: 3}},
		{Number: 42, Title: "leaf with empty block", User: wireUser{"u"}, CreatedAt: hoursAgo(1), SubIssuesSummary: &IssueChildren{}},
		{Number: 43, Title: "leaf", User: wireUser{"u"}, CreatedAt: hoursAgo(1)},
	}
	server := httptest.NewServer(buildMux(t, org, repo, issues, nil))
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	byNumber := map[int]Issue{}
	for _, is := range result.Issues.Items {
		byNumber[is.Number] = is
	}
	if got := byNumber[40].Children; got == nil || *got != (IssueChildren{Total: 2, Completed: 1}) {
		t.Errorf("#40 children = %+v", got)
	}
	if got := byNumber[41].Children; got == nil || !got.AllClosed() {
		t.Errorf("#41 children = %+v, want all closed", got)
	}
	if byNumber[42].Children != nil || byNumber[43].Children != nil {
		t.Errorf("leaves must carry no children: %+v / %+v", byNumber[42].Children, byNumber[43].Children)
	}
}
