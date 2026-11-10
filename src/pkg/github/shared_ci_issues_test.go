package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/redmain"
)

const sharedCIIssueZizmorLine = "zizmor: unit-tests.yml:45: action's hash pin has mismatched or missing version comment: tag points to commit cfdb7ed4ccc7"

// zizmorPR is a red PR shaped like projectbluefin/server on 2026-11-10:
// zizmor failing on a head whose own diff did not touch the pin.
func zizmorPR(number int, fork bool) PullRequest {
	return PullRequest{
		Repo: "widget", Number: number, CIStatus: "failure", FromFork: fork,
		FailingChecks:    []string{"zizmor"},
		CIFailureExcerpt: sharedCIIssueZizmorLine,
	}
}

// One check red on N+ open PRs is a cluster, whoever authored them; a check
// red on fewer PRs is not; a fork-publication guard (red on every fork PR by
// design) never counts; a tripped red-main fact is a cluster even with no
// red PR.
func TestSharedCIClustersFindsCheckFailingAcrossPRs(t *testing.T) {
	both := zizmorPR(649, true)
	both.FailingChecks = []string{"zizmor", "unit"}
	both.CIFailureExcerpt = sharedCIIssueZizmorLine + "\nunit: TestBoot failed"
	prs := []PullRequest{zizmorPR(642, false), zizmorPR(594, true), zizmorPR(613, true), both}
	for _, n := range []int{972, 978, 980} {
		prs = append(prs, PullRequest{
			Repo: "widget", Number: n, CIStatus: "failure", FromFork: true,
			FailingChecks:    []string{"Canary"},
			CIFailureExcerpt: "Canary: a fork pull request cannot publish the canary image",
		})
	}
	prs = append(prs, PullRequest{Repo: "widget", Number: 700, CIStatus: "success"})
	since := time.Date(2026, 11, 9, 6, 16, 0, 0, time.UTC)
	facts := map[string]*redmain.Fact{"acme/other": {Repo: "acme/other", Branch: "main", Check: "build", Since: since, RunURL: "https://github.test/acme/other/runs/1"}}
	repoKey := func(r string) string {
		if strings.Contains(r, "/") {
			return r
		}
		return "acme/" + r
	}

	got := SharedCIClusters(prs, facts, repoKey, 3)
	if len(got) != 2 {
		t.Fatalf("clusters = %+v, want zizmor on acme/widget and build on acme/other", got)
	}
	z := got[0]
	if z.Repo != "acme/widget" || z.Check != "zizmor" || z.Main != nil || z.BaseRed {
		t.Fatalf("first cluster = %+v", z)
	}
	if want := []int{594, 613, 642, 649}; len(z.PRs) != len(want) || z.PRs[0] != 594 || z.PRs[3] != 649 {
		t.Fatalf("zizmor PRs = %v, want %v", z.PRs, want)
	}
	if len(z.Excerpt) != 1 || !strings.HasPrefix(z.Excerpt[0], "unit-tests.yml:45: action's hash pin") {
		t.Fatalf("excerpt = %q, want the one distinct zizmor line without its check prefix", z.Excerpt)
	}
	m := got[1]
	if m.Repo != "acme/other" || m.Check != "build" || m.Main == nil || len(m.PRs) != 0 {
		t.Fatalf("red-main cluster = %+v", m)
	}

	body := RenderSharedCIIssueBody(z)
	for _, want := range []string{SharedCIIssueMarker, "`zizmor` check is failing on 4 open pull requests in acme/widget", "#594, #613, #642, #649", "unit-tests.yml:45", "base branch is not red"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %q:\n%s", want, body)
		}
	}
	if span, bad := bodyPlaceholder(body); bad {
		t.Fatalf("body trips the issue template guard on %q", span)
	}
	if mb := RenderSharedCIIssueBody(m); !strings.Contains(mb, "red on `main` of acme/other since 2026-11-09 06:16 UTC") || !strings.Contains(mb, "runs/1") {
		t.Fatalf("red-main body:\n%s", mb)
	}
	if line := sharedCIIssueExcerptLine("expected <some value here> got ```x```"); strings.ContainsAny(line, "<>") || strings.Contains(line, "```") {
		t.Fatalf("excerpt line not made safe: %q", line)
	}
	if title := SharedCIIssueTitle("zizmor", "acme/widget"); title != "[shared-ci] zizmor failing across acme/widget" {
		t.Fatalf("title = %q", title)
	} else if check, ok := SharedCITrackingCheck(title, "acme/widget"); !ok || check != "zizmor" {
		t.Fatalf("the title must parse as the incident the red-main breaker and the rerun sweep read: %q %v", check, ok)
	}
	if check, ok := sharedCIIncidentCheck("[scanner] [shared-ci] zizmor failing across acme/widget", "acme/widget"); !ok || check != "zizmor" {
		t.Fatalf("an agent-tagged incident title must count as the incident: %q %v", check, ok)
	}
	if _, ok := sharedCIIncidentCheck("[scanner] zizmor is red", "acme/widget"); ok {
		t.Fatal("an ordinary agent issue is not an incident")
	}
	if got := SharedCIClusters(prs, nil, repoKey, 5); len(got) != 0 {
		t.Fatalf("min 5 PRs: clusters = %+v", got)
	}
}

// sharedCIIssueServer serves acme/widget's issue list and records creates
// and edits. open is what the open-issue listing returns; closed what the
// closed-issue listing returns.
type sharedCIIssueServer struct {
	mu      sync.Mutex
	open    []map[string]any
	closed  []map[string]any
	created []map[string]any
	edits   []string
}

func (s *sharedCIIssueServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/user":
			_, _ = io.WriteString(w, `{"login":"hive-bot"}`)
		case strings.Contains(r.URL.Path, "/labels"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues":
			list := []map[string]any{}
			switch r.URL.Query().Get("state") {
			case "open", "":
				list = s.open
			case "closed":
				list = s.closed
			}
			_ = json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues":
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			s.created = append(s.created, req)
			_, _ = io.WriteString(w, `{"id":7701,"number":77,"html_url":"https://github.test/acme/widget/issues/77"}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/widget/issues/77":
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			body, _ := req["body"].(string)
			s.edits = append(s.edits, body)
			_, _ = io.WriteString(w, `{"number":77}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// The sweep files ONE tracking issue for a settled cluster, under the title
// the agents and the red-main breaker use, then keeps that issue's body in
// step (one edit per change, none when nothing changed) and never edits an
// incident it did not file.
func TestSweepSharedCIIssuesFilesOnceThenUpdatesOwnIssue(t *testing.T) {
	s := &sharedCIIssueServer{}
	srv := httptest.NewServer(s.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widget"})
	tracker := NewSharedCIIssueTracker()
	t0 := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	snap := &ActionableResult{}
	snap.PRs.Items = []PullRequest{zizmorPR(594, true), zizmorPR(637, false), zizmorPR(642, false)}
	var events []SharedCIIssueEvent
	pass := func(at time.Time) {
		t.Helper()
		if _, err := c.SweepSharedCIIssues(context.Background(), snap, SharedCIIssueOptions{
			Tracker: tracker, Now: func() time.Time { return at },
			Audit: func(ev SharedCIIssueEvent) { events = append(events, ev) },
		}); err != nil {
			t.Fatal(err)
		}
	}

	pass(t0)
	if len(s.created) != 0 {
		t.Fatalf("filed on first sight, before the cluster settled: %+v", s.created)
	}
	pass(t0.Add(sharedCIIssueSettle + time.Minute))
	if len(s.created) != 1 {
		t.Fatalf("creates = %d, want 1", len(s.created))
	}
	if title := s.created[0]["title"]; title != "[shared-ci] zizmor failing across acme/widget" {
		t.Fatalf("title = %v", title)
	}
	body, _ := s.created[0]["body"].(string)
	if !strings.Contains(body, SharedCIIssueMarker) || !strings.Contains(body, "#594, #637, #642") {
		t.Fatalf("body:\n%s", body)
	}
	labels, _ := json.Marshal(s.created[0]["labels"])
	if !strings.Contains(string(labels), "agent/ci-maintainer") {
		t.Fatalf("labels = %s, want the ci-maintainer lane", labels)
	}
	if len(events) != 1 || events[0].Action != "filed" || events[0].Issue != 77 {
		t.Fatalf("events = %+v", events)
	}

	// GitHub now lists the issue. The same failure costs no edit.
	s.mu.Lock()
	s.open = []map[string]any{{"number": 77, "title": "[shared-ci] zizmor failing across acme/widget", "body": body, "user": map[string]any{"login": "hive-bot"}}}
	s.mu.Unlock()
	pass(t0.Add(time.Hour))
	if len(s.created) != 1 || len(s.edits) != 0 {
		t.Fatalf("unchanged failure: creates=%d edits=%d", len(s.created), len(s.edits))
	}

	// A fourth PR goes red: one edit, naming it.
	snap.PRs.Items = append(snap.PRs.Items, zizmorPR(645, true))
	pass(t0.Add(75 * time.Minute))
	if len(s.created) != 1 || len(s.edits) != 1 || !strings.Contains(s.edits[0], "#645") {
		t.Fatalf("grown failure: creates=%d edits=%v", len(s.created), s.edits)
	}
	if last := events[len(events)-1]; last.Action != "updated" || len(last.PRs) != 4 {
		t.Fatalf("last event = %+v", last)
	}

	// An incident an agent or a person opened is theirs: never edited,
	// never duplicated.
	s.mu.Lock()
	s.open = []map[string]any{{"number": 77, "title": "[shared-ci] zizmor failing across acme/widget", "body": "Filed by hand.", "user": map[string]any{"login": "kylerankin"}}}
	s.mu.Unlock()
	snap.PRs.Items = append(snap.PRs.Items, zizmorPR(647, true))
	pass(t0.Add(90 * time.Minute))
	if len(s.created) != 1 || len(s.edits) != 1 {
		t.Fatalf("foreign incident: creates=%d edits=%d", len(s.created), len(s.edits))
	}
}

// A recently closed incident for the same check holds a new filing back; a
// red-main fact files at once (the breaker already waited) and tells the
// breaker its tracking issue.
func TestSweepSharedCIIssuesRespectsRecentCloseAndFilesRedMain(t *testing.T) {
	s := &sharedCIIssueServer{}
	srv := httptest.NewServer(s.handler(t))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widget"})
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	since := now.Add(-30 * time.Hour)
	c.ObserveRedMain("widget", "main", map[string]redmain.RedRun{"build": {URL: "https://github.test/acme/widget/runs/9", CompletedAt: since}}, since)
	fact := c.RedMainFact("widget", now)
	if fact == nil {
		t.Fatal("red-main fact not tripped")
	}
	snap := &ActionableResult{RedMain: map[string]*redmain.Fact{"acme/widget": fact}}
	snap.PRs.Items = []PullRequest{zizmorPR(594, true), zizmorPR(637, false), zizmorPR(642, false)}

	s.closed = []map[string]any{{
		"number": 70, "title": "[shared-ci] zizmor failing across acme/widget",
		"state": "closed", "state_reason": "completed", "closed_at": now.Add(-2 * time.Hour).Format(time.RFC3339),
	}}
	res, err := c.SweepSharedCIIssues(context.Background(), snap, SharedCIIssueOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.created) != 1 || s.created[0]["title"] != "[shared-ci] build failing across acme/widget" {
		t.Fatalf("creates = %+v, want only the red-main build incident (zizmor closed 2h ago)", s.created)
	}
	if res.Clusters != 2 || len(res.Events) != 1 || !res.Events[0].OnMain {
		t.Fatalf("result = %+v", res)
	}
	if f := c.RedMainFact("widget", now); f == nil || f.TrackingIssue != 77 {
		t.Fatalf("red-main fact after filing = %+v, want tracking issue 77", f)
	}
}
