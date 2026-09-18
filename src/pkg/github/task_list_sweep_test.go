package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// hiveTrailer is the byte sequence AppendTrailer stamps on every hive-mediated
// create. isHiveFiledIssue keys on the AttributionTrailerPrefix substring, so
// any body that carries it — trailer or full policy paste — qualifies as
// hive-filed. This is the single source of truth the sweep uses to
// distinguish agent output from a maintainer's issue.
const hiveTrailer = "\n\n" + AttributionTrailerPrefix + " scanner via test"

// TestCountTaskListBoxes covers every parser trap the sweep must survive:
// bullet variants, indentation, mixed case, and — the one that would silently
// close every prose issue that quoted the policy — fenced code blocks.
func TestCountTaskListBoxes(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantChecked   int
		wantUnchecked int
	}{
		{"all ticked", "- [x] one\n- [x] two\n- [X] three\n", 3, 0},
		{"some unticked", "- [x] one\n- [ ] two\n- [ ] three\n", 1, 2},
		{"no boxes at all", "This is a prose finding with no task list.\n", 0, 0},
		{"nested and indented boxes count", "- [x] top\n  - [x] child\n    - [ ] grandchild\n", 2, 1},
		{"asterisk and plus bullets count", "* [x] star\n+ [ ] plus\n- [x] dash\n", 2, 1},
		{"boxes inside triple-backtick fence do NOT count", "- [x] real one\n\n```markdown\n- [ ] not a real box\n- [x] also not\n```\n\n- [x] real two\n", 2, 0},
		{"boxes inside tilde fence do NOT count", "- [x] real\n~~~\n- [ ] fake\n~~~\n- [ ] real unticked\n", 1, 1},
		{"boxes inside longer backtick fence do NOT count", "````\n- [ ] fake\n- [x] fake ticked\n````\n- [x] real\n", 1, 0},
		{"malformed box (no space in brackets) is ignored", "- [] not a task\n- [x] real\n", 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checked, unchecked := countTaskListBoxes(tc.body)
			if checked != tc.wantChecked || unchecked != tc.wantUnchecked {
				t.Fatalf("countTaskListBoxes(%q) = (%d, %d); want (%d, %d)",
					tc.body, checked, unchecked, tc.wantChecked, tc.wantUnchecked)
			}
		})
	}
}

// taskListSweepFixture is one open issue on the wire.
type taskListSweepFixture struct {
	Number      int
	Body        string
	Labels      []string
	IsPR        bool
	AuthorLogin string
	AuthorType  string // "User" (default) or "Bot"
}

// taskListMergedPR is a closed+merged PR on the wire whose title/body may
// reference open issues.
type taskListMergedPR struct {
	Number int
	Title  string
	Body   string
}

// sweepObservations records every mutation the sweep performs against the
// mock, so tests can assert on comment counts (create vs edit), closes, and
// which merged PRs were named.
type sweepObservations struct {
	mu             sync.Mutex
	closed         []int
	commentsPosted map[int][]string // issue number → bodies posted (each element = one CreateComment call)
	commentsEdited map[int64][]string
	comments       map[int][]sweepMockComment // issue number → current in-mock comment list
	labelsAdded    map[int][]string
	nextCommentID  int64
}

type sweepMockComment struct {
	ID   int64
	Body string
}

func newSweepObservations() *sweepObservations {
	return &sweepObservations{
		commentsPosted: map[int][]string{},
		commentsEdited: map[int64][]string{},
		comments:       map[int][]sweepMockComment{},
		labelsAdded:    map[int][]string{},
		nextCommentID:  1_000_000,
	}
}

func (o *sweepObservations) totalCreates() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, v := range o.commentsPosted {
		n += len(v)
	}
	return n
}

func (o *sweepObservations) totalEdits() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, v := range o.commentsEdited {
		n += len(v)
	}
	return n
}

// taskListSweepServer stands up a repo mux serving open issues, closed+merged
// PRs, and per-issue comment lifecycle (list / create / edit) so
// ensureSweepComment's idempotency can be exercised across multiple sweep
// cycles.
func taskListSweepServer(t *testing.T, org, repo string, issues []taskListSweepFixture, merged []taskListMergedPR) (*httptest.Server, *sweepObservations) {
	t.Helper()
	obs := newSweepObservations()
	now := time.Now()

	mux := http.NewServeMux()

	// GET /repos/org/repo/issues — open-issues list.
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		wire := make([]map[string]any, 0, len(issues))
		for _, iss := range issues {
			labels := make([]map[string]any, 0, len(iss.Labels))
			for _, l := range iss.Labels {
				labels = append(labels, map[string]any{"name": l})
			}
			entry := map[string]any{
				"number": iss.Number,
				"body":   iss.Body,
				"state":  "open",
				"labels": labels,
			}
			authorType := iss.AuthorType
			if authorType == "" {
				authorType = "User"
			}
			entry["user"] = map[string]any{"login": iss.AuthorLogin, "type": authorType}
			if iss.IsPR {
				entry["pull_request"] = map[string]any{"url": fmt.Sprintf("/repos/%s/%s/pulls/%d", org, repo, iss.Number)}
			}
			wire = append(wire, entry)
		}
		_ = json.NewEncoder(w).Encode(wire)
	})

	// GET /repos/org/repo/pulls?state=closed — merged-PR settle-scan.
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		wire := make([]map[string]any, 0, len(merged))
		for _, pr := range merged {
			// mergedAt / updatedAt both within window so the sweep counts them.
			ts := now.Add(-30 * time.Minute).Format(time.RFC3339)
			wire = append(wire, map[string]any{
				"number":     pr.Number,
				"title":      pr.Title,
				"body":       pr.Body,
				"state":      "closed",
				"merged_at":  ts,
				"updated_at": ts,
				"user":       map[string]any{"login": "hive-app[bot]", "type": "Bot"},
				"html_url":   fmt.Sprintf("https://github.com/%s/%s/pull/%d", org, repo, pr.Number),
			})
		}
		_ = json.NewEncoder(w).Encode(wire)
	})

	// PATCH /repos/org/repo/issues/comments/{id} — edit a comment in place.
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/comments/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		idStr := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/comments/", org, repo))
		id, _ := strconv.ParseInt(idStr, 10, 64)
		var payload struct {
			Body *string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		obs.mu.Lock()
		defer obs.mu.Unlock()
		if payload.Body != nil {
			obs.commentsEdited[id] = append(obs.commentsEdited[id], *payload.Body)
			// Update the in-mock comment record so subsequent list calls see
			// the new body and ensureSweepComment can no-op on identical
			// re-runs.
			for issueNum, list := range obs.comments {
				for i := range list {
					if list[i].ID == id {
						obs.comments[issueNum][i].Body = *payload.Body
					}
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	})

	// /repos/org/repo/issues/{n} — issue edit (close); /issues/{n}/comments — comment list/create.
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/", org, repo))
		// Skip the /issues/comments/{id} PATCH path — handled by the sibling
		// registration above.
		if strings.HasPrefix(rest, "comments/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		parts := strings.SplitN(rest, "/", 2)
		var n int
		fmt.Sscanf(parts[0], "%d", &n)

		if len(parts) == 2 && parts[1] == "labels" && r.Method == "POST" {
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			obs.mu.Lock()
			obs.labelsAdded[n] = append(obs.labelsAdded[n], labels...)
			obs.mu.Unlock()
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}

		if len(parts) == 2 && parts[1] == "comments" {
			switch r.Method {
			case "GET":
				obs.mu.Lock()
				list := append([]sweepMockComment(nil), obs.comments[n]...)
				obs.mu.Unlock()
				wire := make([]map[string]any, 0, len(list))
				for _, c := range list {
					wire = append(wire, map[string]any{
						"id":   c.ID,
						"body": c.Body,
						"user": map[string]any{"login": "hive-app[bot]", "type": "Bot"},
					})
				}
				_ = json.NewEncoder(w).Encode(wire)
				return
			case "POST":
				var payload struct {
					Body *string `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				body := ""
				if payload.Body != nil {
					body = *payload.Body
				}
				obs.mu.Lock()
				obs.nextCommentID++
				id := obs.nextCommentID
				obs.comments[n] = append(obs.comments[n], sweepMockComment{ID: id, Body: body})
				obs.commentsPosted[n] = append(obs.commentsPosted[n], body)
				obs.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
				return
			}
		}

		if len(parts) == 1 && r.Method == "PATCH" {
			var payload struct {
				State *string `json:"state"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload.State != nil && *payload.State == "closed" {
				obs.mu.Lock()
				obs.closed = append(obs.closed, n)
				obs.mu.Unlock()
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": "closed"})
			return
		}

		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	server := httptest.NewServer(mux)
	return server, obs
}

// TestSweepCompletedTaskListIssues drives the end-to-end sweep against a mock
// GitHub. Every row of the table is one safety gate the sweep MUST enforce.
func TestSweepCompletedTaskListIssues(t *testing.T) {
	org, repo := "hivecommons", "hive"

	fixtures := []taskListSweepFixture{
		{Number: 101, Body: "## Findings\n\n- [x] a\n- [x] b\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
		{Number: 102, Body: "## Findings\n\n- [x] a\n- [ ] b\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
		{Number: 103, Body: "This is a hive-filed prose finding with no task list.\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
		{Number: 104, Body: "- [x] a\n- [x] b\n\n(no attribution trailer — a maintainer wrote this)", AuthorLogin: "some-maintainer"},
		{Number: 105, Body: "- [x] a\n- [x] b\n" + hiveTrailer, Labels: []string{"do-not-merge/hold"}, AuthorLogin: "hive-app[bot]"},
		{Number: 106, Body: "- [x] top\n  - [x] child\n    - [x] grandchild\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
		{Number: 107, Body: "```\n- [ ] fake\n- [ ] also fake\n```\n\n- [x] real\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
		// #108 — all ticked + Bot author + no trailer, merged PR present → closes.
		{Number: 108, Body: "- [x] a\n- [x] b\n", AuthorLogin: "hive-app[bot]", AuthorType: "Bot"},
		// #109 — critical human-safety case: no trailer, non-Bot User → NOT closed even with merged PR.
		{Number: 109, Body: "- [x] a\n- [x] b\n", AuthorLogin: "some-human", AuthorType: "User"},
		// #110 — exempt via do-not-merge/blocked-paths prefix.
		{Number: 110, Body: "- [x] a\n- [x] b\n" + hiveTrailer, Labels: []string{"do-not-merge/blocked-paths"}, AuthorLogin: "hive-app[bot]"},
		// #111 — all boxes ticked, hive-filed, no merged PR references it. #7071 gate: MUST NOT close.
		{Number: 111, Body: "- [x] a\n- [x] b\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}

	// Merged PRs, each referencing every fixture we WANT to close via `Refs #N`
	// or `Fixes #N`. #111 is deliberately absent from every merged PR body.
	merged := []taskListMergedPR{
		{Number: 501, Title: "landed a & b", Body: "Refs #101"},
		{Number: 506, Title: "landed nested boxes work", Body: "Refs #106"},
		{Number: 502, Title: "landed real fenced-safe finding", Body: "Fixes #107"},
		{Number: 503, Title: "landed bot-authored finding", Body: "Refs #108"},
		// #102 is partially ticked; a merged PR referencing it drives the
		// idempotent-progress-comment path.
		{Number: 504, Title: "landed first half of #102", Body: "Refs #102"},
		// #109 gets a merged PR too — proving the human-authored gate stops
		// closure BEFORE the merged-PR check would otherwise pass it.
		{Number: 505, Title: "human-safety case merged ref", Body: "Refs #109"},
	}

	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})

	var audited int32
	result, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{
		Audit: func(TaskListSweepEvent) { atomic.AddInt32(&audited, 1) },
	})
	if err != nil {
		t.Fatalf("SweepCompletedTaskListIssues: %v", err)
	}

	wantClosed := map[int]bool{101: true, 106: true, 107: true, 108: true}
	if len(obs.closed) != len(wantClosed) {
		t.Fatalf("closed=%v; want exactly %v", obs.closed, keys(wantClosed))
	}
	for _, n := range obs.closed {
		if !wantClosed[n] {
			t.Errorf("closed issue #%d that should NOT have been closed by the sweep", n)
		}
	}

	// Every close MUST be preceded by the sweep's marker comment naming the
	// merged PR(s). #7071: "Closure comment names the merged PRs that
	// satisfied it."
	for n := range wantClosed {
		bodies := obs.commentsPosted[n]
		if len(bodies) == 0 {
			t.Errorf("issue #%d was closed with no marker comment", n)
			continue
		}
		last := bodies[len(bodies)-1]
		if !strings.Contains(last, taskListSweepMarker) {
			t.Errorf("issue #%d closure comment does not carry the marker: %q", n, last)
		}
		// The closure body must list at least one PR number.
		if !strings.Contains(last, "#5") { // 501/502/503 all start with 5
			t.Errorf("issue #%d closure comment does not name a merged PR: %q", n, last)
		}
	}

	if got := int(audited); got != len(wantClosed) {
		t.Errorf("audit callback fired %d times; want %d", got, len(wantClosed))
	}
	if got := len(result.Closed); got != len(wantClosed) {
		t.Errorf("result.Closed len = %d; want %d", got, len(wantClosed))
	}
	if result.Seen == 0 {
		t.Errorf("result.Seen = 0; want > 0")
	}
	if result.Skipped == 0 {
		t.Errorf("result.Skipped = 0; every one of the ineligible fixtures should have been counted as skipped")
	}

	// Every closed event must carry the merged-PR numbers it acted on, so the
	// dashboard sink can name them.
	for _, ev := range result.Closed {
		if len(ev.MergedPRs) == 0 {
			t.Errorf("event for #%d has empty MergedPRs; #7071 requires the sweep to record which PRs satisfied it", ev.Number)
		}
	}
}

// TestSweepTaskListIssue_NoMergedPR_DoesNotClose is the standalone assertion
// for the #7071 gate: a hive-filed, fully-ticked, hold-free issue that has NO
// merged referencing PR must remain open. This is the "pre-ticked task list
// with no work landed" attack that a boxes-only sweep would auto-close.
func TestSweepTaskListIssue_NoMergedPR_DoesNotClose(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 700, Body: "- [x] a\n- [x] b\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}
	// Deliberately empty merged-PR set.
	server, obs := taskListSweepServer(t, org, repo, fixtures, nil)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})

	if _, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(obs.closed) != 0 {
		t.Fatalf("closed = %v; want [] — #7071 requires a merged referencing PR before closure", obs.closed)
	}
	if obs.totalCreates() != 0 {
		t.Errorf("commentsPosted total = %d; a no-merged-PR skip must not spam a comment", obs.totalCreates())
	}
}

// TestSweepTaskListIssue_PartialWithMergedPR_PostsProgressComment is the
// standalone assertion for the "progress comment on partial completion" gate.
func TestSweepTaskListIssue_PartialWithMergedPR_PostsProgressComment(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 800, Body: "- [x] a\n- [ ] b\n- [ ] c\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}
	merged := []taskListMergedPR{
		{Number: 900, Title: "landed slice a", Body: "Refs #800"},
	}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})

	if _, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(obs.closed) != 0 {
		t.Fatalf("closed = %v; want [] — partial task list must stay open", obs.closed)
	}
	bodies := obs.commentsPosted[800]
	if len(bodies) != 1 {
		t.Fatalf("commentsPosted[800] = %d; want 1 progress comment", len(bodies))
	}
	body := bodies[0]
	if !strings.Contains(body, taskListSweepMarker) {
		t.Errorf("progress comment does not carry the marker: %q", body)
	}
	if !strings.Contains(body, "#900") {
		t.Errorf("progress comment does not name the merged PR (#900): %q", body)
	}
	if !strings.Contains(body, "Outstanding") {
		t.Errorf("progress comment does not enumerate outstanding items: %q", body)
	}
}

// TestSweepTaskListIssue_ProgressCommentIsIdempotent is the standalone
// assertion for #7071's "updated in place, not duplicated each cycle" gate.
// Two consecutive sweep ticks on an unchanged issue must yield exactly ONE
// CreateComment (from the first tick) and ZERO edits on the second (the body
// is identical, so ensureSweepComment must no-op).
func TestSweepTaskListIssue_ProgressCommentIsIdempotent(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 810, Body: "- [x] a\n- [ ] b\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}
	merged := []taskListMergedPR{
		{Number: 910, Title: "landed a", Body: "Refs #810"},
	}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})

	// First tick — creates the progress comment.
	if _, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{}); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	// Second tick — no state has changed, so the existing comment body must
	// match the desired body and neither create nor edit fires.
	if _, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{}); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}

	if got := obs.totalCreates(); got != 1 {
		t.Errorf("total CreateComment calls = %d; want exactly 1 (second cycle must NOT duplicate)", got)
	}
	if got := obs.totalEdits(); got != 0 {
		t.Errorf("total EditComment calls = %d; want 0 (body unchanged; ensureSweepComment must no-op)", got)
	}
}

// TestSweepTaskListIssue_ProgressCommentEditedOnBodyChange proves the OTHER
// half of idempotence: when the outstanding-items list changes between
// cycles (an item newly ticked, or a new merged PR arrives), the existing
// comment is edited in place, not duplicated.
func TestSweepTaskListIssue_ProgressCommentEditedOnBodyChange(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 820, Body: "- [x] a\n- [ ] b\n- [ ] c\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}
	// First tick: one merged PR.
	merged := []taskListMergedPR{
		{Number: 920, Title: "landed a", Body: "Refs #820"},
	}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})

	if _, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{}); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if obs.totalCreates() != 1 || obs.totalEdits() != 0 {
		t.Fatalf("after sweep 1: creates=%d edits=%d; want creates=1 edits=0", obs.totalCreates(), obs.totalEdits())
	}

	// Redefine fixtures/merged mid-test to simulate progress: an item ticks
	// and a new PR arrives. Rebuild the server to serve the new state.
	server.Close()
	fixtures[0].Body = "- [x] a\n- [x] b\n- [ ] c\n" + hiveTrailer
	merged = append(merged, taskListMergedPR{Number: 921, Title: "landed b", Body: "Refs #820"})
	server2, obs2 := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server2.Close()
	// Prime obs2 with the comment the first cycle wrote so ensureSweepComment
	// finds it via GET comments and edits in place.
	firstBody := obs.commentsPosted[820][0]
	obs2.comments[820] = []sweepMockComment{{ID: 1_000_500, Body: firstBody}}
	c2 := newTestClient(t, server2, org, []string{repo})

	if _, err := c2.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{}); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if obs2.totalCreates() != 0 {
		t.Errorf("second cycle CreateComment calls = %d; want 0 — an existing marker comment must be edited, not duplicated", obs2.totalCreates())
	}
	if obs2.totalEdits() != 1 {
		t.Errorf("second cycle EditComment calls = %d; want exactly 1", obs2.totalEdits())
	}
}

// TestSweepCompletedTaskListIssues_RespectsMaxCloses proves the per-tick cap
// is a real bound. Ten eligible issues; MaxCloses=2; the sweep must close
// exactly two.
func TestSweepCompletedTaskListIssues_RespectsMaxCloses(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := make([]taskListSweepFixture, 0, 10)
	merged := make([]taskListMergedPR, 0, 10)
	for i := 0; i < 10; i++ {
		n := 200 + i
		fixtures = append(fixtures, taskListSweepFixture{
			Number:      n,
			Body:        "- [x] only box\n" + hiveTrailer,
			AuthorLogin: "hive-app[bot]",
		})
		merged = append(merged, taskListMergedPR{
			Number: 400 + i,
			Title:  fmt.Sprintf("landed #%d", n),
			Body:   fmt.Sprintf("Refs #%d", n),
		})
	}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	result, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{MaxCloses: 2})
	if err != nil {
		t.Fatalf("SweepCompletedTaskListIssues: %v", err)
	}
	if len(obs.closed) != 2 {
		t.Fatalf("closed = %v; want exactly 2", obs.closed)
	}
	if len(result.Closed) != 2 {
		t.Fatalf("result.Closed = %d; want 2", len(result.Closed))
	}
}

// TestIsHiveFiledIssue directly exercises the hive-filed classifier.
func TestIsHiveFiledIssue(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		authorType string
		want       bool
	}{
		{"nil body + non-Bot is not hive-filed", "", "User", false},
		{"body with trailer + non-Bot is hive-filed", "some finding\n\n" + AttributionTrailerPrefix + " scanner", "User", true},
		{"body without trailer + non-Bot is not hive-filed", "some finding without any hive stamp", "User", false},
		{"Bot author with no trailer is hive-filed", "no trailer here", "Bot", true},
		{"Bot author (lowercase 'bot') is hive-filed", "no trailer here", "bot", true},
		{"Bot author + trailer is hive-filed", "with trailer\n\n" + AttributionTrailerPrefix + " x", "Bot", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iss := &gh.Issue{
				Body: gh.Ptr(tc.body),
				User: &gh.User{Type: gh.Ptr(tc.authorType)},
			}
			if got := isHiveFiledIssue(iss); got != tc.want {
				t.Errorf("isHiveFiledIssue(body=%q, type=%q) = %v; want %v", tc.body, tc.authorType, got, tc.want)
			}
		})
	}
}

func TestSweepNonTaskListRefsCommentsAndLabelsNeedsHuman(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 830, Body: "Prose finding with no boxes." + hiveTrailer, AuthorLogin: "hive-app[bot]"},
		{Number: 831, Body: "Another prose finding." + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}
	merged := []taskListMergedPR{
		{Number: 930, Title: "land staged work", Body: "## Fix\n\nRefs #830 — docs follow-up remains for a later agent phase"},
		{Number: 931, Title: "land reachable work", Body: "## Fix\n\nRefs #831 (needs-human: denied file requires maintainer edit)\n\n## What this deliberately leaves undone\n\n- Change `.github/settings.yml`; this needs a human with repo settings access."},
	}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})

	if _, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	for _, n := range []int{830, 831} {
		bodies := obs.commentsPosted[n]
		if len(bodies) != 1 {
			t.Fatalf("commentsPosted[%d] = %d; want 1", n, len(bodies))
		}
		body := bodies[0]
		if !strings.Contains(body, taskListSweepMarker) || !strings.Contains(body, "Remainder from the PR body") {
			t.Errorf("comment for #%d missing marker/remainder: %q", n, body)
		}
	}
	if labels := obs.labelsAdded[830]; len(labels) != 0 {
		t.Errorf("labelsAdded[830] = %v; staged work must not get needs-human", labels)
	}
	if labels := obs.labelsAdded[831]; len(labels) != 1 || labels[0] != issueNeedsHumanLabel {
		t.Fatalf("labelsAdded[831] = %v; want [%s]", labels, issueNeedsHumanLabel)
	}
	if body := obs.commentsPosted[831][0]; !strings.Contains(body, "`.github/settings.yml`") || !strings.Contains(body, "needs human") {
		t.Errorf("needs-human comment did not carry the PR remainder section: %q", body)
	}
	if len(obs.closed) != 0 {
		t.Fatalf("closed = %v; non-task-list Refs issues must stay open", obs.closed)
	}
}

func TestSweepNonTaskListRefsCommentIsIdempotent(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 840, Body: "Prose finding." + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}
	merged := []taskListMergedPR{{Number: 940, Title: "land slice", Body: "Refs #840 — one follow-up remains"}}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})

	if _, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{}); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	if _, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{}); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if got := obs.totalCreates(); got != 1 {
		t.Errorf("total creates = %d; want 1", got)
	}
	if got := obs.totalEdits(); got != 0 {
		t.Errorf("total edits = %d; want 0", got)
	}
}

func TestSweepNonTaskListRefsKeepsOlderRemainderWhenWindowShrinks(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 850, Body: "Prose finding." + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}
	merged := []taskListMergedPR{
		{Number: 950, Title: "land first slice", Body: "Refs #850 — first remainder"},
		{Number: 951, Title: "land second slice", Body: "Refs #850 — second remainder"},
	}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})

	if _, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{}); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	firstBody := obs.commentsPosted[850][0]
	if !strings.Contains(firstBody, "#950") || !strings.Contains(firstBody, "#951") {
		t.Fatalf("first comment = %q; want both PRs", firstBody)
	}

	server.Close()
	server2, obs2 := taskListSweepServer(t, org, repo, fixtures, []taskListMergedPR{{Number: 951, Title: "land second slice updated", Body: "Refs #850 — updated second remainder"}, {Number: 952, Title: "land third slice", Body: "Refs #850 — third remainder\n\n## What remains\n\n- #850 update the third protected file; this needs a human with admin access."}})
	defer server2.Close()
	obs2.comments[850] = []sweepMockComment{{ID: 1_000_600, Body: firstBody}}
	c2 := newTestClient(t, server2, org, []string{repo})
	if _, err := c2.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{}); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if got := obs2.totalEdits(); got != 1 {
		t.Fatalf("total edits = %d; want 1 to add new PR while preserving aged-out PR", got)
	}
	updated := obs2.comments[850][0].Body
	for _, want := range []string{"#950", "#951", "#952", "updated second remainder", "third protected file"} {
		if !strings.Contains(updated, want) {
			t.Fatalf("updated comment = %q; missing %s", updated, want)
		}
	}
	if strings.Contains(updated, "second remainder") && !strings.Contains(updated, "updated second remainder") {
		t.Fatalf("updated comment = %q; still-current PR #951 block was not refreshed", updated)
	}
}

func TestRefsRemainderMetadataNeedsHumanSignals(t *testing.T) {
	meta := refsRemainderMetadata("Refs #12 (needs-human: maintainer must change protected file)", "o/r")
	got := meta[claimKey("o/r", 12)]
	if !got.Reference || !got.NeedsHuman || got.Remainder != "maintainer must change protected file" {
		t.Fatalf("explicit marker meta = %+v", got)
	}

	meta = refsRemainderMetadata("Refs #13 — phase two remains\n\n## What remains\n\nA follow-up agent can update docs.", "o/r")
	got = meta[claimKey("o/r", 13)]
	if !got.Reference || got.NeedsHuman || !strings.Contains(got.Remainder, "follow-up agent") {
		t.Fatalf("agent-doable section meta = %+v", got)
	}

	meta = refsRemainderMetadata("Refs #14 — protected file remains\n\n## What this deliberately leaves undone\n\nThese edits must be applied by hand as one atomic diff for a human because the file is denied.", "o/r")
	got = meta[claimKey("o/r", 14)]
	if !got.Reference || !got.NeedsHuman {
		t.Fatalf("precise prose fallback meta = %+v", got)
	}

	meta = refsRemainderMetadata("Refs #15 — docs remain\n\n## What remains\n\nThis does not need a human; another agent can update the docs.", "o/r")
	got = meta[claimKey("o/r", 15)]
	if !got.Reference || got.NeedsHuman {
		t.Fatalf("negated human prose meta = %+v", got)
	}

	meta = refsRemainderMetadata("Refs #10 (needs-human: protected settings require a maintainer)\nRefs #11 — agent-doable docs remain\n\n## What remains\n\n- #10 update protected settings; this requires a human with admin access.\n- #11 regenerate docs; an agent can do this.", "o/r")
	if got := meta[claimKey("o/r", 10)]; !got.Reference || !got.NeedsHuman {
		t.Fatalf("marked ref meta = %+v; want needs-human", got)
	}
	if got := meta[claimKey("o/r", 11)]; !strings.Contains(got.Remainder, "#11 regenerate docs") || strings.Contains(got.Remainder, "#10 update protected") {
		t.Fatalf("unmarked ref remainder = %q; want only its scoped bullet", got.Remainder)
	}

	meta = refsRemainderMetadata("Fixes #20\nRefs #21 — docs remain\n\n## What remains\n\n- #20 protected settings still require a human.\n- #21 docs are agent-doable.", "o/r")
	got = meta[claimKey("o/r", 21)]
	if !got.Reference || got.NeedsHuman || strings.Contains(got.Remainder, "#20 protected") || !strings.Contains(got.Remainder, "#21 docs") {
		t.Fatalf("single Refs with scoped bullets meta = %+v; must not inherit another issue's human-only bullet", got)
	}

	meta = refsRemainderMetadata("Refs #17 — protected file remains\n\n## What remains\n\n- #17 update protected settings:\n  this needs a human with admin access.", "o/r")
	got = meta[claimKey("o/r", 17)]
	if !got.Reference || !got.NeedsHuman || !strings.Contains(got.Remainder, "admin access") {
		t.Fatalf("scoped multi-line bullet meta = %+v; want continuation-line needs-human signal", got)
	}

	for _, tc := range []struct {
		text           string
		wantNeedsHuman bool
	}{
		{"## What remains\n\nscreenshots were last generated by a human designer; an agent should regenerate them", false},
		{"## What remains\n\nan agent cannot do this; it needs a human with admin access", true},
		{"## What remains\n\nrepository settings are not editable by agents, so this needs a human with admin access", true},
	} {
		meta = refsRemainderMetadata("Refs #16 — remainder described below\n\n"+tc.text, "o/r")
		got := meta[claimKey("o/r", 16)]
		if got.NeedsHuman != tc.wantNeedsHuman {
			t.Fatalf("human prose meta for %q = %+v; want needs-human %v", tc.text, got, tc.wantNeedsHuman)
		}
	}
}

func keys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
