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

	refGets  int
	posted   []string
	patched  bool
	added    []string
	removed  []string
	unexpect []string
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

func (f *dedupeFixture) issueJSON() map[string]any {
	labels := make([]map[string]string, 0, len(f.labels))
	for _, l := range f.labels {
		labels = append(labels, map[string]string{"name": l})
	}
	return map[string]any{
		"number": dedupeTestChecklist, "state": "open", "body": f.body,
		"created_at": f.createdAt.Format(time.RFC3339),
		"labels":     labels,
		"user":       map[string]string{"login": "hive[bot]", "type": "Bot"},
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
			f.patched = true
			_ = json.NewEncoder(w).Encode(map[string]any{"number": dedupeTestChecklist, "state": "closed"})
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
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": state})
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
	if len(f.posted) != 1 || f.posted[0] != dedupeAdvisoryRefsClosedComment {
		t.Fatalf("posted = %q, want the one-line refs-closed comment", f.posted)
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
	f.comments = []string{dedupeAdvisoryRefsClosedComment}
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
		{"also parked", func(f *dedupeFixture) { f.labels = append(f.labels, issueNeedsDecisionLabel) }, CloseOnMergeSkipped, "dedupe_advisory_parked"},
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

func TestDedupeAdvisoryReclassifiesOnEditOnce(t *testing.T) {
	f := newDedupeFixture(t, "yes")
	results := f.run(CloseOnMergeOptions{})
	assertDedupeResult(t, results, CloseOnMergeSkipped, "dedupe_reclassified_needs_decision")
	if len(f.added) != 1 || f.added[0] != NeedsDecisionStageLabel {
		t.Fatalf("added labels = %v, want [%s]", f.added, NeedsDecisionStageLabel)
	}
	if len(f.removed) != 1 || f.removed[0] != dedupeAdvisoryLabelName {
		t.Fatalf("removed labels = %v, want [%s]", f.removed, dedupeAdvisoryLabelName)
	}
	if len(f.posted) != 1 || !strings.HasPrefix(f.posted[0], unparkNoticeMarker) {
		t.Fatalf("posted = %q, want one unpark notice", f.posted)
	}
	if f.patched || f.refGets != 0 {
		t.Fatalf("patched=%v refGets=%d, want an action-required checklist neither closed nor ref-checked", f.patched, f.refGets)
	}

	// The next sweep sees a parked, non-advisory issue and leaves it to the
	// unpark sweep.
	if results := f.run(CloseOnMergeOptions{}); len(results) != 0 {
		t.Fatalf("second sweep results = %+v, want none", results)
	}
	if len(f.posted) != 1 || len(f.added) != 1 {
		t.Fatalf("second sweep posted=%d added=%v, want nothing new", len(f.posted), f.added)
	}
}

func TestDedupeAdvisoryReclassifyLabelDrivenPostsNoNotice(t *testing.T) {
	f := newDedupeFixture(t, "yes")
	results := f.run(CloseOnMergeOptions{LabelDriven: func(repo string) bool { return repo == "o/r" }})
	assertDedupeResult(t, results, CloseOnMergeSkipped, "dedupe_reclassified_needs_decision")
	if len(f.posted) != 0 {
		t.Fatalf("posted = %q, want no notice on a label-driven repo", f.posted)
	}
	if len(f.added) != 1 || len(f.removed) != 1 {
		t.Fatalf("added=%v removed=%v, want the label swap", f.added, f.removed)
	}
}

func TestDedupeAdvisoryReclassifyLabelFailureKeepsAdvisory(t *testing.T) {
	f := newDedupeFixture(t, "yes")
	f.failLabel = true
	results := f.run(CloseOnMergeOptions{})
	assertDedupeResult(t, results, CloseOnMergeNoop, "label_failed")
	if len(f.removed) != 0 || len(f.posted) != 0 {
		t.Fatalf("removed=%v posted=%q, want triage/advisory kept so the next sweep retries", f.removed, f.posted)
	}
}

func TestDedupeAdvisoryLeavesOtherIssuesAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*dedupeFixture)
	}{
		{"checklist without triage/advisory", func(f *dedupeFixture) { f.labels = []string{issueNeedsDecisionLabel} }},
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
