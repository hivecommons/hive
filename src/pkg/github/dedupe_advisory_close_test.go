package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var dedupeTestNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

const dedupeTestChecklist = 50

func dedupeTestBody(verdict string) string {
	return "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: " + verdict + "\n\n## Summary\n\n" +
		"| Item | Related | Note |\n|---|---|---|\n| #1 | o/r#2 | merge #1 first |\n\nTracked in #50.\n"
}

// dedupeFixture fakes the REST calls the close-on-merge backfill makes for a
// single open issue, #50, in o/r.
type dedupeFixture struct {
	t         *testing.T
	mu        sync.Mutex
	body      string
	labels    []string
	createdAt time.Time
	refStates map[int]string
	comments  []string
	failRefs  bool
	failPost  bool
	failLabel bool
	// failPatchBody fails the body edit; author overrides the default
	// hive[bot] author.
	failPatchBody bool
	author        map[string]string

	refGets   int
	posted    []string
	patched   bool
	bodyEdits int
	// mergedRefs lists PR refs whose closed state is a merge.
	mergedRefs map[int]bool
	added      []string
	removed    []string
	unexpect   []string
}

func newDedupeFixture(t *testing.T, verdict string) *dedupeFixture {
	return &dedupeFixture{
		t:         t,
		body:      dedupeTestBody(verdict),
		labels:    []string{dedupeAdvisoryLabelName},
		createdAt: dedupeTestNow.Add(-24 * time.Hour),
		refStates: map[int]string{1: "closed", 2: "closed"},
	}
}

func (f *dedupeFixture) user() map[string]string {
	if f.author != nil {
		return f.author
	}
	return map[string]string{"login": "hive[bot]", "type": "Bot"}
}

func (f *dedupeFixture) issueJSON() map[string]any {
	labels := make([]map[string]string, 0, len(f.labels))
	for _, l := range f.labels {
		labels = append(labels, map[string]string{"name": l})
	}
	return map[string]any{
		"number": dedupeTestChecklist, "state": "open", "body": f.body,
		"created_at": f.createdAt.Format(time.RFC3339),
		"labels":     labels,
		"user":       f.user(),
	}
}

func (f *dedupeFixture) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		checklist := "/repos/o/r/issues/" + strconv.Itoa(dedupeTestChecklist)
		switch {
		case r.Method == http.MethodGet && path == "/repos/o/r/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{f.issueJSON()})
		case r.Method == http.MethodGet && path == checklist:
			_ = json.NewEncoder(w).Encode(f.issueJSON())
		case r.Method == http.MethodGet && path == checklist+"/comments":
			out := make([]map[string]any, 0, len(f.comments))
			for i, body := range f.comments {
				out = append(out, map[string]any{"id": i + 1, "body": body})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && path == checklist+"/comments":
			if f.failPost {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var c struct{ Body string }
			_ = json.NewDecoder(r.Body).Decode(&c)
			f.posted = append(f.posted, c.Body)
			f.comments = append(f.comments, c.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(f.comments)})
		case r.Method == http.MethodPatch && path == checklist:
			var req struct {
				State *string `json:"state"`
				Body  *string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Body != nil {
				if f.failPatchBody {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				f.bodyEdits++
				f.body = *req.Body
			}
			if req.State != nil && *req.State == "closed" {
				f.patched = true
			}
			_ = json.NewEncoder(w).Encode(f.issueJSON())
		case r.Method == http.MethodDelete && strings.HasPrefix(path, checklist+"/labels/"):
			name := strings.TrimPrefix(path, checklist+"/labels/")
			if name == issueNeedsReporterConfirmationLabel {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			f.removed = append(f.removed, name)
			kept := f.labels[:0]
			for _, l := range f.labels {
				if l != name {
					kept = append(kept, l)
				}
			}
			f.labels = kept
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case r.Method == http.MethodGet && path == "/repos/o/r/labels/"+NeedsDecisionStageLabel:
			if f.failLabel {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"name": NeedsDecisionStageLabel})
		case r.Method == http.MethodPost && path == checklist+"/labels":
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			f.added = append(f.added, labels...)
			f.labels = append(f.labels, labels...)
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/o/r/issues/"):
			n, err := strconv.Atoi(strings.TrimPrefix(path, "/repos/o/r/issues/"))
			state, ok := f.refStates[n]
			if err != nil || !ok || f.failRefs {
				f.refGets++
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			f.refGets++
			out := map[string]any{"number": n, "state": state}
			if f.mergedRefs[n] {
				out["pull_request"] = map[string]any{"merged_at": dedupeTestNow.Format(time.RFC3339)}
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			f.unexpect = append(f.unexpect, r.Method+" "+path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func (f *dedupeFixture) run(opts CloseOnMergeOptions) []CloseOnMergeResult {
	f.t.Helper()
	srv := f.server()
	defer srv.Close()
	c := newTestClient(f.t, srv, "o", []string{"r"})
	if opts.Now == nil {
		opts.Now = func() time.Time { return dedupeTestNow }
	}
	results := c.CloseOnMergeBackfill(context.Background(), opts)
	if len(f.unexpect) > 0 {
		f.t.Fatalf("unexpected requests: %v", f.unexpect)
	}
	return results
}

func assertDedupeResult(t *testing.T, results []CloseOnMergeResult, action CloseOnMergeAction, reason string) {
	t.Helper()
	if len(results) != 1 || results[0].Issue != dedupeTestChecklist || results[0].Action != action || results[0].Reason != reason {
		t.Fatalf("results = %+v, want one %s/%s for #%d", results, action, reason, dedupeTestChecklist)
	}
}

func TestDedupeAdvisoryClosesWhenEveryRefClosed(t *testing.T) {
	f := newDedupeFixture(t, "no")
	results := f.run(CloseOnMergeOptions{})
	assertDedupeResult(t, results, CloseOnMergeClosed, "dedupe_advisory_refs_closed")
	if !f.patched {
		t.Fatal("checklist was not closed")
	}
	if f.refGets != 2 {
		t.Fatalf("ref reads = %d, want 2 (#1 and o/r#2; the checklist's own #50 is skipped)", f.refGets)
	}
	want := dedupeAdvisoryCloseMarker + "\nAll 2 items resolved (#1 closed, #2 closed) — closing this advisory."
	if len(f.posted) != 1 || f.posted[0] != want {
		t.Fatalf("posted = %q, want %q", f.posted, want)
	}
	if f.bodyEdits != 0 {
		t.Fatalf("body edits = %d, want none for a checklist without task lines", f.bodyEdits)
	}
}

func TestDedupeAdvisoryStaysOpenWhileARefIsOpen(t *testing.T) {
	f := newDedupeFixture(t, "no")
	f.refStates[2] = "open"
	results := f.run(CloseOnMergeOptions{})
	assertDedupeResult(t, results, CloseOnMergeNoop, "dedupe_advisory_refs_open")
	if f.patched || len(f.posted) != 0 {
		t.Fatalf("patched=%v posted=%q, want the checklist untouched", f.patched, f.posted)
	}
}

func TestDedupeAdvisoryExpiresAfterSevenDays(t *testing.T) {
	f := newDedupeFixture(t, "no")
	f.refStates[1] = "open"
	f.createdAt = dedupeTestNow.Add(-DedupeAdvisoryMaxAge - time.Minute)
	results := f.run(CloseOnMergeOptions{})
	assertDedupeResult(t, results, CloseOnMergeClosed, "dedupe_advisory_expired")
	if !f.patched {
		t.Fatal("expired checklist was not closed")
	}
	if f.refGets != 0 {
		t.Fatalf("ref reads = %d, want none for an expired checklist", f.refGets)
	}
	if len(f.posted) != 1 || f.posted[0] != dedupeAdvisoryExpiredComment {
		t.Fatalf("posted = %q, want the one-line expiry comment", f.posted)
	}
}

func TestDedupeAdvisoryReopenedAfterCloseIsLeftAlone(t *testing.T) {
	f := newDedupeFixture(t, "no")
	f.comments = []string{dedupeAdvisoryCloseMarker + "\nAll 2 items resolved (#1 closed, #2 closed) — closing this advisory."}
	results := f.run(CloseOnMergeOptions{})
	assertDedupeResult(t, results, CloseOnMergeSkipped, "reopened_after_close")
	if f.patched || len(f.posted) != 0 {
		t.Fatalf("patched=%v posted=%q, want a reopened checklist left open", f.patched, f.posted)
	}
}

func TestDedupeAdvisoryNoopCases(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*dedupeFixture)
		action CloseOnMergeAction
		reason string
	}{
		{"ref read fails", func(f *dedupeFixture) { f.failRefs = true }, CloseOnMergeNoop, "issue_read_failed"},
		{"no refs to track", func(f *dedupeFixture) {
			f.body = "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: no\n\nNothing to link.\n"
		}, CloseOnMergeNoop, "dedupe_advisory_no_refs_to_track"},
		{"close comment fails", func(f *dedupeFixture) { f.failPost = true }, CloseOnMergeNoop, "close_comment_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDedupeFixture(t, "no")
			tc.mutate(f)
			results := f.run(CloseOnMergeOptions{})
			assertDedupeResult(t, results, tc.action, tc.reason)
			if f.patched {
				t.Fatal("checklist was closed")
			}
		})
	}
}

// hivecommons/hive#15734: a "Decision required: yes" checklist parked at
// 045 with task lines. The sweep ticks the items whose subject closed or
// merged (one body write), leaves the rest, and never reclassifies or parks
// the checklist on a person; once the last item resolves it closes the
// checklist as completed with a note naming each item.
func TestDedupeAdvisoryTicksResolvedTaskLinesThenCloses(t *testing.T) {
	f := newDedupeFixture(t, "yes")
	f.labels = []string{"agent/dedupe", issueNeedsDecisionLabel}
	f.body = "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: yes\n\n## Near-duplicates\n\n" +
		"| Keep | Merge into it |\n|---|---|\n| PR #4 | PR #1 |\n\n" +
		"- [ ] #1 — close as superseded by #4\n- [ ] #2 — close as superseded by #5\n- [ ] PR #3 add `Fixes #6` to its body\n\n" +
		"## Checked and not duplicates\n\n- #7 / #8 — distinct\n"
	f.refStates = map[int]string{1: "closed", 2: "open", 3: "closed"}
	f.mergedRefs = map[int]bool{3: true}

	results := f.run(CloseOnMergeOptions{})
	assertDedupeResult(t, results, CloseOnMergeNoop, "dedupe_advisory_refs_open")
	if results[0].Ticked != 2 || f.bodyEdits != 1 {
		t.Fatalf("ticked=%d bodyEdits=%d, want 2 items ticked in one write", results[0].Ticked, f.bodyEdits)
	}
	if !strings.Contains(f.body, "- [x] #1 — close as superseded by #4") || !strings.Contains(f.body, "- [ ] #2 — close") || !strings.Contains(f.body, "- [x] PR #3 add") {
		t.Fatalf("body after tick:\n%s", f.body)
	}
	if f.refGets != 3 {
		t.Fatalf("ref reads = %d, want 3 (the task-line subjects only; #4–#8 are not items)", f.refGets)
	}
	if f.patched || len(f.posted) != 0 || len(f.added) != 0 || len(f.removed) != 0 {
		t.Fatalf("patched=%v posted=%q added=%v removed=%v, want nothing but the body edit", f.patched, f.posted, f.added, f.removed)
	}

	// Next sweep: ticked lines cost no read; nothing changed, nothing written.
	f.refGets, f.bodyEdits = 0, 0
	results = f.run(CloseOnMergeOptions{})
	assertDedupeResult(t, results, CloseOnMergeNoop, "dedupe_advisory_refs_open")
	if f.refGets != 1 || f.bodyEdits != 0 || results[0].Ticked != 0 {
		t.Fatalf("second sweep refGets=%d bodyEdits=%d ticked=%d, want one read (#2) and no write", f.refGets, f.bodyEdits, results[0].Ticked)
	}

	// #2 closes: the last item ticks and the checklist closes as completed.
	f.refStates[2] = "closed"
	results = f.run(CloseOnMergeOptions{})
	assertDedupeResult(t, results, CloseOnMergeClosed, "dedupe_advisory_refs_closed")
	if !f.patched || f.bodyEdits != 1 || !strings.Contains(f.body, "- [x] #2 — close") {
		t.Fatalf("patched=%v bodyEdits=%d body:\n%s", f.patched, f.bodyEdits, f.body)
	}
	want := dedupeAdvisoryCloseMarker + "\nAll 3 items resolved (#1 resolved, #2 closed, #3 resolved) — closing this advisory."
	if len(f.posted) != 1 || f.posted[0] != want {
		t.Fatalf("posted = %q, want %q", f.posted, want)
	}
}

func TestDedupeAdvisoryMergedItemSaysMerged(t *testing.T) {
	f := newDedupeFixture(t, "no")
	f.body = "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: no\n\n- [ ] PR #1 — add a link\n"
	f.refStates = map[int]string{1: "closed"}
	f.mergedRefs = map[int]bool{1: true}
	results := f.run(CloseOnMergeOptions{})
	assertDedupeResult(t, results, CloseOnMergeClosed, "dedupe_advisory_refs_closed")
	want := dedupeAdvisoryCloseMarker + "\nAll 1 item resolved (#1 merged) — closing this advisory."
	if len(f.posted) != 1 || f.posted[0] != want {
		t.Fatalf("posted = %q, want %q", f.posted, want)
	}
}

func TestDedupeAdvisoryAutocloseOffLeavesChecklistAlone(t *testing.T) {
	f := newDedupeFixture(t, "no")
	results := f.run(CloseOnMergeOptions{AdvisoryAutocloseOff: true})
	assertDedupeResult(t, results, CloseOnMergeSkipped, "dedupe_advisory_autoclose_off")
	if f.patched || f.refGets != 0 || len(f.posted) != 0 || f.bodyEdits != 0 {
		t.Fatalf("patched=%v refGets=%d posted=%q edits=%d, want untouched", f.patched, f.refGets, f.posted, f.bodyEdits)
	}
}

func TestDedupeAdvisoryBodyEditFailureRetriesNextSweep(t *testing.T) {
	f := newDedupeFixture(t, "yes")
	f.body = "<!-- hive:dedupe-checklist repo=o/r -->\nDecision required: yes\n\n- [ ] #1 — close\n- [ ] #2 — close\n"
	f.refStates = map[int]string{1: "closed", 2: "open"}
	f.failPatchBody = true
	results := f.run(CloseOnMergeOptions{})
	assertDedupeResult(t, results, CloseOnMergeNoop, "body_edit_failed")
	if results[0].Settles() {
		t.Fatal("a failed body edit must not settle; the next sweep retries it")
	}
}

func TestDedupeAdvisoryLeavesOtherIssuesAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*dedupeFixture)
	}{
		{"checklist a person wrote", func(f *dedupeFixture) { f.author = map[string]string{"login": "alice", "type": "User"} }},
		{"advisory label without checklist marker", func(f *dedupeFixture) { f.body = "weekly notes about #1 and #2" }},
		{"marker not at the top", func(f *dedupeFixture) { f.body = "intro\n" + dedupeTestBody("no") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDedupeFixture(t, "no")
			f.createdAt = dedupeTestNow.Add(-30 * 24 * time.Hour)
			tc.mutate(f)
			if results := f.run(CloseOnMergeOptions{}); len(results) != 0 {
				t.Fatalf("results = %+v, want none", results)
			}
			if f.patched || len(f.posted) != 0 || f.refGets != 0 {
				t.Fatalf("patched=%v posted=%q refGets=%d, want untouched", f.patched, f.posted, f.refGets)
			}
		})
	}
}

func TestParseDedupeChecklist(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		repo    string
		verdict dedupeChecklistVerdict
		ok      bool
	}{
		{"advisory", dedupeTestBody("no"), "o/r", dedupeVerdictNo, true},
		{"action required, CRLF and case", "\r\n<!-- hive:dedupe-checklist repo=a/b -->\r\n\r\ndecision REQUIRED: Yes\r\n", "a/b", dedupeVerdictYes, true},
		{"no verdict line", "<!-- hive:dedupe-checklist repo=a/b -->\n## Summary\nDecision required: yes\n", "a/b", dedupeVerdictUnknown, true},
		{"placeholder repo", "<!-- hive:dedupe-checklist repo=<owner/repo> -->\nDecision required: no\n", "", dedupeVerdictNo, true},
		{"not a checklist", "Decision required: yes", "", dedupeVerdictUnknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, verdict, ok := parseDedupeChecklist(tc.body)
			if repo != tc.repo || verdict != tc.verdict || ok != tc.ok {
				t.Fatalf("parseDedupeChecklist = (%q, %v, %v), want (%q, %v, %v)", repo, verdict, ok, tc.repo, tc.verdict, tc.ok)
			}
		})
	}
}

func TestDedupeChecklistRefs(t *testing.T) {
	body := "<!-- hive:dedupe-checklist repo=o/r #99 -->\n#1, #1 and x/y#2; this is o/r#50 and #50.\n"
	got := dedupeChecklistRefs(body, "o/r", "o/r", 50)
	want := []ClaimedRef{{Repo: "o/r", Issue: 1}, {Repo: "x/y", Issue: 2}}
	if len(got) != len(want) {
		t.Fatalf("refs = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("refs = %+v, want %+v", got, want)
		}
	}
}
