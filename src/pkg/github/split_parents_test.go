package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// withSplitParentLedger points the #9840 ledger at a temp file and records the
// given child→parent links in it, as the relay would after linking.
func withSplitParentLedger(t *testing.T, repo string, links map[int]int) string {
	t.Helper()
	old := SplitParentsPath
	SplitParentsPath = filepath.Join(t.TempDir(), SplitParentsFile)
	t.Cleanup(func() { SplitParentsPath = old })
	for child, parent := range links {
		if err := RecordSplitParent("", repo, child, parent, "scanner"); err != nil {
			t.Fatalf("RecordSplitParent(%d→%d): %v", child, parent, err)
		}
	}
	return SplitParentsPath
}

func TestSplitParentLedger_RoundTripAndGuards(t *testing.T) {
	path := withSplitParentLedger(t, "o/r", map[int]int{11: 1, 12: 1})

	children, err := LoadSplitParents(path)
	if err != nil {
		t.Fatalf("LoadSplitParents: %v", err)
	}
	if got := splitParentOf(children, "o/r", 11); got != 1 {
		t.Errorf("parent of #11 = %d, want 1", got)
	}
	if got := splitParentOf(children, "o/r", 12); got != 1 {
		t.Errorf("parent of #12 = %d, want 1", got)
	}
	if got := splitParentOf(children, "o/other", 11); got != 0 {
		t.Errorf("ledger keys are repo-scoped; parent of o/other#11 = %d, want 0", got)
	}
	if got := splitParentOf(children, "o/r", 1); got != 0 {
		t.Errorf("the parent itself has no parent: got %d", got)
	}

	// Degenerate links are refused, not recorded.
	for _, tc := range []struct{ child, parent int }{{0, 1}, {5, 0}, {7, 7}} {
		if err := RecordSplitParent(path, "o/r", tc.child, tc.parent, ""); err != nil {
			t.Fatalf("RecordSplitParent(%d→%d): %v", tc.child, tc.parent, err)
		}
	}
	children, _ = LoadSplitParents(path)
	if len(children) != 2 {
		t.Errorf("degenerate links were recorded: %+v", children)
	}

	// A missing ledger is empty, not an error.
	if got, err := LoadSplitParents(filepath.Join(t.TempDir(), "nope.json")); err != nil || len(got) != 0 {
		t.Errorf("missing ledger: got %v, %v; want empty, nil", got, err)
	}
}

func TestPruneSplitParents_OldestFirst(t *testing.T) {
	children := map[string]SplitParent{}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < maxSplitParents+3; i++ {
		children[splitParentKey("o/r", i+100)] = SplitParent{Parent: 1, At: base.Add(time.Duration(i) * time.Second)}
	}
	pruneSplitParents(children)
	if len(children) != maxSplitParents {
		t.Fatalf("len = %d, want %d", len(children), maxSplitParents)
	}
	for i := 0; i < 3; i++ {
		if _, ok := children[splitParentKey("o/r", i+100)]; ok {
			t.Errorf("oldest entry #%d survived pruning", i+100)
		}
	}
}

// TestEnumerateActionable_SplitChildrenInheritParentAcknowledgement pins the
// #9840 rule across the cases the issue lists: a relay-linked child of a
// human-filed parent ranks as acknowledged and says why; the same child under
// a hive-filed, unacknowledged parent does not; a child whose parent carries
// its own approval label does; a held parent confers nothing; a closed parent
// (absent from the open snapshot) confers nothing; a child whose link was NOT
// made by the relay (no ledger entry) inherits nothing; and a child with its
// own acknowledgement keeps AckSource empty.
func TestEnumerateActionable_SplitChildrenInheritParentAcknowledgement(t *testing.T) {
	org, repo := "org", "repo"
	full := org + "/" + repo
	const bot = "hive[bot]"
	issues := []wireIssue{
		// Parents.
		{Number: 1, Title: "human parent", User: wireUser{"maintainer"}, State: "open", CreatedAt: hoursAgo(10)},
		{Number: 2, Title: "hive parent, untouched", User: wireUser{bot}, State: "open", CreatedAt: hoursAgo(10)},
		{Number: 3, Title: "hive parent, approved", User: wireUser{bot}, State: "open", Labels: []wireLabel{{Name: HumanAckLabel}}, CreatedAt: hoursAgo(10)},
		{Number: 4, Title: "human parent, held", User: wireUser{"maintainer"}, State: "open", Labels: []wireLabel{{Name: "hold"}}, CreatedAt: hoursAgo(10)},
		// Children (all hive-filed, no labels, no assignees).
		{Number: 11, Title: "child of human parent", User: wireUser{bot}, State: "open", CreatedAt: hoursAgo(1)},
		{Number: 12, Title: "child of untouched hive parent", User: wireUser{bot}, State: "open", CreatedAt: hoursAgo(1)},
		{Number: 13, Title: "child of approved hive parent", User: wireUser{bot}, State: "open", CreatedAt: hoursAgo(1)},
		{Number: 14, Title: "child of held human parent", User: wireUser{bot}, State: "open", CreatedAt: hoursAgo(1)},
		{Number: 15, Title: "child of closed parent", User: wireUser{bot}, State: "open", CreatedAt: hoursAgo(1)},
		{Number: 16, Title: "linked outside the relay", User: wireUser{bot}, State: "open", CreatedAt: hoursAgo(1)},
		{Number: 17, Title: "child with its own assignee", User: wireUser{bot}, State: "open", Assignees: []wireUser{{"maintainer"}}, CreatedAt: hoursAgo(1)},
		{Number: 18, Title: "grandchild", User: wireUser{bot}, State: "open", CreatedAt: hoursAgo(1)},
		{Number: 19, Title: "plain hive-filed", User: wireUser{bot}, State: "open", CreatedAt: hoursAgo(1)},
	}
	withSplitParentLedger(t, full, map[int]int{
		11: 1,
		12: 2,
		13: 3,
		14: 4,
		15: 999, // not in the open snapshot: closed
		17: 1,
		18: 11, // parent #11 is itself only an inheritor
	})

	mux := buildMux(t, org, repo, issues, nil)
	server := httptest.NewServer(mux)
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
	want := map[int]struct {
		ack    bool
		source string
	}{
		11: {true, "parent #1"},
		12: {false, ""},
		13: {true, "parent #3"},
		14: {false, ""},
		15: {false, ""},
		16: {false, ""},
		17: {true, ""},  // own acknowledgement wins; nothing to attribute
		18: {false, ""}, // one level only
		19: {false, ""},
	}
	for num, w := range want {
		is, ok := byNumber[num]
		if !ok {
			t.Errorf("#%d missing from actionable set", num)
			continue
		}
		if is.HumanAcknowledged != w.ack || is.AckSource != w.source {
			t.Errorf("#%d (%s): human_acknowledged=%v ack_source=%q, want %v %q", num, is.Title, is.HumanAcknowledged, is.AckSource, w.ack, w.source)
		}
	}

	// Ranking: the inheriting child sits in the acknowledged tier, ahead of
	// the untouched hive-filed backlog and behind the human-filed parents.
	RankActionableIssues(result.Issues.Items)
	pos := map[int]int{}
	for i, is := range result.Issues.Items {
		pos[is.Number] = i
	}
	if !(pos[1] < pos[11] && pos[11] < pos[12] && pos[11] < pos[19]) {
		var order []string
		for _, is := range result.Issues.Items {
			order = append(order, "#"+strconv.Itoa(is.Number))
		}
		t.Errorf("rank order = %s; want human #1 < inheriting #11 < untouched #12/#19", strings.Join(order, " "))
	}
}

// TestEvaluateSelfAuthorization_SplitChildInheritsParentApproval pins the
// #5117 side of #9840: a PR closing a hive-filed child the relay split out of
// a human-filed parent is not held, the same child under an unacknowledged
// hive parent is held, and an unreadable parent holds rather than guesses.
func TestEvaluateSelfAuthorization_SplitChildInheritsParentApproval(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	cases := []struct {
		name     string
		parent   *selfAuthIssue // nil: parent GET 404s
		wantHeld bool
		wantWhy  string
	}{
		{"human-filed open parent", &selfAuthIssue{Author: "hanthor", State: "open"}, false, ""},
		{"hive-filed parent with approval label", &selfAuthIssue{Author: botLogin, AuthorType: "Bot", State: "open", Labels: []string{HumanAckLabel}}, false, ""},
		{"hive-filed parent, untouched", &selfAuthIssue{Author: botLogin, AuthorType: "Bot", State: "open"}, true, "no human has acknowledged"},
		{"human-filed parent, closed", &selfAuthIssue{Author: "hanthor", State: "closed"}, true, "no human has acknowledged"},
		{"human-filed parent, held", &selfAuthIssue{Author: "hanthor", State: "open", Labels: []string{"hold"}}, true, "no human has acknowledged"},
		{"parent unreadable", &selfAuthIssue{Author: "hanthor", State: "open", Status: http.StatusInternalServerError}, true, "could not be read"},
		{"parent missing", nil, true, "could not be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &selfAuthServer{issues: map[int]*selfAuthIssue{
				581: {Author: botLogin, AuthorType: "Bot", State: "open"},
			}}
			if tc.parent != nil {
				srv.issues[500] = tc.parent
			}
			withSplitParentLedger(t, "o/r", map[int]int{581: 500})
			c := NewClientForTest(srv.start(t).URL, "o", nil, prTestLogger())
			c.SetAppBotLogin(botLogin)

			got := c.EvaluateSelfAuthorization(context.Background(), "o/r", "some title", "Closes #581", nil)
			if got.Held != tc.wantHeld {
				t.Fatalf("Held = %v, want %v (reason %q)", got.Held, tc.wantHeld, got.Reason)
			}
			if tc.wantWhy != "" && !strings.Contains(got.Reason, tc.wantWhy) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tc.wantWhy)
			}
		})
	}
}

// Without a ledger entry the gate behaves exactly as before: a sub-issue link
// nobody in the hive made confers nothing, and no parent fetch happens.
func TestEvaluateSelfAuthorization_UnrecordedParentConfersNothing(t *testing.T) {
	const botLogin = "kubestellar-hive[bot]"
	srv := &selfAuthServer{issues: map[int]*selfAuthIssue{
		581: {Author: botLogin, AuthorType: "Bot", State: "open"},
		500: {Author: "hanthor", State: "open"},
	}}
	withSplitParentLedger(t, "o/r", nil)
	c := NewClientForTest(srv.start(t).URL, "o", nil, prTestLogger())
	c.SetAppBotLogin(botLogin)

	got := c.EvaluateSelfAuthorization(context.Background(), "o/r", "t", "Closes #581", nil)
	if !got.Held {
		t.Fatalf("expected hold without a relay-recorded parent, got %+v", got)
	}
}
