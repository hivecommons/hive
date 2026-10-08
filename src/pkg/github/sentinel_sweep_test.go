package github

// Tests for the sentinel sweep: label + one comment per head on a finding,
// nothing on a clean PR, no re-application once a maintainer clears the
// label for an unchanged head, fail-closed on an incomplete file list.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/sentinel"
)

type sentinelFixture struct {
	mu          sync.Mutex
	prs         []map[string]any
	files       map[int][]map[string]any
	filesStatus int
	labelExists bool
	created     []string
	labels      map[int][]string
	comments    map[int][]string
	filesHits   int
}

func newSentinelServer(t *testing.T, f *sentinelFixture) *httptest.Server {
	t.Helper()
	if f.labels == nil {
		f.labels = map[int][]string{}
	}
	if f.comments == nil {
		f.comments = map[int][]string{}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		var num int
		var rest string
		if n, _ := fmtSscanfPath(p, "/repos/o/r/pulls/%d/%s", &num, &rest); n == 2 {
			if r.Method == http.MethodGet && rest == "files" {
				f.filesHits++
				if f.filesStatus != 0 {
					w.WriteHeader(f.filesStatus)
					return
				}
				_ = json.NewEncoder(w).Encode(f.files[num])
				return
			}
		}
		if n, _ := fmtSscanfPath(p, "/repos/o/r/issues/%d/%s", &num, &rest); n == 2 {
			switch {
			case r.Method == http.MethodPost && rest == "labels":
				var labels []string
				_ = json.NewDecoder(r.Body).Decode(&labels)
				f.labels[num] = append(f.labels[num], labels...)
				_ = json.NewEncoder(w).Encode([]map[string]string{{"name": labels[0]}})
				return
			case r.Method == http.MethodGet && rest == "comments":
				var out []map[string]any
				for _, body := range f.comments[num] {
					out = append(out, map[string]any{"body": body})
				}
				_ = json.NewEncoder(w).Encode(out)
				return
			case r.Method == http.MethodPost && rest == "comments":
				var body struct {
					Body string `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				f.comments[num] = append(f.comments[num], body.Body)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "body": body.Body})
				return
			}
		}
		switch {
		case r.Method == http.MethodGet && p == "/repos/o/r/pulls":
			_ = json.NewEncoder(w).Encode(f.prs)
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/repos/o/r/labels/"):
			if f.labelExists {
				_ = json.NewEncoder(w).Encode(map[string]string{"name": "sentinel-alert"})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		case r.Method == http.MethodPost && p == "/repos/o/r/labels":
			var lbl map[string]string
			_ = json.NewDecoder(r.Body).Decode(&lbl)
			f.created = append(f.created, lbl["name"]+"|"+lbl["color"])
			f.labelExists = true
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(lbl)
		default:
			t.Logf("unexpected %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// fmtSscanfPath is a tiny path matcher: it handles the "/repos/o/r/<kind>/%d/%s"
// shapes the fixture needs without pulling in fmt.Sscanf's whitespace rules.
func fmtSscanfPath(p, pattern string, num *int, rest *string) (int, error) {
	prefix := pattern[:strings.Index(pattern, "%d")]
	if !strings.HasPrefix(p, prefix) {
		return 0, nil
	}
	tail := strings.TrimPrefix(p, prefix)
	parts := strings.SplitN(tail, "/", 2)
	if len(parts) != 2 {
		return 0, nil
	}
	n := 0
	for _, ch := range parts[0] {
		if ch < '0' || ch > '9' {
			return 0, nil
		}
		n = n*10 + int(ch-'0')
	}
	*num = n
	*rest = parts[1]
	return 2, nil
}

func sentinelPR(num int, author, head string, labels ...string) map[string]any {
	var ls []map[string]string
	for _, l := range labels {
		ls = append(ls, map[string]string{"name": l})
	}
	return map[string]any{
		"number": num, "title": "pr " + author, "state": "open",
		"user":   map[string]string{"login": author},
		"head":   map[string]string{"sha": head},
		"labels": ls, "changed_files": 1,
	}
}

func defaultSentinelOpts(audit func(SentinelSweepEvent)) SentinelSweepOptions {
	return SentinelSweepOptions{
		Label: "sentinel-alert", LabelColor: "b60205", LabelDescription: "desc",
		Audit: audit,
	}
}

func TestSweepSentinelFlagsOwnersSelfNomination(t *testing.T) {
	f := &sentinelFixture{
		prs: []map[string]any{sentinelPR(7, "vjymisal0", "aaa111")},
		files: map[int][]map[string]any{7: {{
			"filename": "OWNERS", "status": "modified", "additions": 1, "deletions": 0,
			"patch": "@@ -5,3 +5,4 @@\n reviewers:\n   - clubanderson\n+  - vjymisal0\n",
		}}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})

	var audited []SentinelSweepEvent
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(func(e SentinelSweepEvent) { audited = append(audited, e) }))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Seen != 1 || len(res.Flagged) != 1 || res.Skipped != 0 || len(res.Errors) != 0 {
		t.Fatalf("result=%+v", res)
	}
	ev := res.Flagged[0]
	if ev.Repo != "o/r" || ev.Number != 7 || ev.Author != "vjymisal0" || ev.HeadSHA != "aaa111" {
		t.Fatalf("event=%+v", ev)
	}
	rules := ev.Rules()
	if len(rules) != 2 || rules[0] != sentinel.RuleSensitivePath || rules[1] != sentinel.RuleOwnerSelfNomination {
		t.Fatalf("rules=%v", rules)
	}
	if len(audited) != 1 {
		t.Fatalf("audit calls=%d", len(audited))
	}
	if got := f.labels[7]; len(got) != 1 || got[0] != "sentinel-alert" {
		t.Fatalf("labels=%v", got)
	}
	if len(f.created) != 1 || f.created[0] != "sentinel-alert|b60205" {
		t.Fatalf("label creation=%v", f.created)
	}
	if len(f.comments[7]) != 1 {
		t.Fatalf("comments=%v", f.comments[7])
	}
	body := f.comments[7][0]
	for _, want := range []string{SentinelMarker + " aaa111", "owner_self_nomination", "@vjymisal0", "`OWNERS`", "`sentinel-alert` label"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment missing %q:\n%s", want, body)
		}
	}
	if res.ReposHit["o/r"] != 1 {
		t.Fatalf("reposHit=%v", res.ReposHit)
	}

	// Second pass, same head: no files fetch, no second comment, no relabel.
	hits := f.filesHits
	res, err = c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if len(res.Flagged) != 0 || res.Skipped != 1 || f.filesHits != hits || len(f.comments[7]) != 1 || len(f.labels[7]) != 1 {
		t.Fatalf("second pass should be a no-op: res=%+v files=%d comments=%d labels=%d", res, f.filesHits, len(f.comments[7]), len(f.labels[7]))
	}
}

func TestSweepSentinelCleanPRUntouched(t *testing.T) {
	f := &sentinelFixture{
		prs: []map[string]any{sentinelPR(1, "alice", "c1")},
		files: map[int][]map[string]any{1: {{
			"filename": "pkg/x/x.go", "status": "modified", "additions": 2, "deletions": 1, "patch": "@@ -1 +1 @@\n-a\n+b\n",
		}}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Flagged) != 0 || res.Skipped != 1 || len(f.labels) != 0 || len(f.comments) != 0 || len(f.created) != 0 {
		t.Fatalf("clean PR must be untouched: res=%+v labels=%v comments=%v created=%v", res, f.labels, f.comments, f.created)
	}
}

func TestSweepSentinelSkipsLabelledExemptAndNewHeadRecomments(t *testing.T) {
	f := &sentinelFixture{
		prs: []map[string]any{
			sentinelPR(2, "mallory", "h2", "sentinel-alert"), // already alerted
			sentinelPR(3, "dependabot[bot]", "h3"),           // exempt
			sentinelPR(4, "eve", "h4"),
		},
		files: map[int][]map[string]any{
			2: {{"filename": "OWNERS", "status": "modified"}},
			3: {{"filename": ".github/workflows/ci.yml", "status": "modified", "patch": "+permissions: write-all\n"}},
			4: {{"filename": "SECURITY.md", "status": "removed"}},
		},
		labelExists: true,
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	opts := defaultSentinelOpts(nil)
	opts.Evaluator = sentinel.Config{ExemptLogins: []string{"dependabot[bot]"}}
	res, err := c.SweepSentinel(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Seen != 3 || len(res.Flagged) != 1 || res.Flagged[0].Number != 4 || res.Skipped != 2 {
		t.Fatalf("res=%+v", res)
	}
	if len(f.created) != 0 {
		t.Fatalf("existing label must not be recreated: %v", f.created)
	}
	if _, ok := f.labels[2]; ok {
		t.Fatal("already-labelled PR must not be relabelled")
	}
	if _, ok := f.labels[3]; ok {
		t.Fatal("exempt author must not be labelled")
	}

	// Maintainer removes the label; same head → stays quiet. New push → new
	// comment carrying the new head marker.
	f.mu.Lock()
	f.prs[2] = sentinelPR(4, "eve", "h4")
	f.mu.Unlock()
	res, _ = c.SweepSentinel(context.Background(), opts)
	if len(res.Flagged) != 0 {
		t.Fatalf("cleared label on unchanged head must stay quiet: %+v", res)
	}
	f.mu.Lock()
	f.prs[2] = sentinelPR(4, "eve", "h5")
	f.mu.Unlock()
	res, _ = c.SweepSentinel(context.Background(), opts)
	if len(res.Flagged) != 1 || len(f.comments[4]) != 2 || !strings.Contains(f.comments[4][1], SentinelMarker+" h5") {
		t.Fatalf("new head should re-alert: res=%+v comments=%v", res, f.comments[4])
	}
}

func TestSweepSentinelExistingMarkerCommentNotReposted(t *testing.T) {
	f := &sentinelFixture{
		prs:         []map[string]any{sentinelPR(9, "eve", "h9")},
		files:       map[int][]map[string]any{9: {{"filename": "OWNERS", "status": "modified"}}},
		labelExists: true,
		comments:    map[int][]string{9: {SentinelMarker + " h9\nolder alert"}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || len(res.Flagged) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(f.comments[9]) != 1 {
		t.Fatalf("marker already present for this head; comments=%v", f.comments[9])
	}
	if got := f.labels[9]; len(got) != 1 {
		t.Fatalf("label should still be (re)applied: %v", got)
	}
}

func TestSweepSentinelFilesErrorFailsClosed(t *testing.T) {
	f := &sentinelFixture{
		prs:         []map[string]any{sentinelPR(5, "eve", "h5")},
		filesStatus: http.StatusBadGateway,
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Flagged) != 0 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "o/r#5") {
		t.Fatalf("expected one per-PR error, got %+v", res)
	}
	if len(f.labels) != 0 {
		t.Fatal("no label on error")
	}

	// Incomplete list (reported > returned) is also an error.
	f.mu.Lock()
	f.filesStatus = 0
	f.prs[0]["changed_files"] = 3
	f.prs[0]["head"] = map[string]string{"sha": "h6"}
	f.files = map[int][]map[string]any{5: {{"filename": "a.go"}}}
	f.mu.Unlock()
	res, _ = c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "incomplete") {
		t.Fatalf("expected incomplete error, got %+v", res)
	}
}

func TestSweepSentinelOptionsAndGuards(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.SweepSentinel(context.Background(), SentinelSweepOptions{Label: "x"}); err != ErrNoGitHubClient {
		t.Fatalf("nil client: %v", err)
	}
	f := &sentinelFixture{
		prs: []map[string]any{sentinelPR(1, "eve", "a"), sentinelPR(2, "eve", "b")},
		files: map[int][]map[string]any{
			1: {{"filename": "OWNERS", "status": "modified"}},
			2: {{"filename": "OWNERS", "status": "modified"}},
		},
		labelExists: true,
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r", "o/other"})
	if _, err := c.SweepSentinel(context.Background(), SentinelSweepOptions{}); err == nil {
		t.Fatal("blank label must be rejected")
	}

	// Repo filter excludes everything → nothing seen.
	opts := defaultSentinelOpts(nil)
	opts.RepoAllowed = func(repo string) bool { return false }
	res, err := c.SweepSentinel(context.Background(), opts)
	if err != nil || res.Seen != 0 {
		t.Fatalf("filtered sweep: res=%+v err=%v", res, err)
	}

	// MaxActions caps flagged PRs per pass; the unknown repo lists as an error.
	opts = defaultSentinelOpts(nil)
	opts.RepoAllowed = func(repo string) bool { return true }
	opts.MaxActions = 1
	res, err = c.SweepSentinel(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Flagged) != 1 {
		t.Fatalf("max_actions=1 should flag exactly one: %+v", res)
	}
}

func TestSentinelCommentShape(t *testing.T) {
	body := SentinelComment("lbl", "0123456789abcdef", "eve", []sentinel.Finding{
		{Rule: sentinel.RuleTestRemoval, Summary: "deletes 2 test file(s)", Paths: []string{"b_test.go", "a_test.go"}},
		{Rule: "custom", Summary: "no description"},
	})
	if !strings.HasPrefix(body, SentinelMarker+" 0123456789abcdef\n") {
		t.Fatalf("marker line: %q", body[:40])
	}
	for _, want := range []string{"`012345678", "@eve", "**test_removal**", "  - `a_test.go`\n  - `b_test.go`", "**custom** — no description\n", "`lbl` label", "`sentinel`"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestSentinelHeadsSeen(t *testing.T) {
	var s sentinelHeadsSeen
	if s.unchanged("k", "a") {
		t.Fatal("first sighting is a change")
	}
	if !s.unchanged("k", "a") {
		t.Fatal("same head is unchanged")
	}
	if s.unchanged("k", "b") {
		t.Fatal("new head is a change")
	}
	if s.unchanged("k", "") || s.unchanged("k", "") {
		t.Fatal("an empty head is never treated as unchanged")
	}
}
