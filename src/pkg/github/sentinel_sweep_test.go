package github

// Tests for the sentinel sweep: label + one comment per head on a finding,
// nothing on a clean PR, no re-application once a maintainer clears the
// label for an unchanged head (including across a restart, where the
// clearance is read back from the alert comment), fail-closed on an
// incomplete file list.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
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
	removed     []string
	comments    map[int][]string
	// commentUsers[num][i] is the login the fixture reports as the author of
	// comments[num][i]; absent entries render a comment with no user, which
	// is how the fixture started and what the any-author fallback tests use.
	commentUsers map[int][]string
	// timeline[num] is the PR's issue timeline as GitHub would render it
	// (label events with actor and created_at, committed events with a
	// committer date, head_ref_force_pushed events); absent means empty.
	timeline map[int][]map[string]any
	// checkSuites[sha] and statuses[sha] are the server-stamped created_at
	// values of the check suites and commit statuses GitHub holds for that
	// SHA; absent means none.
	checkSuites map[string][]string
	statuses    map[string][]string
	filesHits   int
	listHits    int
	getHits     []int
	// repoPrivate is what GET /repos/o/r reports; repoStatus overrides the status code.
	repoPrivate bool
	repoStatus  int
}

// prByNumber returns the fixture PR with that number, or nil.
func (f *sentinelFixture) prByNumber(num int) map[string]any {
	for _, pr := range f.prs {
		if n, ok := pr["number"].(int); ok && n == num {
			return pr
		}
	}
	return nil
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
			case r.Method == http.MethodDelete && strings.HasPrefix(rest, "labels/"):
				f.removed = append(f.removed, fmt.Sprintf("%d|%s", num, strings.TrimPrefix(rest, "labels/")))
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode([]map[string]string{})
				return
			case r.Method == http.MethodGet && rest == "timeline":
				_ = json.NewEncoder(w).Encode(f.timeline[num])
				return
			case r.Method == http.MethodGet && rest == "comments":
				var out []map[string]any
				for i, body := range f.comments[num] {
					entry := map[string]any{"id": num*1000 + i + 1, "body": body}
					if users := f.commentUsers[num]; i < len(users) && users[i] != "" {
						entry["user"] = map[string]string{"login": users[i]}
					}
					out = append(out, entry)
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
		if r.Method == http.MethodGet && strings.HasPrefix(p, "/repos/o/r/pulls/") && !strings.Contains(strings.TrimPrefix(p, "/repos/o/r/pulls/"), "/") {
			n := 0
			for _, ch := range strings.TrimPrefix(p, "/repos/o/r/pulls/") {
				n = n*10 + int(ch-'0')
			}
			f.getHits = append(f.getHits, n)
			if pr := f.prByNumber(n); pr != nil {
				_ = json.NewEncoder(w).Encode(pr)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		if sha, rest, ok := strings.Cut(strings.TrimPrefix(p, "/repos/o/r/commits/"), "/"); ok && strings.HasPrefix(p, "/repos/o/r/commits/") && r.Method == http.MethodGet {
			switch rest {
			case "check-suites":
				var suites []map[string]any
				for i, at := range f.checkSuites[sha] {
					suites = append(suites, map[string]any{"id": i + 1, "head_sha": sha, "created_at": at})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(suites), "check_suites": suites})
				return
			case "statuses":
				out := []map[string]any{}
				for i, at := range f.statuses[sha] {
					out = append(out, map[string]any{"id": i + 1, "state": "success", "context": "ci", "created_at": at})
				}
				_ = json.NewEncoder(w).Encode(out)
				return
			}
		}
		switch {
		case r.Method == http.MethodGet && p == "/repos/o/r":
			if f.repoStatus != 0 {
				w.WriteHeader(f.repoStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"private": f.repoPrivate})
		case r.Method == http.MethodGet && p == "/repos/o/r/pulls":
			f.listHits++
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
		case r.Method == http.MethodPatch && strings.HasPrefix(p, "/repos/o/r/issues/comments/"):
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := 0
			for _, ch := range strings.TrimPrefix(p, "/repos/o/r/issues/comments/") {
				if ch < '0' || ch > '9' {
					id = 0
					break
				}
				id = id*10 + int(ch-'0')
			}
			num, idx := id/1000, id%1000-1
			if idx >= 0 && idx < len(f.comments[num]) {
				f.comments[num][idx] = body.Body
				_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "body": body.Body})
				return
			}
			// Newly-created comments in older tests use id=1.
			if id == 1 {
				for num := range f.comments {
					if len(f.comments[num]) == 1 {
						f.comments[num][0] = body.Body
						_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "body": body.Body})
						return
					}
				}
			}
			w.WriteHeader(http.StatusNotFound)
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

// A blocking alert comment for the current head with the label absent is a
// maintainer clearance. A fresh client (empty sentinelHeads, as after a hub
// restart or on a second replica) must not re-apply the label or touch the
// comment; the previous behaviour re-labelled #11614 on the first pass after
// a restart.
func TestSweepSentinelMaintainerClearanceSurvivesRestart(t *testing.T) {
	alert := SentinelComment(DecideSentinelAction("sentinel-alert", SentinelAuthorTrust{}, false), "h9", "eve", []sentinel.Finding{{Rule: "sensitive_path", Summary: "OWNERS", Paths: []string{"OWNERS"}}})
	f := &sentinelFixture{
		prs:         []map[string]any{sentinelPR(9, "eve", "h9")},
		files:       map[int][]map[string]any{9: {{"filename": "OWNERS", "status": "modified"}}},
		labelExists: true,
		comments:    map[int][]string{9: {alert}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	for pass := 0; pass < 2; pass++ {
		c := newTestClient(t, srv, "o", []string{"o/r"}) // fresh sentinelHeads each time
		res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
		if err != nil || len(res.Flagged) != 0 || res.Skipped != 1 {
			t.Fatalf("pass %d: cleared alert must stay quiet: res=%+v err=%v", pass, res, err)
		}
		if got := f.labels[9]; len(got) != 0 {
			t.Fatalf("pass %d: label re-applied after maintainer removed it: %v", pass, got)
		}
		if len(f.comments[9]) != 1 || f.comments[9][0] != alert {
			t.Fatalf("pass %d: alert comment must be left alone: %v", pass, f.comments[9])
		}
	}

	// A new push gets a fresh alert and the label again.
	f.mu.Lock()
	f.prs[0] = sentinelPR(9, "eve", "h10")
	f.mu.Unlock()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || len(res.Flagged) != 1 || len(f.labels[9]) != 1 || len(f.comments[9]) != 2 || !strings.Contains(f.comments[9][1], SentinelMarker+" h10") {
		t.Fatalf("new head must re-alert: res=%+v err=%v labels=%v comments=%d", res, err, f.labels[9], len(f.comments[9]))
	}
}

// sentinelTimelineLabel is a labeled/unlabeled timeline entry for the
// sentinel label by actor at the RFC3339 time at; actorType is GitHub's
// user type ("User" or "Bot").
func sentinelTimelineLabel(kind, actor, actorType, at string) map[string]any {
	return map[string]any{
		"event":      kind,
		"label":      map[string]string{"name": "sentinel-alert"},
		"actor":      map[string]string{"login": actor, "type": actorType},
		"created_at": at,
	}
}

// sentinelTimelineCommit is a committed timeline entry for sha whose
// committer date is at. The date is whatever the committer's clock (or
// GIT_COMMITTER_DATE) said, so the sweep must never read it as a push time.
func sentinelTimelineCommit(sha, at string) map[string]any {
	return map[string]any{
		"event":     "committed",
		"sha":       sha,
		"committer": map[string]any{"name": "eve", "date": at},
	}
}

// sentinelTimelineForcePush is a head_ref_force_pushed timeline entry that
// moved the head to sha at the server time at.
func sentinelTimelineForcePush(sha, at string) map[string]any {
	return map[string]any{
		"event":      "head_ref_force_pushed",
		"commit_id":  sha,
		"actor":      map[string]string{"login": "renovate[bot]", "type": "Bot"},
		"created_at": at,
	}
}

// A maintainer's clearance on a head the sweep never alerted on (#13602):
// the alert and marker are for head A, the PR moves to head B, and a human
// removes the label with B already in place. The next sweep must not
// re-label or post a new alert; it rewrites the hive's marker comment as
// the alert for B so the clearance survives a restart (the #11686 path).
// Every pass here starts a fresh client (empty sentinelHeads), the restart
// case from the acceptance criteria. The push time of B is read only from
// server-stamped signals: the force-push event naming B, or for an ordinary
// push the earliest check suite or commit status GitHub created for B. The
// committer date on B's commit is always later than the removal here, so a
// sweep that trusted it would wrongly re-alert.
func TestSweepSentinelClearanceOnNewerHeadIsHonoured(t *testing.T) {
	alertA := SentinelComment(DecideSentinelAction("sentinel-alert", SentinelAuthorTrust{}, false), "hA", "eve", []sentinel.Finding{{Rule: "sensitive_path", Summary: "OWNERS", Paths: []string{"OWNERS"}}})
	cases := []struct {
		name        string
		timeline    []map[string]any
		checkSuites map[string][]string
		statuses    map[string][]string
	}{
		{"force push", []map[string]any{
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineCommit("hB", "2026-10-22T23:00:00Z"),
			sentinelTimelineForcePush("hB", "2026-10-22T06:25:30Z"),
			sentinelTimelineLabel("unlabeled", "danathar", "User", "2026-10-22T18:19:00Z"),
		}, nil, nil},
		{"ordinary push with check suite", []map[string]any{
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineCommit("hB", "2026-10-22T23:00:00Z"),
			sentinelTimelineLabel("unlabeled", "danathar", "User", "2026-10-22T18:19:00Z"),
		}, map[string][]string{"hB": {"2026-10-22T06:25:31Z", "2026-10-22T06:25:30Z"}}, nil},
		{"ordinary push with commit status", []map[string]any{
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineCommit("hB", "2026-10-22T23:00:00Z"),
			sentinelTimelineLabel("unlabeled", "danathar", "User", "2026-10-22T18:19:00Z"),
		}, nil, map[string][]string{"hB": {"2026-10-22T06:25:30Z"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &sentinelFixture{
				prs:          []map[string]any{sentinelPR(9, "eve", "hB")},
				files:        map[int][]map[string]any{9: {{"filename": "OWNERS", "status": "modified"}}},
				labelExists:  true,
				comments:     map[int][]string{9: {alertA}},
				commentUsers: map[int][]string{9: {"hive[bot]"}},
				timeline:     map[int][]map[string]any{9: tc.timeline},
				checkSuites:  tc.checkSuites,
				statuses:     tc.statuses,
			}
			srv := newSentinelServer(t, f)
			defer srv.Close()
			for pass := 0; pass < 2; pass++ {
				c := newTestClient(t, srv, "o", []string{"o/r"})
				c.SetAppBotLogin("hive[bot]")
				res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
				if err != nil || len(res.Flagged) != 0 || res.Skipped != 1 {
					t.Fatalf("pass %d: clearance on newer head must stay quiet: res=%+v err=%v", pass, res, err)
				}
				if got := f.labels[9]; len(got) != 0 {
					t.Fatalf("pass %d: label re-applied after maintainer removed it on the current head: %v", pass, got)
				}
				if len(f.comments[9]) != 1 {
					t.Fatalf("pass %d: no new alert comment may be posted: %v", pass, f.comments[9])
				}
				if body := f.comments[9][0]; !strings.Contains(body, SentinelMarker+" hB") || !strings.Contains(body, sentinelAlertHeading) || !strings.Contains(body, "@danathar removed the `sentinel-alert` label") {
					t.Fatalf("pass %d: marker comment must be rewritten as the cleared alert for hB: %q", pass, body)
				}
			}

			// A push after the clearance re-alerts: the security property.
			// The new commit carries a committer date from before the
			// removal, as a contributor hiding a push behind the clearance
			// would set it; only GitHub's check-suite timestamp counts.
			f.mu.Lock()
			f.prs[0] = sentinelPR(9, "eve", "hC")
			f.timeline[9] = append(f.timeline[9], sentinelTimelineCommit("hC", "2026-10-22T02:00:00Z"))
			if f.checkSuites == nil {
				f.checkSuites = map[string][]string{}
			}
			f.checkSuites["hC"] = []string{"2026-10-22T19:00:00Z"}
			f.mu.Unlock()
			c := newTestClient(t, srv, "o", []string{"o/r"})
			c.SetAppBotLogin("hive[bot]")
			res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
			if err != nil || len(res.Flagged) != 1 || len(f.labels[9]) != 1 || len(f.comments[9]) != 2 || !strings.Contains(f.comments[9][1], SentinelMarker+" hC") {
				t.Fatalf("push after clearance must re-alert: res=%+v err=%v labels=%v comments=%d", res, err, f.labels[9], len(f.comments[9]))
			}
		})
	}
}

// A dashboard Approve releases the head it names (dibs#510): the alert and
// marker are for head A, the PR moves to head B without a fresh alert (the
// label from A still blocks it), and the owner approves B from the
// dashboard, which takes the label off as the hive's own account and posts
// the `/lgtm` + `/approve` comments bound to B. The `unlabeled` event is by
// a hive account and so is not a human clearance on its own; the approval
// comment is. The sweep must stay quiet for B across restarts, rewrite the
// marker as the released alert for B, and still re-alert on a later push.
func TestSweepSentinelDashboardApprovalReleasesHead(t *testing.T) {
	alertA := SentinelComment(DecideSentinelAction("sentinel-alert", SentinelAuthorTrust{}, false), "hA", "eve", []sentinel.Finding{{Rule: "sensitive_path", Summary: "workflows", Paths: []string{".github/workflows/ci.yml"}}})
	approvals := func(head string) []string {
		var out []string
		for _, command := range PRApprovalCommands {
			out = append(out, renderPRApprovalCommandComment(command, "clubanderson", head))
		}
		return out
	}
	timeline := []map[string]any{
		sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-28T23:30:22Z"),
		sentinelTimelineCommit("hB", "2026-10-28T23:55:54Z"),
		sentinelTimelineLabel("unlabeled", "hive[bot]", "Bot", "2026-10-29T00:32:07Z"),
	}
	f := &sentinelFixture{
		prs:          []map[string]any{sentinelPR(9, "eve", "hB")},
		files:        map[int][]map[string]any{9: {{"filename": ".github/workflows/ci.yml", "status": "modified"}}},
		labelExists:  true,
		comments:     map[int][]string{9: append([]string{alertA}, approvals("hB")...)},
		commentUsers: map[int][]string{9: {"hive[bot]", "hive[bot]", "hive[bot]"}},
		timeline:     map[int][]map[string]any{9: timeline},
		checkSuites:  map[string][]string{"hB": {"2026-10-28T23:56:00Z"}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	for pass := 0; pass < 2; pass++ {
		c := newTestClient(t, srv, "o", []string{"o/r"})
		c.SetAppBotLogin("hive[bot]")
		res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
		if err != nil || len(res.Flagged) != 0 || res.Skipped != 1 {
			t.Fatalf("pass %d: dashboard-approved head must stay quiet: res=%+v err=%v", pass, res, err)
		}
		if got := f.labels[9]; len(got) != 0 {
			t.Fatalf("pass %d: label re-applied after the owner approved this head: %v", pass, got)
		}
		if len(f.comments[9]) != 3 {
			t.Fatalf("pass %d: no new alert comment may be posted: %d comments", pass, len(f.comments[9]))
		}
		if body := f.comments[9][0]; !strings.Contains(body, SentinelMarker+" hB") || !strings.Contains(body, sentinelAlertHeading) || !strings.Contains(body, "@clubanderson approved this head from the Hive dashboard") {
			t.Fatalf("pass %d: marker comment must be rewritten as the released alert for hB: %q", pass, body)
		}
	}

	// New commits re-arm: the approval named hB, not hC.
	f.mu.Lock()
	f.prs[0] = sentinelPR(9, "eve", "hC")
	f.checkSuites["hC"] = []string{"2026-10-29T01:00:00Z"}
	f.mu.Unlock()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	c.SetAppBotLogin("hive[bot]")
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || len(res.Flagged) != 1 || len(f.labels[9]) != 1 || len(f.comments[9]) != 4 || !strings.Contains(f.comments[9][3], SentinelMarker+" hC") {
		t.Fatalf("push after the approval must re-alert: res=%+v err=%v labels=%v comments=%d", res, err, f.labels[9], len(f.comments[9]))
	}
}

// An approval comment releases nothing unless the hive wrote it for this
// head: a forged one by the PR author and the hive's own approval of an
// earlier head both leave the new head alerted on and labelled.
func TestSweepSentinelDashboardApprovalNotHonoured(t *testing.T) {
	alertA := SentinelComment(DecideSentinelAction("sentinel-alert", SentinelAuthorTrust{}, false), "hA", "eve", []sentinel.Finding{{Rule: "sensitive_path", Summary: "OWNERS", Paths: []string{"OWNERS"}}})
	approval := func(head string) string { return renderPRApprovalCommandComment("/approve", "clubanderson", head) }
	for _, tc := range []struct {
		name   string
		body   string
		author string
	}{
		{"forged by the author", approval("hB"), "eve"},
		{"hive approval of an earlier head", approval("hA"), "hive[bot]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &sentinelFixture{
				prs:          []map[string]any{sentinelPR(9, "eve", "hB")},
				files:        map[int][]map[string]any{9: {{"filename": "OWNERS", "status": "modified"}}},
				labelExists:  true,
				comments:     map[int][]string{9: {alertA, tc.body}},
				commentUsers: map[int][]string{9: {"hive[bot]", tc.author}},
				timeline: map[int][]map[string]any{9: {
					sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
					sentinelTimelineForcePush("hB", "2026-10-22T06:25:00Z"),
					sentinelTimelineLabel("unlabeled", "hive[bot]", "Bot", "2026-10-22T18:19:00Z"),
				}},
			}
			srv := newSentinelServer(t, f)
			defer srv.Close()
			c := newTestClient(t, srv, "o", []string{"o/r"})
			c.SetAppBotLogin("hive[bot]")
			res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
			if err != nil || len(res.Flagged) != 1 || len(f.labels[9]) != 1 || len(f.comments[9]) != 3 || !strings.Contains(f.comments[9][2], SentinelMarker+" hB") {
				t.Fatalf("new head must re-alert: res=%+v err=%v labels=%v comments=%v", res, err, f.labels[9], f.comments[9])
			}
		})
	}
}

// A hive that cannot name its own accounts (no configured identity, GET
// /user unresolved) keeps the legacy any-author rule for its other once-only
// markers, but not for the sentinel release: the PR author's forged
// approval comment for their own head must leave it alerted on and
// labelled (hivecommons/hive#15262).
func TestSweepSentinelDashboardApprovalIdentityUnknownForgedNotHonoured(t *testing.T) {
	alertA := SentinelComment(DecideSentinelAction("sentinel-alert", SentinelAuthorTrust{}, false), "hA", "eve", []sentinel.Finding{{Rule: "sensitive_path", Summary: "OWNERS", Paths: []string{"OWNERS"}}})
	f := &sentinelFixture{
		prs:          []map[string]any{sentinelPR(9, "eve", "hB")},
		files:        map[int][]map[string]any{9: {{"filename": "OWNERS", "status": "modified"}}},
		labelExists:  true,
		comments:     map[int][]string{9: {alertA, renderPRApprovalCommandComment("/approve", "clubanderson", "hB")}},
		commentUsers: map[int][]string{9: {"hive[bot]", "eve"}},
		timeline: map[int][]map[string]any{9: {
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineForcePush("hB", "2026-10-22T06:25:00Z"),
			sentinelTimelineLabel("unlabeled", "hive[bot]", "Bot", "2026-10-22T18:19:00Z"),
		}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	if c.knowsOwnAccounts() {
		t.Fatal("fixture must leave the hive unable to name its own accounts")
	}
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || len(res.Flagged) != 1 || len(f.labels[9]) != 1 || len(f.comments[9]) != 3 || !strings.Contains(f.comments[9][2], SentinelMarker+" hB") {
		t.Fatalf("forged approval must not release the head when identity is unknown: res=%+v err=%v labels=%v comments=%v", res, err, f.labels[9], f.comments[9])
	}
}

// A hive that cannot name its own accounts records a maintainer's clearance
// in a comment it posts itself rather than editing the newest marker comment
// of unverified authorship, which the PR author may have written.
func TestSweepSentinelClearanceIdentityUnknownPostsOwnRecord(t *testing.T) {
	alertA := SentinelComment(DecideSentinelAction("sentinel-alert", SentinelAuthorTrust{}, false), "hA", "eve", []sentinel.Finding{{Rule: "sensitive_path", Summary: "OWNERS", Paths: []string{"OWNERS"}}})
	forged := SentinelMarker + " hA\nnot the hive's\n"
	f := &sentinelFixture{
		prs:          []map[string]any{sentinelPR(9, "eve", "hB")},
		files:        map[int][]map[string]any{9: {{"filename": "OWNERS", "status": "modified"}}},
		labelExists:  true,
		comments:     map[int][]string{9: {alertA, forged}},
		commentUsers: map[int][]string{9: {"hive[bot]", "eve"}},
		timeline: map[int][]map[string]any{9: {
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineForcePush("hB", "2026-10-22T06:25:30Z"),
			sentinelTimelineLabel("unlabeled", "danathar", "User", "2026-10-22T18:19:00Z"),
		}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || len(res.Flagged) != 0 || len(f.labels[9]) != 0 {
		t.Fatalf("maintainer clearance must stay quiet: res=%+v err=%v labels=%v", res, err, f.labels[9])
	}
	if len(f.comments[9]) != 3 || f.comments[9][1] != forged || !strings.Contains(f.comments[9][2], SentinelMarker+" hB") {
		t.Fatalf("clearance must be posted as a new comment, leaving the unverified one alone: %v", f.comments[9])
	}
}

// The label removal counts only when it postdates a server-stamped push of
// the current head and was made by a human who is not one of the hive's
// accounts. A removal before the push, a removal by a hive account or
// another bot, a head whose only "push time" is its own committer date, a
// backdated commit pushed after the removal, and a force push of some
// other SHA each leave the existing behaviour: the new head is alerted on
// and labelled.
func TestSweepSentinelClearanceOnNewerHeadRejected(t *testing.T) {
	alertA := SentinelComment(DecideSentinelAction("sentinel-alert", SentinelAuthorTrust{}, false), "hA", "eve", []sentinel.Finding{{Rule: "sensitive_path", Summary: "OWNERS", Paths: []string{"OWNERS"}}})
	cases := []struct {
		name        string
		timeline    []map[string]any
		checkSuites map[string][]string
		statuses    map[string][]string
	}{
		{"removed before the force push", []map[string]any{
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineLabel("unlabeled", "danathar", "User", "2026-10-22T02:00:00Z"),
			sentinelTimelineForcePush("hB", "2026-10-22T06:25:00Z"),
		}, nil, nil},
		{"removed before the check suite", []map[string]any{
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineLabel("unlabeled", "danathar", "User", "2026-10-22T02:00:00Z"),
		}, map[string][]string{"hB": {"2026-10-22T06:25:00Z"}}, nil},
		{"removed by a hive account", []map[string]any{
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineForcePush("hB", "2026-10-22T06:25:00Z"),
			sentinelTimelineLabel("unlabeled", "hive[bot]", "Bot", "2026-10-22T18:19:00Z"),
		}, nil, nil},
		{"removed by another bot", []map[string]any{
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineForcePush("hB", "2026-10-22T06:25:00Z"),
			sentinelTimelineLabel("unlabeled", "renovate[bot]", "Bot", "2026-10-22T18:19:00Z"),
		}, nil, nil},
		{"committer date is not a push time", []map[string]any{
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineCommit("hB", "2026-10-22T06:25:00Z"),
			sentinelTimelineLabel("unlabeled", "danathar", "User", "2026-10-22T18:19:00Z"),
		}, nil, nil},
		{"backdated commit pushed after the removal", []map[string]any{
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineCommit("hB", "2026-10-22T06:25:00Z"),
			sentinelTimelineLabel("unlabeled", "danathar", "User", "2026-10-22T18:19:00Z"),
		}, map[string][]string{"hB": {"2026-10-22T19:00:00Z"}}, map[string][]string{"hB": {"2026-10-22T19:00:01Z"}}},
		{"force push of another sha", []map[string]any{
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineForcePush("hX", "2026-10-22T06:25:00Z"),
			sentinelTimelineLabel("unlabeled", "danathar", "User", "2026-10-22T18:19:00Z"),
		}, nil, nil},
		{"no push time known", []map[string]any{
			sentinelTimelineLabel("labeled", "hive[bot]", "Bot", "2026-10-22T01:00:00Z"),
			sentinelTimelineLabel("unlabeled", "danathar", "User", "2026-10-22T18:19:00Z"),
		}, nil, nil},
		{"empty timeline", nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &sentinelFixture{
				prs:          []map[string]any{sentinelPR(9, "eve", "hB")},
				files:        map[int][]map[string]any{9: {{"filename": "OWNERS", "status": "modified"}}},
				labelExists:  true,
				comments:     map[int][]string{9: {alertA}},
				commentUsers: map[int][]string{9: {"hive[bot]"}},
				timeline:     map[int][]map[string]any{9: tc.timeline},
				checkSuites:  tc.checkSuites,
				statuses:     tc.statuses,
			}
			srv := newSentinelServer(t, f)
			defer srv.Close()
			c := newTestClient(t, srv, "o", []string{"o/r"})
			c.SetAppBotLogin("hive[bot]")
			res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
			if err != nil || len(res.Flagged) != 1 {
				t.Fatalf("new head must re-alert: res=%+v err=%v", res, err)
			}
			if got := f.labels[9]; len(got) != 1 || got[0] != "sentinel-alert" {
				t.Fatalf("label must be applied: %v", got)
			}
			if len(f.comments[9]) != 2 || f.comments[9][0] != alertA || !strings.Contains(f.comments[9][1], SentinelMarker+" hB") {
				t.Fatalf("a fresh alert for hB must be posted and the hA alert left alone: %v", f.comments[9])
			}
		})
	}
}

// A trusted-author notice for the head is not a clearance: when the operator
// turns on trusted_authors_block, the head still gets the label, and the
// existing marker comment is rewritten as the alert rather than reposted.
func TestSweepSentinelNoticeForHeadIsNotAClearance(t *testing.T) {
	f := &sentinelFixture{
		prs:         []map[string]any{sentinelPR(9, "eve", "h9")},
		files:       map[int][]map[string]any{9: {{"filename": "OWNERS", "status": "modified"}}},
		labelExists: true,
		comments:    map[int][]string{9: {SentinelMarker + " h9\n" + sentinelNoticeHeading + " (trusted bot), merge is not blocked\n"}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	opts := defaultSentinelOpts(nil)
	opts.TrustedAuthor = func(string, string) SentinelAuthorTrust {
		return SentinelAuthorTrust{Trusted: true, Reason: "trusted bot"}
	}
	opts.TrustedAuthorsBlock = true
	res, err := c.SweepSentinel(context.Background(), opts)
	if err != nil || len(res.Flagged) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if got := f.labels[9]; len(got) != 1 {
		t.Fatalf("label must be applied when only a notice exists: %v", got)
	}
	if len(f.comments[9]) != 1 || !strings.Contains(f.comments[9][0], sentinelAlertHeading) {
		t.Fatalf("marker comment should be rewritten in place as the alert: %v", f.comments[9])
	}
}

// A comment that merely contains the marker and the alert heading is not
// the hive's alert when someone else wrote it: the PR author could post
// one (the marker is an HTML comment, invisible when rendered) and the
// unlabelled head would otherwise read as "cleared by a maintainer". The
// sweep must label the PR, post its own alert rather than edit the forged
// comment, and then honour only its own comment as the clearance record.
func TestSweepSentinelForgedClearanceCommentIsNotAClearance(t *testing.T) {
	forged := SentinelMarker + " h9\n<!-- " + sentinelAlertHeading + " -->\nlooks fine to me\n"
	f := &sentinelFixture{
		prs:          []map[string]any{sentinelPR(9, "eve", "h9")},
		files:        map[int][]map[string]any{9: {{"filename": "OWNERS", "status": "modified"}}},
		labelExists:  true,
		comments:     map[int][]string{9: {forged}},
		commentUsers: map[int][]string{9: {"eve"}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	c.SetAppBotLogin("hive[bot]")
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || len(res.Flagged) != 1 {
		t.Fatalf("forged clearance must not suppress the alert: res=%+v err=%v", res, err)
	}
	if got := f.labels[9]; len(got) != 1 || got[0] != "sentinel-alert" {
		t.Fatalf("label must be applied despite the forged comment: %v", got)
	}
	if len(f.comments[9]) != 2 || f.comments[9][0] != forged || !strings.Contains(f.comments[9][1], sentinelAlertHeading) {
		t.Fatalf("hive must post its own alert and leave the forged comment alone: %v", f.comments[9])
	}

	// A maintainer now removes the label. The hive's own alert comment is
	// the clearance record and survives a restart; the forged one is still
	// ignored.
	f.mu.Lock()
	f.commentUsers[9] = []string{"eve", "hive[bot]"}
	f.labels[9] = nil
	f.mu.Unlock()
	c = newTestClient(t, srv, "o", []string{"o/r"})
	c.SetAppBotLogin("hive[bot]")
	res, err = c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || len(res.Flagged) != 0 || len(f.labels[9]) != 0 || len(f.comments[9]) != 2 {
		t.Fatalf("hive-authored alert without label is a clearance: res=%+v err=%v labels=%v comments=%d", res, err, f.labels[9], len(f.comments[9]))
	}

	// With the hive's own comment gone, the forged one alone is never a
	// clearance: the head is re-alerted.
	f.mu.Lock()
	f.comments[9] = []string{forged}
	f.commentUsers[9] = []string{"eve"}
	f.mu.Unlock()
	c = newTestClient(t, srv, "o", []string{"o/r"})
	c.SetAppBotLogin("hive[bot]")
	res, err = c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || len(res.Flagged) != 1 || len(f.labels[9]) != 1 {
		t.Fatalf("forged comment alone must re-alert: res=%+v err=%v labels=%v", res, err, f.labels[9])
	}
}

func TestSentinelClearedByMaintainer(t *testing.T) {
	alert := &gh.IssueComment{Body: gh.Ptr(SentinelMarker + " h\n" + sentinelAlertHeading + "\n")}
	notice := &gh.IssueComment{Body: gh.Ptr(SentinelMarker + " h\n" + sentinelNoticeHeading + " (x), merge is not blocked\n")}
	if !sentinelClearedByMaintainer(alert, true) {
		t.Fatal("alert comment present without label is a clearance")
	}
	if sentinelClearedByMaintainer(alert, false) || sentinelClearedByMaintainer(nil, true) {
		t.Fatal("no comment is not a clearance")
	}
	if sentinelClearedByMaintainer(notice, true) {
		t.Fatal("a trusted-author notice is not a clearance")
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

func TestDecideSentinelActionTrustedTable(t *testing.T) {
	tests := []struct {
		name         string
		trust        SentinelAuthorTrust
		blockTrusted bool
		wantBlock    bool
		wantReason   string
	}{
		{name: "trusted agent", trust: SentinelAuthorTrust{Trusted: true, Reason: "hive agent"}, wantReason: "hive agent"},
		{name: "owner", trust: SentinelAuthorTrust{Trusted: true, Reason: "owner"}, wantReason: "owner"},
		{name: "allow-listed bot", trust: SentinelAuthorTrust{Trusted: true, Reason: "allow-listed bot"}, wantReason: "allow-listed bot"},
		{name: "untrusted fork author", trust: SentinelAuthorTrust{}, wantBlock: true},
		{name: "trusted but opt-in block", trust: SentinelAuthorTrust{Trusted: true, Reason: "hive agent"}, blockTrusted: true, wantBlock: true, wantReason: "hive agent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideSentinelAction("sentinel-alert", tc.trust, tc.blockTrusted)
			if got.Label != "sentinel-alert" || got.Block != tc.wantBlock || got.TrustedReason != tc.wantReason {
				t.Fatalf("decision=%+v, want block=%v reason=%q", got, tc.wantBlock, tc.wantReason)
			}
		})
	}
}

func TestSweepSentinelTrustedAuthorNoticeOnly(t *testing.T) {
	f := &sentinelFixture{
		prs: []map[string]any{sentinelPR(10, "hivecommons-hive[bot]", "ha")},
		files: map[int][]map[string]any{10: {{
			"filename": ".github/workflows/ci.yml", "status": "modified", "patch": "+permissions: write-all\n",
		}}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	opts := defaultSentinelOpts(nil)
	opts.TrustedAuthor = func(repo, author string) SentinelAuthorTrust {
		return SentinelAuthorTrust{Trusted: true, Reason: "hive agent"}
	}
	res, err := c.SweepSentinel(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Flagged) != 0 || len(res.Notified) != 1 || len(f.created) != 0 || len(f.labels[10]) != 0 {
		t.Fatalf("trusted author should notify only: res=%+v created=%v labels=%v", res, f.created, f.labels)
	}
	if len(f.comments[10]) != 1 {
		t.Fatalf("comments=%v", f.comments[10])
	}
	for _, want := range []string{"ℹ️ Sentinel notice", "author is trusted (hive agent)", "merge is not blocked", "did not add the `sentinel-alert` label"} {
		if !strings.Contains(f.comments[10][0], want) {
			t.Errorf("notice missing %q:\n%s", want, f.comments[10][0])
		}
	}
}

func TestSweepSentinelRemediatesTrustedAuthorOnlyWhenCommentSHAOk(t *testing.T) {
	f := &sentinelFixture{
		prs: []map[string]any{
			sentinelPR(20, "github-actions[bot]", "same", "sentinel-alert"),
			sentinelPR(21, "github-actions[bot]", "moved", "sentinel-alert"),
			sentinelPR(22, "mallory", "same", "sentinel-alert"),
		},
		files: map[int][]map[string]any{
			20: {{"filename": ".github/workflows/ci.yml", "status": "modified"}},
			21: {{"filename": ".github/workflows/ci.yml", "status": "modified"}},
			22: {{"filename": ".github/workflows/ci.yml", "status": "modified"}},
		},
		comments: map[int][]string{
			20: {SentinelMarker + " same\n## ⚠️ old alert"},
			21: {SentinelMarker + " old\n## ⚠️ old alert"},
			22: {SentinelMarker + " same\n## ⚠️ old alert"},
		},
		labelExists: true,
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	opts := defaultSentinelOpts(nil)
	opts.TrustedAuthor = func(repo, author string) SentinelAuthorTrust {
		if author == "github-actions[bot]" {
			return SentinelAuthorTrust{Trusted: true, Reason: "allow-listed bot"}
		}
		return SentinelAuthorTrust{}
	}
	res, err := c.SweepSentinel(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Remediated) != 1 || res.Remediated[0].Number != 20 || len(res.Flagged) != 0 {
		t.Fatalf("expected only PR 20 remediated: %+v", res)
	}
	if len(f.removed) != 1 || f.removed[0] != "20|sentinel-alert" {
		t.Fatalf("removed=%v", f.removed)
	}
	if !strings.Contains(f.comments[20][0], "ℹ️ Sentinel notice") || !strings.Contains(f.comments[20][0], "allow-listed bot") {
		t.Fatalf("comment was not rewritten as trusted notice:\n%s", f.comments[20][0])
	}
	if strings.Contains(f.comments[21][0], "ℹ️ Sentinel notice") || strings.Contains(f.comments[22][0], "ℹ️ Sentinel notice") {
		t.Fatalf("only matching trusted SHA should be rewritten: comments=%v", f.comments)
	}
}

func TestSentinelCommentShape(t *testing.T) {
	body := SentinelComment(SentinelDecision{Label: "lbl", Block: true}, "0123456789abcdef", "eve", []sentinel.Finding{
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

func TestSentinelCommentTrustedNoticeShape(t *testing.T) {
	body := SentinelComment(SentinelDecision{Label: "sentinel-alert", TrustedReason: "owner"}, "abc123", "clubanderson", []sentinel.Finding{
		{Rule: sentinel.RuleSensitivePath, Summary: "changes 1 sensitive path(s)", Paths: []string{".github/workflows/ci.yml"}},
	})
	for _, want := range []string{"ℹ️ Sentinel notice — informational; author is trusted (owner), merge is not blocked", "did not add the `sentinel-alert` label", "trusted_authors_block: true"} {
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
	if s.unchanged("k", "") {
		t.Fatal("an empty head is never treated as unchanged")
	}
}
